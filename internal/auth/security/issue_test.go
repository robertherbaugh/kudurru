package security

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func loginDependencies(t *testing.T, f *sessionFixture) servingauth.Dependencies {
	t.Helper()
	hasher := users.NewHasher(1)
	hash, err := hasher.Hash(t.Context(), "test-local-password-1")
	if err != nil {
		t.Fatal(err)
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		changed := f.user.DeepCopy()
		changed.Spec.PasswordHash = hash
		if _, err := Apply(ctx, tx, changed, store.ApplyOptions{Manager: store.ManagerAPI}, f.now); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: publicv1.KudurruSpec{Settings: map[string]apiextensionsv1.JSON{
			"auth": {Raw: []byte(`{"localLogin":"enabled","session":{"idleTimeout":"2m","lifetime":"10m"}}`)},
		}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	return servingauth.Dependencies{Identity: "test", ExpectedIdentity: "test", License: servingauth.LicensePolicy{RBAC: true}, Permissions: permissions, CurrentKey: bytes.Repeat([]byte{5}, 32), Hasher: hasher}
}

func TestCurrentLocalLoginEnrollsBeforeCookieAndUsesCanonicalPolicy(t *testing.T) {
	f := newSessionFixture(t)
	deps := loginDependencies(t, f)
	proof := servingauth.Credential{Kind: servingauth.Password, Username: "alice", Value: "test-local-password-1"}
	now := f.now.Add(time.Second)
	old := f.view(t)
	var enrolled Enrollment
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		enrolled, err = IssueLocal(ctx, tx, deps, proof, now)
		return err
	})
	if _, _, err := enrolled.Seal(old, now); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("unpublished enrollment issued a cookie", err)
	}
	cookie, session, err := enrolled.Seal(f.view(t), now)
	if err != nil {
		t.Fatal(err)
	}
	if cookie == "" || len(session.CSRF) < 32 || session.ID != enrolled.ID() || session.Principal.AccountUID != string(f.user.UID) || !session.ExpiresAt.Equal(now.Add(10*time.Minute)) || !session.IdleExpiresAt.Equal(now.Add(2*time.Minute)) || len(session.RolesSnapshot) != 1 || session.RolesSnapshot[0] != "viewer" {
		t.Fatal("issued session lost current identity, grants or canonical lifetime")
	}
	before, _ := f.st.ReadGeneration(t.Context())
	var rolledBack Enrollment
	boom := errors.New("post-enrollment failure")
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		rolledBack, err = IssueLocal(ctx, tx, deps, proof, now)
		if err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !after.Generation().Same(before.Generation()) {
		t.Fatal("failed issuance persisted enrollment")
	}
	if _, _, err := rolledBack.Seal(f.view(t), now); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("rolled-back issuance produced cookie", err)
	}
}

func TestLocalIssuanceRejectsCurrentPolicyAndTokenConversion(t *testing.T) {
	f := newSessionFixture(t)
	deps := loginDependencies(t, f)
	password := servingauth.Credential{Kind: servingauth.Password, Username: "alice", Value: "test-local-password-1"}
	for _, mode := range []string{"disabled", "admins-only"} {
		err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var settings publicv1.Kudurru
			if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
				return err
			}
			settings.Spec.Settings["auth"] = apiextensionsv1.JSON{Raw: []byte(`{"localLogin":"` + mode + `"}`)}
			if _, err := tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
			_, err := IssueLocal(ctx, tx, deps, password, f.now.Add(time.Second))
			return err
		})
		if !errors.Is(err, servingauth.ErrDenied) {
			t.Fatal("current local-login policy ignored", err)
		}
	}
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.APIToken, Value: "untrusted-token"}, f.now.Add(time.Second))
		return err
	})
	if !errors.Is(err, servingauth.ErrUnsupported) {
		t.Fatal("token converted into unrestricted session", err)
	}
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		changed := f.user.DeepCopy()
		if err := tx.Get(ctx, store.KeyOf(changed), changed); err != nil {
			return err
		}
		changed.Spec.TOTPSecret = "test-owner-sealed-totp"
		if _, err := tx.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		_, err := IssueLocal(ctx, tx, deps, password, f.now.Add(time.Second))
		return err
	})
	if !errors.Is(err, servingauth.ErrUnsupported) {
		t.Fatal("TOTP account accepted password-only issuance", err)
	}
}
