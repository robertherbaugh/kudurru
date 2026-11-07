package controller

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// sqliteStatusProjection is an explicit TEST adapter. Root supplies protected
// whole leases, complete dependency pins and original elected tokens in the
// production adapter. This fixture exercises the controller contract against
// real transaction CAS/no-op/final rollback, without a permissive production
// status path or pretending a planned immutable image is a publication proof.
type sqliteStatusProjection struct {
	s       *sqlite.Store
	scheme  *runtime.Scheme
	repo    *zonestate.Repository
	fence   pr.PublicationFence
	mu      sync.Mutex
	plan    *pr.PreparedImage
	current atomic.Pointer[pr.PreparedImage]
	source  *public.DNSZone
	changed chan struct{}
	writes  atomic.Int64
	calls   atomic.Int64
	before  func()
	final   func(context.Context) error
}

func (p *sqliteStatusProjection) Subscribe() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}
func (p *sqliteStatusProjection) capture(t *testing.T) {
	t.Helper()
	image, err := p.s.ReadGeneration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Explicit local registry, including every zero manifest.
	known := p.scheme.KnownTypes(public.GroupVersion)
	kinds := []replication.Kind{}
	for name, typ := range known {
		if _, ok := known[name+"List"]; ok {
			if _, ok := reflect.New(typ).Interface().(store.Object); ok {
				kinds = append(kinds, replication.Kind{APIVersion: public.GroupVersion.String(), Kind: name})
			}
		}
	}
	slices.SortFunc(kinds, func(a, b replication.Kind) int { return strings.Compare(a.Kind, b.Kind) })
	plan, err := pr.PrepareImage(context.Background(), *image, pr.ImageOptions{ClusterID: p.s.Info().ClusterID, Scheme: p.scheme, Validation: replication.Validation{SchemaVersion: store.SchemaVersion, Kinds: kinds}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var source public.DNSZone
	if err := p.s.Get(context.Background(), store.Key{Kind: KindDNSZone, Name: "primary"}, &source); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.plan, p.source = plan, source.DeepCopy()
	p.current.Store(plan)
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}
func (p *sqliteStatusProjection) Reconcile(ctx context.Context, cached *public.DNSZone) (bool, time.Duration, error) {
	p.calls.Add(1)
	p.mu.Lock()
	plan, source := p.plan, p.source.DeepCopy()
	p.mu.Unlock()
	if cached.UID != source.UID || cached.ResourceVersion != source.ResourceVersion {
		return false, 25 * time.Millisecond, nil
	}
	observation, ok := plan.CommittedPrimaryStatus(source.Name)
	if !ok || !observation.ConfigMatchesDesired {
		return false, 25 * time.Millisecond, nil
	}
	if p.before != nil {
		p.before()
	}
	wrote := false
	err := p.s.Command(ctx, p.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if tx.Incarnation() != p.fence.Incarnation {
			return pr.ErrUnavailable
		}
		var actual public.DNSZone
		if err := tx.Get(ctx, store.KeyOf(source), &actual); err != nil {
			return err
		}
		if actual.UID != source.UID || actual.ResourceVersion != source.ResourceVersion || actual.Generation != source.Generation || !equality.Semantic.DeepEqual(actual.Spec, source.Spec) {
			return store.ErrConflict
		}
		var lifecycle zk.ZoneLifecycle
		if err := tx.Get(ctx, store.Key{Kind: zk.KindZoneLifecycle, Name: zk.LifecycleName(p.fence.Term.ClusterID, source.Name)}, &lifecycle); err != nil {
			return err
		}
		if lifecycle.ResourceVersion != observation.LifecycleResourceVersion || lifecycle.Spec.Source != observation.Source || lifecycle.Spec.HeadZone() != observation.HeadZone {
			return store.ErrConflict
		}
		var head zk.ZoneState
		if err := tx.Get(ctx, store.Key{Kind: zk.KindZoneState, Name: zk.HeadName(observation.HeadZone)}, &head); err != nil {
			return err
		}
		if head.ResourceVersion != observation.HeadResourceVersion || head.Spec.CommitSequence != observation.Member.Version.CommitSequence || head.Spec.RootDigest != observation.Member.Version.RootDigest {
			return store.ErrConflict
		}
		if err := tx.RequireCommitCheck(func(ctx context.Context) error {
			if p.final != nil {
				if err := p.final(ctx); err != nil {
					return err
				}
			}
			if p.current.Load() != plan {
				return store.ErrConflict
			}
			return ctx.Err()
		}); err != nil {
			return err
		}
		next := actual.DeepCopy()
		next.Status.ObservedGeneration = actual.Generation
		next.Status.Serial, next.Status.ContentHash, next.Status.RecordCount = observation.Serial, observation.ContentHash, observation.RecordCount
		meta.SetStatusCondition(&next.Status.Conditions, metav1.Condition{Type: public.ConditionReady, Status: metav1.ConditionTrue, Reason: "CommittedPrimary", Message: "Exact committed primary is published.", ObservedGeneration: actual.Generation, LastTransitionTime: metav1.NewTime(time.Now().Truncate(time.Second))})
		if equality.Semantic.DeepEqual(next.Status, actual.Status) {
			return nil
		}
		if err := tx.UpdateStatus(ctx, next); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	if wrote {
		p.writes.Add(1)
	}
	return wrote, 0, nil
}

func newSQLiteStatusProjection(t *testing.T) *sqliteStatusProjection {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := public.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := zk.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "status.db"), sqlite.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	backend, err := s.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	p := &sqliteStatusProjection{s: s, scheme: scheme, repo: zonestate.NewRepository(backend, zonestate.Limits{}), changed: make(chan struct{}), fence: pr.PublicationFence{Term: s.Authority().Term, Incarnation: s.Authority().Incarnation}}
	ctx := context.Background()
	desired := zoneObj("primary", "example.com.")
	desired.Spec.Nameservers = []string{"ns.example.com."}
	desired.Spec.Policy.QueryAccess = public.AccessAllow
	object, err := s.Apply(ctx, desired, store.ApplyOptions{Manager: "api"})
	if err != nil {
		t.Fatal(err)
	}
	source := object.(*public.DNSZone)
	l, err := pr.NewLifecycle(s.Info().ClusterID, source, nil)
	if err != nil {
		t.Fatal(err)
	}
	in, err := primaryzone.NewReader(s, primaryzone.ReaderOptions{PageSize: 1}).ReadDesired(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := p.repo.Read(ctx, l.Spec.HeadZone())
	compilation, err := primaryzone.NewCompiler(primaryzone.CompilerOptions{}).Project(ctx, base, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Real committed dynamic data is deliberately absent from desired RRsets.
	rr, err := dns.NewRR("dynamic.example.com. 60 IN A 192.0.2.77")
	if err != nil {
		t.Fatal(err)
	}
	record, err := zonestate.FromRR(rr, zonestate.Provenance{Key: "test-key.", CredentialVersion: "test-generation"})
	if err != nil {
		t.Fatal(err)
	}
	compilation.Aggregate.Records = append(compilation.Aggregate.Records, record)
	prepared, err := zonestate.Prepare(compilation.Aggregate, nil, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.repo.Commit(ctx, base.Expected, prepared, nil); err != nil {
		t.Fatal(err)
	}
	// Fixture construction only; production transitions use sqliteadapter.
	l.Spec.Phase, l.Spec.Sequence = zk.PhaseActive, 3
	if _, err := s.Apply(ctx, l, store.ApplyOptions{Manager: "primaryruntime"}); err != nil {
		t.Fatal(err)
	}
	p.capture(t)
	return p
}
func (p *sqliteStatusProjection) alignStatusEcho(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var source public.DNSZone
	if err := p.s.Get(ctx, store.Key{Kind: KindDNSZone, Name: "primary"}, &source); err != nil {
		t.Fatal(err)
	}
	var l zk.ZoneLifecycle
	if err := p.s.Get(ctx, store.Key{Kind: zk.KindZoneLifecycle, Name: zk.LifecycleName(p.fence.Term.ClusterID, source.Name)}, &l); err != nil {
		t.Fatal(err)
	}
	token, err := primaryzone.SourceToken(&source)
	if err != nil {
		t.Fatal(err)
	}
	l.Spec.Sequence++
	l.Spec.Source = zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest}
	if _, err := p.s.Apply(ctx, &l, store.ApplyOptions{Manager: "primaryruntime", ExpectedResourceVersion: l.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	p.capture(t)
}

func TestCommittedPrimaryStatusActualSQLiteProjectionAndQuiescentEcho(t *testing.T) {
	p := newSQLiteStatusProjection(t)
	e := newEnvOver(t, p.s, func(o *Options) { o.CommittedPrimaryStatus = p })
	e.start()
	expected, _ := p.plan.CommittedPrimaryStatus("primary")
	e.eventually("committed dynamic metadata", func() bool { return e.zone("primary").Status.ContentHash == expected.ContentHash })
	z := e.zone("primary")
	if z.Status.Serial != expected.Serial || z.Status.RecordCount != expected.RecordCount || expected.RecordCount == 0 || e.st.Writes(KindDNSZone) != 0 || e.eventCount(EventSerialBumped, key(KindDNSZone, "", "primary")) != 0 {
		t.Fatal("legacy status source competed", z.Status)
	}
	before := p.calls.Load()
	p.alignStatusEcho(t)
	e.eventually("publication echo observed", func() bool { return p.calls.Load() > before })
	e.settle()
	if p.writes.Load() != 1 {
		t.Fatal("status RV echo never quiesced", p.writes.Load())
	}
	if o, _ := p.plan.CommittedPrimaryStatus("primary"); o.Member.Version != expected.Member.Version || o.CommittedSource != expected.CommittedSource {
		t.Fatal("echo fabricated new head/receipt", o)
	}
}

func TestCommittedPrimaryStatusActualSQLiteHeldHeadCASAndFinalRollback(t *testing.T) {
	p := newSQLiteStatusProjection(t)
	ctx := context.Background()
	captured := p.source.DeepCopy()
	entered, release := make(chan struct{}), make(chan struct{})
	p.before = func() { close(entered); <-release }
	result := make(chan error, 1)
	go func() { _, _, err := p.Reconcile(ctx, captured); result <- err }()
	<-entered
	o, _ := p.plan.CommittedPrimaryStatus("primary")
	view, err := p.repo.Read(ctx, o.HeadZone)
	if err != nil {
		t.Fatal(err)
	}
	a := *view.Aggregate
	rr, err := dns.NewRR("new.example.com. 60 IN A 192.0.2.88")
	if err != nil {
		t.Fatal(err)
	}
	record, err := zonestate.FromRR(rr, zonestate.Provenance{Key: "test-key.", CredentialVersion: "test-generation"})
	if err != nil {
		t.Fatal(err)
	}
	a.Records = append(append([]zonestate.Record(nil), a.Records...), record)
	for i, r := range a.Records {
		decoded, _, err := dns.UnpackRR(r.Wire, 0)
		if err != nil {
			t.Fatal(err)
		}
		if soa, ok := decoded.(*dns.SOA); ok {
			soa.Serial++
			a.Records[i], err = zonestate.FromRR(soa, r.Provenance)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	prepared, err := zonestate.Prepare(&a, view.Head, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.repo.Commit(ctx, view.Expected, prepared, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; !errors.Is(err, store.ErrConflict) {
		t.Fatal("head advanced without source RV but old projection wrote", err)
	}
	if p.writes.Load() != 0 {
		t.Fatal("stale status persisted")
	}
	p.before = nil
	p.capture(t)
	denied := errors.New("test complete dependency expired at final fence")
	p.final = func(context.Context) error { return denied }
	if wrote, _, err := p.Reconcile(ctx, p.source.DeepCopy()); wrote || !errors.Is(err, denied) {
		t.Fatal("final check failed to roll back status", wrote, err)
	}
	var actual public.DNSZone
	if err := p.s.Get(ctx, store.KeyOf(captured), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.Serial != 0 || actual.Status.ContentHash != "" {
		t.Fatal("failed final check leaked status", actual.Status)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if wrote, _, err := p.Reconcile(canceled, p.source.DeepCopy()); wrote || err == nil {
		t.Fatal("canceled source request wrote", err)
	}
	p.final = nil
	if wrote, _, err := p.Reconcile(ctx, p.source.DeepCopy()); !wrote || err != nil {
		t.Fatal("fresh exact head rejected", err)
	}
	p.alignStatusEcho(t)
	p.final = func(context.Context) error { return denied }
	if wrote, _, err := p.Reconcile(ctx, p.source.DeepCopy()); wrote || !errors.Is(err, denied) {
		t.Fatal("unchanged status skipped final admission", wrote, err)
	}
}
