package providerhost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (p *providerServer) token(t *testing.T, at time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"iss": p.server.URL, "sub": "subject", "aud": "browser", "iat": at.Unix(), "exp": at.Add(time.Hour).Unix(), "preferred_username": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := signer.Sign(data)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func view(t *testing.T, x *fixture) *servingauth.View {
	t.Helper()
	var out *servingauth.View
	await(t, func() bool {
		lease, err := x.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		if !lease.Bundle().Identity().Base.Same(image(t, x.s).Generation()) {
			lease.Release()
			return false
		}
		v, ok := lease.Bundle().Component("auth")
		if !ok {
			lease.Release()
			return false
		}
		t.Cleanup(lease.Release)
		out = v.(*servingauth.View)
		return true
	})
	return out
}
func readyResources(t *testing.T, x *fixture, count int) {
	t.Helper()
	await(t, func() bool {
		c, err := x.coordinator.Capture(t.Context(), image(t, x.s))
		if err != nil {
			return false
		}
		defer c.Release()
		if len(c.ProviderResources) != count {
			return false
		}
		for _, r := range c.ProviderResources {
			if r.Availability != servingauth.ProviderReady {
				return false
			}
		}
		return true
	})
}
func TestRealWorkersBothOriginCurrentAdmissionRollbackAndReadFreeJWT(t *testing.T) {
	a, b := newProviderServer(t), newProviderServer(t)
	in := a.input(t)
	in.LocalProviders = []providerregistry.DesiredConfig{{ID: "config", Spec: b.spec()}}
	in.ConfigCredentials = map[string]*credentials.Resolver{"config": in.ObjectCredentials}
	in.AllowedOrigins = append(in.AllowedOrigins, b.server.URL)
	in.LocalOrigins = append(in.LocalOrigins, b.server.URL)
	in.TrustPEM = append(in.TrustPEM, b.ca...)
	x := newFixture(t, in)
	// RunWriter retries against its supplied actual clock. Unlike one-shot
	// commands, an always-on lifecycle needs that clock to progress.
	clockStop, clockDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(clockDone)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-clockStop:
				return
			case <-tick.C:
				x.f.clock.Add(int64(10 * time.Millisecond))
			}
		}
	}()
	defer func() { close(clockStop); <-clockDone }()
	addObject(t, x, "object", a.spec())
	command(t, x.s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := tx.Apply(ctx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "external-viewer", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "User", Name: "object:alice"}, {Kind: "User", Name: "config:alice"}}}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		return security.ReconcileProviderConfigs(ctx, tx, in.LocalProviders, x.f.now())
	})
	e := reset(t, x)
	before := image(t, x.s)
	h := capture(t, x)
	defer h.Release()
	abort := errors.New("test root admission rollback")
	attempted := make(chan struct{}, 1)
	admissionErrors := make(chan error, 1)
	failed, err := providercoordinator.New(providercoordinator.Options{Store: x.s, Resources: x.resources, Publisher: x.publisher, Schema: x.host.opts.Schema, Host: x.host.Host, HostChanges: x.host.Changes, PinHost: x.host.PinHost, Now: x.f.now, RetryInterval: 20 * time.Millisecond, OperationTimeout: time.Second, Admit: func(ctx context.Context, tx *sqlite.CommandTx, admission *providercoordinator.Admission) error {
		if err := x.host.CheckAdmission(ctx, tx, admission); err != nil {
			select {
			case admissionErrors <- err:
			default:
			}
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error {
			select {
			case attempted <- struct{}{}:
			default:
			}
			return abort
		})
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, target := writer(t, x.s)
	failCtx, failCancel := context.WithCancel(ctx)
	failDone := make(chan error, 1)
	go func() { failDone <- failed.RunWriter(failCtx, target) }()
	select {
	case <-attempted:
	case <-time.After(5 * time.Second):
		select {
		case err := <-admissionErrors:
			t.Fatal("real admission failed before rollback hook", err)
		case err := <-failDone:
			t.Fatal("real writer ended before rollback hook", err)
		default:
			t.Fatalf("real candidate admission never reached rollback; calls=%d/%d phase=%d", a.calls.Load(), b.calls.Load(), x.host.value.Load().phase)
		}
	}
	failCancel()
	<-failDone
	if !before.Generation().Same(image(t, x.s).Generation()) {
		t.Fatal("candidate callback rollback committed")
	}
	// New writer on the SAME original activation. No self-admission or fabricated
	// candidate: real worker refresh supplies provenance after rollback retired it.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- x.coordinator.RunWriter(runCtx, target) }()
	defer func() {
		cancel()
		if done != nil {
			<-done
		}
	}()
	readyResources(t, x, 2)
	next := capture(t, x)
	defer next.Release()
	if h.Identity != next.Identity || h.Providers != next.Providers {
		t.Fatal("first BOTH-origin admission changed pending host lineage")
	}
	for _, r := range registry(t, x.s).Authorities() {
		if !r.Active || r.Binding.EntitlementIdentity != e {
			t.Fatal("receipt did not bind actual pending E")
		}
	}
	x.f.clock.Add(int64(2 * time.Second))
	v := view(t, x)
	proof := servingauth.Credential{Kind: servingauth.OIDCJWT, Value: a.token(t, x.f.now())}
	calls := a.calls.Load() + b.calls.Load()
	p, err := v.Authenticate(t.Context(), proof, x.f.now())
	if err != nil || p.Name != "object:alice" {
		t.Fatal("actual published offline JWT denied", err)
	}
	if a.calls.Load()+b.calls.Load() != calls {
		t.Fatal("auth path performed provider network I/O")
	}
	if err := x.coordinator.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		_, err := servingauth.AuthenticateCurrent(ctx, tx, deps, proof, x.f.now())
		return err
	}); err != nil {
		t.Fatal("real current source/resource command auth denied", err)
	}
	// Immutable prepared verification remains a pure snapshot after actual Store
	// close; root gates new protected leases/commands separately.
	cancel()
	<-done
	done = nil
	x.stop()
	_ = x.coordinator.Close(t.Context())
	x.publisher.Close()
	if err := x.s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Authenticate(t.Context(), proof, x.f.now()); err != nil {
		t.Fatal("held immutable verifier fell back to closed Store", err)
	}
}
