package main

import (
	"context"
	"net/netip"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/driver"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/network"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

type primaryRuntimeOptions struct {
	Store     *sqlite.Store
	Publisher *serving.Publisher
	Schema    *servingbuild.Schema
	// Pin retains the complete production dependency exclusions through each
	// command. It includes the publisher writer gate, host/provider authority,
	// keys and transferred managers in the same order as the serving source.
	Pin          currentguard.PinFunc
	NotifySource netip.Addr
	Resolver     network.Resolver
	Limits       zonestate.Limits
	UpdateLimits dynamicupdate.Limits
	Observe      func(pr.UpdateOutcome)
}

// primaryRuntime assembles the production SQLite authority adapters. The root
// mounts Update on the complete DNS chain and Run under the actual elector,
// keeping the complete publisher always on. Construction performs no writes.
type primaryRuntime struct {
	writer       primaryWriter
	publisher    *serving.Publisher
	publication  *servingbuild.PrimaryPublication
	ingress      *primaryUpdateIngress
	driver       *driver.Driver
	runtime      *pr.Runtime
	notifyIntent func(context.Context, *sqlite.CommandTx, pr.PrimaryStatusObservation) (sqliteadapter.NotifyIntentResult, error)
	// update is the protocol adapter that Update and a forwarded member
	// command both run, so that both paths select, admit and commit alike.
	update pipeline.Next
	// follower answers an UPDATE that this node cannot apply because it is
	// an enrolled follower: it forwards the verified request to the primary
	// (D381). handled=false keeps the local SERVFAIL. Root sets it once,
	// before any listener runs.
	follower func(*pipeline.Request, string) (*pipeline.Response, bool)
	Update   pipeline.Next
}

func newPrimaryRuntime(ctx context.Context, o primaryRuntimeOptions) (*primaryRuntime, error) {
	if ctx == nil || o.Store == nil || o.Publisher == nil || o.Schema == nil || o.Pin == nil {
		return nil, pr.ErrInvalid
	}
	p := &primaryRuntime{writer: primaryWriter{store: o.Store}, publisher: o.Publisher, ingress: &primaryUpdateIngress{authority: o.Store.Authority}}
	var err error
	p.publication, err = servingbuild.NewPrimaryPublication(o.Publisher, time.Now)
	if err != nil {
		return nil, err
	}
	sender, err := network.NewNotify(network.NotifyOptions{Capture: p.captureNotify, SourceAddr: o.NotifySource, Resolver: o.Resolver})
	if err != nil {
		return nil, err
	}
	guard, err := currentguard.New(currentguard.Options{Limits: o.Limits, Pin: func(ctx context.Context, f pr.PublicationFence, fn func(currentguard.Pinned) error) error {
		return o.Pin(ctx, f, func(current currentguard.Pinned) error {
			current.Notify = sender.Validator(current.Identity, current.Keys)
			return fn(current)
		})
	}})
	if err != nil {
		return nil, err
	}
	adapter, err := sqliteadapter.New(sqliteadapter.Options{Store: o.Store, Limits: o.Limits, Credentials: guard.Credentials(), Background: guard.Background()})
	if err != nil {
		return nil, err
	}
	p.notifyIntent = adapter.RequestNotifyTx
	backend, err := o.Store.ZoneStateBackend(o.Limits)
	if err != nil {
		return nil, err
	}
	p.runtime, err = pr.New(pr.Options{Store: o.Store, Repository: zonestate.NewRepository(backend, o.Limits), Reader: primaryzone.NewReader(o.Store, primaryzone.ReaderOptions{Policy: guard.CapturePolicy}), LifecycleCAS: adapter.LifecycleCAS, Admit: adapter.Admission, Publish: p.publication.Publish, Proofs: p.ingress, Notify: sender, Limits: o.Limits, UpdateLimits: o.UpdateLimits})
	if err != nil {
		return nil, err
	}
	image := o.Schema.PrimaryImageOptions(ctx, o.Store.Info().ClusterID)
	image.Limits = o.Limits
	p.driver, err = driver.New(driver.Options{Store: o.Store, Runtime: p.runtime, Publication: p.publication, Image: image})
	if err != nil {
		return nil, err
	}
	update, err := network.NewUpdate(network.UpdateOptions{Runtime: p.runtime, Capture: p.captureUpdate, Limits: o.UpdateLimits, Observe: o.Observe})
	if err != nil {
		return nil, err
	}
	p.update = update
	p.Update = func(req *pipeline.Request) (*pipeline.Response, error) {
		// The protocol adapter owns malformed/synthetic request responses.
		if req == nil || req.Msg == nil || req.Client.Synthetic {
			return update(req)
		}
		tx, protocol := dynamicupdate.Parse(req.Msg, o.UpdateLimits)
		if protocol != nil {
			return update(req)
		}
		status := p.driver.Status()
		if !status.Running || !status.Ready {
			// An enrolled follower forwards; every other node that is not the
			// ready writer (fenced, conflicted, standby) refuses here.
			if p.follower != nil {
				if response, handled := p.follower(req, tx.Apex()); handled {
					return response, nil
				}
			}
			return pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusAuthoritative), nil
		}
		bound, release, err := p.writer.bind(req.Ctx, status.Writer)
		if err != nil {
			return pipeline.Reply(req, dns.RcodeServerFailure, pipeline.StatusAuthoritative), nil
		}
		transferred := false
		defer func() {
			if !transferred {
				release()
			}
		}()
		request := *req
		request.Ctx = bound
		response, err := update(&request)
		if response != nil {
			// The original writer binding remains alive through actual output,
			// including a committed command whose publication acknowledgment failed.
			response.AddCleanup(release)
			transferred = true
			if err == nil {
				if output, ok := dnsserver.CapturedOutput(&request).(*rootAnchorOutput); ok {
					output.completeUpdate(&request, response, &p.writer)
				}
			}
		}
		return response, err
	}
	return p, nil
}

// Run receives the actual elector token/context. The holder rejects overlapping
// invocations and Driver.Run joins restoration/reconciliation and NOTIFY work.
func (p *primaryRuntime) Run(ctx context.Context) error {
	return p.writer.run(ctx, p.driver.Run)
}

func (p *primaryRuntime) captureUpdate(req *pipeline.Request, apex string) (pr.VerifiedUpdate, error) {
	reject := func(code int) (pr.VerifiedUpdate, error) {
		return pr.VerifiedUpdate{}, &network.UpdateReject{Rcode: code, Err: pr.ErrUnavailable}
	}
	fence, ok := pr.PublicationFenceFrom(req.Ctx)
	if !ok {
		return reject(dns.RcodeServerFailure)
	}
	target := serving.TermActivation{Term: fence.Term, Incarnation: fence.Incarnation}
	forwarded, isForwarded := forwardedUpdateFrom(req.Ctx)
	// stale rejects a forwarded UPDATE whose lease is no longer this node's
	// current writer publication, or whose publication does not serve the
	// zone for updates at this moment (its lifecycle is being realigned
	// after a commit). Nothing has been admitted yet, so the member command
	// may wait for the next publication and capture again within its bound
	// (D381).
	stale := func() (pr.VerifiedUpdate, error) {
		if isForwarded {
			forwarded.stale = true
		}
		return reject(dns.RcodeServerFailure)
	}
	if err := servingdns.CheckWriter(req, p.publisher, target); err != nil {
		return stale()
	}
	owner, err := servingdns.Component[*servingbuild.RuntimeOwner](req, "runtime")
	if err != nil || owner.Primary() == nil || owner.Zones() == nil {
		return reject(dns.RcodeServerFailure)
	}
	now := time.Now()
	member, found := owner.Primary().Cut(now).Member(apex)
	if !found || member.Zone.Apex != apex || member.Type != public.ZoneTypePrimary {
		return reject(dns.RcodeNotAuth)
	}
	if member.Phase == zk.PhaseRetiring || member.Phase == zk.PhaseRetired {
		// A lifecycle never returns from retirement: no later publication
		// admits this zone, so even a forwarded UPDATE is answered at once.
		return reject(dns.RcodeServerFailure)
	}
	if member.Phase != zk.PhaseActive || member.Incarnation == 0 || member.Version.CommitSequence == 0 || !owner.Primary().Admitted(apex, now) {
		return stale()
	}
	route := owner.Zones().Zones(now).Match(apex)
	if route == nil || route.Name() != apex || route.Type() != public.ZoneTypePrimary || !route.RoutingAvailableAt(now) {
		return stale()
	}
	_, keys, current := owner.NotifyKeys()
	var proof pr.VerifiedUpdate
	if isForwarded {
		// A follower verified the signature (D381). This node admits only the
		// exact key identity of its own current keyring: same name, algorithm
		// and secret-derived credential version.
		if keys == nil || current == nil {
			return reject(dns.RcodeRefused)
		}
		if !current() {
			return stale() // the source changed under the request
		}
		identity, ok := keys.Identity(forwarded.key.Key)
		if !ok || identity.Key != forwarded.key.Key || identity.Algorithm != forwarded.key.Algorithm || identity.CredentialVersion != forwarded.key.CredentialVersion {
			return reject(dns.RcodeRefused)
		}
		var err error
		proof, err = p.ingress.captureForwarded(req, member.Zone, identity, forwarded, current)
		if err != nil {
			return pr.VerifiedUpdate{}, err
		}
	} else {
		native, ok := dnsserver.VerifiedTSIGFor(req, now)
		if !ok || keys == nil || current == nil {
			return reject(dns.RcodeRefused)
		}
		if !current() {
			return reject(dns.RcodeServerFailure) // the source changed under the request
		}
		identity, ok := keys.Identity(native.Identity().Key)
		if !ok || identity != native.Identity() {
			return reject(dns.RcodeRefused)
		}
		var err error
		proof, err = p.ingress.capture(req, member.Zone)
		if err != nil {
			return pr.VerifiedUpdate{}, err
		}
	}
	issued, err := p.ingress.Verify(req.Ctx, proof.Proof, proof.Wire)
	if err != nil || !issued.Term.Same(fence.Term) || issued.AuthorityIncarnation != fence.Incarnation {
		if current != nil && !current() {
			return stale()
		}
		return reject(dns.RcodeServerFailure)
	}
	if err := servingdns.CheckWriter(req, p.publisher, target); err != nil {
		return stale()
	}
	return proof, nil
}

func (p *primaryRuntime) captureNotify(ctx context.Context, fence pr.PublicationFence, member pr.Member) (network.NotifyCut, error) {
	t := p.writer.active.Load()
	if !p.writer.current(t) || !t.fence.Term.Same(fence.Term) || t.fence.Incarnation != fence.Incarnation {
		return network.NotifyCut{}, pr.ErrUnavailable
	}
	lease, err := p.publication.Capture(ctx, fence, member)
	if err != nil {
		return network.NotifyCut{}, err
	}
	value, _ := lease.Bundle().Component("runtime")
	owner, ok := value.(*servingbuild.RuntimeOwner)
	if !ok || owner == nil {
		lease.Release()
		return network.NotifyCut{}, serving.ErrComponents
	}
	identity, keys, current := owner.NotifyKeys()
	if identity == "" || keys == nil || current == nil || owner.Primary() == nil {
		lease.Release()
		return network.NotifyCut{}, serving.ErrDependency
	}
	target := serving.TermActivation{Term: fence.Term, Incarnation: fence.Incarnation}
	return network.NotifyCut{Identity: identity, Keys: keys, Release: lease.Release, Current: func() bool {
		return ctx.Err() == nil && p.writer.current(t) && current() && owner.Primary().Admitted(member.Zone.Apex, time.Now()) && p.publisher.CheckWriterLease(ctx, lease, target) == nil
	}}, nil
}
