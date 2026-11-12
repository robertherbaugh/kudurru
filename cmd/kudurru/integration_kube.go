package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/cloudflare"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"k8s.io/apimachinery/pkg/api/meta"
)

// Kubernetes refusal reasons (D384). Tranche 1 runs Cloudflare publication
// only; every other instance is refused on its object before any provider
// exists, instead of sitting inert.
const (
	reasonUnsupportedOnKubernetes   = "UnsupportedOnKubernetes"
	reasonDeletionPolicyUnsupported = "DeletionPolicyUnsupported"
)

// kubeIntegrationTerm is the constant authority term of the Kubernetes
// root. The ledger guard and the captured instances need a valid term; on
// Kubernetes the per-instance Lease, verified before every effect, is the
// writer authority, so the term only binds the cluster identity.
func kubeIntegrationTerm(clusterID string) replication.Term {
	return replication.Term{ClusterID: clusterID, Epoch: 1, PrimaryID: "kubernetes", PromotionID: "kubernetes"}
}

// kubeIntegrationOptions is the wiring of the Kubernetes root adapter.
type kubeIntegrationOptions struct {
	Store          *kube.Store
	Cache          *zonecache.Cache
	License        *license.Manager
	Credentials    config.CredentialGrants
	FingerprintKey []byte
	Logger         *slog.Logger
	Now            func() time.Time
	// Factory overrides the provider constructors (tests only).
	Factory func(string) (integration.Integration, error)
	// PollInterval overrides the host capture interval (tests only).
	PollInterval time.Duration
}

// kubeIntegrationRuntime is the Kubernetes root adapter of the integration
// host (D384). It captures from the replica's zonecache snapshot, fences every
// provider request and store write with the instance's Lease (verified by a
// live read), a live read of the Integration and the captured license cut,
// and writes only private ledger kinds by one exact create or
// exact-resourceVersion write. It never retries a write.
type kubeIntegrationRuntime struct {
	opts    kubeIntegrationOptions
	host    *ih.Host
	mu      sync.RWMutex
	cut     *kubeIntegrationCut
	allowed atomic.Pointer[license.Cut]
	keyID   string
}

type kubeIntegrationCut struct {
	input     ih.Input
	license   license.Cut
	instances map[kinds.Owner]*ih.Instance
	sources   []plan.Source
}

type kubeIntegrationRequestKey struct{}

func newKubeIntegrationRuntime(o kubeIntegrationOptions) (*kubeIntegrationRuntime, error) {
	if o.Store == nil || o.Cache == nil || o.License == nil || len(o.FingerprintKey) < 32 {
		return nil, errors.New("kubernetes integrations: store, cache, license and a 32-byte fingerprint key are required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	sum := sha256.Sum256(o.FingerprintKey)
	r := &kubeIntegrationRuntime{opts: o, keyID: hex.EncodeToString(sum[:])}
	repository, err := ledger.New(ledger.Options{Reader: o.Store, Now: o.Now, Write: r.write, Delete: r.forget})
	if err != nil {
		return nil, err
	}
	r.host, err = ih.New(ih.Options{
		Capture: r.capture, Elector: o.Store.Elector, Fence: r.fence, Ledger: repository, Reader: o.Store,
		Allowed: func() bool {
			c := r.allowed.Load()
			return c != nil && c.Current() && c.Status().Allows(license.FeatureIntegrations)
		},
		Factory: r.factory, Status: r.status, Refuse: kubeIntegrationRefusal, Now: o.Now, Logger: o.Logger, PollInterval: o.PollInterval,
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Run starts the host once the cache serves live objects and wakes it on
// every published snapshot. It returns when ctx ends.
func (r *kubeIntegrationRuntime) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-r.opts.Cache.Initialized():
	}
	r.opts.Cache.OnPublish(func(*zonecache.Snapshot) { r.host.Wake() })
	return r.host.Run(ctx)
}

// kubeIntegrationRefusal names what tranche 1 does not run on Kubernetes.
// UniFi and webhook instances need the source commit and replica-side source
// runtime (and webhook hooks their delivery producer); deletionPolicy Delete
// needs the cleanup finalizer protocol, which the Kubernetes driver does not
// arm. Running Delete as Retain would silently break its promise (D239).
func kubeIntegrationRefusal(d *public.Integration) string {
	switch {
	case d.Spec.Type != "cloudflare":
		return reasonUnsupportedOnKubernetes
	case d.Spec.DeletionPolicy == public.IntegrationDeletionDelete:
		return reasonDeletionPolicyUnsupported
	}
	return ""
}

func (r *kubeIntegrationRuntime) factory(typ string) (integration.Integration, error) {
	if r.opts.Factory != nil {
		return r.opts.Factory(typ)
	}
	if typ == "cloudflare" {
		return cloudflare.New(), nil
	}
	return nil, ih.ErrUnsupported
}

// capture builds one immutable input from the replica's live zonecache
// snapshot. A replica serving a local.db snapshot, or one that has not
// learned the cluster identity, captures nothing.
func (r *kubeIntegrationRuntime) capture(ctx context.Context) (ih.Input, error) {
	snap := r.opts.Cache.Snapshot()
	if snap == nil || snap.State != zonecache.Live || !r.opts.Cache.Synced() {
		return ih.Input{}, ih.ErrUnavailable
	}
	clusterID := r.opts.Store.Info().ClusterID
	if clusterID == "" {
		return ih.Input{}, ih.ErrUnavailable
	}
	lc := r.opts.License.Capture()
	if !lc.Current() {
		return ih.Input{}, ih.ErrFenced
	}
	var refs []public.CredentialReference
	for _, obj := range snap.Objects.List("Integration") {
		if d, ok := obj.(*public.Integration); ok {
			for _, ref := range d.Spec.Credentials {
				refs = append(refs, ref)
			}
		}
	}
	// Native Secret references stay disabled on Kubernetes (D384): the
	// credential namespace would be the release namespace, which holds
	// kudurru's own keys. File and environment allowlists apply as on SQLite.
	resolver, err := credentials.New(credentials.Options{Files: r.opts.Credentials.AllowedFiles, Environment: r.opts.Credentials.AllowedEnvironment, FingerprintKey: r.opts.FingerprintKey})
	if err != nil {
		return ih.Input{}, ih.ErrCredentials
	}
	resolver, err = resolver.Snapshot(ctx, refs)
	if err != nil {
		return ih.Input{}, err
	}
	in := ih.Input{Objects: snap.Objects, Credentials: resolver, Term: kubeIntegrationTerm(clusterID), BundleID: "preparing", Complete: true}
	instances, err := ih.CaptureInstances(ctx, in)
	if err != nil {
		return ih.Input{}, err
	}
	raw, err := json.Marshal(struct{ License, Keys string }{lc.Identity(), r.keyID})
	if err != nil {
		return ih.Input{}, ih.ErrInvalid
	}
	in.BundleID = kinds.Digest(raw)
	needsSources := false
	for _, i := range instances {
		d := i.Definition()
		needsSources = needsSources || kubeIntegrationRefusal(d) == "" && integrationPublishes(d) && !d.Spec.Suspend && d.DeletionTimestamp.IsZero()
	}
	if needsSources {
		in.Sources, err = kubeIntegrationSources(snap.Objects)
		if err != nil {
			return ih.Input{}, err
		}
	}
	in.ReconcileDigests = map[ih.InstanceID]string{}
	in.AuthorityDigests = map[ih.InstanceID]string{}
	for _, i := range instances {
		d := i.Definition()
		var dependencies []any
		switch {
		case !d.DeletionTimestamp.IsZero():
			in.ReconcileDigests[i.Owner()] = kinds.Digest([]byte("terminating"))
		case kubeIntegrationRefusal(d) != "":
			// A refused instance has no inputs: source changes neither wake
			// nor replace it, only a change of its own object does.
			in.ReconcileDigests[i.Owner()] = kinds.Digest([]byte("refused"))
		case integrationPublishes(d):
			dependencies = append(dependencies, in.Sources)
		}
		// Authority excludes the status and resourceVersion the lease holder
		// itself writes, so a status write never replaces the instance.
		definition := d.DeepCopy()
		definition.Status = public.IntegrationStatus{}
		definition.ResourceVersion = ""
		definition.ManagedFields = nil
		raw, err := json.Marshal(struct {
			Definition   *public.Integration
			Fingerprints map[string]string
			Dependencies []any
		}{definition, i.CredentialFingerprints(), dependencies})
		if err != nil {
			return ih.Input{}, ih.ErrInvalid
		}
		in.AuthorityDigests[i.Owner()] = kinds.Digest(raw)
	}
	if !lc.Current() {
		return ih.Input{}, ih.ErrFenced
	}
	next := &kubeIntegrationCut{license: lc, instances: map[kinds.Owner]*ih.Instance{}, sources: in.Sources}
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

// kubeIntegrationSources selects the record sets a publisher may read: the
// ones each enabled primary zone admits. On Kubernetes the zonecache serves
// these objects directly; there is no committed primary head to wait for.
func kubeIntegrationSources(objects zonecache.ObjectSet) ([]plan.Source, error) {
	var out []plan.Source
	for _, record := range objects.RecordSets() {
		z, ok := objects.Zone(record.Spec.ZoneRef.Name)
		if !ok || public.IsDisabled(z) || !z.DeletionTimestamp.IsZero() || public.IsDisabled(record) || !record.DeletionTimestamp.IsZero() || z.Spec.Type != public.ZoneTypePrimary {
			continue
		}
		selector, err := zone.NewRecordSetSelector(z)
		if err != nil {
			return nil, ih.ErrInvalid
		}
		if selector.Admits(record) {
			out = append(out, plan.Source{Zone: z.DeepCopy(), Records: record.DeepCopy()})
		}
	}
	return out, nil
}

// current checks the guard against the latest capture: same term, license
// current and allowing integrations, the instance still captured with the
// same bundle and in the mode the guard claims, and its guard unchanged.
func (r *kubeIntegrationRuntime) current(g ih.ExecutionGuard) (*kubeIntegrationCut, error) {
	c := r.cut
	if c == nil || !c.input.Term.Same(g.Ledger.Term) || !c.license.Current() || !c.license.Status().Allows(license.FeatureIntegrations) || g.Mode != ih.GuardNormal {
		return nil, ih.ErrFenced
	}
	i := c.instances[g.Ledger.Owner]
	if i == nil || i.BundleID() != g.BundleID || !integrationGuardAdmits(i.Definition(), g) || kubeIntegrationRefusal(i.Definition()) != "" {
		return nil, ih.ErrFenced
	}
	expected, err := i.Guard(g.Ledger.LeaseID)
	if err != nil || expected != g.Ledger {
		return nil, ih.ErrFenced
	}
	return c, nil
}

// requestContext binds the host's ORIGINAL request cut and lease, never a
// newer capture.
func (r *kubeIntegrationRuntime) requestContext(ctx context.Context, expected ledger.Guard) (context.Context, context.Context, ih.ExecutionGuard, *kubeIntegrationCut, error) {
	guard, lease, err := r.host.RequestGuard(ctx, expected)
	if err != nil {
		return nil, nil, ih.ExecutionGuard{}, nil, err
	}
	resource, err := r.host.RequestResource(ctx, expected)
	if err != nil {
		return nil, nil, ih.ExecutionGuard{}, nil, err
	}
	original, ok := resource.(*kubeIntegrationCut)
	if !ok || original == nil || original.instances[expected.Owner] == nil || original.instances[expected.Owner].BundleID() != guard.BundleID {
		return nil, nil, ih.ExecutionGuard{}, nil, ih.ErrFenced
	}
	return context.WithValue(ctx, kubeIntegrationRequestKey{}, original), lease, guard, original, nil
}

// check is the pre-effect fence of every provider request and store write.
// In order: the guard against the latest capture, the original cut's license,
// the instance's Lease by a live read (store.VerifyTerm), the stored
// Integration by a live read (UID, resourceVersion, spec digest and mode),
// and, for a publisher, every source record set it captured against the
// replica's current snapshot. Any failure fences; nothing is repaired.
func (r *kubeIntegrationRuntime) check(ctx, lease context.Context, g ih.ExecutionGuard, original *kubeIntegrationCut, refusal bool) error {
	r.mu.RLock()
	var err error
	if refusal {
		err = r.currentRefused(g)
	} else {
		_, err = r.current(g)
	}
	r.mu.RUnlock()
	if err != nil {
		return err
	}
	if !original.license.Current() || !original.license.Status().Allows(license.FeatureIntegrations) {
		return ih.ErrFenced
	}
	if err := store.VerifyTerm(ctx, lease); err != nil {
		return ih.ErrFenced
	}
	var stored public.Integration
	if err := r.opts.Store.Get(ctx, store.Key{Kind: "Integration", Namespace: g.Ledger.Owner.Namespace, Name: g.Ledger.Owner.Name}, &stored); err != nil {
		return ih.ErrFenced
	}
	spec, _ := json.Marshal(stored.Spec)
	if string(stored.UID) != g.Ledger.Owner.UID || stored.ResourceVersion != g.Ledger.IntegrationResourceVersion || kinds.Digest(spec) != g.Ledger.ConfigDigest {
		return ih.ErrFenced
	}
	if refusal {
		if !kubeRefusalAdmits(&stored, g) || kubeIntegrationRefusal(&stored) == "" {
			return ih.ErrFenced
		}
		return nil
	}
	if !integrationGuardAdmits(&stored, g) {
		return ih.ErrFenced
	}
	if kubeIntegrationRefusal(&stored) != "" {
		return ih.ErrFenced
	}
	if integrationPublishes(&stored) {
		snap := r.opts.Cache.Snapshot()
		if snap == nil || snap.State != zonecache.Live {
			return ih.ErrFenced
		}
		for _, source := range original.sources {
			z, ok := snap.Objects.Get(store.KeyOf(source.Zone))
			rs, present := snap.Objects.Get(store.KeyOf(source.Records))
			if !ok || !present || z.GetUID() != source.Zone.UID || z.GetResourceVersion() != source.Zone.ResourceVersion || rs.GetUID() != source.Records.UID || rs.GetResourceVersion() != source.Records.ResourceVersion {
				return ih.ErrFenced
			}
		}
	}
	return nil
}

// currentRefused is current for the one write a refused instance makes: its
// refusal status. It requires the captured instance to still be refused.
func (r *kubeIntegrationRuntime) currentRefused(g ih.ExecutionGuard) error {
	c := r.cut
	if c == nil || !c.input.Term.Same(g.Ledger.Term) || !c.license.Current() || !c.license.Status().Allows(license.FeatureIntegrations) || g.Mode != ih.GuardNormal {
		return ih.ErrFenced
	}
	i := c.instances[g.Ledger.Owner]
	if i == nil || i.BundleID() != g.BundleID || !kubeRefusalAdmits(i.Definition(), g) || kubeIntegrationRefusal(i.Definition()) == "" {
		return ih.ErrFenced
	}
	expected, err := i.Guard(g.Ledger.LeaseID)
	if err != nil || expected != g.Ledger {
		return ih.ErrFenced
	}
	return nil
}

// kubeRefusalAdmits is the guard rule for a refusal status: a live object
// in normal mode. It does not ask for the cleanup finalizer, which the
// Kubernetes driver never arms; the refusal of policy Delete is why.
func kubeRefusalAdmits(d *public.Integration, g ih.ExecutionGuard) bool {
	mode, ok := ih.InstanceMode(d)
	return ok && mode == ih.GuardNormal && g.Mode == ih.GuardNormal && !d.Spec.Suspend
}

// fence implements ih.Options.Fence for provider requests and setup.
func (r *kubeIntegrationRuntime) fence(ctx context.Context, g ih.ExecutionGuard) error {
	ctx, lease, original, cut, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return ih.ErrFenced
	}
	// A refused instance never has a provider (Options.Refuse), so the only
	// effect it is fenced for is its refusal status.
	refusal := kubeIntegrationRefusal(cut.instances[g.Ledger.Owner].Definition()) != ""
	return r.check(ctx, lease, g, cut, refusal)
}

// kubeIntegrationWritable lists the only kinds the ledger seam may write or
// forget on Kubernetes; the shared budget may be written but never forgotten.
func kubeIntegrationWritable(kind store.Kind) bool {
	switch kind {
	case kinds.KindIntegrationLedger, kinds.KindIntegrationOperation, kinds.KindIntegrationBudget, kinds.KindIntegrationDelivery, kinds.KindIntegrationEventCursor:
		return true
	}
	return false
}

// write implements ledger.Write: the fence, then exactly one create-only or
// exact-resourceVersion write of a private kind. It never forces and never
// retries; a conflict is returned as is.
func (r *kubeIntegrationRuntime) write(ctx context.Context, g ledger.Guard, obj store.Object, opt store.ApplyOptions) (store.Object, error) {
	if obj == nil || !kubeIntegrationWritable(store.KindOf(obj)) || obj.GetNamespace() != "" || opt.Force || !opt.CreateOnly && opt.ExpectedResourceVersion == "" || opt.CreateOnly && opt.ExpectedResourceVersion != "" {
		return nil, ih.ErrInvalid
	}
	ctx, lease, guard, cut, err := r.requestContext(ctx, g)
	if err != nil {
		return nil, err
	}
	var out store.Object
	err = r.host.WithLease(ctx, g, func(ctx context.Context) error {
		if err := r.check(ctx, lease, guard, cut, false); err != nil {
			return err
		}
		var e error
		out, e = r.opts.Store.Apply(ctx, obj, opt)
		return e
	})
	return out, err
}

// forget implements ledger.Delete: each exact UID and resourceVersion delete
// is fenced and sent in order, ledger row first, and the sequence stops at the
// first failure. Kubernetes has no multi-object transaction (D384).
func (r *kubeIntegrationRuntime) forget(ctx context.Context, g ledger.Guard, requests []store.DeleteRequest) error {
	if len(requests) == 0 {
		return ih.ErrInvalid
	}
	for _, request := range requests {
		if !kubeIntegrationWritable(request.Key.Kind) || request.Key.Kind == kinds.KindIntegrationBudget || request.Key.Namespace != "" || request.Options.ExpectedUID == "" || request.Options.ExpectedResourceVersion == "" || request.Options.Force {
			return ih.ErrInvalid
		}
	}
	ctx, lease, guard, cut, err := r.requestContext(ctx, g)
	if err != nil {
		return err
	}
	return r.host.WithLease(ctx, g, func(ctx context.Context) error {
		for _, request := range requests {
			if err := r.check(ctx, lease, guard, cut, false); err != nil {
				return err
			}
			if err := r.opts.Store.Delete(ctx, request.Key, request.Options); err != nil {
				return err
			}
		}
		return nil
	})
}

// status writes the host's sanitized status on the Integration, by the lease
// holder, with the captured resourceVersion as the precondition.
func (r *kubeIntegrationRuntime) status(ctx context.Context, g ih.ExecutionGuard, status ih.InstanceStatus) error {
	ctx, lease, original, cut, err := r.requestContext(ctx, g.Ledger)
	if err != nil || original != g {
		return ih.ErrFenced
	}
	refused := kubeIntegrationRefusal(cut.instances[g.Ledger.Owner].Definition())
	if refused != "" && status.Reason != refused {
		return ih.ErrFenced
	}
	return r.host.WithLease(ctx, g.Ledger, func(ctx context.Context) error {
		if err := r.check(ctx, lease, g, cut, refused != ""); err != nil {
			return err
		}
		d := integrationStatusObject(cut.instances[g.Ledger.Owner].Definition(), status)
		if refused != "" {
			message := "This integration type does not run on the Kubernetes store yet; only Cloudflare publication does."
			if refused == reasonDeletionPolicyUnsupported {
				message = "deletionPolicy Delete is not supported on the Kubernetes store yet; use Retain."
			}
			if c := meta.FindStatusCondition(d.Status.Conditions, "Ready"); c != nil {
				c.Message = message
			}
		}
		d.ResourceVersion = g.Ledger.IntegrationResourceVersion
		return r.opts.Store.UpdateStatus(ctx, d)
	})
}
