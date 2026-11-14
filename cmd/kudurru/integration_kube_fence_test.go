package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// fenceTestCache is a zonecache the test publishes by hand. It never calls
// the publish hook, so the host keeps its captured cut while the test moves
// the live objects: only the root adapter's own pre-effect check can notice.
type fenceTestCache struct {
	snap atomic.Pointer[zonecache.Snapshot]
	init chan struct{}
}

func (c *fenceTestCache) Snapshot() *zonecache.Snapshot       { return c.snap.Load() }
func (c *fenceTestCache) Synced() bool                        { return true }
func (c *fenceTestCache) Initialized() <-chan struct{}        { return c.init }
func (c *fenceTestCache) OnPublish(func(*zonecache.Snapshot)) {}
func (c *fenceTestCache) publish(t *testing.T, objects []store.Object) {
	t.Helper()
	snap, err := zonecache.Prepare(zonecache.PrepareOptions{Kinds: []store.Kind{"Integration", "DNSZone", "DNSRecordSet", "ClientGroup"}}, objects, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.snap.Store(snap)
}

// fenceTestProvider hands its long-lived Start context and its request host
// to the test, and makes no request of its own.
type fenceTestProvider struct {
	host    integration.Host
	started chan<- fenceTestStart
}

type fenceTestStart struct {
	ctx  context.Context
	host ih.RequestHost
}

func (p *fenceTestProvider) Manifest() integration.Manifest {
	return integration.Manifest{Type: "cloudflare"}
}
func (p *fenceTestProvider) Provision(_ context.Context, h integration.Host, _ json.RawMessage, _ integration.SecretResolver) error {
	p.host = h
	return nil
}
func (p *fenceTestProvider) Validate(context.Context) error { return nil }
func (p *fenceTestProvider) Check(context.Context) (integration.Status, error) {
	return integration.Status{Authenticated: true, Reachable: true}, nil
}
func (p *fenceTestProvider) Start(ctx context.Context) error {
	rh, ok := p.host.(ih.RequestHost)
	if !ok {
		return errors.New("no request host")
	}
	select {
	case p.started <- fenceTestStart{ctx: ctx, host: rh}:
	case <-ctx.Done():
	}
	<-ctx.Done()
	return nil
}
func (p *fenceTestProvider) Stop(context.Context) error { return nil }

// TestKubeIntegrationFenceRefusesChangedAuthority drives the Kubernetes root
// adapter's pre-effect check (D384) with a real envtest store, a real Lease
// elector and a real host, while the host's captured cut stays unchanged.
// After each change, a provider request and its budget write, and a status
// write, are refused with ErrFenced: no request runs and no Secret or status
// is written. The changes: the Lease rewritten to another holder while the
// term context is still live; the Integration deleted and re-created under
// the same name; its spec changed; only its resourceVersion changed; a
// captured record set's spec changed; a captured record set reported
// Duplicated by the controller (status only); the captured license expired.
// The license line of check itself is driven directly with a lapsed
// original cut while the latest capture is current. A
// controller status write on a captured record set, which changes no
// published input, does not fence. An unchanged control passes every step.
func TestKubeIntegrationFenceRefusesChangedAuthority(t *testing.T) {
	control := &envtest.Environment{BinaryAssetsDirectory: mountedKubeTestAssets(t), CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
	control.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "127.0.0.1").Set("endpoint-reconciler-type", "none")
	restConfig, err := control.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := control.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, add := range []func(*apiruntime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	var identity corev1.Namespace
	if err := admin.Get(t.Context(), client.ObjectKey{Name: "kube-system"}, &identity); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("test-only-private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	type harness struct {
		name    string
		store   *kube.Store
		root    *kubeIntegrationRuntime
		cache   *fenceTestCache
		start   fenceTestStart
		advance func(time.Duration)
		objects func() []store.Object
	}
	setup := func(t *testing.T, name string) *harness {
		t.Helper()
		ctx := t.Context()
		st, err := kube.New(restConfig, kube.Options{Namespace: "default", Identity: "fence-" + name, Scheme: scheme, InternalKinds: privateStoreKinds(), ExpectedClusterID: string(identity.UID), StartupTimeout: 5 * time.Second, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		if err := st.CheckClusterIdentity(ctx); err != nil {
			t.Fatal(err)
		}
		var offset atomic.Int64
		clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
		lic, err := license.New(license.Options{Keyring: license.Keyring{"t": pub}, Now: clock})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := license.Sign(priv, "t", license.Claims{ID: "lic_t", Licensee: "Fence Test", Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureIntegrations}, IssuedAt: time.Now().Add(-time.Hour).Unix(), ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		if err := lic.Set(raw, "test"); err != nil {
			t.Fatal(err)
		}
		zoneName, recordName := "fence-"+name, "fence-"+name
		for _, object := range []client.Object{
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: zoneName}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: name + ".example.", Nameservers: []string{"ns." + name + ".example."}}},
			&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: recordName, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: zoneName}, Endpoints: []public.Endpoint{{DNSName: "web." + name + ".example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}}}}},
			&public.Integration{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.IntegrationSpec{Type: "cloudflare", Resync: metav1.Duration{Duration: time.Hour}, Credentials: map[string]public.CredentialReference{"apiToken": {File: credential}}, Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: name + ".example", ZoneID: integrationCloudflareZone}}}}},
		} {
			if err := admin.Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		}
		objects := func() []store.Object {
			var z public.DNSZone
			var rs public.DNSRecordSet
			var d public.Integration
			for key, into := range map[store.Key]store.Object{
				{Kind: "DNSZone", Name: zoneName}:                              &z,
				{Kind: "DNSRecordSet", Namespace: "default", Name: recordName}: &rs,
				{Kind: "Integration", Namespace: "default", Name: name}:        &d,
			} {
				if err := st.Get(ctx, key, into); err != nil {
					t.Fatal(err)
				}
			}
			return []store.Object{&z, &rs, &d}
		}
		cache := &fenceTestCache{init: make(chan struct{})}
		close(cache.init)
		cache.publish(t, objects())
		started := make(chan fenceTestStart, 1)
		r, err := newKubeIntegrationRuntime(kubeIntegrationOptions{
			Store: st, Cache: cache, Namespace: "default", License: lic, FingerprintKey: key, Now: clock, Logger: slog.New(slog.DiscardHandler),
			Credentials: config.CredentialGrants{AllowedFiles: []string{credential}},
			Factory: func(string) (integration.Integration, error) {
				return &fenceTestProvider{started: started}, nil
			},
			// No capture after the first within the test: the host's cut stays
			// the one taken before each change.
			PollInterval: time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Run(runCtx); err != nil {
				t.Error("host:", err)
			}
		}()
		t.Cleanup(func() { cancel(); wg.Wait() })
		h := &harness{name: name, store: st, root: r, cache: cache, advance: func(d time.Duration) { offset.Add(int64(d)) }, objects: objects}
		select {
		case h.start = <-started:
		case <-time.After(60 * time.Second):
			t.Fatal("the instance never started under its Lease")
		}
		return h
	}
	budgets := func(t *testing.T) map[string]string {
		t.Helper()
		var list corev1.SecretList
		if err := admin.List(t.Context(), &list, client.InNamespace("default"), client.MatchingLabels{kube.InternalKindLabel: kinds.KindIntegrationBudget}); err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, sec := range list.Items {
			out[sec.Name] = sec.ResourceVersion
		}
		return out
	}
	integrationOf := func(t *testing.T, name string) *public.Integration {
		t.Helper()
		var d public.Integration
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: name}, &d); err != nil {
			t.Fatal(err)
		}
		return &d
	}
	// attempt sends one provider request through the host (fence, budget
	// write, fence) and reports whether it ran and what the budget became.
	attempt := func(t *testing.T, h *harness) (bool, bool, error) {
		t.Helper()
		before := budgets(t)
		ran := false
		err := h.start.host.Request(h.start.ctx, "cloudflare", "all", true, func(context.Context) error { ran = true; return nil })
		after := budgets(t)
		changed := len(before) != len(after)
		for name, rv := range after {
			changed = changed || before[name] != rv
		}
		return ran, changed, err
	}
	refused := func(t *testing.T, h *harness) {
		t.Helper()
		ran, budget, err := attempt(t, h)
		if !errors.Is(err, ih.ErrFenced) || ran || budget {
			t.Fatalf("a changed authority was not fenced: err=%v request ran=%v budget written=%v", err, ran, budget)
		}
		before := integrationOf(t, h.name)
		if err := h.start.host.SetCondition(h.start.ctx, "Ready", "ProviderCondition", "", true); err == nil {
			after := integrationOf(t, h.name)
			if after.ResourceVersion != before.ResourceVersion || meta.FindStatusCondition(after.Status.Conditions, "Ready") != nil {
				t.Fatal("a fenced status write reached the Integration")
			}
		}
	}

	t.Run("unchanged", func(t *testing.T) {
		h := setup(t, "unchanged")
		ran, budget, err := attempt(t, h)
		if err != nil || !ran || !budget {
			t.Fatalf("the unchanged control was refused: err=%v request ran=%v budget written=%v", err, ran, budget)
		}
		if err := h.start.host.SetCondition(h.start.ctx, "Ready", "ProviderCondition", "", true); err != nil {
			t.Fatal(err)
		}
		if c := meta.FindStatusCondition(integrationOf(t, h.name).Status.Conditions, "Ready"); c == nil {
			t.Fatal("the unchanged control's status was not written")
		}
	})
	t.Run("lease-successor", func(t *testing.T) {
		h := setup(t, "lease")
		var leases coordinationv1.LeaseList
		if err := admin.List(t.Context(), &leases, client.InNamespace("default")); err != nil {
			t.Fatal(err)
		}
		var held *coordinationv1.Lease
		for i := range leases.Items {
			if l := &leases.Items[i]; l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == "fence-lease" {
				held = l
			}
		}
		if held == nil {
			t.Fatal("no Lease held by the instance")
		}
		successor, now := "fence-successor", metav1.NewMicroTime(time.Now())
		transitions := int32(1)
		if held.Spec.LeaseTransitions != nil {
			transitions = *held.Spec.LeaseTransitions + 1
		}
		held.Spec.HolderIdentity, held.Spec.AcquireTime, held.Spec.RenewTime, held.Spec.LeaseTransitions = &successor, &now, &now, &transitions
		if err := admin.Update(t.Context(), held); err != nil {
			t.Fatal(err)
		}
		if h.start.ctx.Err() != nil {
			t.Fatal("the term ended before the check could be exercised")
		}
		refused(t, h)
		if h.start.ctx.Err() != nil {
			t.Log("the elector noticed the successor during the check; the refusal above came first")
		}
	})
	t.Run("integration-recreated", func(t *testing.T) {
		h := setup(t, "recreated")
		d := integrationOf(t, h.name)
		if err := admin.Delete(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		again := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: d.Name, Namespace: d.Namespace}, Spec: d.Spec}
		if err := admin.Create(t.Context(), again); err != nil {
			t.Fatal(err)
		}
		refused(t, h)
	})
	t.Run("integration-spec", func(t *testing.T) {
		h := setup(t, "spec")
		d := integrationOf(t, h.name)
		d.Spec.Resync = metav1.Duration{Duration: 2 * time.Hour}
		if err := admin.Update(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		refused(t, h)
	})
	t.Run("integration-resourceversion", func(t *testing.T) {
		h := setup(t, "version")
		d := integrationOf(t, h.name)
		d.Annotations = map[string]string{"example.com/touched": "yes"}
		if err := admin.Update(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		refused(t, h)
	})
	t.Run("source-spec", func(t *testing.T) {
		h := setup(t, "source")
		var rs public.DNSRecordSet
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "fence-source"}, &rs); err != nil {
			t.Fatal(err)
		}
		rs.Spec.Endpoints[0].Targets = []string{"192.0.2.81"}
		if err := admin.Update(t.Context(), &rs); err != nil {
			t.Fatal(err)
		}
		h.cache.publish(t, h.objects())
		refused(t, h)
	})
	t.Run("source-status-only", func(t *testing.T) {
		h := setup(t, "status")
		var rs public.DNSRecordSet
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "fence-status"}, &rs); err != nil {
			t.Fatal(err)
		}
		rs.Status.ObservedGeneration = rs.Generation
		rs.Status.Names = 1
		meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Programmed", Message: "test"})
		if err := admin.Status().Update(t.Context(), &rs); err != nil {
			t.Fatal(err)
		}
		h.cache.publish(t, h.objects())
		ran, budget, err := attempt(t, h)
		if err != nil || !ran || !budget {
			t.Fatalf("a record set status write fenced the instance: err=%v request ran=%v budget written=%v", err, ran, budget)
		}
	})
	t.Run("license-expired", func(t *testing.T) {
		h := setup(t, "license")
		h.advance(24*time.Hour + license.Grace + time.Minute)
		refused(t, h)
	})
	// The identity of a source leaves its status out, so a conflict the
	// controller reports after the capture is read from the live snapshot
	// by check: the next effect is fenced, before any new capture.
	t.Run("source-conflict-reported", func(t *testing.T) {
		h := setup(t, "conflict")
		var rs public.DNSRecordSet
		if err := admin.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: "fence-conflict"}, &rs); err != nil {
			t.Fatal(err)
		}
		rs.Status.ObservedGeneration = rs.Generation
		meta.SetStatusCondition(&rs.Status.Conditions, metav1.Condition{Type: public.ConditionDegraded, Status: metav1.ConditionTrue, Reason: public.ReasonDuplicated, Message: "test", ObservedGeneration: rs.Generation})
		if err := admin.Status().Update(t.Context(), &rs); err != nil {
			t.Fatal(err)
		}
		h.cache.publish(t, h.objects())
		refused(t, h)
	})
	// The license line of check itself: with the latest capture current, a
	// request whose original cut's license is no longer current is fenced
	// by that line alone. Every earlier step passes, as the control shows.
	t.Run("license-line-in-check", func(t *testing.T) {
		h := setup(t, "cutlicense")
		h.root.mu.RLock()
		c := h.root.cut
		h.root.mu.RUnlock()
		var owner ih.InstanceID
		var revision string
		for o, i := range c.instances {
			owner, revision = o, i.Definition().ResourceVersion
		}
		capture, err := h.root.host.CaptureCommand(t.Context(), owner, revision)
		if err != nil {
			t.Fatal(err)
		}
		_, g, err := capture.Bind(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := h.root.check(t.Context(), h.start.ctx, g, c, false); err != nil {
			t.Fatal("the control check was refused:", err)
		}
		lapsed := *c
		lapsed.license = license.Cut{}
		if err := h.root.check(t.Context(), h.start.ctx, g, &lapsed, false); !errors.Is(err, ih.ErrFenced) {
			t.Fatal("an original cut whose license is not current passed check:", err)
		}
	})
}

// Kubernetes source selection names, like SQLite's duplicated projection
// refusal, every admitted record set for whose current generation the
// controller reports that the zone drops or rejects part of it; the capture
// then refuses every publisher (SourceConflicted). A stale report (an older
// generation), a healthy one and a status-only change do not block, and a
// status write does not change a source's identity (D384).
func TestKubeIntegrationSourcesHonorReportedConflicts(t *testing.T) {
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home", UID: "z", ResourceVersion: "1"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}}}
	record := func(name string, generation int64, conditions ...metav1.Condition) *public.DNSRecordSet {
		return &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: "u-" + apitypes.UID(name), ResourceVersion: "2", Generation: generation}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}}}}, Status: public.DNSRecordSetStatus{ObservedGeneration: generation, Conditions: conditions}}
	}
	serving := metav1.Condition{Type: public.ConditionReady, Status: metav1.ConditionTrue, Reason: public.ReasonServing, ObservedGeneration: 2}
	duplicated := metav1.Condition{Type: public.ConditionDegraded, Status: metav1.ConditionTrue, Reason: public.ReasonDuplicated, ObservedGeneration: 2}
	occluded := metav1.Condition{Type: public.ConditionReady, Status: metav1.ConditionFalse, Reason: public.ReasonOccluded, ObservedGeneration: 2}
	staleDuplicated := duplicated
	staleDuplicated.ObservedGeneration = 1
	sources := func(records ...*public.DNSRecordSet) ([]plan.Source, []string, error) {
		objects := []store.Object{z}
		for _, r := range records {
			objects = append(objects, r)
		}
		snap, err := zonecache.Prepare(zonecache.PrepareOptions{Kinds: []store.Kind{"DNSZone", "DNSRecordSet"}}, objects, nil)
		if err != nil {
			t.Fatal(err)
		}
		return kubeIntegrationSources(snap.Objects)
	}
	for name, tc := range map[string]struct {
		records    []*public.DNSRecordSet
		want       int
		conflicted []string
	}{
		"healthy":              {[]*public.DNSRecordSet{record("a", 2, serving)}, 1, nil},
		"not reported yet":     {[]*public.DNSRecordSet{record("a", 2)}, 1, nil},
		"duplicated":           {[]*public.DNSRecordSet{record("a", 2, serving), record("b", 2, serving, duplicated)}, 1, []string{"default/b (Duplicated)"}},
		"occluded":             {[]*public.DNSRecordSet{record("a", 2, occluded)}, 0, []string{"default/a (Occluded)"}},
		"stale report":         {[]*public.DNSRecordSet{record("a", 2, staleDuplicated)}, 1, nil},
		"duplicate unreported": {[]*public.DNSRecordSet{record("a", 2), record("b", 2)}, 2, nil},
	} {
		got, conflicted, err := sources(tc.records...)
		if err != nil || len(got) != tc.want || !slices.Equal(conflicted, tc.conflicted) {
			t.Errorf("%s: %d sources, conflicted %v, %v; want %d, %v", name, len(got), conflicted, err, tc.want, tc.conflicted)
		}
	}
	// The message names at most five record sets and counts the rest.
	many := []string{"n/1 (Invalid)", "n/2 (Invalid)", "n/3 (Invalid)", "n/4 (Invalid)", "n/5 (Invalid)", "n/6 (Invalid)", "n/7 (Invalid)"}
	if m := kubeConflictMessage(many); !strings.Contains(m, "n/5 (Invalid) and 2 more") || strings.Contains(m, "n/6") {
		t.Error("conflict message", m)
	}
	healthy := record("a", 2, serving)
	written := healthy.DeepCopy()
	written.ResourceVersion = "3"
	written.Status.Names = 1
	written.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "controller", Subresource: "status"}}
	if kubeSourceIdentity(healthy) == "" || kubeSourceIdentity(healthy) != kubeSourceIdentity(written) {
		t.Fatal("a status write changed the source identity")
	}
	changed := healthy.DeepCopy()
	changed.Spec.Endpoints[0].Targets = []string{"192.0.2.81"}
	relabeled := healthy.DeepCopy()
	relabeled.Labels = map[string]string{"publish": "no"}
	recreated := healthy.DeepCopy()
	recreated.UID = "other"
	for name, o := range map[string]*public.DNSRecordSet{"spec": changed, "labels": relabeled, "uid": recreated} {
		if kubeSourceIdentity(o) == kubeSourceIdentity(healthy) {
			t.Errorf("a %s change kept the source identity", name)
		}
	}
}
