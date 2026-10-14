package servingauth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCurrentCommandReverificationRejectsOldViewAndStagedRevocation(t *testing.T) {
	f := newFixture(t)
	v := f.view(t)
	cookie, err := v.SealSession("abcdefghijklmnop", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	proof := Credential{Kind: Cookie, Value: cookie}
	// The command reads its own staged authoritative revocation, never the View.
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, d, e := ReauthorizeCurrent(ctx, tx, f.deps, proof, readAttrs(), f.now)
		if e != nil || !d.Allowed {
			return e
		}
		changed := f.security
		changed.Sessions = append([]SessionRecord(nil), changed.Sessions...)
		changed.Sessions[0].Revoked = true
		changed.Epoch++
		rows, e := EncodeSecurity(changed)
		if e != nil {
			return e
		}
		for _, row := range rows {
			if e = tx.PutSection(ctx, "security", row); e != nil {
				return e
			}
		}
		_, _, e = ReauthorizeCurrent(ctx, tx, f.deps, proof, readAttrs(), f.now)
		if !errors.Is(e, ErrCredentials) {
			t.Fatal("staged revocation ignored", e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Authenticate(t.Context(), proof, f.now); err != nil {
		t.Fatal("old request lost its cut", err)
	}
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, _, e := ReauthorizeCurrent(ctx, tx, f.deps, proof, readAttrs(), f.now)
		return e
	})
	if !errors.Is(err, ErrCredentials) {
		t.Fatal("current/dedup authorization accepted old view credential", err)
	}
}
func TestPersistedRevocationRestartAndActualReplicaPromotion(t *testing.T) {
	f := newFixture(t)
	v := f.view(t)
	cookie, err := v.SealSession("abcdefghijklmnop", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	f.security.Epoch++
	f.security.AccountFloors = []AccountFloor{{FormatVersion: 1, Account: accountRef(f.user), ValidAfter: f.security.Sessions[0].IssuedAt}}
	f.writeSecurity(t)
	// Durable state survives a fresh process/store open; no watch history exists.
	if err = f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openStore(t, f.path, replication.Standalone)
	restarted := f.view(t)
	if _, err = restarted.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("restart lost floor", err)
	}
	source := f.image(t)
	follower := openStore(t, filepath.Join(t.TempDir(), "state.db"), replication.Follower)
	if err = follower.Enroll(t.Context(), source.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *source)
	if err != nil {
		t.Fatal(err)
	}
	a := follower.Authority()
	installed, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed})
	if err != nil {
		t.Fatal(err)
	}
	_, err = follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: source.Term, ExpectedInstalled: installed, RequestID: "auth-promotion"})
	if err != nil {
		t.Fatal(err)
	}
	promotedImage, err := follower.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cut, err := CaptureImage(t.Context(), promotedImage, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	promoted, err := Prepare(t.Context(), cut, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	defer promoted.Release()
	if _, err = promoted.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("promotion lost canonical floor", err)
	}
	if _, err = promoted.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now); err != nil {
		t.Fatal("session floor rewrote token issuance", err)
	}
	// Every token still has the exact recorded UID, issuance and verifier.
	got, err := follower.List(t.Context(), "APIToken", store.ListOptions{})
	if err != nil || len(got.Items) != 2 {
		t.Fatal(err)
	}
	for _, tok := range got.Items {
		if tok.GetName() == f.token.Name && tok.GetUID() != f.token.UID {
			t.Fatal("promotion changed token UID")
		}
	}
}

func TestResolveSessionCurrentCapturesStagedGrantsSettingsAndPrivateAdmin(t *testing.T) {
	f := newFixture(t)
	proof := Credential{Kind: Password, Username: "alice", Value: "correct-password"}
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		first, err := ResolveSessionCurrent(ctx, tx, f.deps, proof, f.now)
		if err != nil || len(first.Grants) != 1 || first.Grants[0] != "viewer" || first.Principal.BuiltinAdmin {
			t.Fatalf("current session grants: %v", err)
		}
		first.Grants[0] = "admin"
		first.Principal.Groups[0] = "injected"
		first.Settings.Name = "injected"
		binding := &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "read", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "operator", Subjects: []publicv1.BindingSubject{{Kind: "Group", Name: "operators"}}}}
		if _, err = tx.Apply(ctx, binding, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		settings := &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: publicv1.KudurruSpec{Settings: map[string]apiextensionsv1.JSON{"auth": {Raw: []byte(`{"localLogin":"admins-only"}`)}}}}
		if _, err = tx.Apply(ctx, settings, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		user := f.user.DeepCopy()
		user.Spec.BuiltinAdmin = true
		if _, err = tx.Apply(ctx, user, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		next, err := ResolveSessionCurrent(ctx, tx, f.deps, proof, f.now)
		if err != nil || len(next.Grants) != 1 || next.Grants[0] != "operator" || !next.Principal.BuiltinAdmin || next.Settings.Name != "default" || string(next.Settings.Spec.Settings["auth"].Raw) != `{"localLogin":"admins-only"}` {
			t.Fatalf("resolver ignored staged current inputs or widened private admin into grants: %v", err)
		}
		deps := f.deps
		deps.License.RBAC = false
		unlicensed, err := ResolveSessionCurrent(ctx, tx, deps, proof, f.now)
		if err != nil || len(unlicensed.Grants) != 0 || !unlicensed.Principal.BuiltinAdmin {
			t.Fatalf("license/private admin separation: %v", err)
		}
		if _, err = ResolveSessionCurrent(ctx, tx, f.deps, Credential{Kind: APIToken, Value: f.wire}, f.now); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("token session conversion discarded scope restrictions: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
