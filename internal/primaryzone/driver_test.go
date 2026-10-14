package primaryzone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func scheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{public.AddToScheme, zonekinds.AddToScheme, corev1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func openSQLite(t testing.TB, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func kubeStore(t testing.TB) *kube.Store {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		matches, _ := filepath.Glob("../../bin/k8s/*")
		if len(matches) > 0 {
			assets = matches[len(matches)-1]
		}
	}
	if _, err := os.Stat(filepath.Join(assets, "kube-apiserver")); err != nil { //nolint:gosec // G703: trusted test asset path selects local envtest binaries.
		t.Skip("real envtest binaries unavailable; run make envtest")
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: scheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dns"}}); err != nil {
		t.Fatal(err)
	}
	s, err := kube.New(cfg, kube.Options{Scheme: scheme(t), InternalKinds: []store.Kind{zonekinds.KindZoneState, zonekinds.KindZoneChunk}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type stateDriver interface {
	store.Store
	ZoneStateBackend(zonestate.Limits) (zonestate.Backend, error)
}

func apply(t testing.TB, st store.Store, obj store.Object, manager string) store.Object {
	t.Helper()
	saved, err := st.Apply(context.Background(), obj, store.ApplyOptions{Manager: manager, ExpectedResourceVersion: obj.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

type compileFunc func(context.Context, *zonestate.View, *zonestate.DesiredInputs, time.Time) (*zonestate.Compilation, error)

func (f compileFunc) Project(ctx context.Context, v *zonestate.View, in *zonestate.DesiredInputs, now time.Time) (*zonestate.Compilation, error) {
	return f(ctx, v, in, now)
}
func TestActualDriversDesiredReconcileRaceAndExpiry(t *testing.T) {
	for _, driver := range []string{"sqlite", "kube"} {
		t.Run(driver, func(t *testing.T) {
			var st stateDriver
			if driver == "sqlite" {
				st = openSQLite(t, filepath.Join(t.TempDir(), "state.db"))
			} else {
				st = kubeStore(t)
			}
			backend, err := st.ZoneStateBackend(zonestate.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			repo := zonestate.NewRepository(backend, zonestate.Limits{})
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			z := desiredZone()
			z.UID = ""
			z.ResourceVersion = ""
			z.CreationTimestamp = metav1.Time{}
			z = apply(t, st, z, "api").(*public.DNSZone)
			rs := desiredSet("source", endpoint("static", "192.0.2.1"))
			rs.UID = ""
			rs.ResourceVersion = ""
			rs.CreationTimestamp = metav1.Time{}
			rs = apply(t, st, rs, "api").(*public.DNSRecordSet)
			id := zonekinds.ZoneID{ClusterID: st.Info().ClusterID, ObjectName: z.Name, UID: string(z.UID), Apex: "example."}
			if id.ClusterID == "" {
				id.ClusterID = "envtest-cluster"
			}
			var clock atomic.Int64
			clock.Store(fixedNow.UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()) }
			m, err := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			policy := &zonestate.Policy{Enabled: true, AuthEpoch: 1, Credentials: []dynamicupdate.Credential{{Key: "key.", Version: "v1"}}, Grants: []dynamicupdate.Grant{{Key: "key.", Owner: "*.example."}}}
			reader := NewReader(st, ReaderOptions{PageSize: 1, Policy: func(context.Context, *public.DNSZone) (*zonestate.Policy, error) { return policy, nil }})
			c := NewCompiler(CompilerOptions{})
			coord, err := zonestate.NewCoordinator(zonestate.CoordinatorOptions{Repository: repo, Reader: reader, Compiler: c, Materializer: m, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			initial, err := coord.Reconcile(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if m.Capture(now()).Match("static.example.").Lookup("static.example.", dns.TypeA).Kind != zone.Success {
				t.Fatal("actual primary not materialized")
			}
			token, _ := SourceToken(rs)
			target := zonestate.DesiredTarget{Zone: id, Source: token, Operation: "apply"}
			if got, err := m.WaitDesired(ctx, target); err != nil || got.Outcome != "committed" || !got.Published {
				t.Fatalf("initial ack %+v %v", got, err)
			}
			// Pause a candidate after the old head and desired source were read.
			reached := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			blocked := compileFunc(func(ctx context.Context, v *zonestate.View, in *zonestate.DesiredInputs, at time.Time) (*zonestate.Compilation, error) {
				projection, err := c.Project(ctx, v, in, at)
				if calls.Add(1) == 1 {
					close(reached)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return projection, err
			})
			raced, err := zonestate.NewCoordinator(zonestate.CoordinatorOptions{Repository: repo, Reader: reader, Compiler: blocked, Materializer: m, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			// Force a pending desired revision so the stale candidate performs a CAS.
			rs.Spec.Endpoints[0].Targets = []string{"192.0.2.2"}
			rs = apply(t, st, rs, "api").(*public.DNSRecordSet)
			done := make(chan error, 1)
			go func() { _, err := raced.Reconcile(ctx, id); done <- err }()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			fresh, err := repo.Read(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			msg := new(dns.Msg)
			msg.SetUpdate(id.Apex)
			rr, _ := dns.NewRR("dynamic.example. 61 IN TYPE65200 \\# 3 ABCDEF")
			msg.Ns = []dns.RR{rr}
			tx, protocol := dynamicupdate.Parse(msg, dynamicupdate.Limits{})
			if protocol != nil {
				t.Fatal(protocol)
			}
			state, err := fresh.UpdateState()
			if err != nil {
				t.Fatal(err)
			}
			principal := dynamicupdate.Principal{Key: "key.", Version: "v1"}
			plan, protocol := dynamicupdate.Plan(state, tx, principal, now(), dynamicupdate.Limits{})
			if protocol != nil {
				t.Fatal(protocol)
			}
			aggregate, err := zonestate.ApplyResult(fresh.Aggregate, plan, principal)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := zonestate.Prepare(aggregate, fresh.Head, zonestate.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			update, err := repo.Commit(ctx, fresh.Expected, prepared, nil)
			if err != nil {
				t.Fatal(err)
			}
			// File writer creates another source while API writer replaces its source.
			file := desiredSet("file", endpoint("file", "192.0.2.4"))
			file.UID = ""
			file.ResourceVersion = ""
			file.CreationTimestamp = metav1.Time{}
			_ = apply(t, st, file, store.ManagerFile("/tmp/primary-zone.yaml"))
			rs.Spec.Endpoints[0].Targets = []string{"192.0.2.3"}
			rs = apply(t, st, rs, "api").(*public.DNSRecordSet)
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			final, err := repo.Read(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() < 2 || final.Head.Spec.CommitSequence != update.Head.Spec.CommitSequence+1 || !dynamicupdate.RFC1982Greater(final.Head.Spec.Serial, update.Head.Spec.Serial) {
				t.Fatal("stale candidate not retried from fresh head")
			}
			model := m.Capture(now()).Match("static.example.")
			if model.Lookup("static.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.3" || model.Lookup("dynamic.example.", 65200).Answer[0].(*dns.RFC3597).Rdata != "abcdef" || model.Lookup("file.example.", dns.TypeA).Kind != zone.Success {
				t.Fatal("lost API/file/UPDATE data")
			}
			if initial.View.Head.Spec.Serial == final.Head.Spec.Serial {
				t.Fatal("serial not committed")
			}
			// A committed deadline fails closed even while no expiry worker can run.
			expires := metav1.NewTime(now().Add(time.Minute))
			rs.Spec.Endpoints[0].Kudurru = &public.EndpointExt{ExpiresAt: &expires}
			rs = apply(t, st, rs, "api").(*public.DNSRecordSet)
			lease, err := coord.Reconcile(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			pinned := m.Capture(now())
			clock.Store(expires.UnixNano())
			if m.Capture(now()).Match("static.example.").AvailableAt(now()) {
				t.Fatal("expired generation available")
			}
			if pinned.Match("static.example.").Lookup("static.example.", dns.TypeA).Kind != zone.Success {
				t.Fatal("captured generation changed")
			}
			expired, err := coord.Expire(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if expired.View.Head.Spec.Serial == lease.View.Head.Spec.Serial || len(expired.View.Aggregate.Reservations) == 0 || m.Capture(now()).Match("dynamic.example.").Lookup("dynamic.example.", 65200).Kind != zone.Success {
				t.Fatal("expiry commit lost serial/ownership/unrelated data")
			}
			// Cold materializer consumes the actual durable aggregate, no desired reader.
			cold, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted, Now: now})
			durable, err := repo.Read(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := cold.Load(ctx, durable); err != nil {
				t.Fatal(err)
			}
			if cold.Capture(now()).Match("file.example.").Lookup("file.example.", dns.TypeA).Kind != zone.Success {
				t.Fatal("durable offline materialization")
			}
			// Revocation is policy-only: new auth epoch, same serial, preserved versions.
			policy.Enabled = false
			revoked, err := coord.Reconcile(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if revoked.View.Head.Spec.AuthEpoch != durable.Head.Spec.AuthEpoch+1 || revoked.View.Head.Spec.Serial != durable.Head.Spec.Serial {
				t.Fatal("revocation barrier")
			}
			// Reader errors cannot become an accidental prune.
			if _, err := NewReader(st, ReaderOptions{MaxSources: 1}).ReadDesired(ctx, id); !errors.Is(err, zonestate.ErrCapacity) {
				t.Fatalf("incomplete list accepted %v", err)
			}
			if err := st.Delete(ctx, store.KeyOf(rs), store.DeleteOptions{Manager: "api"}); err != nil {
				t.Fatal(err)
			}
			removed, err := coord.Reconcile(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			lastToken, _ := SourceToken(rs)
			got := m.Desired(zonestate.DesiredTarget{Zone: id, Source: lastToken, Operation: "delete"})
			if got.Outcome != "committed" || got.CommitSequence != removed.View.Head.Spec.CommitSequence {
				t.Fatalf("durable prune ack %+v", got)
			}
		})
	}
}

func TestCoordinatorPrepareAndGuardFailuresLeaveDurableDesiredPending(t *testing.T) {
	st := openSQLite(t, filepath.Join(t.TempDir(), "state.db"))
	z := desiredZone()
	z.UID = ""
	z.ResourceVersion = ""
	z.CreationTimestamp = metav1.Time{}
	z = apply(t, st, z, "api").(*public.DNSZone)
	rs := desiredSet("source", endpoint("host", "192.0.2.1"))
	rs.UID = ""
	rs.ResourceVersion = ""
	rs.CreationTimestamp = metav1.Time{}
	rs = apply(t, st, rs, "api").(*public.DNSRecordSet)
	id := zonekinds.ZoneID{ClusterID: st.Info().ClusterID, ObjectName: z.Name, UID: string(z.UID), Apex: "example."}
	backend, err := st.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	repo := zonestate.NewRepository(backend, zonestate.Limits{})
	fault := errors.New("prepare fault")
	var failBuild atomic.Bool
	m, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: func(ctx context.Context, v *zonestate.View) (*zone.Zone, error) {
		if failBuild.Load() {
			return nil, fault
		}
		return BuildCommitted(ctx, v)
	}})
	opts := zonestate.CoordinatorOptions{Repository: repo, Reader: NewReader(st, ReaderOptions{Targets: m.PendingTargets}), Compiler: NewCompiler(CompilerOptions{}), Materializer: m, Now: func() time.Time { return fixedNow }}
	c, _ := zonestate.NewCoordinator(opts)
	initial, err := c.Reconcile(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	rs.Spec.Endpoints[0].Targets = []string{"192.0.2.2"}
	rs = apply(t, st, rs, "api").(*public.DNSRecordSet)
	token, _ := SourceToken(rs)
	target := zonestate.DesiredTarget{Zone: id, Source: token, Operation: "apply"}
	if err := m.RegisterDesired(target); err != nil {
		t.Fatal(err)
	}
	failBuild.Store(true)
	if _, err := c.Reconcile(context.Background(), id); !errors.Is(err, fault) {
		t.Fatalf("prebuild failure %v", err)
	}
	failBuild.Store(false)
	denied := errors.New("authority revoked at commit")
	opts.Guard = func(context.Context, *zonekinds.ZoneState) error { return denied }
	guarded, _ := zonestate.NewCoordinator(opts)
	if _, err := guarded.Reconcile(context.Background(), id); !errors.Is(err, denied) {
		t.Fatalf("guard failure %v", err)
	}
	latest, err := repo.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Head.Spec.RootDigest != initial.View.Head.Spec.RootDigest || m.Capture(fixedNow).Match("host.example.").Lookup("host.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.1" {
		t.Fatal("failed candidate became served or durable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	pending, err := m.WaitDesired(ctx, target)
	if !errors.Is(err, context.DeadlineExceeded) || pending.Outcome != "pending" {
		t.Fatalf("durable desired acceptance misreported %+v %v", pending, err)
	}
	var current public.DNSRecordSet
	if err := st.Get(context.Background(), store.KeyOf(rs), &current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Endpoints[0].Targets[0] != "192.0.2.2" {
		t.Fatal("desired acceptance rolled back")
	}
	if _, err := c.Reconcile(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got, err := m.WaitDesired(context.Background(), target); err != nil || !got.Published {
		t.Fatalf("retry publication %+v %v", got, err)
	}
}
