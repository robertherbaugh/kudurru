package providerregistry_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRegistryRemovalRecreationInstallPromotionAndMissingRoot(t *testing.T) {
	s, _ := newStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	r := register(t, s, "corp", strings.Repeat("a", 32), at)
	remove, err := pr.PlanReconcile(r, []pr.DesiredConfig{}, at.Add(time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := floors(ctx, tx, remove.Retired(), at.Add(time.Second)); err != nil {
			return err
		}
		return remove.Stage(ctx, tx)
	})
	removed := registry(t, s)
	if removed.Sources()[0].Active || len(removed.ConfigSources()) != 0 {
		t.Fatal("removal lost explicit tombstone")
	}
	readd, err := pr.PlanReconcile(removed, []pr.DesiredConfig{{ID: "corp", Spec: *spec()}}, at.Add(2*time.Second), func() (string, error) { return strings.Repeat("b", 32), nil })
	if err != nil {
		t.Fatal(err)
	}
	command(t, s, readd.Stage)
	r = registry(t, s)
	if r.Sources()[0].UID != strings.Repeat("b", 32) || !r.Sources()[0].CreatedAt.After(removed.Sources()[0].CreatedAt) {
		t.Fatal("recreation adopted retired UID/time")
	}
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	follower := open(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
	if err := follower.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	a := follower.Authority()
	installed, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed})
	if err != nil {
		t.Fatal(err)
	}
	if registry(t, follower).Digest() != r.Digest() {
		t.Fatal("install lost source authority")
	}
	p, err := pr.PlanReconcile(r, []pr.DesiredConfig{}, at.Add(3*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Command(t.Context(), follower.Authority().Term, p.Stage); err == nil {
		t.Fatal("follower rewrote canonical registry")
	}
	if _, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "registry-promotion"}); err != nil {
		t.Fatal(err)
	}
	if registry(t, follower).Digest() != r.Digest() {
		t.Fatal("promotion recreated config UID")
	}
	promoted, err := follower.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	auth, err := servingauth.DecodeSecurity(promoted.Payload.Security)
	if err != nil {
		t.Fatal(err)
	}
	if len(auth.ProviderFloors) != 1 || auth.ProviderFloors[0].UID != strings.Repeat("a", 32) {
		t.Fatal("promotion lost retired floor")
	}
	command(t, follower, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.DeleteSection(ctx, "security", pr.RootKey)
	})
	init, err := pr.PlanInitialize(follower.Authority().Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Command(t.Context(), follower.Authority().Term, init.Stage); !errors.Is(err, pr.ErrManifest) {
		t.Fatal("root loss reset history", err)
	}
}

func TestRegistryConcurrentPlansHaveOneExactUIDAndNoABA(t *testing.T) {
	s, _ := newStore(t)
	r := registry(t, s)
	at := time.Now().UTC()
	result := make(chan error, 2)
	var wg sync.WaitGroup
	for _, uid := range []string{strings.Repeat("a", 32), strings.Repeat("b", 32)} {
		p, err := pr.PlanReconcile(r, []pr.DesiredConfig{{ID: "corp", Spec: *spec()}}, at, func() (string, error) { return uid, nil })
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() { defer wg.Done(); result <- s.Command(t.Context(), s.Authority().Term, p.Stage) }()
	}
	wg.Wait()
	close(result)
	ok, conflicts := 0, 0
	for err := range result {
		if err == nil {
			ok++
		} else if errors.Is(err, pr.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if ok != 1 || conflicts != 1 || len(registry(t, s).Sources()) != 1 {
		t.Fatal("concurrent plans adopted two identities")
	}
}

func TestRegistryActualPrecommitExpiryRollsBackStagedRowsAndNoop(t *testing.T) {
	s, _ := newStore(t)
	base := registry(t, s)
	at := time.Now().UTC()
	p, err := pr.PlanReconcile(base, []pr.DesiredConfig{{ID: "corp", Spec: *spec()}}, at, func() (string, error) { return strings.Repeat("a", 32), nil })
	if err != nil {
		t.Fatal(err)
	}
	deadline := at.Add(time.Second)
	observed := at
	err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.RequireCommitCheck(func(context.Context) error {
			if !observed.Before(deadline) {
				return context.DeadlineExceeded
			}
			return nil
		}); err != nil {
			return err
		}
		if err := p.Stage(ctx, tx); err != nil {
			return err
		}
		observed = deadline // Simulate work crossing the hard deadline without sleeps.
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || registry(t, s).Digest() != base.Digest() {
		t.Fatal("expired changed command committed", err)
	}
	// Exactly the same opaque plan may retry after a rolled-back admission.
	command(t, s, p.Stage)
	r := registry(t, s)
	noop, err := pr.PlanReconcile(r, []pr.DesiredConfig{{ID: "corp", Spec: *spec()}}, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := noop.Stage(ctx, tx); err != nil {
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error { return context.DeadlineExceeded })
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired no-op admitted", err)
	}
}

func TestCompleteAuthorityInventoryRetiresObjectAndConfigShadow(t *testing.T) {
	s, _ := newStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp"}, Spec: *spec()}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	r := registry(t, s)
	var sources []servingauth.ProviderDescriptor
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		sources, err = servingauth.ProviderSourcesCurrent(ctx, tx, r.ConfigSources())
		return err
	})
	accept, err := pr.PlanAcceptAuthority(r, []pr.DesiredAuthority{{Source: sources[0], Binding: receipt()}}, at)
	if err != nil {
		t.Fatal(err)
	}
	command(t, s, accept.Stage)
	r = registry(t, s)
	// Withdrawal is complete and explicit; history persists but no longer occupies
	// active authority capacity. A missing floor cannot withdraw an incarnation.
	withdrawn, err := pr.PlanAcceptAuthority(r, []pr.DesiredAuthority{}, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn.Retired()) != 1 {
		t.Fatal("omitted object receipt stayed active")
	}
	if err := s.Command(t.Context(), s.Authority().Term, withdrawn.Stage); !errors.Is(err, pr.ErrFloor) {
		t.Fatal(err)
	}
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := floors(ctx, tx, withdrawn.Retired(), at.Add(time.Second)); err != nil {
			return err
		}
		return withdrawn.Stage(ctx, tx)
	})
	if registry(t, s).Authorities()[0].Active {
		t.Fatal("withdrawal lost tombstone")
	}
	// Reactivating the exact object requires a floor, then adding config shadows
	// and retires it before any config authority acceptance/network operation.
	reaccept, err := pr.PlanAcceptAuthority(registry(t, s), []pr.DesiredAuthority{{Source: sources[0], Binding: receipt()}}, at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := floors(ctx, tx, reaccept.Changed(), at.Add(2*time.Second)); err != nil {
			return err
		}
		return reaccept.Stage(ctx, tx)
	})
	config, err := pr.PlanReconcile(registry(t, s), []pr.DesiredConfig{{ID: "corp", Spec: *spec()}}, at.Add(3*time.Second), func() (string, error) { return strings.Repeat("c", 32), nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Retired()) != 1 || config.Retired()[0].UID != sources[0].Ref.UID {
		t.Fatal("config shadow retained object authority")
	}
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := floors(ctx, tx, config.Retired(), at.Add(3*time.Second)); err != nil {
			return err
		}
		return config.Stage(ctx, tx)
	})
	if registry(t, s).Authorities()[0].Active {
		t.Fatal("shadow active")
	}
	// A deleted/recreated object UID is a separate authority, never adoption of
	// the old receipt; a complete next inventory floors the old one atomically.
	if _, err := pr.PlanAcceptAuthority(registry(t, s), []pr.DesiredAuthority{{Source: sources[0], Binding: receipt()}}, at.Add(4*time.Second)); !errors.Is(err, pr.ErrInvalid) {
		t.Fatal("shadowed receipt reactivated", err)
	}
}
