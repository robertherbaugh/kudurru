package primaryruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type driver interface {
	store.Store
	ZoneStateBackend(zonestate.Limits) (zonestate.Backend, error)
}
type readApply interface {
	Get(context.Context, store.Key, store.Object) error
	Apply(context.Context, store.Object, store.ApplyOptions) (store.Object, error)
}
type harness struct {
	t           *testing.T
	st          driver
	r           *Runtime
	repo        *zonestate.Repository
	publishFail bool
	authFail    bool
	admissions  atomic.Int64
	policy      zonestate.Policy
}

func runtimeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{public.AddToScheme, zk.AddToScheme, corev1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func runtimeDriver(t *testing.T, kind string) driver {
	t.Helper()
	if kind == "sqlite" {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Scheme: runtimeScheme(t)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		assets = "../../bin/k8s/1.37.0-linux-amd64"
	}
	//nolint:gosec // G703: trusted local envtest asset directory.
	if _, err := os.Stat(filepath.Join(assets, "kube-apiserver")); err != nil {
		t.Fatal("native envtest assets required", err)
	} //nolint:gosec // Trusted local test asset directory.
	e := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../deploy/kustomize/crd/bases"}, ErrorIfCRDPathMissing: true}
	args := e.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	kinds := make([]store.Kind, 0, len(zk.Kinds))
	for _, k := range zk.Kinds {
		kinds = append(kinds, store.Kind(k))
	}
	s, err := kube.New(cfg, kube.Options{Scheme: runtimeScheme(t), InternalKinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func newHarness(t *testing.T, kind string) *harness {
	t.Helper()
	st := runtimeDriver(t, kind)
	b, err := st.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, st: st, repo: zonestate.NewRepository(b, zonestate.Limits{}), policy: zonestate.Policy{Enabled: true, AuthEpoch: 1, Credentials: []dynamicupdate.Credential{{Key: "key.", Version: "v1"}}, Grants: []dynamicupdate.Grant{{Key: "key.", Owner: "*.example."}}}}
	h.r, err = New(Options{Store: st, Repository: h.repo, Reader: primaryzone.NewReader(st, primaryzone.ReaderOptions{PageSize: 1, Policy: func(context.Context, *public.DNSZone) (*zonestate.Policy, error) { return &h.policy, nil }}), LifecycleCAS: h.lifecycleCAS, Admit: h.admit, Publish: func(context.Context, Cut, Member) error {
		if h.publishFail {
			return ErrUnavailable
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func (h *harness) source(ctx context.Context, st readApply, l *zk.ZoneLifecycle) error {
	var z public.DNSZone
	err := st.Get(ctx, store.Key{Kind: "DNSZone", Name: l.Spec.Zone.ObjectName}, &z)
	if l.Spec.Source.Removed {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return ErrConflict
	}
	if err != nil {
		return err
	}
	token, err := primaryzone.SourceToken(&z)
	if err != nil {
		return err
	}
	if token.UID != l.Spec.Source.UID || token.ResourceVersion != l.Spec.Source.ResourceVersion || token.IntentDigest != l.Spec.Source.IntentDigest {
		return ErrConflict
	}
	return nil
}
func (h *harness) lifecycleCAS(ctx context.Context, current, next *zk.ZoneLifecycle) (*zk.ZoneLifecycle, error) {
	var saved *zk.ZoneLifecycle
	write := func(st readApply) error {
		var actual zk.ZoneLifecycle
		err := st.Get(ctx, store.KeyOf(next), &actual)
		var prior *zk.ZoneLifecycle
		if err == nil {
			prior = &actual
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		rv := ""
		if current != nil {
			rv = current.ResourceVersion
			if prior == nil || !reflect.DeepEqual(prior.Spec, current.Spec) {
				return ErrConflict
			}
		}
		if zk.ValidateLifecycleTransition(prior, rv, next) != nil {
			return ErrConflict
		}
		if err := h.source(ctx, st, next); err != nil {
			return err
		}
		obj, err := st.Apply(ctx, next, store.ApplyOptions{Manager: "primaryruntime", ExpectedResourceVersion: rv, CreateOnly: current == nil})
		if err == nil {
			saved = obj.(*zk.ZoneLifecycle)
		}
		return err
	}
	if sql, ok := h.st.(*sqlite.Store); ok {
		err := sql.Command(ctx, sql.Authority().Term, func(txctx context.Context, tx *sqlite.CommandTx) error { ctx = txctx; return write(tx) })
		return saved, err
	}
	// Explicit test adapter: kube has a real exact-object CAS, not a cross-CR
	// source/HA transaction. Production publication/admission remains root-owned.
	err := write(h.st)
	return saved, err
}
func (h *harness) admit(ctx context.Context, c Command) (*zonestate.View, error) {
	h.admissions.Add(1)
	if h.authFail {
		return nil, ErrUnavailable
	}
	check := func(st readApply) error {
		for _, l := range []*zk.ZoneLifecycle{c.Lifecycle, c.Parent} {
			if l == nil {
				continue
			}
			var actual zk.ZoneLifecycle
			if err := st.Get(ctx, store.KeyOf(l), &actual); err != nil {
				return err
			}
			if actual.ResourceVersion != l.ResourceVersion || !reflect.DeepEqual(actual.Spec, l.Spec) {
				return ErrConflict
			}
			if err := h.source(ctx, st, l); err != nil {
				return err
			}
		}
		return nil
	}
	if sql, ok := h.st.(*sqlite.Store); ok {
		term := sql.Authority().Term
		if c.Expected.ResourceVersion != "" || c.Prepared != nil {
			term = c.Expected.Term
		}
		var head *zk.ZoneState
		err := sql.Command(ctx, term, func(txctx context.Context, tx *sqlite.CommandTx) error {
			if err := checkTx(txctx, tx, c, h); err != nil {
				return err
			}
			if c.Proof != nil && !c.Proof.Term.Same(tx.Authority()) {
				return ErrConflict
			}
			if c.Prepared != nil {
				var err error
				head, err = tx.CommitZoneState(txctx, c.Expected, c.Prepared, zonestate.Limits{}, nil)
				return err
			}
			if c.Expected.ResourceVersion != "" {
				var actual zk.ZoneState
				if err := tx.Get(txctx, store.Key{Kind: zk.KindZoneState, Name: zk.HeadName(c.Expected.Zone)}, &actual); err != nil {
					return err
				}
				if actual.ResourceVersion != c.Expected.ResourceVersion || actual.Spec.CommitSequence != c.Expected.CommitSequence || actual.Spec.RootDigest != c.Expected.RootDigest {
					return ErrConflict
				}
				head = &actual
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if head == nil {
			return nil, nil
		}
		return h.repo.Read(ctx, head.Spec.Zone)
	}
	if err := check(h.st); err != nil {
		return nil, err
	}
	if c.Prepared != nil {
		return h.repo.Commit(ctx, c.Expected, c.Prepared, nil)
	}
	if c.Expected.ResourceVersion == "" {
		return nil, nil
	}
	v, err := h.repo.Read(ctx, c.Expected.Zone)
	if err != nil {
		return nil, err
	}
	if v.Head.ResourceVersion != c.Expected.ResourceVersion {
		return nil, ErrConflict
	}
	return v, nil
}
func checkTx(ctx context.Context, tx *sqlite.CommandTx, c Command, h *harness) error {
	for _, l := range []*zk.ZoneLifecycle{c.Lifecycle, c.Parent} {
		if l == nil {
			continue
		}
		var actual zk.ZoneLifecycle
		if err := tx.Get(ctx, store.KeyOf(l), &actual); err != nil {
			return err
		}
		if actual.ResourceVersion != l.ResourceVersion || !reflect.DeepEqual(actual.Spec, l.Spec) {
			return ErrConflict
		}
		if err := h.source(ctx, tx, l); err != nil {
			return err
		}
	}
	return nil
}
func (h *harness) zone(name, apex string) *public.DNSZone {
	h.t.Helper()
	z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: public.DNSZoneSpec{ZoneName: apex, Type: public.ZoneTypePrimary, Nameservers: []string{"ns." + apex}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow}}}
	saved, err := h.st.Apply(context.Background(), z, store.ApplyOptions{Manager: "api"})
	if err != nil {
		h.t.Fatal(err)
	}
	return saved.(*public.DNSZone)
}
func (h *harness) begin(z *public.DNSZone, parents ...*zk.ZoneLifecycle) *zk.ZoneLifecycle {
	h.t.Helper()
	cluster := h.st.Info().ClusterID
	if cluster == "" {
		cluster = "envtest-cluster"
	}
	l, err := NewLifecycle(cluster, z, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, p := range parents {
		l.Spec.Parents = append(l.Spec.Parents, ParentObligation(p, l))
	}
	l, err = h.r.Transition(context.Background(), nil, l)
	if err != nil {
		h.t.Fatal(err)
	}
	return l
}
func TestActualLifecycleParentsRetirementAndCapturedCut(t *testing.T) {
	for _, kind := range []string{"sqlite", "kube"} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t, kind)
			ctx := context.Background()
			p := h.begin(h.zone("parent", "example."))
			pv, err := h.r.Reconcile(ctx, p.Spec.Zone)
			if err != nil {
				t.Fatal(err)
			}
			p, err = h.r.ReadLifecycle(ctx, p.Spec.Zone)
			if err != nil {
				t.Fatal(err)
			}
			child := h.begin(h.zone("child", "child.example."), p)
			var z public.DNSZone
			if err := h.st.Get(ctx, store.Key{Kind: "DNSZone", Name: "child"}, &z); err != nil {
				t.Fatal(err)
			}
			if err := h.r.Observe(ctx, child, nil, &z); err != nil {
				t.Fatal(err)
			}
			held := h.r.Capture(time.Now())
			if got := held.Match("x.child.example."); got == nil || got.AvailableAt(time.Now()) {
				t.Fatal("preparing child escaped coverage")
			}
			cv, err := h.r.Reconcile(ctx, child.Spec.Zone)
			if err != nil {
				t.Fatal(err)
			}
			live := h.r.Capture(time.Now())
			member, ok := live.Member("child.example.")
			if !ok || !member.Published || member.Version != version(cv) {
				t.Fatal("child activation/publication")
			}
			parentHead, err := h.repo.Read(ctx, p.Spec.HeadZone())
			if err != nil || !hasFence(parentHead, child.Spec.Zone.Apex) || parentHead.Head.Spec.Serial != pv.Head.Spec.Serial {
				t.Fatal("parent fence/serial", err)
			}
			if held.Match("x.child.example.").AvailableAt(time.Now()) {
				t.Fatal("held cut changed")
			}
			member.Parents[0].RootDigest = "mutated"
			again, _ := live.Member("child.example.")
			if again.Parents[0].RootDigest == "mutated" {
				t.Fatal("mutable membership obligations")
			}
			retired, err := h.r.Retire(ctx, child.Spec.Zone)
			if err != nil {
				t.Fatal(err)
			}
			dead, err := h.repo.Read(ctx, child.Spec.HeadZone())
			if err != nil || dead.Head.Spec.Lifecycle != zk.LifecycleTombstoned {
				t.Fatal("no durable tombstone", err)
			}
			if err := h.r.Observe(ctx, retired, cv, nil); err == nil {
				t.Fatal("tombstoned head resurrected")
			}
			if _, err := h.r.ReleaseParents(ctx, child.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			parentHead, err = h.repo.Read(ctx, p.Spec.HeadZone())
			if err != nil || hasFence(parentHead, child.Spec.Zone.Apex) {
				t.Fatal("fence not released after retirement", err)
			}
			if h.r.Capture(time.Now()).Match("x.child.example.").AvailableAt(time.Now()) {
				t.Fatal("retired child fell through")
			}
		})
	}
}
