package security

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 production interoperability profile.
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Simulate the authenticator app receiving the deliberately disclosed enrollment
// secret. Production only accepts a code and never generates one for a caller.
func enrollmentCode(t *testing.T, seed *totp.Seed, now time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(seed.ProvisioningSecret())
	if err != nil {
		t.Fatal(err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30)) //nolint:gosec // Test clock is positive.
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := int(sum[len(sum)-1] & 15)
	code := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1000000)
}

func TestIssueLocalKeepsStagedTOTPConsumptionAndRollsBackTogether(t *testing.T) {
	f := newSessionFixture(t)
	deps := loginDependencies(t, f)
	keys, err := totp.NewKeyring(deps.CurrentKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	deps.TOTPKeys = keys
	seed, err := totp.Generate(totp.Account{Name: f.user.Name, UID: string(f.user.UID)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := keys.Seal(seed)
	if err != nil {
		t.Fatal(err)
	}
	account, version, id := seed.Identity()
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		user := f.user.DeepCopy()
		if err := tx.Get(ctx, store.KeyOf(user), user); err != nil {
			return err
		}
		user.Spec.TOTPSecret = sealed
		if _, err := tx.Apply(ctx, user, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		s.value.TOTP = []totp.Record{{FormatVersion: totp.FormatVersion, Account: account, SeedVersion: version, SeedID: id, Enabled: true}}
		s.changed = true
		return s.Commit(ctx)
	})
	now := f.now.Add(time.Second)
	password := servingauth.Credential{Kind: servingauth.Password, Username: f.user.Name, Value: "test-local-password-1"}
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueLocal(ctx, tx, deps, password, now)
		return err
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("password bypassed enrolled factor", err)
	}
	password.TOTPCode = enrollmentCode(t, seed, now)
	before, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owner := ownerState(t, f.st)
	fault := errors.New("failed enclosing session command")
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := IssueLocal(ctx, tx, deps, password, now); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatal(err)
	}
	rolledBack, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(rolledBack.Generation()) {
		t.Fatal("failed enrollment consumed TOTP")
	}
	var enrolled Enrollment
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		enrolled, err = IssueLocal(ctx, tx, deps, password, now)
		return err
	})
	after := ownerState(t, f.st)
	if len(after.TOTP) != 1 || !after.TOTP[0].HasConsumed || after.TOTP[0].LastConsumedStep != uint64(now.Unix()/30) || after.Epoch != owner.Epoch+2 || len(after.Sessions) != len(owner.Sessions)+1 { //nolint:gosec // Positive test clock.
		t.Fatal("session State.Load/Commit lost staged replay consumption")
	}
	image, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	cut, err := servingauth.CaptureImage(t.Context(), image, servingauth.Schema{Scheme: scheme, Kinds: image.KindManifest, SchemaVersion: store.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	view, err := servingauth.Prepare(t.Context(), cut, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Release()
	if _, _, err := enrolled.Seal(view, now); err != nil {
		t.Fatal("committed factor session failed to seal", err)
	}
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueLocal(ctx, tx, deps, password, now)
		return err
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("same step enrolled another session", err)
	}
}
