package main

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// managedDNSOnlyFixture is an actual managed/direct runtime: root admission, the
// elected RunWriter mount that bootstraps from a signed TCP response, the
// complete publisher and UDP/TCP listeners with the production output guard.
// It forges no anchor state, sample or scheduler step.
type managedDNSOnlyFixture struct {
	s          *servingRuntime
	net        *primaryNetworkFixture
	writerLoss atomic.Bool // only the explicit fencing of the original writer
}

func newManagedDNSOnlyFixture(t *testing.T, endpoint string, seeds []string) *managedDNSOnlyFixture {
	t.Helper()
	base := newServingRuntimeFixture(t)
	cfg, err := config.FromFlat(config.Flatten(base.opts.Config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.IdentityProviders = nil
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.TrustAnchors = seeds
	cfg.Upstreams.Recursion.RootHints = []string{endpoint}
	cfg.Upstreams.Recursion.IPv6 = "off"
	cfg.EnvLocked = append(cfg.EnvLocked, "upstreams.recursion.anchorMode", "upstreams.recursion.trustAnchors", "upstreams.recursion.rootHints", "upstreams.recursion.ipv6")
	opts := base.opts
	opts.Config = cfg
	s, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	f := &managedDNSOnlyFixture{s: s}
	runServingFixture(t, s)
	writerCtx, cancelWriter := context.WithCancel(t.Context())
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- s.opts.Store.Elector("managed-dns-only").Run(writerCtx, func(term context.Context) {
			if err := s.RunWriter(term); err != nil && term.Err() == nil && !f.writerLoss.Load() {
				t.Error("managed writer ended early", err)
			}
		}, nil)
	}()
	t.Cleanup(func() {
		cancelWriter()
		select {
		case err := <-writerDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("managed elector did not join")
		}
	})
	awaitServingRuntime(t, s, func() bool {
		lease, err := s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		owner := value.(*servingbuild.RuntimeOwner)
		return s.primary.driver.Status().Ready && owner.Primary().Admitted("example.", time.Now())
	})
	f.net = &primaryNetworkFixture{store: s.opts.Store, publisher: s.publisher, keys: s.opts.Keyrings.Capture().Keys(), keyrings: s.opts.Keyrings, runtime: s.primary}
	i, a, u := identify.New(nil, nil), authoritative.New(nil), upstream.NewStage(nil)
	a.Update = s.primary.Update
	b, err := blocking.New(blocking.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(cachedata.New(cachedata.Options{}), cache.Settings{}, u)
	r := &postprocess.Stage{}
	ctl := control.New(control.Options{})
	f.net.chain, err = pipeline.New(i, ctl, r, a, b, c, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := servingbuild.BindDNS(s.publisher, servingbuild.DNSStages{Chain: f.net.chain, Control: ctl, Identify: i, Authority: a, Blocking: b, Cache: c, Response: r, Resolution: u}, nil); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	// The same once-configured capture, key selection and final output guard as
	// the production listeners: emission retains the original publication.
	handler := &dnsserver.Handler{Chain: f.net.chain, Logger: logger, Capture: servingdns.Capture(s.publisher), Keys: servingbuild.DNSKeys, Output: captureRootAnchorOutput}
	f.net.listeners, err = dnsserver.NewRegistry(dnsserver.Options{Handler: handler, Logger: logger, TSIG: f.net.keys, Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.net.listeners.Add(dnsserver.Spec{Protocol: dnsserver.ProtoUDP, Address: "127.0.0.1:0"}, dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := f.net.listeners.Bind(); err != nil {
		t.Fatal(err)
	}
	listenerCtx, stop := context.WithCancel(t.Context())
	listenerDone := make(chan error, 1)
	go func() { listenerDone <- f.net.listeners.Run(listenerCtx) }()
	t.Cleanup(func() {
		stop()
		if err := <-listenerDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	awaitServingRuntime(t, s, func() bool { ready, _ := f.net.listeners.Ready(); return ready })
	return f
}

// authoritative reports whether one fresh SOA exchange returned this node's
// last-good authoritative data.
func (f *managedDNSOnlyFixture) authoritative(t *testing.T, tcp bool) (bool, int) {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion("example.", dns.TypeSOA)
	m, err := f.net.exchange(q, tcp)
	if err != nil {
		t.Fatal(err)
	}
	return m.Rcode == dns.RcodeSuccess && m.Authoritative && len(m.Answer) == 1, m.Rcode
}

// A higher authenticated term durably fences the original writer.
func (f *managedDNSOnlyFixture) fence(t *testing.T) replication.Authority {
	t.Helper()
	f.writerLoss.Store(true)
	term := f.s.opts.Store.Authority().Term
	term.Epoch++
	term.PrimaryID, term.PromotionID = "other", "later"
	if _, err := f.s.opts.Store.ObserveAuthority(t.Context(), term); err != nil {
		t.Fatal(err)
	}
	a := f.s.opts.Store.Authority()
	if a.Mode != replication.Fenced || a.Writable() {
		t.Fatal("fixture did not durably fence the writer", a.Mode)
	}
	return a
}

// A different authenticated term in the same epoch is a durable promotion
// conflict: the member keeps its own branch and its original term.
func (f *managedDNSOnlyFixture) conflict(t *testing.T) replication.Authority {
	t.Helper()
	f.writerLoss.Store(true)
	term := f.s.opts.Store.Authority().Term
	term.PrimaryID, term.PromotionID = "other", "same-epoch"
	if _, err := f.s.opts.Store.ObserveAuthority(t.Context(), term); !errors.Is(err, replication.ErrPromotionConflict) {
		t.Fatal("fixture did not observe an equal-epoch conflict", err)
	}
	a := f.s.opts.Store.Authority()
	if a.Mode != replication.Conflict || a.Conflict == nil || a.Writable() {
		t.Fatal("fixture did not durably record the conflict", a.Mode)
	}
	return a
}

// awaitDNSOnly reports what clients actually receive when the DNS-only
// publication never arrives, instead of only the publisher's internal state.
func (f *managedDNSOnlyFixture) awaitDNSOnly(t *testing.T) {
	t.Helper()
	until := time.Now().Add(15 * time.Second)
	for f.s.publisher.Status().Reason != "dns_only" {
		if time.Now().After(until) {
			_, udp := f.authoritative(t, false)
			_, tcp := f.authoritative(t, true)
			t.Fatalf("DNS-only publication never arrived: reason=%q UDP rcode=%s TCP rcode=%s", f.s.publisher.Status().Reason, dns.RcodeToString[udp], dns.RcodeToString[tcp])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Both DNS-only roles share one body: only how the writer lost its term differs.
func managedDNSOnlyPublication(t *testing.T, lose func(*managedDNSOnlyFixture, *testing.T) replication.Authority) {
	endpoint, seeds, reads := anchorSupervisorServer(t, func(int32, *dns.Msg) {})
	f := newManagedDNSOnlyFixture(t, endpoint, seeds)
	for _, tcp := range []bool{false, true} {
		if ok, rcode := f.authoritative(t, tcp); !ok {
			t.Fatal("writable managed fixture did not answer authoritatively", rcode)
		}
	}
	if anchorOperationOwner(t.Context(), t, f.s).Inspect().Phase != accepted.Managed || reads.Load() != 1 {
		t.Fatal("fixture did not bootstrap managed trust exactly once", reads.Load())
	}
	lost := lose(f, t)
	// No writer remains: this image is the member's last-good data.
	before := anchorFollowerImage(t.Context(), t, f.s.opts.Store)
	owner := anchorOperationOwner(t.Context(), t, f.s)
	f.awaitDNSOnly(t)
	status := f.s.publisher.Status()
	if !status.DNSOnly || status.ProtectedReady || !status.Served.Base.Term.Same(lost.Installed.Term) {
		t.Fatal("managed publication was not DNS-only last-good data", status.Reason)
	}
	if _, err := f.s.publisher.CaptureHTTP(t.Context()); err == nil {
		t.Fatal("DNS-only managed publication admitted protected HTTP")
	}
	for _, tcp := range []bool{false, true} {
		if ok, rcode := f.authoritative(t, tcp); !ok {
			t.Fatal("managed node did not keep answering its last-good authoritative data", rcode)
		}
	}
	lease, err := f.s.publisher.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	part, ok := lease.Bundle().Component(recursiveRootComponent)
	seal, typed := part.(*recursiveRootSeal)
	if !ok || !typed || seal.managed == nil || seal.static != nil || !seal.current() || !lease.Bundle().DNSOnly() {
		t.Fatal("DNS-only publication did not retain the last accepted managed root")
	}
	// The member keeps independent deny-only supervision: exactly one new
	// acquisition for this authority, which cannot advance accepted history.
	awaitServingRuntime(t, f.s, func() bool { return reads.Load() == 2 })
	time.Sleep(1200 * time.Millisecond)
	if reads.Load() != 2 {
		t.Fatal("DNS-only supervision reacquired inside its cooldown", reads.Load())
	}
	if !anchorFollowerSameImage(before, anchorFollowerImage(t.Context(), t, f.s.opts.Store)) || anchorOperationOwner(t.Context(), t, f.s).Version() != owner.Version() {
		t.Fatal("DNS-only member changed canonical data or accepted trust history")
	}
	if !reflect.DeepEqual(lost, f.s.opts.Store.Authority()) {
		t.Fatal("DNS-only serving or supervision changed the durable authority")
	}
	if ok, rcode := f.authoritative(t, true); !ok {
		t.Fatal("deny-only supervision interrupted last-good DNS", rcode)
	}
}

// The managed-mode copy of TestRootAnchorPreparationPublishesFencedDNSOnly.
// Before D369, root admission accepted only a writable or follower role: a
// fenced managed node failed every preparation with the anchor policy error,
// never built the DNS-only bundle and answered every query, authoritative data
// included, with an empty SERVFAIL through the retired writable bundle.
func TestRootAnchorManagedPreparationPublishesFencedDNSOnly(t *testing.T) {
	managedDNSOnlyPublication(t, (*managedDNSOnlyFixture).fence)
}

// The same defect applied to a member holding an unresolved promotion conflict,
// which stays in that role until an operator resolves it offline (D361).
func TestRootAnchorManagedPreparationPublishesConflictDNSOnly(t *testing.T) {
	managedDNSOnlyPublication(t, (*managedDNSOnlyFixture).conflict)
}

// A fenced or conflicted member no longer has an elected updater, so its own
// deny-only supervisor is the only thing that can see a root key revocation.
// The signed REVOKE it acquires independently must retire the DNS-only output
// it was serving, durably and without touching canonical history or authority.
func TestAnchorDNSOnlySupervisorRevocationRetiresOutput(t *testing.T) {
	for name, lose := range map[string]func(*managedDNSOnlyFixture, *testing.T) replication.Authority{
		"fenced":   (*managedDNSOnlyFixture).fence,
		"conflict": (*managedDNSOnlyFixture).conflict,
	} {
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			endpoint, seeds, reads := anchorOperationServer(t, 2, func(n int) {
				if n == 2 {
					close(entered)
					<-release
				}
			})
			f := newManagedDNSOnlyFixture(t, endpoint, seeds)
			lost := lose(f, t)
			before := anchorFollowerImage(t.Context(), t, f.s.opts.Store)
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				t.Fatal("DNS-only member never acquired its independent receipt")
			}
			f.awaitDNSOnly(t)
			for _, tcp := range []bool{false, true} {
				if ok, rcode := f.authoritative(t, tcp); !ok {
					t.Fatal("member did not serve last-good DNS before the revocation", rcode)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			lease, err := f.s.publisher.Capture()
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			output, err := captureRootAnchorOutput(&pipeline.Request{Ctx: ctx, Serving: servingdns.Wrap(lease)})
			if err != nil {
				t.Fatal(err)
			}
			unblock()
			// The durable local denial retires the exact published input. Nothing
			// else is admitted afterwards: this node has no other trust to use.
			awaitServingRuntime(t, f.s, func() bool {
				_, token, err := f.s.opts.Store.ReadServingCandidate(ctx)
				if err != nil {
					return false
				}
				_, err = f.s.opts.Store.CaptureAnchorPublication(ctx, token)
				return errors.Is(err, sqlite.ErrAnchorUnavailable)
			})
			if err := output.Emit(func() error { t.Error("held DNS-only output emitted after durable denial"); return nil }); err == nil {
				t.Fatal("held output attempt survived local denial")
			}
			for _, tcp := range []bool{false, true} {
				q := new(dns.Msg)
				q.SetQuestion("example.", dns.TypeSOA)
				m, err := f.net.exchange(q, tcp)
				if err != nil {
					t.Fatal(err)
				}
				if m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 || len(m.Ns) != 0 || m.AuthenticatedData {
					t.Fatal("member kept emitting data under a revoked root key", tcp, m.Rcode)
				}
			}
			if reads.Load() != 2 || !anchorFollowerSameImage(before, anchorFollowerImage(t.Context(), t, f.s.opts.Store)) || !reflect.DeepEqual(lost, f.s.opts.Store.Authority()) {
				t.Fatal("DNS-only supervision changed canonical history or authority", reads.Load())
			}
		})
	}
}
