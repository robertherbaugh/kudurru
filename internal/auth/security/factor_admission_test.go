package security

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestFactorAdmissionSharesFailuresAcrossOperationsAndRestart(t *testing.T) {
	f, deps, proof := factorFixture(t)
	now := f.now.Add(time.Second)
	challenge, seed := beginFactor(t, f, deps, proof, now)
	badCode := ""
	for i := range 10 {
		candidate := fmt.Sprintf("%06d", i)
		if _, err := totp.Match(seed, candidate, now); err != nil {
			badCode = candidate
			break
		}
	}
	if badCode == "" {
		t.Fatal("did not obtain an actually invalid candidate code")
	}
	hasher := &loginAdmissionHasher{base: deps.Hasher}
	deps.Hasher = hasher
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	before, err := f.st.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var factorRefusal error
	for _, operation := range []string{"setup", "challenge", "disable", "code", "setup"} {
		err = f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			badProof := proof
			badProof.Value = "wrong-password"
			clock := func() time.Time { return now }
			var refused, err error
			switch operation {
			case "setup":
				var setup FactorSetup
				setup, refused, err = BeginTOTPWithAdmission(ctx, tx, deps, badProof, "192.0.2.1", clock)
				if !setup.ExpiresAt().IsZero() {
					t.Fatal("wrong password disclosed a setup")
				}
			case "disable":
				refused, err = DisableTOTPWithAdmission(ctx, tx, deps, badProof, "192.0.2.1", clock)
			case "challenge":
				refused, err = ConfirmTOTPWithAdmission(ctx, tx, deps, proof, "invalid-challenge", badCode, "192.0.2.1", clock)
				factorRefusal = refused
			default:
				refused, err = ConfirmTOTPWithAdmission(ctx, tx, deps, proof, challenge, badCode, "192.0.2.1", clock)
			}
			if !errors.Is(refused, servingauth.ErrCredentials) {
				t.Fatal("proof did not reach the expected pre-mutation refusal", operation, refused, err)
			}
			return err
		})
		if err != nil {
			t.Fatal("failure counter did not commit", err)
		}
	}
	after, err := f.st.ReadGeneration(ctx)
	if err != nil || after.Revision != before.Revision+5 || !reflect.DeepEqual(after.Payload.Security, before.Payload.Security) || !reflect.DeepEqual(after.Payload.Objects, before.Payload.Objects) {
		t.Fatal("failed proofs changed authentication state or did not count", err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openSecurityStore(t, f.path)
	err = f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if isFactorAdmissionRefusal(factorRefusal, tx) || isFactorAdmissionRefusal(servingauth.ErrCredentials, tx) {
			t.Fatal("factor refusal transferred to a different command")
		}
		refused, err := ConfirmTOTPWithAdmission(ctx, tx, deps, proof, challenge, enrollmentCode(t, seed, now), "192.0.2.1", func() time.Time { return now })
		var locked *loginlimit.Locked
		if !errors.As(refused, &locked) || hasher.calls != 3 {
			t.Fatal("restart forgot lockout or verified an admitted proof twice", refused, hasher.calls)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	last, err := f.st.ReadGeneration(ctx)
	if err != nil || !last.Generation().Same(after.Generation()) {
		t.Fatal("locked operation changed authentication or counters", err)
	}
}

func TestFactorAdmissionActivationAndFinalErrorsAreAtomic(t *testing.T) {
	f, deps, proof := factorFixture(t)
	now := f.now.Add(time.Second)
	challenge, seed := beginFactor(t, f, deps, proof, now)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	before, err := f.st.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("test: original factor final check refused")
	for _, mode := range []string{"final-error", "clock-rollback", "poison", "success"} {
		t.Run(mode, func(t *testing.T) {
			now := now
			reached := false
			err := f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				refused, err := ConfirmTOTPWithAdmission(ctx, tx, deps, proof, challenge, enrollmentCode(t, seed, now), "192.0.2.2", func() time.Time { return now })
				if refused != nil || err != nil {
					t.Fatal("did not stage exact valid activation", refused, err)
				}
				reached = true
				switch mode {
				case "final-error":
					return tx.RequireCommitCheck(func(context.Context) error { return fault })
				case "clock-rollback":
					now = now.Add(-time.Nanosecond)
				case "poison":
					_ = tx.PutSection(ctx, "invalid-section", replication.Entry{Key: "bad", Data: []byte(`{}`)})
				}
				return nil
			})
			after, readErr := f.st.ReadGeneration(ctx)
			if readErr != nil || !reached {
				t.Fatal("target not reached", readErr)
			}
			if mode != "success" {
				want := fault
				if mode == "clock-rollback" {
					want = loginlimit.ErrState
				}
				if mode == "poison" {
					want = replication.ErrInvalid
				}
				if !errors.Is(err, want) || !after.Generation().Same(before.Generation()) || !reflect.DeepEqual(after.Payload, before.Payload) {
					t.Fatal("failed activation partly committed", err)
				}
				return
			}
			state := ownerState(t, f.st)
			if err != nil || after.Revision != before.Revision+1 || len(state.TOTP) != 1 || !state.TOTP[0].Enabled || !state.TOTP[0].HasConsumed {
				t.Fatal("successful activation lost factor consumption", err)
			}
		})
	}
}
