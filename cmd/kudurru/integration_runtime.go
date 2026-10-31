package main

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/cloudflare"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/ee/integration/unifi"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/license"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// integrationRuntime is the root adapter, not a query owner. Run is joined by
// the complete serving lifetime. It never mints a token or retries a command.
type integrationRuntime struct {
	s       *servingRuntime
	schema  *servingbuild.Schema
	pin     currentguard.PinFunc
	host    *ih.Host
	mu      sync.RWMutex
	cut     *integrationCut
	allowed atomic.Pointer[license.Cut]
	factory func(string) (integration.Integration, error)
}
type integrationCut struct {
	input       ih.Input
	image       *replication.Image
	license     license.Cut
	keysCurrent func() bool
	instances   map[kinds.Owner]*ih.Instance
}

func newIntegrationRuntime(s *servingRuntime, schema *servingbuild.Schema, pin currentguard.PinFunc) (*integrationRuntime, error) {
	r := &integrationRuntime{s: s, schema: schema, pin: pin}
	repository, err := ledger.New(ledger.Options{Reader: s.opts.Store, Now: s.opts.Now, Write: r.write})
	if err != nil {
		return nil, err
	}
	r.host, err = ih.New(ih.Options{Capture: r.capture, Elector: s.opts.Store.Elector, Fence: r.fence, Ledger: repository, Reader: s.opts.Store, Allowed: func() bool {
		c := r.allowed.Load()
		return s.life.Err() == nil && c != nil && c.Current() && c.Status().Allows(license.FeatureIntegrations)
	}, Factory: func(typ string) (integration.Integration, error) {
		if r.factory != nil {
			return r.factory(typ)
		}
		switch typ {
		case "cloudflare":
			return cloudflare.New(), nil
		case "unifi":
			return unifi.NewWithOptions(unifi.Options{Allocations: r.allocate}), nil
		default:
			return nil, ih.ErrUnsupported
		}
	}, ValidateSource: r.validateSource, BeforeObserve: r.beforeObserve, CommitSource: r.commitSource, Status: r.status, Now: s.opts.Now})
	return r, err
}

func (r *integrationRuntime) capture(ctx context.Context) (ih.Input, error) {
	image, err := r.s.opts.Store.ReadGeneration(ctx)
	if err != nil {
		return ih.Input{}, err
	}
	// Acquisition must not start against a writer whose complete local
	// dependencies have not been activated yet. This is a read-only readiness
	// check, not a writer token or command admission; the original elector and
	// actual transaction independently fence every subsequent effect.
	authority, blocked := r.s.opts.Store.ServingAuthority()
	if blocked || !authority.Writable() || !authority.Term.Same(image.Term) {
		return ih.Input{}, ih.ErrUnavailable
	}
	if err = r.pin(ctx, pr.PublicationFence{Term: image.Term, Incarnation: authority.Incarnation}, func(p currentguard.Pinned) error {
		if p.Current == nil || !p.Current() {
			return ih.ErrUnavailable
		}
		return nil
	}); err != nil {
		return ih.Input{}, ih.ErrUnavailable
	}
	lc := r.s.opts.License.Capture()
	keys := r.s.opts.AuthKeys.owner.Capture()
	if !lc.Current() || !keys.Current() || keys.Value() == nil {
		return ih.Input{}, ih.ErrFenced
	}
	var objects []store.Object
	var refs []public.CredentialReference
	options := r.schema.PrimaryImageOptions(ctx, image.Term.ClusterID)
	for _, raw := range image.Payload.Objects {
		var header metav1.TypeMeta
		if err = json.Unmarshal(raw, &header); err != nil {
			return ih.Input{}, ih.ErrInvalid
		}
		// Private ledger/source/status changes never schedule an outbound self-loop.
		if !slices.Contains([]string{"Integration", "DNSZone", "DNSRecordSet", "ClientGroup"}, header.Kind) {
			continue
		}
		obj, e := options.Scheme.New(public.GroupVersion.WithKind(header.Kind))
		if e != nil {
			return ih.Input{}, ih.ErrInvalid
		}
		if e = json.Unmarshal(raw, obj); e != nil {
			return ih.Input{}, ih.ErrInvalid
		}
		objects = append(objects, obj.(store.Object))
		if d, ok := obj.(*public.Integration); ok {
			for _, ref := range d.Spec.Credentials {
				refs = append(refs, ref)
			}
		}
	}
	resolver, err := credentials.New(credentials.Options{Files: r.s.opts.Config.Credentials.AllowedFiles, Environment: r.s.opts.Config.Credentials.AllowedEnvironment, Namespace: r.s.opts.Namespace, FingerprintKey: keys.Value().current})
	if err != nil {
		return ih.Input{}, ih.ErrCredentials
	}
	resolver, err = resolver.Snapshot(ctx, refs)
	if err != nil {
		return ih.Input{}, err
	}
	snap, err := zonecache.Prepare(zonecache.PrepareOptions{Kinds: []store.Kind{"Integration", "DNSZone", "DNSRecordSet", "ClientGroup"}, Generation: image.Revision, Now: r.s.opts.Now()}, objects, nil)
	if err != nil {
		return ih.Input{}, ih.ErrInvalid
	}
	in := ih.Input{Objects: snap.Objects, Credentials: resolver, Term: image.Term, BundleID: "preparing", Complete: true}
	instances, err := ih.CaptureInstances(ctx, in)
	if err != nil {
		return ih.Input{}, err
	}
	raw, err := json.Marshal(struct {
		License, Keys string
	}{lc.Identity(), keys.Identity()})
	if err != nil {
		return ih.Input{}, ih.ErrInvalid
	}
	in.BundleID = kinds.Digest(raw)
	needsSources := false
	for _, i := range instances {
		needsSources = needsSources || i.Definition().Spec.Cloudflare != nil && !i.Definition().Spec.Suspend
	}
	var primary *pr.PreparedImage
	if needsSources {
		options.Now = r.s.opts.Now()
		primary, err = pr.PrepareImage(ctx, *image, options)
		if err != nil {
			return ih.Input{}, err
		}
	}
	for _, record := range snap.Objects.RecordSets() {
		if !needsSources {
			break
		}
		z, ok := snap.Objects.Zone(record.Spec.ZoneRef.Name)
		if !ok || public.IsDisabled(z) || !z.DeletionTimestamp.IsZero() || public.IsDisabled(record) || !record.DeletionTimestamp.IsZero() {
			continue
		}
		if z.Spec.Type != public.ZoneTypePrimary {
			continue
		}
		selector, e := zone.NewRecordSetSelector(z)
		if e != nil {
			return ih.Input{}, ih.ErrInvalid
		}
		if !selector.Admits(record) {
			continue
		}
		if !primary.Admitted(z.Spec.ZoneName, r.s.opts.Now()) {
			return ih.Input{}, ih.ErrUnavailable
		}
		observation, ok := primary.CommittedPrimaryStatus(z.Name)
		if !ok {
			return ih.Input{}, ih.ErrUnavailable
		}
		_, aggregate, e := integrationAggregate(image, observation.HeadZone)
		if e != nil {
			return ih.Input{}, e
		}
		token, e := primaryzone.SourceToken(record)
		if e != nil {
			return ih.Input{}, e
		}
		admitted := false
		for _, source := range aggregate.Sources {
			if source.ID != token.ID || source.UID != token.UID {
				continue
			}
			envelope, e := zonestate.DecodeSourceEnvelope(source.Desired)
			if e != nil {
				return ih.Input{}, e
			}
			admitted = envelope.State == "present" && envelope.IntentDigest == token.IntentDigest && envelope.Projection != "duplicated"
		}
		if !admitted {
			return ih.Input{}, ih.ErrUnavailable
		}
		in.Sources = append(in.Sources, plan.Source{Zone: z.DeepCopy(), Records: record.DeepCopy()})
	}
	// Inbound reconciliation watches only its explicit target/group grants;
	// its own fresh records must not recursively schedule another provider poll.
	in.ReconcileDigests = map[ih.InstanceID]string{}
	in.AuthorityDigests = map[ih.InstanceID]string{}
	for _, i := range instances {
		var dependencies []any
		if u := i.Definition().Spec.UniFi; u != nil {
			if u.Inventory != nil {
				if z, ok := snap.Objects.Zone(u.Inventory.ZoneRef.Name); ok {
					dependencies = append(dependencies, struct {
						UID    string
						Active bool
						Spec   public.DNSZoneSpec
					}{string(z.UID), !public.IsDisabled(z) && z.DeletionTimestamp.IsZero(), z.Spec})
				}
			}
			for _, group := range snap.Objects.ClientGroups() {
				ref := group.Spec.Sources
				if ref != nil && ref.IntegrationRef != nil && group.Namespace == i.Owner().Namespace && ref.IntegrationRef.Name == i.Owner().Name {
					dependencies = append(dependencies, struct {
						UID    string
						Active bool
						Spec   public.ClientGroupSpec
					}{string(group.UID), !public.IsDisabled(group) && group.DeletionTimestamp.IsZero(), group.Spec})
				}
			}
			raw, _ := json.Marshal(dependencies)
			in.ReconcileDigests[i.Owner()] = kinds.Digest(raw)
		} else {
			dependencies = append(dependencies, in.Sources)
		}
		raw, err := json.Marshal(struct {
			Definition   *public.Integration
			Fingerprints map[string]string
			Dependencies []any
		}{i.Definition(), i.CredentialFingerprints(), dependencies})
		if err != nil {
			return ih.Input{}, ih.ErrInvalid
		}
		in.AuthorityDigests[i.Owner()] = kinds.Digest(raw)
	}
	// Resolve only captured bytes on subsequent host validation. Recheck original
	// authority after acquisition, before publishing this private resource cut.
	if !lc.Current() || !keys.Current() {
		return ih.Input{}, ih.ErrFenced
	}
	next := &integrationCut{image: image, license: lc, keysCurrent: keys.Current, instances: map[kinds.Owner]*ih.Instance{}}
	in.Resource = next
	next.input = in
	instances, err = ih.CaptureInstances(ctx, in)
	if err != nil {
		return ih.Input{}, err
	}
	for _, i := range instances {
		next.instances[i.Owner()] = i
	}
	r.mu.Lock()
	r.cut = next
	r.allowed.Store(&lc)
	r.mu.Unlock()
	return in, nil
}

func (r *integrationRuntime) current(g ih.ExecutionGuard) (*integrationCut, error) {
	c := r.cut
	if c == nil || !c.input.Term.Same(g.Ledger.Term) || !c.license.Current() || !c.license.Status().Allows(license.FeatureIntegrations) || !c.keysCurrent() {
		return nil, ih.ErrFenced
	}
	i := c.instances[g.Ledger.Owner]
	// Delete requires a separate original-authority cleanup/finalizer command.
	// Never silently execute a configuration whose deletion promise is absent.
	if i != nil && i.Definition().Spec.DeletionPolicy == "Delete" {
		return nil, ih.ErrUnsupported
	}
	if i == nil || i.BundleID() != g.BundleID {
		return nil, ih.ErrFenced
	}
	expected, err := i.Guard(g.Ledger.LeaseID)
	if err != nil || expected != g.Ledger {
		return nil, ih.ErrFenced
	}
	return c, nil
}

// original checks current authority only to reject the retained original cut.
// Equal authority permits incidental facts to change, never a successor input
// to replace the original targets, sources, credentials or object versions.
func (r *integrationRuntime) original(ctx context.Context, g ih.ExecutionGuard) (*integrationCut, error) {
	if _, err := r.current(g); err != nil {
		return nil, err
	}
	var c *integrationCut
	if observation, ok := ctx.Value(integrationObservationKey{}).(*integrationObservation); ok {
		c = observation.cut
	}
	if c == nil {
		c, _ = ctx.Value(integrationRequestKey{}).(*integrationCut)
	}
	if c == nil || !c.license.Current() || !c.keysCurrent() || c.instances[g.Ledger.Owner] == nil || c.instances[g.Ledger.Owner].BundleID() != g.BundleID {
		return nil, ih.ErrFenced
	}
	return c, nil
}

type integrationRequestKey struct{}

// fence is background-only. Current desired objects are compared to the
// ORIGINAL cut; a fresher read may reject it but can never repair or replace it.
func (r *integrationRuntime) fence(ctx context.Context, g ih.ExecutionGuard) error {
	bound, original, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return ih.ErrFenced
	}
	ctx = bound
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, err := r.original(ctx, g)
	if err != nil {
		return err
	}
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok || !token.Term().Same(g.Ledger.Term) {
		return ih.ErrFenced
	}
	if err = r.s.writerCurrent(ctx, writerTarget(token)); err != nil {
		return err
	}
	for _, obj := range c.input.Objects.List("Integration") {
		if store.KeyOf(obj) != (store.Key{Kind: "Integration", Namespace: g.Ledger.Owner.Namespace, Name: g.Ledger.Owner.Name}) {
			continue
		}
		var current public.Integration
		if err = r.s.opts.Store.Get(ctx, store.KeyOf(obj), &current); err != nil {
			return ih.ErrFenced
		}
		if current.UID != obj.GetUID() || current.ResourceVersion != obj.GetResourceVersion() || current.Spec.Suspend || !current.DeletionTimestamp.IsZero() {
			return ih.ErrFenced
		}
	}
	return r.pin(ctx, pr.PublicationFence{Term: token.Term(), Incarnation: token.Incarnation()}, func(p currentguard.Pinned) error {
		if p.Current == nil || !p.Current() || !c.license.Current() || !c.keysCurrent() {
			return ih.ErrFenced
		}
		return nil
	})
}

func writerTarget(t sqlite.WriterToken) serving.TermActivation {
	return serving.TermActivation{Term: t.Term(), Incarnation: t.Incarnation()}
}

// command holds the original integration resource and complete serving
// dependency pins through one concrete transaction. The final checks run even
// for dedup/no-op and contain no I/O or successor acquisition.
func (r *integrationRuntime) command(ctx context.Context, g ih.ExecutionGuard, fn func(context.Context, *sqlite.CommandTx, *integrationCut) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, err := r.original(ctx, g)
	if err != nil {
		return err
	}
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok || !token.Term().Same(g.Ledger.Term) {
		return ih.ErrFenced
	}
	fence := pr.PublicationFence{Term: token.Term(), Incarnation: token.Incarnation()}
	return r.pin(ctx, fence, func(p currentguard.Pinned) error {
		return r.s.opts.Store.Command(ctx, g.Ledger.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			final := func(ctx context.Context) error {
				if !p.Current() || !c.license.Current() || !c.keysCurrent() {
					return ih.ErrFenced
				}
				return r.s.writerCurrent(ctx, writerTarget(token))
			}
			if err := final(ctx); err != nil {
				return err
			}
			if err := tx.RequireCommitCheck(final); err != nil {
				return err
			}
			var definition public.Integration
			if err := tx.Get(ctx, store.Key{Kind: "Integration", Namespace: g.Ledger.Owner.Namespace, Name: g.Ledger.Owner.Name}, &definition); err != nil {
				return err
			}
			digest, _ := json.Marshal(definition.Spec)
			if string(definition.UID) != g.Ledger.Owner.UID || definition.ResourceVersion != g.Ledger.IntegrationResourceVersion || kinds.Digest(digest) != g.Ledger.ConfigDigest || definition.Spec.Suspend || !definition.DeletionTimestamp.IsZero() {
				return ih.ErrFenced
			}
			var selected []plan.Source
			if c.instances[g.Ledger.Owner].Definition().Spec.Cloudflare != nil {
				selected = c.input.Sources
			}
			for _, source := range selected {
				var zone public.DNSZone
				var records public.DNSRecordSet
				if err := tx.Get(ctx, store.KeyOf(source.Zone), &zone); err != nil {
					return err
				}
				if err := tx.Get(ctx, store.KeyOf(source.Records), &records); err != nil {
					return err
				}
				if zone.UID != source.Zone.UID || zone.ResourceVersion != source.Zone.ResourceVersion || records.UID != source.Records.UID || records.ResourceVersion != source.Records.ResourceVersion {
					return ih.ErrFenced
				}
			}
			return fn(ctx, tx, c)
		})
	})
}
func (r *integrationRuntime) write(ctx context.Context, g ledger.Guard, obj store.Object, opt store.ApplyOptions) (store.Object, error) {
	ctx, guard, err := r.requestContext(ctx, g)
	if err != nil {
		return nil, err
	}
	var out store.Object
	err = r.host.WithLease(ctx, g, func(ctx context.Context) error {
		return r.command(ctx, guard, func(ctx context.Context, tx *sqlite.CommandTx, _ *integrationCut) error {
			var e error
			out, e = tx.Apply(ctx, obj, opt)
			return e
		})
	})
	return out, err
}

// requestContext binds only the original elector's token, never a current
// WriterToken sample. Explicit host commands keep their own deadline/cancellation.
func (r *integrationRuntime) requestContext(ctx context.Context, expected ledger.Guard) (context.Context, ih.ExecutionGuard, error) {
	guard, lease, err := r.host.RequestGuard(ctx, expected)
	if err != nil {
		return nil, ih.ExecutionGuard{}, err
	}
	resource, err := r.host.RequestResource(ctx, expected)
	if err != nil {
		return nil, ih.ExecutionGuard{}, err
	}
	original, ok := resource.(*integrationCut)
	if !ok || original == nil {
		return nil, ih.ExecutionGuard{}, ih.ErrFenced
	}
	bound, err := integrationBindWriter(ctx, lease)
	if err != nil {
		return nil, ih.ExecutionGuard{}, err
	}
	return context.WithValue(bound, integrationRequestKey{}, original), guard, nil
}

func integrationBindWriter(ctx, lease context.Context) (context.Context, error) {
	original, ok := sqlite.WriterTokenFromContext(lease)
	token, present := sqlite.WriterTokenFromContext(ctx)
	if !ok || present && original != token {
		return nil, ih.ErrFenced
	}
	return sqlite.WithWriterToken(ctx, original), nil
}

func (r *integrationRuntime) status(ctx context.Context, g ih.ExecutionGuard, status ih.InstanceStatus) error {
	bound, original, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return ih.ErrFenced
	}
	ctx = bound
	return r.host.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
		return r.command(ctx, g, func(ctx context.Context, tx *sqlite.CommandTx, c *integrationCut) error {
			d := c.instances[g.Ledger.Owner].Definition()
			d.Status.ObservedGeneration = d.Generation
			d.Status.Clients = int64(status.Result.Clients)
			d.Status.CredentialFingerprints = status.CredentialFingerprints
			d.Status.Records = public.IntegrationRecordCounts{Managed: int64(status.Result.Applied + status.Result.Unchanged), Conflicts: int64(status.Result.Conflicts), Drift: int64(status.Result.Pending)}
			condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: status.Reason, Message: "Background integration is not synchronized.", ObservedGeneration: d.Generation}
			if status.Phase == "Ready" {
				condition.Status = metav1.ConditionTrue
				condition.Message = "Background integration is synchronized."
			}
			meta.SetStatusCondition(&d.Status.Conditions, condition)
			if !status.LastSync.IsZero() {
				at := metav1.NewTime(status.LastSync)
				d.Status.LastSyncTime = &at
			}
			return tx.UpdateStatus(ctx, d)
		})
	})
}

// beforeObserve retains the original cut for acquisition through acknowledgment.
// No lock or transaction spans provider I/O. Replacement invalidates this cut;
// the subsequent command cannot obtain a newer credential or writer instead.
func (r *integrationRuntime) beforeObserve(ctx, lease context.Context, g ih.ExecutionGuard) (context.Context, func(), error) {
	ctx, original, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return nil, nil, ih.ErrFenced
	}
	if ctx, err = integrationBindWriter(ctx, lease); err != nil {
		return nil, nil, err
	}
	r.mu.RLock()
	c, err := r.original(ctx, g)
	r.mu.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	return context.WithValue(ctx, integrationObservationKey{}, &integrationObservation{cut: c, guard: g}), func() {}, nil
}

type integrationObservationKey struct{}
type integrationObservation struct {
	cut         *integrationCut
	guard       ih.ExecutionGuard
	allocations []*kinds.IntegrationNameAllocation
	used        []*kinds.IntegrationNameAllocation
}

// unavailableSourceResult never reports an uncommitted observation as applied.
func unavailableSourceResult(err error) (*ih.Result, error) {
	return &ih.Result{Source: &ih.SourceOutcome{}}, err
}
