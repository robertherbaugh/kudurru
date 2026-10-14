package servingauth

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC6238 HMAC-SHA1 fixture codes.
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func setupTOTP(t *testing.T, f *fixture) *totp.Seed {
	t.Helper()
	keys, err := totp.NewKeyring(f.deps.CurrentKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.deps.TOTPKeys = keys
	seed, err := totp.Generate(totp.Account{Name: f.user.Name, UID: string(f.user.UID)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := keys.Seal(seed)
	if err != nil {
		t.Fatal(err)
	}
	a, version, id := seed.Identity()
	f.security.TOTP = []totp.Record{{FormatVersion: 1, Account: a, SeedVersion: version, SeedID: id, Enabled: true}}
	user := f.user.DeepCopy()
	user.Spec.TOTPSecret = sealed
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		obj, err := tx.Apply(ctx, user, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		f.user = obj.(*internalv1.LocalUser)
		return stageSecurity(ctx, tx, f.security)
	})
	if err != nil {
		t.Fatal(err)
	}
	return seed
}
func factorCode(t *testing.T, s *totp.Seed, now time.Time) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(s.ProvisioningSecret())
	if err != nil {
		t.Fatal("fixture secret decoding failed")
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/30)) // #nosec G115 -- fixtures use current positive Unix time.
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := int(digest[len(digest)-1] & 15)
	value := binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}
func TestTOTPAtomicConcurrentSessionEnrollmentAndRollback(t *testing.T) {
	f := newFixture(t)
	seed := setupTOTP(t, f)
	proof := Credential{Kind: Password, Username: "alice", Value: "correct-password", TOTPCode: factorCode(t, seed, f.now)}
	view := f.view(t)
	if _, err := view.Authenticate(t.Context(), proof, f.now); !errors.Is(err, ErrUnsupported) {
		t.Fatal("View consumed factor proof", err)
	}
	before := view.Base()
	abort := errors.New("abort enrollment")
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := ResolveSessionCurrent(ctx, tx, f.deps, proof, f.now); err != nil {
			return err
		}
		if _, err := ResolveSessionCurrent(ctx, tx, f.deps, proof, f.now); !errors.Is(err, ErrCredentials) {
			t.Fatal("staged duplicate factor accepted", err)
		}
		return abort
	})
	if !errors.Is(err, abort) || !before.Same(f.image(t).Generation()) {
		t.Fatal("rollback leaked counter or session", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wg.Go(func() {
			results <- f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				d, err := ResolveSessionCurrent(ctx, tx, f.deps, proof, f.now)
				if err != nil {
					return err
				}
				rows, err := tx.ListSections(ctx, "security")
				if err != nil {
					return err
				}
				s, err := DecodeSecurity(rows)
				if err != nil {
					return err
				}
				s.Epoch++
				s.Sessions = append(s.Sessions, SessionRecord{FormatVersion: 1, ID: fmt.Sprintf("totpsession000000000000000%d", i), Account: AccountRef{Kind: "LocalUser", Name: d.Principal.Name, UID: d.Principal.AccountUID}, Provider: "local", Epoch: s.Epoch, IssuedAt: f.now, ExpiresAt: f.now.Add(time.Hour), IdleExpiresAt: f.now.Add(30 * time.Minute), Grants: d.Grants, CSRFHash: CSRFHash(f.csrf)})
				return stageSecurity(ctx, tx, s)
			})
		})
	}
	wg.Wait()
	close(results)
	success, denied := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrCredentials) {
			denied++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatal("concurrent replay created multiple enrollments")
	}
	s, err := DecodeSecurity(f.image(t).Payload.Security)
	if err != nil || len(s.Sessions) != 2 || !s.TOTP[0].HasConsumed {
		t.Fatal("session/counter commit not atomic", err)
	}
	current := f.view(t)
	id := ""
	for _, row := range s.Sessions {
		if row.ID != "abcdefghijklmnop" {
			id = row.ID
		}
	}
	cookie, err := current.SealSession(id, f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = current.CookieSession(cookie, f.now); err != nil {
		t.Fatal("committed enrollment unavailable", err)
	}
	if _, err = view.SealSession(id, f.csrf); !errors.Is(err, ErrCredentials) {
		t.Fatal("held old View fabricated new enrollment", err)
	}
}
func TestTOTPCurrentProofFailuresAndSeedRotationPreserveFloor(t *testing.T) {
	f := newFixture(t)
	seed := setupTOTP(t, f)
	proof := Credential{Kind: Password, Username: "alice", Value: "correct-password", TOTPCode: factorCode(t, seed, f.now)}
	for _, bad := range []Credential{{Kind: Password, Username: "alice", Value: "wrong", TOTPCode: proof.TOTPCode}, {Kind: Password, Username: "alice", Value: "correct-password"}, {Kind: Password, Username: "alice", Value: "correct-password", TOTPCode: "invalid"}} {
		before := f.image(t).Generation()
		err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := AuthenticateCurrent(ctx, tx, f.deps, bad, f.now)
			return err
		})
		if !errors.Is(err, ErrCredentials) || !before.Same(f.image(t).Generation()) {
			t.Fatal("failed proof consumed step", err)
		}
	}
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := AuthenticateCurrent(ctx, tx, f.deps, proof, f.now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := DecodeSecurity(f.image(t).Payload.Security)
	if err != nil {
		t.Fatal(err)
	}
	newSeed, err := totp.Generate(state.TOTP[0].Account, 2)
	if err != nil {
		t.Fatal(err)
	}
	// The rare adjacent-step collision is covered separately with a fixed RFC
	// fixture. Avoid accidentally selecting a later candidate in this floor test.
	for tries := 0; tries < 16; tries++ {
		step, err := totp.Match(newSeed, factorCode(t, newSeed, f.now), f.now)
		if err != nil {
			t.Fatal(err)
		}
		if step <= state.TOTP[0].LastConsumedStep {
			break
		}
		newSeed, err = totp.Generate(state.TOTP[0].Account, 2)
		if err != nil {
			t.Fatal(err)
		}
		if tries == 15 {
			t.Fatal("fixture repeatedly selected a future collision")
		}
	}
	sealed, err := f.deps.TOTPKeys.Seal(newSeed)
	if err != nil {
		t.Fatal(err)
	}
	_, version, id := newSeed.Identity()
	state.TOTP[0].SeedVersion, state.TOTP[0].SeedID = version, id
	user := f.user.DeepCopy()
	user.Spec.TOTPSecret = sealed
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := tx.Apply(ctx, user, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		return stageSecurity(ctx, tx, state)
	})
	if err != nil {
		t.Fatal(err)
	}
	proof.TOTPCode = factorCode(t, newSeed, f.now)
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := AuthenticateCurrent(ctx, tx, f.deps, proof, f.now)
		return err
	})
	if !errors.Is(err, ErrCredentials) {
		t.Fatal("seed rotation reset consumed floor", err)
	}
	next := f.now.Add(60 * time.Second)
	proof.TOTPCode = factorCode(t, newSeed, next)
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := AuthenticateCurrent(ctx, tx, f.deps, proof, next)
		return err
	})
	if err != nil {
		t.Fatal("strictly newer step rejected", err)
	}
}
func TestTOTPReplayRestartAndRealReplicaPromotion(t *testing.T) {
	f := newFixture(t)
	seed := setupTOTP(t, f)
	proof := Credential{Kind: Password, Username: "alice", Value: "correct-password", TOTPCode: factorCode(t, seed, f.now)}
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := AuthenticateCurrent(ctx, tx, f.deps, proof, f.now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openStore(t, f.path, replication.Standalone)
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := AuthenticateCurrent(ctx, tx, f.deps, proof, f.now)
		return err
	})
	if !errors.Is(err, ErrCredentials) {
		t.Fatal("restart forgot replay floor", err)
	}
	source := f.image(t)
	follower := openStore(t, filepath.Join(t.TempDir(), "replica.db"), replication.Follower)
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
	if _, err = follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: source.Term, ExpectedInstalled: installed, RequestID: "totp-promotion"}); err != nil {
		t.Fatal(err)
	}
	err = follower.Command(t.Context(), follower.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := AuthenticateCurrent(ctx, tx, f.deps, proof, f.now)
		return err
	})
	if !errors.Is(err, ErrCredentials) {
		t.Fatal("promotion forgot factor floor", err)
	}
}
func legacyRows(t *testing.T, s Security) []replication.Entry {
	t.Helper()
	s.TOTP = []totp.Record{}
	rows, err := EncodeSecurity(s)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		if row.Key == RootKey {
			var root RootEntry
			_ = json.Unmarshal(row.Data, &root)
			old := legacyRootEntry{FormatVersion: 1, Epoch: root.Epoch, GrantFloors: root.GrantFloors, AccountFloors: root.AccountFloors, ProviderFloors: root.ProviderFloors, Sessions: root.Sessions}
			rows[i].Data, _ = json.Marshal(old)
		}
	}
	return rows
}
func TestExplicitStrictSecurityV1UpgradePreservesState(t *testing.T) {
	f := newFixture(t)
	old := f.security
	old.GrantFloors = []GrantFloor{{FormatVersion: 1, Grant: "viewer", ValidAfter: f.now.Add(-time.Hour)}}
	old.AccountFloors = []AccountFloor{{FormatVersion: 1, Account: accountRef(f.user), ValidAfter: f.now.Add(-time.Hour)}}
	old.ProviderFloors = []ProviderFloor{{FormatVersion: 1, Provider: "test-idp", UID: "provider-uid", ValidAfter: f.now.Add(-time.Hour)}}
	old.Sessions = slices.Clone(old.Sessions)
	old.Sessions[0].Revoked = true
	rows := legacyRows(t, old)
	if _, err := DecodeSecurity(rows); !errors.Is(err, ErrVersion) {
		t.Fatal("normal decoder upgraded legacy root", err)
	}
	upgraded, err := UpgradeSecurityV1(rows)
	if err != nil {
		t.Fatal(err)
	}
	want := old
	want.Epoch++
	if !reflect.DeepEqual(upgraded, want) {
		t.Fatal("upgrade changed prior canonical auth values")
	}
	encoded, err := EncodeSecurity(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Key == RootKey {
			continue
		}
		found := false
		for _, next := range encoded {
			if next.Key == row.Key {
				found = reflect.DeepEqual(row.Data, next.Data)
			}
		}
		if !found {
			t.Fatal("migration changed old row bytes")
		}
	}
	if _, err = UpgradeSecurityV1(encoded); err == nil {
		t.Fatal("root v2 was migrated twice")
	}
	if _, err = UpgradeSecurityV1(nil); !errors.Is(err, ErrManifest) {
		t.Fatal("missing root was initialized", err)
	}
	old.Epoch = math.MaxUint64
	if _, err = UpgradeSecurityV1(legacyRows(t, old)); !errors.Is(err, ErrBounds) {
		t.Fatal("migration epoch overflow accepted", err)
	}
	rows = append(rows, rows[0])
	if _, err = UpgradeSecurityV1(rows); err == nil {
		t.Fatal("duplicate legacy row migrated")
	}
}

func TestTOTPCompleteCutRejectsMissingDisabledOrWrongSeedIdentity(t *testing.T) {
	f := newFixture(t)
	setupTOTP(t, f)
	for _, what := range []string{"missing-row", "disabled-row", "wrong-uid", "wrong-version", "wrong-seed-id", "missing-keys", "wrong-keys", "missing-inventory"} {
		t.Run(what, func(t *testing.T) {
			image := f.image(t)
			state, err := DecodeSecurity(image.Payload.Security)
			if err != nil {
				t.Fatal(err)
			}
			deps := f.deps
			switch what {
			case "missing-row":
				state.TOTP = []totp.Record{}
			case "disabled-row":
				state.TOTP[0].Enabled = false
			case "wrong-uid":
				state.TOTP[0].Account.UID = "replacement-uid"
			case "wrong-version":
				state.TOTP[0].SeedVersion++
			case "wrong-seed-id":
				state.TOTP[0].SeedID = "00000000000000000000000000000000"
			case "missing-keys":
				deps.TOTPKeys = nil
			case "wrong-keys":
				deps.TOTPKeys, _ = totp.NewKeyring([]byte("11234567890123456789012345678901"), nil)
			}
			image.Payload.Security, err = EncodeSecurity(state)
			if err != nil {
				t.Fatal(err)
			}
			if what == "missing-inventory" {
				for i, row := range image.Payload.Security {
					if row.Key == RootKey {
						var doc map[string]json.RawMessage
						_ = json.Unmarshal(row.Data, &doc)
						delete(doc, "totp")
						image.Payload.Security[i].Data, _ = json.Marshal(doc)
					}
				}
			}
			if err = replication.Seal(image); err != nil {
				t.Fatal(err)
			}
			cut, err := CaptureImage(t.Context(), image, f.registry)
			if err == nil {
				_, err = Prepare(t.Context(), cut, deps)
			}
			if err == nil {
				t.Fatal("incomplete/mismatched TOTP cut was accepted")
			}
		})
	}
	v := f.view(t)
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.localWire}, f.now); err != nil {
		t.Fatal("compiled owner read store after Prepare", err)
	}
}
