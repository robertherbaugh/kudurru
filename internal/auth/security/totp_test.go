package security

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func factorFixture(t *testing.T) (*sessionFixture, servingauth.Dependencies, servingauth.Credential) {
	t.Helper()
	f := newSessionFixture(t)
	d := loginDependencies(t, f)
	var err error
	d.TOTPKeys, err = totp.NewKeyring(d.CurrentKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f, d, servingauth.Credential{Kind: servingauth.Password, Username: f.user.Name, Value: "test-local-password-1"}
}

func factorView(t *testing.T, st *sqlite.Store, d servingauth.Dependencies) *servingauth.View {
	t.Helper()
	i, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	cut, err := servingauth.CaptureImage(t.Context(), i, servingauth.Schema{Scheme: scheme, Kinds: i.KindManifest, SchemaVersion: store.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	v, err := servingauth.Prepare(t.Context(), cut, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Release)
	return v
}

func beginFactor(t *testing.T, f *sessionFixture, d servingauth.Dependencies, proof servingauth.Credential, now time.Time) (string, *totp.Seed) {
	t.Helper()
	var setup FactorSetup
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		setup, err = BeginTOTP(ctx, tx, d, proof, now)
		return err
	})
	challenge, secret := setup.Disclosure()
	_, seed, err := openFactor(d, challenge, now)
	if err != nil || secret == "" || seed.ProvisioningSecret() != secret {
		t.Fatal("invalid provisioning response")
	}
	raw, _ := json.Marshal(setup)
	for _, diagnostic := range []string{fmt.Sprint(setup), fmt.Sprintf("%#v", setup), string(raw)} {
		if strings.Contains(diagnostic, secret) || strings.Contains(diagnostic, challenge) {
			t.Fatal("generic factor diagnostic disclosed credentials")
		}
	}
	return challenge, seed
}

func assertFactorRollback(t *testing.T, f *sessionFixture, run func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	before, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Command(t.Context(), f.st.Authority().Term, run); err == nil {
		t.Fatal("invalid factor command accepted")
	}
	after, err := f.st.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("failed factor command changed objects or replay state", err)
	}
}

func TestFactorEnrollmentCommitsProofAndRevocationTogether(t *testing.T) {
	f, d, proof := factorFixture(t)
	now := f.now.Add(time.Second)
	var session Enrollment
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		session, err = IssueLocal(ctx, tx, d, proof, now)
		return err
	})
	cookie, _, err := session.Seal(factorView(t, f.st, d), now)
	if err != nil {
		t.Fatal(err)
	}
	challenge, seed := beginFactor(t, f, d, proof, now.Add(time.Second))
	if len(ownerState(t, f.st).TOTP) != 0 {
		t.Fatal("setup activated unconfirmed seed")
	}
	confirmAt := now.Add(2 * time.Second)
	code := enrollmentCode(t, seed, confirmAt)
	for _, bad := range []struct{ challenge, code string }{{challenge, "wrong"}, {"tampered", code}} {
		assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
			return ConfirmTOTP(ctx, tx, d, proof, bad.challenge, bad.code, confirmAt)
		})
	}
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := ConfirmTOTP(ctx, tx, d, proof, challenge, code, confirmAt); err != nil {
			return err
		}
		return errors.New("failure after factor installation")
	})
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTP(ctx, tx, d, proof, challenge, code, confirmAt)
	})
	s := ownerState(t, f.st)
	if len(s.TOTP) != 1 || !s.TOTP[0].Enabled || !s.TOTP[0].HasConsumed || s.TOTP[0].SeedVersion != 1 {
		t.Fatal("factor was not durably enrolled")
	}
	if _, err := factorView(t, f.st, d).CookieSession(cookie, confirmAt.Add(time.Second)); err == nil {
		t.Fatal("factor enrollment did not revoke existing session")
	}
	proof.TOTPCode = code
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueLocal(ctx, tx, d, proof, confirmAt)
		return err
	})
	proof.TOTPCode = enrollmentCode(t, seed, confirmAt.Add(time.Minute))
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueLocal(ctx, tx, d, proof, confirmAt.Add(time.Minute))
		return err
	})
}

func TestFactorConcurrentConfirmationAndCurrentChallengeBinding(t *testing.T) {
	f, d, proof := factorFixture(t)
	now := f.now.Add(time.Second)
	challenge, seed := beginFactor(t, f, d, proof, now)
	code := enrollmentCode(t, seed, now)
	for _, at := range []time.Time{now.Add(-time.Nanosecond), now.Add(factorChallengeLifetime)} {
		assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
			return ConfirmTOTP(ctx, tx, d, proof, challenge, code, at)
		})
	}
	changed := d
	changed.Identity, changed.ExpectedIdentity = "new", "new"
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTP(ctx, tx, changed, proof, challenge, code, now)
	})
	changed = d
	changed.CurrentKey, changed.PreviousKey = bytes.Repeat([]byte{8}, 32), d.CurrentKey
	changed.TOTPKeys, _ = totp.NewKeyring(changed.CurrentKey, changed.PreviousKey)
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTP(ctx, tx, changed, proof, challenge, code, now)
	})
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			errorsSeen <- f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				return ConfirmTOTP(ctx, tx, d, proof, challenge, code, now)
			})
		})
	}
	wg.Wait()
	close(errorsSeen)
	success := 0
	for err := range errorsSeen {
		if err == nil {
			success++
		}
	}
	if success != 1 || len(ownerState(t, f.st).TOTP) != 1 {
		t.Fatal("challenge enrolled multiple factors")
	}
}

func TestFactorRotationDisableRestartAndRecoveryKeepFloor(t *testing.T) {
	f, d, proof := factorFixture(t)
	now := f.now.Add(time.Second)
	challenge, seed := beginFactor(t, f, d, proof, now)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTP(ctx, tx, d, proof, challenge, enrollmentCode(t, seed, now), now)
	})
	now = now.Add(time.Minute)
	proof.TOTPCode = enrollmentCode(t, seed, now)
	challenge, replacement := beginFactor(t, f, d, proof, now)
	now = now.Add(time.Minute)
	proof.TOTPCode = enrollmentCode(t, seed, now)
	confirm := func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTP(ctx, tx, d, proof, challenge, enrollmentCode(t, replacement, now), now)
	}
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := confirm(ctx, tx); err != nil {
			return err
		}
		return errors.New("rotation callback failed")
	})
	command(t, f.st, confirm)
	rotated := ownerState(t, f.st).TOTP[0]
	if rotated.SeedVersion != 2 || !rotated.Enabled || !rotated.HasConsumed {
		t.Fatal("rotation did not advance identity and retain replay floor")
	}
	now = now.Add(time.Minute)
	proof.TOTPCode = enrollmentCode(t, replacement, now)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error { return DisableTOTP(ctx, tx, d, proof, now) })
	disabled := ownerState(t, f.st).TOTP[0]
	if disabled.Enabled || disabled.SeedVersion != rotated.SeedVersion || disabled.LastConsumedStep <= rotated.LastConsumedStep {
		t.Fatal("disable lost replay authority")
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openSecurityStore(t, f.path)
	proof.TOTPCode = ""
	challenge, third := beginFactor(t, f, d, proof, now.Add(time.Second))
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTP(ctx, tx, d, proof, challenge, enrollmentCode(t, third, now), now.Add(time.Second))
	})
	now = now.Add(time.Minute)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTP(ctx, tx, d, proof, challenge, enrollmentCode(t, third, now), now)
	})
	target := totp.Account{Name: f.user.Name, UID: string(f.user.UID)}
	now = now.Add(time.Minute)
	proof.TOTPCode = enrollmentCode(t, third, now)
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return RecoverTOTP(ctx, tx, d, proof, target, now)
	})
	// A current role grant is required even for recovery of the caller's own
	// account; the ordinary self-service path instead requires its factor.
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := Apply(ctx, tx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "recovery", Namespace: "default"}, Spec: publicv1.RoleBindingSpec{Role: "admin", Subjects: []publicv1.BindingSubject{{Kind: "User", Name: "alice"}}}}, store.ApplyOptions{Manager: store.ManagerAPI}, now)
		return err
	})
	badTarget := target
	badTarget.UID = "wrong-incarnation"
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return RecoverTOTP(ctx, tx, d, proof, badTarget, now)
	})
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return RecoverTOTP(ctx, tx, d, proof, target, now)
	})
	recovered := ownerState(t, f.st).TOTP[0]
	if recovered.Enabled || recovered.SeedVersion != 3 || recovered.LastConsumedStep <= disabled.LastConsumedStep {
		t.Fatal("recovery reset factor incarnation or floor")
	}
}

func TestFactorChallengeRejectsMutatedAndRecreatedAccount(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(fmt.Sprint(recreate), func(t *testing.T) {
			f, d, proof := factorFixture(t)
			now := f.now.Add(time.Second)
			challenge, seed := beginFactor(t, f, d, proof, now)
			command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var user internalv1.LocalUser
				if err := tx.Get(ctx, store.KeyOf(f.user), &user); err != nil {
					return err
				}
				if recreate {
					if err := Delete(ctx, tx, &user, store.DeleteOptions{Manager: store.ManagerAPI}, now); err != nil {
						return err
					}
					user.ObjectMeta = metav1.ObjectMeta{Name: user.Name}
				}
				user.Spec.DisplayName = "Changed"
				_, err := Apply(ctx, tx, &user, store.ApplyOptions{Manager: store.ManagerAPI}, now)
				return err
			})
			assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
				return ConfirmTOTP(ctx, tx, d, proof, challenge, enrollmentCode(t, seed, now), now.Add(time.Second))
			})
		})
	}
}

func TestFactorSetupRequiresCurrentLocalPasswordPolicy(t *testing.T) {
	f, d, proof := factorFixture(t)
	now := f.now.Add(time.Second)
	for _, kind := range []servingauth.CredentialKind{servingauth.APIToken, servingauth.Cookie, "unknown"} {
		bad := proof
		bad.Kind = kind
		assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := BeginTOTP(ctx, tx, d, bad, now)
			return err
		})
	}
	for _, mode := range []string{"disabled", "admins-only"} {
		assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var settings publicv1.Kudurru
			if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
				return err
			}
			settings.Spec.Settings["auth"] = apiextensionsv1.JSON{Raw: []byte(`{"localLogin":"` + mode + `"}`)}
			if _, err := tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
			_, err := BeginTOTP(ctx, tx, d, proof, now)
			if !errors.Is(err, servingauth.ErrDenied) {
				t.Error("current local-login policy did not reject factor setup")
			}
			return err
		})
	}
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.KeyOf(f.user), &user); err != nil {
			return err
		}
		user.Spec.MustChangePassword = true
		if _, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		_, err := BeginTOTP(ctx, tx, d, proof, now)
		if !errors.Is(err, servingauth.ErrDenied) {
			t.Error("must-change account enrolled another credential")
		}
		return err
	})
}
