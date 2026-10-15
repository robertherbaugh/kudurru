package security

import (
	"context"
	"errors"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFactorClockSetupImmutableBoundsAndFinalNoop(t *testing.T) {
	f, deps, proof := factorFixture(t)
	observed := f.now.Add(time.Second)
	before, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var setup FactorSetup
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		setup, err = BeginTOTPWithClock(ctx, tx, deps, proof, func() time.Time { return observed })
		return err
	})
	after, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) || !setup.ExpiresAt().Equal(observed.Add(factorChallengeLifetime)) {
		t.Fatal("setup without prior factor must be an admitted no-op with original bounds")
	}
	if setup.ValidAt(observed) != nil || setup.ValidAt(setup.ExpiresAt().Add(-time.Nanosecond)) != nil {
		t.Fatal("valid immutable bounds rejected")
	}
	for _, now := range []time.Time{{}, observed.Add(-time.Nanosecond), setup.ExpiresAt(), setup.ExpiresAt().Add(time.Nanosecond)} {
		if !errors.Is(setup.ValidAt(now), servingauth.ErrCredentials) {
			t.Fatal("invalid challenge disclosure time accepted")
		}
	}
	if (FactorSetup{}).ValidAt(observed) == nil || !(FactorSetup{}).ExpiresAt().IsZero() {
		t.Fatal("zero setup accepted")
	}
	for _, mode := range []string{"expiry", "regression", "context"} {
		t.Run(mode, func(t *testing.T) {
			clock := observed
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			final := false
			err := f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.RequireCommitCheck(func(context.Context) error {
					final = true
					if mode == "context" {
						cancel()
					}
					return nil
				}); err != nil {
					return err
				}
				if _, err := BeginTOTPWithClock(ctx, tx, deps, proof, func() time.Time { return clock }); err != nil {
					t.Error("setup failed before final check")
					return err
				}
				switch mode {
				case "expiry":
					clock = observed.Add(factorChallengeLifetime)
				case "regression":
					clock = observed.Add(-time.Nanosecond)
				}
				return nil
			})
			if !final || err == nil || mode != "context" && !errors.Is(err, servingauth.ErrCredentials) {
				t.Fatal("setup no-op missed final time/context rejection", err)
			}
			current, _ := f.st.ReadGeneration(t.Context())
			if !before.Generation().Same(current.Generation()) {
				t.Fatal("failed setup wrote durable state")
			}
		})
	}
}

func TestFactorClockConfirmFinalRollbackRetryAndReplay(t *testing.T) {
	f, deps, proof := factorFixture(t)
	issued := f.now.Add(time.Second)
	challenge, seed := beginFactor(t, f, deps, proof, issued)
	observed := issued.Add(time.Second)
	code := enrollmentCode(t, seed, observed)
	for _, finalAt := range []time.Time{{}, observed.Add(-time.Nanosecond), issued.Add(factorChallengeLifetime)} {
		clock := observed
		verified, final := false, false
		assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if err := tx.RequireCommitCheck(func(context.Context) error { final = true; return nil }); err != nil {
				return err
			}
			if err := ConfirmTOTPWithClock(ctx, tx, deps, proof, challenge, code, func() time.Time { return clock }); err != nil {
				return err
			}
			verified = true
			clock = finalAt
			return nil
		})
		if !verified || !final {
			t.Fatal("confirmation did not reach final rollback check")
		}
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTPWithClock(ctx, tx, deps, proof, challenge, code, func() time.Time { return observed })
	})
	state := ownerState(t, f.st)
	if len(state.TOTP) != 1 || !state.TOTP[0].Enabled || !state.TOTP[0].HasConsumed || len(state.AccountFloors) == 0 {
		t.Fatal("confirmation lost activation, replay or revocation")
	}
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTPWithClock(ctx, tx, deps, proof, challenge, code, func() time.Time { return observed })
	})
	proof.TOTPCode = code
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueLocal(ctx, tx, deps, proof, observed)
		return err
	})
}

func TestFactorClockRotationAndDisableConsumeOnlyOnce(t *testing.T) {
	f, deps, proof := factorFixture(t)
	now := f.now.Add(time.Second)
	challenge, seed := beginFactor(t, f, deps, proof, now)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTP(ctx, tx, deps, proof, challenge, enrollmentCode(t, seed, now), now)
	})
	now = now.Add(time.Minute)
	proof.TOTPCode = enrollmentCode(t, seed, now)
	clock := now
	var setup FactorSetup
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		setup, err = BeginTOTPWithClock(ctx, tx, deps, proof, func() time.Time { return clock })
		if err != nil {
			t.Error("rotation proof failed before final check")
			return err
		}
		clock = setup.ExpiresAt()
		return nil
	})
	clock = now
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		setup, err = BeginTOTPWithClock(ctx, tx, deps, proof, func() time.Time { return clock })
		return err
	})
	candidate, _ := setup.Disclosure()
	_, nextSeed, err := openFactor(deps, candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	proof.TOTPCode = enrollmentCode(t, seed, now)
	clock = now
	newCode := enrollmentCode(t, nextSeed, now)
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := ConfirmTOTPWithClock(ctx, tx, deps, proof, candidate, newCode, func() time.Time { return clock }); err != nil {
			t.Error("old/candidate proof failed before final check")
			return err
		}
		clock = setup.ExpiresAt()
		return nil
	})
	clock = now
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTPWithClock(ctx, tx, deps, proof, candidate, newCode, func() time.Time { return clock })
	})
	rotated := ownerState(t, f.st).TOTP[0]
	if rotated.SeedVersion != 2 || !rotated.Enabled {
		t.Fatal("rotation failed")
	}
	now = now.Add(time.Minute)
	proof.TOTPCode = enrollmentCode(t, nextSeed, now)
	clock = now
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := DisableTOTPWithClock(ctx, tx, deps, proof, func() time.Time { return clock }); err != nil {
			t.Error("disable failed before final check")
			return err
		}
		clock = now.Add(-time.Nanosecond)
		return nil
	})
	clock = now
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return DisableTOTPWithClock(ctx, tx, deps, proof, func() time.Time { return clock })
	})
	disabled := ownerState(t, f.st).TOTP[0]
	if disabled.Enabled || disabled.SeedVersion != 2 || disabled.LastConsumedStep <= rotated.LastConsumedStep {
		t.Fatal("disable lost replay floor")
	}
	proof.TOTPCode = ""
	clock = now
	final := false
	assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.RequireCommitCheck(func(context.Context) error { final = true; return nil }); err != nil {
			return err
		}
		if err := DisableTOTPWithClock(ctx, tx, deps, proof, func() time.Time { return clock }); err != nil {
			return err
		}
		clock = now.Add(-time.Nanosecond)
		return nil
	})
	if !final {
		t.Fatal("disabled no-op skipped final checks")
	}
	before, _ := f.st.ReadGeneration(t.Context())
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return DisableTOTPWithClock(ctx, tx, deps, proof, func() time.Time { return now })
	})
	after, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("disabled replay changed generation")
	}
}

// Recovery proofs are actual enrolled/sealed cookies, hashed API tokens, or
// signed JWTs. No principal, expiry override or fake transaction is supplied.
func factorRecoveryProof(t *testing.T, kind string, enabled bool) (*sessionFixture, servingauth.Dependencies, servingauth.Credential, totp.Account, time.Time, time.Time) {
	t.Helper()
	var f *sessionFixture
	var deps servingauth.Dependencies
	var password servingauth.Credential
	var external *externalIssueFixture
	if kind == "jwt" {
		external = newExternalIssueFixture(t, true)
		f = external.sessionFixture
		local := loginDependencies(t, f)
		deps = external.deps
		deps.Hasher = local.Hasher
		deps.TOTPKeys, _ = totp.NewKeyring(deps.CurrentKey, nil)
		external.deps = deps
		password = servingauth.Credential{Kind: servingauth.Password, Username: f.user.Name, Value: "test-local-password-1"}
	} else {
		f, deps, password = factorFixture(t)
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := Apply(ctx, tx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "factor-recovery", Namespace: "default"}, Spec: publicv1.RoleBindingSpec{Role: "admin", Subjects: []publicv1.BindingSubject{{Kind: "User", Name: "alice"}, {Kind: "Group", Name: "corp:ops"}}}}, store.ApplyOptions{Manager: store.ManagerAPI}, f.now)
		return err
	})
	var seed *totp.Seed
	if enabled {
		challenge, initial := beginFactor(t, f, deps, password, f.now.Add(time.Second))
		seed = initial
		command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
			return ConfirmTOTP(ctx, tx, deps, password, challenge, enrollmentCode(t, seed, f.now.Add(time.Second)), f.now.Add(time.Second))
		})
	}
	issued := f.now.Add(time.Minute)
	observed := issued.Add(time.Second)
	deadline := issued.Add(time.Minute)
	target := totp.Account{Name: f.user.Name, UID: string(f.user.UID)}
	if kind == "jwt" {
		proof := external.proof(t, issued, "recovery-actor", "ops")
		return f, deps, servingauth.Credential{Kind: servingauth.OIDCJWT, Value: proof.RawIDToken}, target, observed, time.Unix(issued.Add(5*time.Minute).Unix(), 0).UTC()
	}
	if kind == "token" {
		wire, id := tokens.Generate()
		command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(issued)}, Spec: internalv1.APITokenSpec{Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: target.Name, UID: target.UID, ID: "local:" + target.Name, Provider: "local"}, SecretHash: tokens.Hash(wire), Grants: []string{"admin"}, Scopes: []string{"admin"}, ExpiresAt: &metav1.Time{Time: deadline}}}, store.ApplyOptions{Manager: store.ManagerAPI})
			return err
		})
		return f, deps, servingauth.Credential{Kind: servingauth.APIToken, Value: wire}, target, observed, deadline
	}
	if seed != nil {
		password.TOTPCode = enrollmentCode(t, seed, issued)
	}
	var enrollment Enrollment
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		enrollment, err = IssueLocal(ctx, tx, deps, password, issued)
		return err
	})
	wire, session, err := enrollment.Seal(factorView(t, f.st, deps), issued)
	if err != nil {
		t.Fatal(err)
	}
	deadline = session.IdleExpiresAt
	if kind == "cookie-absolute" {
		command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
			state, err := Load(ctx, tx)
			if err != nil {
				return err
			}
			for i := range state.value.Sessions {
				if state.value.Sessions[i].ID == enrollment.ID() {
					state.value.Sessions[i].IdleExpiresAt = state.value.Sessions[i].ExpiresAt
				}
			}
			state.changed = true
			return state.Commit(ctx)
		})
		wire, _, err = enrollment.Seal(factorView(t, f.st, deps), issued)
		if err != nil {
			t.Fatal(err)
		}
		deadline = session.ExpiresAt
	}
	return f, deps, servingauth.Credential{Kind: servingauth.Cookie, Value: wire}, target, observed, deadline
}

func TestFactorClockRecoveryFinalExpiryRollbackNoopAndSelfFloor(t *testing.T) {
	for _, kind := range []string{"cookie", "cookie-absolute", "token", "jwt"} {
		for _, enabled := range []bool{false, true} {
			t.Run(kind+"/enabled="+map[bool]string{false: "false", true: "true"}[enabled], func(t *testing.T) {
				f, deps, proof, target, observed, deadline := factorRecoveryProof(t, kind, enabled)
				for _, finalAt := range []time.Time{observed.Add(-time.Nanosecond), deadline} {
					clock := observed
					verified, final := false, false
					assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
						if err := tx.RequireCommitCheck(func(context.Context) error { final = true; return nil }); err != nil {
							return err
						}
						if err := RecoverTOTPWithClock(ctx, tx, deps, proof, target, func() time.Time { return clock }); err != nil {
							return err
						}
						verified = true
						clock = finalAt
						return nil
					})
					if !verified || !final {
						t.Fatal("recovery failed before final clock check")
					}
				}
				command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
					return RecoverTOTPWithClock(ctx, tx, deps, proof, target, func() time.Time { return observed })
				})
				state := ownerState(t, f.st)
				if enabled && (len(state.TOTP) != 1 || state.TOTP[0].Enabled || !state.TOTP[0].HasConsumed) {
					t.Fatal("recovery lost retained factor floor")
				}
				if enabled && kind == "cookie" {
					if _, err := factorView(t, f.st, deps).CookieSession(proof.Value, observed); !errors.Is(err, servingauth.ErrCredentials) {
						t.Fatal("self recovery did not revoke original session")
					}
				}
			})
		}
	}
}

func TestFactorClockNilUsesActualTime(t *testing.T) {
	f, deps, proof := factorFixture(t)
	before := time.Now()
	var setup FactorSetup
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		setup, err = BeginTOTPWithClock(ctx, tx, deps, proof, nil)
		return err
	})
	after := time.Now()
	if setup.issuedAt.Before(before) || setup.issuedAt.After(after) || setup.ValidAt(after) != nil {
		t.Fatal("nil clock did not observe actual time")
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return DisableTOTPWithClock(ctx, tx, deps, proof, nil)
	})
	challenge, _ := setup.Disclosure()
	_, seed, err := openFactor(deps, challenge, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code := enrollmentCode(t, seed, time.Now())
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTPWithClock(ctx, tx, deps, proof, challenge, code, nil)
	})
}

func TestFactorClockRecoveryCurrentScopeUIDAndLicense(t *testing.T) {
	f, deps, proof, target, observed, _ := factorRecoveryProof(t, "token", true)
	for _, mode := range []string{"uid", "scope", "binding", "license"} {
		t.Run(mode, func(t *testing.T) {
			assertFactorRollback(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
				currentDeps, currentTarget := deps, target
				want := servingauth.ErrDenied
				switch mode {
				case "uid":
					currentTarget.UID = "another-incarnation"
					want = ErrConflict
				case "scope":
					id, _ := tokens.Parse(proof.Value)
					var token internalv1.APIToken
					if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
						return err
					}
					token.Spec.Scopes = []string{"viewer"}
					if _, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						return err
					}
				case "binding":
					var binding publicv1.RoleBinding
					if err := tx.Get(ctx, store.Key{Kind: "RoleBinding", Namespace: "default", Name: "factor-recovery"}, &binding); err != nil {
						return err
					}
					binding.Spec.Scope = publicv1.BindingScope{Kind: "zone", Name: "example"}
					if _, err := tx.Apply(ctx, &binding, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
						return err
					}
				case "license":
					currentDeps.License.RBAC = false
				}
				err := RecoverTOTPWithClock(ctx, tx, currentDeps, proof, currentTarget, func() time.Time { return observed })
				if !errors.Is(err, want) {
					t.Error("wrong current recovery denial", err)
				}
				return err
			})
		})
	}
}

func TestFactorClockRecoveryPasswordConsumesOnce(t *testing.T) {
	f, deps, proof := factorFixture(t)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := Apply(ctx, tx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "factor-recovery", Namespace: "default"}, Spec: publicv1.RoleBindingSpec{Role: "admin", Subjects: []publicv1.BindingSubject{{Kind: "User", Name: f.user.Name}}}}, store.ApplyOptions{Manager: store.ManagerAPI}, f.now)
		return err
	})
	now := f.now.Add(time.Second)
	challenge, seed := beginFactor(t, f, deps, proof, now)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return ConfirmTOTP(ctx, tx, deps, proof, challenge, enrollmentCode(t, seed, now), now)
	})
	now = now.Add(time.Minute)
	proof.TOTPCode = enrollmentCode(t, seed, now)
	previous := ownerState(t, f.st).TOTP[0]
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return RecoverTOTPWithClock(ctx, tx, deps, proof, previous.Account, func() time.Time { return now })
	})
	current := ownerState(t, f.st).TOTP[0]
	if current.Enabled || current.LastConsumedStep <= previous.LastConsumedStep || current.SeedVersion != previous.SeedVersion {
		t.Fatal("recovery did not consume exactly one current password/factor proof")
	}
}
