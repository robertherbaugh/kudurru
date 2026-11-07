package providercoordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestActualCurrentProofStagedUIDDisabledAndNoStoreReadsInHeldView(t *testing.T) {
	server := newProviderServer(t)
	f := newFixture(t, server)
	accept(t, f, candidates(t, f, 1))
	ready(t, f, 1)
	proof := servingauth.Credential{Kind: servingauth.OIDCJWT, Value: server.token(t, f.host.now().Add(-time.Minute))}
	ctx, target := writer(t, f.store)
	if err := f.coordinator.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		_, err := servingauth.AuthenticateCurrent(ctx, tx, deps, proof, f.host.now())
		return err
	}); err != nil {
		t.Fatal("current raw proof denied", err)
	}
	before, _ := f.store.ReadGeneration(t.Context())
	for _, change := range []func(context.Context, *sqlite.CommandTx) error{
		func(ctx context.Context, tx *sqlite.CommandTx) error {
			var idp publicv1.IdentityProvider
			if err := tx.Get(ctx, store.Key{Kind: "IdentityProvider", Name: "corp0"}, &idp); err != nil {
				return err
			}
			idp.Spec.ClientID = "replaced"
			_, err := tx.Apply(ctx, &idp, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		},
		func(ctx context.Context, tx *sqlite.CommandTx) error {
			if err := tx.Delete(ctx, store.Key{Kind: "IdentityProvider", Name: "corp0"}, store.DeleteOptions{}); err != nil {
				return err
			}
			_, err := tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp0", CreationTimestamp: metav1.NewTime(f.host.now().Add(-time.Hour))}, Spec: server.spec()}, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		},
	} {
		err := f.coordinator.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			if err := change(ctx, tx); err != nil {
				return err
			}
			_, err := servingauth.AuthenticateCurrent(ctx, tx, deps, proof, f.host.now())
			return err
		})
		if err == nil {
			t.Fatal("staged spec/recreated UID accepted old verifier")
		}
		after, _ := f.store.ReadGeneration(t.Context())
		if !before.Generation().Same(after.Generation()) {
			t.Fatal("rejected proof did not roll back")
		}
	}
	local := servingauth.Credential{Kind: servingauth.APIToken, Value: f.wire}
	err := f.coordinator.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var sa publicv1.ServiceAccount
		if err := tx.Get(ctx, store.Key{Kind: "ServiceAccount", Namespace: "apps", Name: "ci"}, &sa); err != nil {
			return err
		}
		sa.Spec.Disabled = true
		if _, err := tx.Apply(ctx, &sa, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		_, err := servingauth.AuthenticateCurrent(ctx, tx, deps, local, f.host.now())
		return err
	})
	if !isDenied(err) {
		t.Fatal("disabled current owner accepted", err)
	}
	view := f.view(t)
	calls := server.calls.Load()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Authenticate(t.Context(), proof, f.host.now()); err != nil {
		t.Fatal("held auth owner reads closed Store", err)
	}
	if server.calls.Load() != calls {
		t.Fatal("held auth owner called network")
	}
}

func TestPinHostExcludesSwapAfterFinalHostHookThroughActualCommit(t *testing.T) {
	f := newFixture(t)
	ctx, target := writer(t, f.store)
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- f.coordinator.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies) error {
			var sa publicv1.ServiceAccount
			if err := tx.Get(ctx, store.Key{Kind: "ServiceAccount", Namespace: "apps", Name: "ci"}, &sa); err != nil {
				return err
			}
			sa.Spec.Description = "committed under actual host pin"
			if _, err := tx.Apply(ctx, &sa, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
			return tx.RequireCommitCheck(func(context.Context) error { close(entered); <-release; return nil })
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("did not reach final hook")
	}
	swapped := make(chan struct{})
	go func() {
		f.host.install(&hostGeneration{id: "installation-2", allowed: false, secret: "replacement"})
		close(swapped)
	}()
	select {
	case <-swapped:
		t.Fatal("independent host mutation crossed final hook/commit")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	select {
	case <-swapped:
	case <-time.After(time.Second):
		t.Fatal("host pin leaked")
	}
	var sa publicv1.ServiceAccount
	if err := f.store.Get(t.Context(), store.Key{Kind: "ServiceAccount", Namespace: "apps", Name: "ci"}, &sa); err != nil || sa.Spec.Description != "committed under actual host pin" {
		t.Fatal("real commit missing", err)
	}
}

func TestFinalHostDeadlineRollsBackChangedAndNoopCommands(t *testing.T) {
	f := newFixture(t)
	deadline := f.host.now().Add(time.Second)
	f.host.install(&hostGeneration{id: "deadline-host", allowed: true, secret: "test-only", deadline: deadline})
	wait(t, func() bool { return f.publisher.Status().ProtectedReady })
	ctx, target := writer(t, f.store)
	before, _ := f.store.ReadGeneration(t.Context())
	err := f.coordinator.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies) error {
		var sa publicv1.ServiceAccount
		if err := tx.Get(ctx, store.Key{Kind: "ServiceAccount", Namespace: "apps", Name: "ci"}, &sa); err != nil {
			return err
		}
		sa.Spec.Description = "must roll back"
		if _, err := tx.Apply(ctx, &sa, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		f.clock.Store(deadline.UnixNano())
		return nil
	})
	if !errors.Is(err, ErrStale) {
		t.Fatal("expired host allowed changed commit", err)
	}
	after, _ := f.store.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("host expiry did not roll back actual SQLite")
	}
	f.clock.Store(deadline.Add(-time.Millisecond).UnixNano())
	wait(t, func() bool { return f.publisher.Status().ProtectedReady })
	err = f.coordinator.WithCommand(ctx, target, func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error {
		f.clock.Store(deadline.UnixNano())
		return nil
	})
	if !errors.Is(err, ErrStale) {
		t.Fatal("expired host allowed no-op", err)
	}
}
