package providerresources

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBrowserActualSQLiteReceiptFloorsRestartReplicaAndPromotion(t *testing.T) {
	f := newBrowserFixture(t)
	path := filepath.Join(t.TempDir(), "browser-primary.db")
	s := resourceStore(t, path, replication.Standalone)
	source := f.source(t, servingauth.ProviderObject)
	mustCommand(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := writeSecurity(ctx, tx, servingauth.EmptySecurity(1)); err != nil {
			return err
		}
		plan, err := providerregistry.PlanInitialize(tx.Authority().ClusterID)
		if err != nil {
			return err
		}
		if err = plan.Stage(ctx, tx); err != nil {
			return err
		}
		if _, err = tx.Apply(ctx, &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: source.Ref.ID, CreationTimestamp: metav1.NewTime(source.CreatedAt)}, Spec: source.Spec}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	d := actualDesired(t, s, nil)
	d.Host = f.host(t, d.Sources[0])
	o := f.owner(t, d)
	c := candidate(t, o)
	if err := c.Match(d); err != nil {
		t.Fatal(err)
	}
	mustCommand(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return security.AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{c.Receipt()}, f.now())
	})
	before, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort after receipt and floor staging")
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := security.AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{}, f.now()); err != nil {
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error { return abort })
	}); !errors.Is(err, abort) {
		t.Fatal("receipt/floor rollback", err)
	}
	after, _ := s.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("receipt/floor partial commit")
	}
	mustCommand(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return security.AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{}, f.now())
	})
	mustCommand(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return security.AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{c.Receipt()}, f.now().Add(time.Second))
	})
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	securityState, err := servingauth.DecodeSecurity(image.Payload.Security)
	if err != nil || len(securityState.ProviderFloors) != 1 {
		t.Fatal("missing actual durable floor", err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := resourceStore(t, path, replication.Standalone)
	follower := resourceStore(t, filepath.Join(t.TempDir(), "browser-follower.db"), replication.Follower)
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
	f.clock.Add(int64(2 * time.Second))
	for name, currentStore := range map[string]*sqlite.Store{"restart": restarted, "replica": follower} {
		t.Run(name, func(t *testing.T) {
			current := actualDesiredReadOnly(t, currentStore, d.Host)
			worker := f.owner(t, current)
			await(t, func() bool {
				lease, err := worker.Acquire(t.Context(), current)
				if err != nil {
					return false
				}
				defer lease.Release()
				return lease.Resources()[0].Availability == servingauth.ProviderReady
			})
			lease, err := worker.Acquire(t.Context(), current)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			id, err := servingauth.ProviderResourceIdentity(current.Sources[0], lease.Resources()[0])
			if err != nil {
				t.Fatal(err)
			}
			browser, err := lease.Browser(current.Sources[0].Ref, id)
			if err != nil {
				t.Fatal(err)
			}
			state, _ := browseroidc.New("corp", "/", f.now())
			f.signedResponse(t, state.Nonce())
			old, err := currentStore.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			proof, err := browser.Exchange(t.Context(), "one-shot-code", state.Verifier())
			if err != nil {
				t.Fatal(err)
			}
			raw := proof.LoginProof(state.Nonce())
			if _, err := lease.Resources()[0].Verifier.VerifyBrowser(t.Context(), raw.RawIDToken, raw.Browser, f.now()); err != nil {
				t.Fatal(err)
			}
			fresh, _ := currentStore.ReadGeneration(t.Context())
			if !old.Generation().Same(fresh.Generation()) {
				t.Fatal("resource exchange changed durable authority")
			}
			decoded, err := servingauth.DecodeSecurity(fresh.Payload.Security)
			if err != nil || len(decoded.ProviderFloors) != 1 || decoded.ProviderFloors[0] != securityState.ProviderFloors[0] {
				t.Fatal("restart/install lost floor", err)
			}
			if name == "replica" {
				if !browser.Current() {
					t.Fatal("pure resource not current")
				}
				// Resource currency intentionally NEVER grants writer admission.
				if err := currentStore.Command(t.Context(), image.Term, func(context.Context, *sqlite.CommandTx) error { return nil }); err == nil {
					t.Fatal("resource currency admitted follower write")
				}
			}
		})
	}
	if _, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "browser-resource-promote"}); err != nil {
		t.Fatal(err)
	}
	promoted, err := follower.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := servingauth.DecodeSecurity(promoted.Payload.Security)
	if err != nil || len(decoded.ProviderFloors) != 1 || decoded.ProviderFloors[0] != securityState.ProviderFloors[0] {
		t.Fatal("promotion lost authority floor", err)
	}
}

// ReadGeneration is intentionally a read-only fixture path on the follower.
// Production capture still goes through the existing validated coordinator.
func actualDesiredReadOnly(t *testing.T, s *sqlite.Store, h *HostCut) Desired {
	t.Helper()
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return imageDesired(t, image, h)
}
