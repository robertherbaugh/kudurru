package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func integrationOwner(s *servingRuntime, d *public.Integration) kinds.Owner {
	return kinds.Owner{ClusterID: s.opts.Store.Info().ClusterID, Namespace: d.Namespace, Name: d.Name, UID: string(d.UID)}
}
func integrationAwaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(15 * time.Second):
		t.Fatal("background integration did not reach the expected barrier")
	}
}
func integrationReadyCommand(t *testing.T, s *servingRuntime, owner kinds.Owner) {
	t.Helper()
	awaitServingRuntime(t, s, func() bool {
		ready := false
		for _, status := range s.integrations.host.Statuses() {
			ready = ready || status.Owner == owner && status.Phase == "Ready"
		}
		if !ready {
			return false
		}
		// Read-only provider preflight settles the captured instance after its
		// status write. The challenged Sync below remains a single invocation.
		_, err := s.integrations.host.Check(t.Context(), owner)
		return err == nil
	})
}

func integrationNoSource(t *testing.T, s *servingRuntime, owner kinds.Owner) {
	t.Helper()
	var head kinds.IntegrationSourceHead
	if err := s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationSourceHead, Name: kinds.SourceHeadName(owner)}, &head); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("refused observation acquired durable source authority", err != nil)
	}
}

func TestIntegrationRuntimeIndependentReplacementJoinsOldProvider(t *testing.T) {
	for _, replacement := range []string{"configuration", "credential"} {
		t.Run(replacement, func(t *testing.T) {
			entered, stopping, stopped, successor := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var release sync.Once
			releaseStop := func() { release.Do(func() { close(stopped) }) }
			t.Cleanup(releaseStop)
			var aPolls, bPolls, bStops atomic.Int32
			var joined atomic.Bool
			otherPath := filepath.Join(t.TempDir(), "independent")
			if err := os.WriteFile(otherPath, []byte("independent-private-value"), 0600); err != nil {
				t.Fatal(err)
			}
			s, path := integrationFixture(t, func(o *servingRuntimeOptions) {
				o.Config.Credentials.AllowedFiles = append(o.Config.Credentials.AllowedFiles, otherPath)
			}, func(string) (integration.Integration, error) {
				p := &integrationProbe{}
				p.observe = func(ctx context.Context, h ih.OwnerHost) (ih.SourceSnapshot, error) {
					if h.IntegrationOwner().Name == "independent" {
						bPolls.Add(1)
						return ih.SourceSnapshot{Complete: true}, nil
					}
					if aPolls.Add(1) == 1 {
						close(entered)
						<-ctx.Done()
						return ih.SourceSnapshot{}, ctx.Err()
					}
					if !joined.Load() {
						t.Error("successor ran before original Stop completed")
					}
					select {
					case <-successor:
					default:
						close(successor)
					}
					return ih.SourceSnapshot{Complete: true}, nil
				}
				p.stopped = func() {
					if p.host.IntegrationOwner().Name == "independent" {
						bStops.Add(1)
						return
					}
					if !joined.Load() {
						close(stopping)
						<-stopped
						joined.Store(true)
					}
				}
				return p, nil
			})
			integrationRun(t, s)
			a := integrationApply(t, s, integrationDefinition("replace", path))
			integrationAwaitSignal(t, entered)
			independent := integrationApply(t, s, integrationDefinition("independent", otherPath))
			integrationReadyCommand(t, s, integrationOwner(s, independent))
			if replacement == "configuration" {
				a.Spec.UniFi.Site = "replacement"
				integrationApply(t, s, a)
			} else {
				if err := os.WriteFile(path, []byte("rotated-private-value"), 0600); err != nil {
					t.Fatal(err)
				}
				s.integrations.host.Wake()
			}
			integrationAwaitSignal(t, stopping)
			if bStops.Load() != 0 {
				t.Fatal("replacement canceled independent instance")
			}
			releaseStop()
			integrationAwaitSignal(t, successor)
			if bPolls.Load() != 1 || bStops.Load() != 0 {
				t.Fatal("replacement restarted independent work")
			}
		})
	}
}

func TestIntegrationRuntimeActualReplicaPromotionFencesHeldObservation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHeld)
	var primaryPolls, replicaPolls atomic.Int32
	primary, path := integrationFixture(t, nil, func(string) (integration.Integration, error) {
		return &integrationProbe{observe: func(_ context.Context, h ih.OwnerHost) (ih.SourceSnapshot, error) {
			if primaryPolls.Add(1) > 1 {
				close(entered)
				<-release // Deliberately return complete stale facts after cancellation.
			}
			return ih.SourceSnapshot{Complete: true, Clients: []inventory.Client{integrationClient(h.IntegrationOwner())}}, nil
		}}, nil
	})
	stopWriter := integrationRun(t, primary)
	d := integrationApply(t, primary, integrationDefinition("inventory", path))
	owner := integrationOwner(primary, d)
	integrationReadyCommand(t, primary, owner)
	result := make(chan error, 1)
	go func() { _, err := primary.integrations.host.Sync(t.Context(), owner, ""); result <- err }()
	integrationAwaitSignal(t, entered)
	image, err := primary.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "follower.db"), sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "integration-successor", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := st.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := st.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal(err)
	}
	opts := primary.opts
	opts.Store = st
	follower, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := follower.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	follower.integrations.factory = func(string) (integration.Integration, error) {
		return &integrationProbe{observe: func(_ context.Context, h ih.OwnerHost) (ih.SourceSnapshot, error) {
			replicaPolls.Add(1)
			return ih.SourceSnapshot{Complete: true, Clients: []inventory.Client{integrationClient(h.IntegrationOwner())}}, nil
		}}, nil
	}
	runServingFixture(t, follower)
	awaitServingRuntime(t, follower, func() bool { return follower.publisher.Status().ProtectedReady })
	before := integrationInventory(t, primary).Lookup(netip.MustParseAddr("192.0.2.19"), time.Now())
	after := integrationInventory(t, follower).Lookup(netip.MustParseAddr("192.0.2.19"), time.Now())
	if before == nil || after == nil || !before.ExpiresAt.Equal(after.ExpiresAt) || !before.ObservedAt.Equal(after.ObservedAt) || replicaPolls.Load() != 0 {
		t.Fatal("follower renewed original observation or acquired provider authority")
	}
	promoted, err := st.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "integration-failover"})
	if err != nil {
		t.Fatal(err)
	}
	stopWriter()
	if _, err = primary.opts.Store.ObserveAuthority(t.Context(), promoted.Term); err != nil {
		t.Fatal(err)
	}
	releaseHeld()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("old writer committed a held complete observation")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("old observation did not terminate")
	}
	integrationWriter(t, follower)
	awaitServingRuntime(t, follower, func() bool {
		var head kinds.IntegrationSourceHead
		if st.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationSourceHead, Name: kinds.SourceHeadName(owner)}, &head) != nil || head.Spec.Sequence != 2 {
			return false
		}
		snapshot := integrationInventory(t, follower)
		return snapshot != nil && snapshot.Lookup(netip.MustParseAddr("192.0.2.19"), time.Now()) != nil && replicaPolls.Load() == 1
	})
	var old kinds.IntegrationSourceHead
	if err = primary.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationSourceHead, Name: kinds.SourceHeadName(owner)}, &old); err != nil || old.Spec.Sequence != 1 {
		t.Fatal("original writer source advanced after loss", err != nil)
	}
}

func TestIntegrationRuntimeFinalLicenseExpiryRollsBackSource(t *testing.T) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Truncate(time.Second)
	var clock atomic.Int64
	clock.Store(at.UnixNano())
	lic, err := license.New(license.Options{Keyring: license.Keyring{"fixture": pub}, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	expires := at.Add(time.Hour)
	key, err := license.Sign(private, "fixture", license.Claims{ID: "lic_fixture", Licensee: "Fixture", Plan: license.PlanPersonal, Nodes: 1, Features: []license.Feature{license.FeatureIntegrations}, IssuedAt: at.Add(-time.Hour).Unix(), ExpiresAt: expires.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err = lic.Set(key, "fixture"); err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	var once sync.Once
	s, path := integrationFixture(t, func(o *servingRuntimeOptions) { o.License = lic }, func(string) (integration.Integration, error) {
		return &integrationProbe{observe: func(_ context.Context, h ih.OwnerHost) (ih.SourceSnapshot, error) {
			return ih.SourceSnapshot{Complete: true, Clients: []inventory.Client{integrationClient(h.IntegrationOwner())}}, nil
		}}, nil
	})
	pin := s.integrations.pin
	s.integrations.pin = func(ctx context.Context, f primaryruntime.PublicationFence, fn func(currentguard.Pinned) error) error {
		return pin(ctx, f, func(p currentguard.Pinned) error {
			if _, err := integrationObservationFrom(ctx); err == nil {
				original, calls := p.Current, 0
				p.Current = func() bool {
					calls++
					if calls == 2 {
						clock.Store(expires.Add(license.Grace).UnixNano())
					}
					return original()
				}
				err := fn(p)
				if calls >= 2 {
					if err == nil {
						t.Error("expired final command succeeded")
					}
					once.Do(func() { close(finished) })
				}
				return err
			}
			return fn(p)
		})
	}
	integrationRun(t, s)
	d := integrationApply(t, s, integrationDefinition("expires", path))
	integrationAwaitSignal(t, finished)
	integrationNoSource(t, s, integrationOwner(s, d))
}

// Foreign desired ownership must roll back the entire source transaction,
// including staged allocations, rather than exposing partial inventory.
func TestIntegrationRuntimeActualForeignDesiredRollback(t *testing.T) {
	server, _, _ := integrationController(t)
	s, path := integrationFixture(t, nil, nil)
	d := integrationApply(t, s, integrationPinnedDefinition(t, server, path))
	owner := integrationOwner(s, d)
	name := "unifi-rr-" + kinds.Digest([]byte(ih.SourceID(owner) + "11111111-1111-4111-8111-111111111111/00:11:22:33:44:55"))[:32]
	foreign := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "foreign.example.", RecordType: "A", Targets: []string{"192.0.2.77"}, RecordTTL: 120}}}}
	if _, err := s.opts.Store.Apply(t.Context(), foreign, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	// Grant groups so the ownership collision, rather than an unrelated missing
	// group, is the transaction's refusal.
	group := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "devices", Namespace: "default"}, Spec: public.ClientGroupSpec{Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: "inventory"}}}}
	if _, err := s.opts.Store.Apply(t.Context(), group, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	integrationRun(t, s)
	awaitServingRuntime(t, s, func() bool {
		for _, status := range s.integrations.host.Statuses() {
			if status.Reason == "ReconcileFailed" {
				return true
			}
		}
		return false
	})
	integrationNoSource(t, s, owner)
	rows, err := s.opts.Store.List(t.Context(), kinds.KindIntegrationNameAllocation, store.ListOptions{})
	if err != nil || len(rows.Items) != 0 {
		t.Fatal("failed desired ownership left an allocation", err != nil)
	}
	var actual public.DNSRecordSet
	if err = s.opts.Store.Get(t.Context(), store.KeyOf(foreign), &actual); err != nil || actual.Spec.Endpoints[0].DNSName != "foreign.example." || store.ManagedBy(&actual) != store.ManagerAPI {
		t.Fatal("foreign ownership changed", err != nil)
	}
}
