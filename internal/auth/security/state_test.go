package security

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func openSecurityStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	private := []store.Kind{}
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InternalKinds: private, Validator: v, ValidateReplicaEntry: servingauth.ValidateEntry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func command(t *testing.T, st *sqlite.Store, f func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	if err := st.Command(t.Context(), st.Authority().Term, f); err != nil {
		t.Fatal(err)
	}
}

func ownerState(t *testing.T, st *sqlite.Store) servingauth.Security {
	t.Helper()
	i, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s, err := servingauth.DecodeSecurity(i.Payload.Security)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInitializeDoesNotResetMissingManifestOrOtherOwner(t *testing.T) {
	st := openSecurityStore(t, filepath.Join(t.TempDir(), "state.db"))
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.PutSection(ctx, "security", replication.Entry{Key: "other-owner/test", Data: json.RawMessage(`{"value":"opaque"}`)}); err != nil {
			return err
		}
		return Initialize(ctx, tx)
	})
	first, _ := st.ReadGeneration(t.Context())
	command(t, st, Initialize)
	again, _ := st.ReadGeneration(t.Context())
	if !first.Generation().Same(again.Generation()) || ownerState(t, st).Epoch != 1 || len(again.Payload.Security) != 2 {
		t.Fatal("idempotent initialization changed owner or unrelated rows")
	}
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.InvalidateGrant("viewer", time.Now()); err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.DeleteSection(ctx, "security", servingauth.RootKey)
	})
	before, _ := st.ReadGeneration(t.Context())
	if err := st.Command(t.Context(), st.Authority().Term, Initialize); !errors.Is(err, servingauth.ErrManifest) {
		t.Fatal("missing root erased existing revocation state", err)
	}
	after, _ := st.ReadGeneration(t.Context())
	if !after.Generation().Same(before.Generation()) {
		t.Fatal("failed initialization changed the database")
	}
}

type sessionFixture struct {
	st      *sqlite.Store
	path    string
	now     time.Time
	user    *internalv1.LocalUser
	binding *publicv1.RoleBinding
	csrf    string
	row     servingauth.SessionRecord
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	f := &sessionFixture{path: filepath.Join(t.TempDir(), "state.db"), now: time.Now().UTC(), csrf: "test-csrf-012345678901234567890123456789"}
	f.st = openSecurityStore(t, f.path)
	command(t, f.st, Initialize)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		created := metav1.NewTime(f.now.Add(-time.Hour))
		u, err := Apply(ctx, tx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice", CreationTimestamp: created}, Spec: internalv1.LocalUserSpec{Groups: []string{"staff"}}}, store.ApplyOptions{Manager: store.ManagerAPI}, f.now)
		if err != nil {
			return err
		}
		f.user = u.(*internalv1.LocalUser)
		b, err := Apply(ctx, tx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "read", Namespace: "default"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "Group", Name: "staff"}}}}, store.ApplyOptions{Manager: store.ManagerAPI}, f.now)
		if err != nil {
			return err
		}
		f.binding = b.(*publicv1.RoleBinding)
		if _, err := tx.Apply(ctx, &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		f.row, err = s.Enroll(servingauth.SessionRecord{ID: "testsession00000000000000000000001", Account: accountRef(f.user), Provider: "local", IssuedAt: f.now.Add(-time.Minute), ExpiresAt: f.now.Add(time.Hour), IdleExpiresAt: f.now.Add(30 * time.Minute), Grants: []string{"viewer"}, CSRFHash: servingauth.CSRFHash(f.csrf)})
		if err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	return f
}

func (f *sessionFixture) view(t *testing.T) *servingauth.View {
	t.Helper()
	image, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, _ := runtimekinds.NewScheme()
	cut, err := servingauth.CaptureImage(t.Context(), image, servingauth.Schema{Scheme: scheme, Kinds: image.KindManifest, SchemaVersion: store.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	v, err := servingauth.Prepare(t.Context(), cut, servingauth.Dependencies{Identity: "test", ExpectedIdentity: "test", License: servingauth.LicensePolicy{RBAC: true}, Permissions: permissions, CurrentKey: bytes.Repeat([]byte{5}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Release)
	return v
}

func TestBindingNarrowRegrantAndRestartCannotReviveSession(t *testing.T) {
	f := newSessionFixture(t)
	old := f.view(t)
	cookie, err := old.SealSession(f.row.ID, f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	proof := servingauth.Credential{Kind: servingauth.Cookie, Value: cookie}
	if _, err := old.Authenticate(t.Context(), proof, f.now); err != nil {
		t.Fatal(err)
	}
	narrow := f.binding.DeepCopy()
	narrow.Spec.Subjects = []publicv1.BindingSubject{{Kind: "User", Name: "bob"}}
	before, _ := f.st.ReadGeneration(t.Context())
	boom := errors.New("after object and floor staging")
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := Apply(ctx, tx, narrow, store.ApplyOptions{Manager: store.ManagerAPI}, f.now); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !after.Generation().Same(before.Generation()) {
		t.Fatal("rollback separated binding from revocation floor")
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := Apply(ctx, tx, narrow, store.ApplyOptions{Manager: store.ManagerAPI}, f.now)
		return err
	})
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := Apply(ctx, tx, f.binding, store.ApplyOptions{Manager: store.ManagerAPI}, f.now.Add(time.Second))
		return err
	})
	if _, err := f.view(t).Authenticate(t.Context(), proof, f.now.Add(2*time.Second)); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("regrant revived old session", err)
	}
	if _, err := old.Authenticate(t.Context(), proof, f.now); err != nil {
		t.Fatal("held old read view was mutated", err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openSecurityStore(t, f.path)
	if _, err := f.view(t).Authenticate(t.Context(), proof, f.now.Add(2*time.Second)); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("restart forgot persisted floor", err)
	}
}

func TestAccountDisableEnableAndClockRollbackKeepFloor(t *testing.T) {
	f := newSessionFixture(t)
	cookie, err := f.view(t).SealSession(f.row.ID, f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []bool{true, false} {
		command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
			changed := f.user.DeepCopy()
			changed.Spec.Disabled = disabled
			_, err := Apply(ctx, tx, changed, store.ApplyOptions{Manager: store.ManagerAPI}, f.now)
			return err
		})
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.InvalidateAccount(accountRef(f.user), f.now.Add(-time.Hour)); err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	state := ownerState(t, f.st)
	if len(state.AccountFloors) != 1 || !state.AccountFloors[0].ValidAfter.Equal(f.now) {
		t.Fatal("clock rollback lowered account revocation floor")
	}
	if _, err := f.view(t).CookieSession(cookie, f.now.Add(time.Second)); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("reenabling account revived old session", err)
	}
}

func TestEnrollmentRevokeCollectAndStaleEdit(t *testing.T) {
	f := newSessionFixture(t)
	cookie, err := f.view(t).SealSession(f.row.ID, f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := s.Enroll(f.row); !errors.Is(err, ErrConflict) {
			t.Fatal("session ID reused", err)
		}
		if err := s.Revoke(f.row.ID); err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	if _, err := f.view(t).CookieSession(cookie, f.now); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("revoked enrollment authenticated", err)
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.CollectExpired(f.now.Add(31 * time.Minute)); err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	if len(ownerState(t, f.st).Sessions) != 1 {
		t.Fatal("revoked enrollment was collected before absolute expiry")
	}
	before, _ := f.st.ReadGeneration(t.Context())
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		stale, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		fresh, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if err := fresh.InvalidateGrant("viewer", f.now); err != nil {
			return err
		}
		if err := fresh.Commit(ctx); err != nil {
			return err
		}
		return stale.Commit(ctx)
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatal("stale owner edit overwrote newer state", err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !after.Generation().Same(before.Generation()) {
		t.Fatal("failed command persisted a partial owner edit")
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.CollectExpired(f.row.ExpiresAt); err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	if len(ownerState(t, f.st).Sessions) != 0 {
		t.Fatal("absolute-expired enrollment remained")
	}
}
