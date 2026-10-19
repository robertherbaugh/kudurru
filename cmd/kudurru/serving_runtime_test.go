package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newServingRuntimeFixture(t *testing.T) *servingRuntime {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Scheme: scheme, Validator: v, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := runtimeowners.Initialize(ctx, tx, time.Now().UTC()); err != nil {
			return err
		}
		for _, obj := range []store.Object{
			&public.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updates", Name: "*.example.", Types: []string{"A"}}}}}},
		} {
			if _, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	static := providerStaticFixture(t)
	static.Config.Auth.IdentityProviders = []map[string]any{{"id": "corp", "issuer": "https://issuer.example", "clientID": "browser", "clientSecretFile": filepath.Join(t.TempDir(), "absent-secret")}}
	lic, err := license.New(license.Options{})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "updates.", Algorithm: dnsserver.TSIGHMACSHA256, Secret: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	rings, err := servingbuild.NewKeyringOwner(keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rings.Close(context.Background()) })
	lists, err := listfetcher.OpenDiskCache(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServingRuntime(t.Context(), servingRuntimeOptions{Store: st, Config: static.Config, Validator: v, License: lic, AuthKeys: static.Keys, Keyrings: rings, Catalog: catalogue.NewHolder(nil), Transferred: servingbuild.NewTransferredRuntime(nil, nil, nil), Lists: lists, Namespace: "default"})
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
	// The constructor detaches both host configuration and local provider specs.
	static.Config.Auth.IdentityProviders[0]["issuer"] = "https://mutated.example"
	return s
}

func runServingFixture(t *testing.T, s *servingRuntime) {
	t.Helper()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("complete runtime failed to join")
		}
	})
}

func awaitServingRuntime(t *testing.T, s *servingRuntime, predicate func() bool) {
	t.Helper()
	until := time.Now().Add(15 * time.Second)
	for !predicate() {
		if time.Now().After(until) {
			t.Fatalf("complete assembly did not converge: publisher=%+v driver=%+v", s.publisher.Status(), s.primary.driver.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServingRuntimeActualElectorNativeUpdateAndJoinedShutdown(t *testing.T) {
	s := newServingRuntimeFixture(t)
	before, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunWriter(t.Context()); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("missing carried writer was accepted", err)
	}
	runServingFixture(t, s)
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
	after, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("always-on workers performed durable admission", err)
	}
	if ready, _ := s.ready(); ready {
		t.Fatal("desired primary placeholder made the node ready before admission")
	}
	writerCtx, cancelWriter := context.WithCancel(t.Context())
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- s.opts.Store.Elector("complete-test").Run(writerCtx, func(ctx context.Context) {
			if err := s.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error("writer ended early", err)
			}
		}, nil)
	}()
	t.Cleanup(func() {
		cancelWriter()
		if err := <-writerDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("shutdown: %v", err)
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
	awaitServingRuntime(t, s, func() bool { ready, _ := s.ready(); return ready })
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := providerregistry.Decode(image.Payload.Security, image.Term.ClusterID)
	if err != nil || len(registry.ConfigSources()) != 1 || registry.ConfigSources()[0].Spec.Issuer != "https://issuer.example" || len(registry.Authorities()) != 0 {
		t.Fatal("startup manifest was mutated or unlicensed authority fabricated", err)
	}
	f := &primaryNetworkFixture{store: s.opts.Store, publisher: s.publisher, keys: s.opts.Keyrings.Capture().Keys(), keyrings: s.opts.Keyrings, runtime: s.primary}
	i, a, u := identify.New(nil, nil), authoritative.New(nil), upstream.NewStage(nil)
	a.Update = s.primary.Update
	b, err := blocking.New(blocking.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(cachedata.New(cachedata.Options{}), cache.Settings{}, u)
	r := &postprocess.Stage{}
	f.chain, err = pipeline.New(i, r, a, b, c, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := servingbuild.BindDNS(s.publisher, servingbuild.DNSStages{Chain: f.chain, Identify: i, Authority: a, Blocking: b, Cache: c, Response: r, Resolution: u}, nil); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	f.listeners, err = dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: f.chain, Logger: logger}, Logger: logger, TSIG: f.keys, Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.listeners.Add(dnsserver.Spec{Protocol: dnsserver.ProtoUDP, Address: "127.0.0.1:0"}, dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := f.listeners.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.listeners.Run(ctx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("shutdown: %v", err)
		}
	})
	awaitServingRuntime(t, s, func() bool { ready, _ := f.listeners.Ready(); return ready })
	for _, tcp := range []bool{false, true} {
		answer, err := f.exchange(primaryUpdateMessage("example.", "complete.example.", true), tcp)
		if err != nil || answer == nil || answer.Rcode != dns.RcodeSuccess || answer.IsTsig() == nil {
			t.Fatal("complete signed UPDATE failed", answer, err)
		}
		query := new(dns.Msg)
		query.SetQuestion("complete.example.", dns.TypeA)
		answer, err = f.exchange(query, tcp)
		if err != nil || !answer.Authoritative || len(answer.Answer) != 1 {
			t.Fatal("committed data missing from same complete query assembly", answer, err)
		}
	}
	// A same-policy local installation replacement invalidates the held whole
	// dependency cut immediately. The elected writer must join recovery, retain
	// canonical config UIDs and republish the already committed dynamic data.
	held, err := s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	value, _ := held.Bundle().Component("runtime")
	_, _, oldCurrent := value.(*servingbuild.RuntimeOwner).NotifyKeys()
	if err := s.opts.License.Set("invalid-test-license", "replacement"); err == nil {
		t.Fatal("malformed replacement license unexpectedly verified")
	}
	if oldCurrent() {
		t.Fatal("held dependency cut survived license installation replacement")
	}
	awaitServingRuntime(t, s, func() bool {
		fresh, err := s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer fresh.Release()
		value, _ := fresh.Bundle().Component("runtime")
		return s.primary.driver.Status().Ready && value.(*servingbuild.RuntimeOwner).Primary().Admitted("example.", time.Now())
	})
	image, err = s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := providerregistry.Decode(image.Payload.Security, image.Term.ClusterID)
	if err != nil || replacement.ConfigSources()[0].Ref != registry.ConfigSources()[0].Ref {
		t.Fatal("host recovery recreated durable provider UID", err)
	}
	query := new(dns.Msg)
	query.SetQuestion("complete.example.", dns.TypeA)
	answer, err := f.exchange(query, true)
	if err != nil || len(answer.Answer) != 1 {
		t.Fatal("host recovery lost committed data", answer, err)
	}
	// Cancellation stops new UPDATE immediately, while the last immutable DNS
	// data remains readable. All owned workers must join before Close returns.
	cancelWriter()
	awaitServingRuntime(t, s, func() bool { return !s.primary.driver.Status().Running })
	answer, err = f.exchange(primaryUpdateMessage("example.", "late.example.", true), false)
	if err != nil || answer == nil || answer.Rcode != dns.RcodeServerFailure {
		t.Fatal("stopped original writer accepted a request", answer, err)
	}
}

func TestServingRuntimeConstructorFailureAndNoWriterMutation(t *testing.T) {
	if _, err := newServingRuntime(t.Context(), servingRuntimeOptions{}); !errors.Is(err, serving.ErrComponents) {
		t.Fatal(err)
	}
	s := newServingRuntimeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.RunWriter(ctx); !errors.Is(err, replication.ErrFenced) && !errors.Is(err, context.Canceled) {
		t.Fatal("canceled writer was admitted", err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Run(t.Context()); !errors.Is(err, serving.ErrClosed) {
		t.Fatal("closed runtime restarted", err)
	}
}

func TestServingRuntimeActualFollowerRestartAndOriginalPromotion(t *testing.T) {
	primary := newServingRuntimeFixture(t)
	image, err := primary.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "follower.db")
	open := func() *sqlite.Store {
		s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	follower := open()
	if err := follower.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	follower = open()
	t.Cleanup(func() { _ = follower.Close() })
	opts := primary.opts
	opts.Store = follower
	s, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	runServingFixture(t, s)
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
	held, err := s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if !held.Bundle().Identity().Base.Same(installed) {
		t.Fatal("follower substituted a new serving base")
	}
	after, err := follower.ReadGeneration(t.Context())
	if err != nil || !after.Generation().Same(installed) {
		t.Fatal("follower host observation changed durable image", err)
	}
	promoted, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "root-serving-runtime"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- follower.Elector("promoted-complete-test").Run(ctx, func(ctx context.Context) {
			if err := s.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("shutdown: %v", err)
		}
	})
	awaitServingRuntime(t, s, func() bool {
		lease, err := s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		return lease.Bundle().Identity().Base.Term.Same(promoted.Term) && value.(*servingbuild.RuntimeOwner).Primary().Admitted("example.", time.Now())
	})
	if !held.Bundle().Identity().Base.Same(installed) {
		t.Fatal("promotion rewrote a held old bundle")
	}
}

func TestServingRuntimeReadinessWaitsForForwardAdmission(t *testing.T) {
	s := newServingRuntimeFixture(t)
	if err := s.opts.Store.Command(t.Context(), s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: public.DNSZoneSpec{ZoneName: ".", Type: public.ZoneTypeForward, Forward: &public.ForwardSpec{UpstreamGroup: "default"}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runServingFixture(t, s)
	awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
	if ready, _ := s.ready(); ready {
		t.Fatal("unadmitted root forward placeholder made the node ready")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector("forward-ready-test").Run(ctx, func(ctx context.Context) {
			if err := s.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}, nil)
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	awaitServingRuntime(t, s, func() bool { ready, _ := s.ready(); return ready })
}

func TestServingRuntimePrimaryWithoutConfiguredNameservers(t *testing.T) {
	s := newServingRuntimeFixture(t)
	if err := s.opts.Store.Command(t.Context(), s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var z public.DNSZone
		if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
			return err
		}
		z.Spec.Nameservers = nil
		z.Spec.Policy = public.ZonePolicy{}
		_, err := tx.Apply(ctx, &z, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runServingFixture(t, s)
	ctrl, err := controller.New(s.opts.Store, controller.Options{CommittedPrimaryStatus: s.status, ZoneCoalesce: -1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector("no-ns-test").Run(ctx, func(ctx context.Context) {
			child, stop := context.WithCancel(ctx)
			controllerDone := make(chan error, 1)
			go func() { controllerDone <- ctrl.Run(child) }()
			if err := s.RunWriter(child); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
			stop()
			if err := <-controllerDone; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}, nil)
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if ready, _ := s.ready(); ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nameserver-free primary did not become ready", s.primary.driver.Status(), s.publisher.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServingRuntimePreparingSourceStatusChange(t *testing.T) {
	for _, typ := range []public.ZoneType{public.ZoneTypePrimary, public.ZoneTypeForward} {
		t.Run(string(typ), func(t *testing.T) {
			s := newServingRuntimeFixture(t)
			var expected zk.LifecycleSource
			// Restore the exact durable checkpoint: Preparing was admitted,
			// then a status-only write advanced the public source RV.
			if err := s.opts.Store.Command(t.Context(), s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var z public.DNSZone
				if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
					return err
				}
				if typ == public.ZoneTypeForward {
					z.Spec.Type, z.Spec.Nameservers, z.Spec.Policy = typ, nil, public.ZonePolicy{}
					z.Spec.Forward = &public.ForwardSpec{UpstreamGroup: "default"}
					if _, err := tx.Apply(ctx, &z, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
						return err
					}
					if err := tx.Get(ctx, store.KeyOf(&z), &z); err != nil {
						return err
					}
				}
				l, err := primaryruntime.NewLifecycle(tx.Authority().ClusterID, &z, nil)
				if err != nil {
					return err
				}
				if _, err := tx.Apply(ctx, l, store.ApplyOptions{Manager: "primaryruntime", CreateOnly: true}); err != nil {
					return err
				}
				z.Status.ObservedGeneration = z.Generation
				if err := tx.UpdateStatus(ctx, &z); err != nil {
					return err
				}
				if err := tx.Get(ctx, store.KeyOf(&z), &z); err != nil {
					return err
				}
				token, err := primaryzone.SourceToken(&z)
				expected = zk.LifecycleSource{UID: token.UID, ResourceVersion: token.ResourceVersion, IntentDigest: token.IntentDigest}
				if expected == l.Spec.Source {
					t.Fatal("fixture did not advance the source RV")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			runServingFixture(t, s)
			awaitServingRuntime(t, s, func() bool { return s.publisher.Status().ProtectedReady })
			if ready, _ := s.ready(); ready {
				t.Fatal("stale Preparing receipt was ready")
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- s.opts.Store.Elector("preparing-status-test").Run(ctx, func(ctx context.Context) {
					if err := s.RunWriter(ctx); err != nil && ctx.Err() == nil {
						t.Error(err)
					}
				}, nil)
			}()
			defer func() {
				cancel()
				if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}()
			awaitServingRuntime(t, s, func() bool { ready, _ := s.ready(); return ready })
			var actual zk.ZoneLifecycle
			if err := s.opts.Store.Get(t.Context(), store.Key{Kind: zk.KindZoneLifecycle, Name: zk.LifecycleName(s.opts.Store.Info().ClusterID, "home")}, &actual); err != nil {
				t.Fatal(err)
			}
			if actual.Spec.Phase != zk.PhaseActive || actual.Spec.Source != expected || actual.Spec.Incarnation != 1 {
				t.Fatal("published lifecycle lost the exact source or incarnation")
			}
		})
	}
}
