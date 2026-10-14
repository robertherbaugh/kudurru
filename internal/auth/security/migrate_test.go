package security

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Write the complete previous format through the old driver boundary. Opening
// the new driver must allow the explicit startup migration before publication.
func legacySecurity(t *testing.T, st *sqlite.Store, state servingauth.Security) {
	t.Helper()
	legacySecurityVersion(t, st, state, 1)
}

func legacySecurityVersion(t *testing.T, st *sqlite.Store, state servingauth.Security, version int) {
	t.Helper()
	rows, err := servingauth.EncodeSecurity(state)
	if err != nil {
		t.Fatal(err)
	}
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, row := range rows {
			if row.Key == servingauth.RootKey {
				var root map[string]json.RawMessage
				if err := json.Unmarshal(row.Data, &root); err != nil {
					return err
				}
				if version == 1 {
					delete(root, "totp")
				}
				delete(root, "externalIdentities")
				delete(root, "externalSessions")
				root["formatVersion"], _ = json.Marshal(version)
				row.Data, err = json.Marshal(root)
				if err != nil {
					return err
				}
			}
			if err := tx.PutSection(ctx, "security", row); err != nil {
				return err
			}
		}
		return tx.PutSection(ctx, "security", replication.Entry{Key: "other-owner/value", Data: json.RawMessage(`{"keep":true}`)})
	})
}

func TestExplicitV2UpgradePreservesActiveFactorAndAllSecurityRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st := openSecurityStoreValidator(t, path, nil)
	key := make([]byte, 32)
	key[0] = 1
	keys, err := totp.NewKeyring(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	user, err := st.Apply(t.Context(), &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "factor-user"}}, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	account := totp.Account{Name: user.GetName(), UID: string(user.GetUID())}
	seed, err := totp.Generate(account, 8)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := keys.Seal(seed)
	if err != nil {
		t.Fatal(err)
	}
	u := user.(*internalv1.LocalUser)
	u.Spec.TOTPSecret = sealed
	if _, err := st.Apply(t.Context(), u, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	before := servingauth.EmptySecurity(14)
	_, seedVersion, seedID := seed.Identity()
	before.TOTP = []totp.Record{{FormatVersion: totp.FormatVersion, Account: account, SeedVersion: seedVersion, SeedID: seedID, Enabled: true, HasConsumed: true, LastConsumedStep: 12345}}
	before.AccountFloors = []servingauth.AccountFloor{{FormatVersion: 1, Account: accountRef(u), ValidAfter: now.Add(-time.Hour)}}
	before.ProviderFloors = []servingauth.ProviderFloor{{FormatVersion: 1, Provider: "retired-provider", UID: "prior-provider-uid", ValidAfter: now}}
	before.GrantFloors = []servingauth.GrantFloor{{FormatVersion: 1, Grant: "viewer", ValidAfter: now.Add(-time.Hour)}}
	before.Sessions = []servingauth.SessionRecord{{FormatVersion: 1, ID: "preserved-v2-session-0123456789", Account: accountRef(u), Provider: "local", Epoch: 12, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), IdleExpiresAt: now.Add(time.Minute), Grants: []string{"viewer"}, CSRFHash: servingauth.CSRFHash("private-csrf-value-at-least-32-bytes"), Revoked: true}}
	legacySecurityVersion(t, st, before, 2)
	_ = st.Close()
	st = openSecurityStore(t, path)
	old, _ := st.ReadGeneration(t.Context())
	if _, err := servingauth.DecodeSecurity(old.Payload.Security); err == nil {
		t.Fatal("ordinary read accepted old root-v2")
	}
	fault := errors.New("post-migration failure")
	err = st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := Initialize(ctx, tx); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatal("supported active factor prevented explicit upgrade", err)
	}
	rolledBack, _ := st.ReadGeneration(t.Context())
	if !old.Generation().Same(rolledBack.Generation()) {
		t.Fatal("v2 upgrade rollback changed durable authority")
	}
	command(t, st, Initialize)
	want := before
	want.Epoch++
	if !reflect.DeepEqual(ownerState(t, st), want) {
		t.Fatal("v2 upgrade lost factor replay or session/floor provenance")
	}
	var current internalv1.LocalUser
	if err := st.Get(t.Context(), store.KeyOf(u), &current); err != nil || current.Spec.TOTPSecret != sealed {
		t.Fatal("upgrade altered active seed", err)
	}
	first, _ := st.ReadGeneration(t.Context())
	command(t, st, Initialize)
	again, _ := st.ReadGeneration(t.Context())
	if !first.Generation().Same(again.Generation()) {
		t.Fatal("v3 initialization advanced twice")
	}
}

func TestExplicitSecurityUpgradePreservesAuthorityAndRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st := openSecurityStoreValidator(t, path, nil)
	now := time.Now().UTC()
	before := servingauth.EmptySecurity(8)
	account := servingauth.AccountRef{Kind: "LocalUser", Name: "alice", UID: "original-account"}
	before.AccountFloors = []servingauth.AccountFloor{{FormatVersion: 1, Account: account, ValidAfter: now.Add(-time.Hour)}}
	before.GrantFloors = []servingauth.GrantFloor{{FormatVersion: 1, Grant: "viewer", ValidAfter: now.Add(-time.Hour)}}
	before.Sessions = []servingauth.SessionRecord{{FormatVersion: 1, ID: "existing-session-0123456789", Account: account, Provider: "local", Epoch: 7, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), IdleExpiresAt: now.Add(time.Minute), Grants: []string{"viewer"}, CSRFHash: servingauth.CSRFHash("private-csrf-value-at-least-32-bytes"), Revoked: true}}
	legacySecurity(t, st, before)
	_ = st.Close()
	st = openSecurityStore(t, path)
	old, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := servingauth.DecodeSecurity(old.Payload.Security); err == nil {
		t.Fatal("ordinary reads silently upgraded v1")
	}
	fault := errors.New("enclosing command failed")
	err = st.Command(t.Context(), st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := Initialize(ctx, tx); err != nil {
			return err
		}
		return fault
	})
	if !errors.Is(err, fault) {
		t.Fatal(err)
	}
	rolledBack, _ := st.ReadGeneration(t.Context())
	if !old.Generation().Same(rolledBack.Generation()) {
		t.Fatal("failed migration changed persisted history")
	}
	command(t, st, Initialize)
	got := ownerState(t, st)
	want := before
	want.Epoch++
	if !reflect.DeepEqual(got, want) {
		t.Fatal("upgrade changed old authority or failed to add explicit empty inventory")
	}
	installed, _ := st.ReadGeneration(t.Context())
	command(t, st, Initialize)
	again, _ := st.ReadGeneration(t.Context())
	if !installed.Generation().Same(again.Generation()) || len(again.Payload.Security) != len(old.Payload.Security) {
		t.Fatal("migration was repeated or changed other owners")
	}
}

func TestInitializeRefusesLegacySeedBeforeSeedingOrUpgrading(t *testing.T) {
	for _, previousRoot := range []bool{false, true} {
		t.Run(map[bool]string{false: "uninitialized", true: "v1"}[previousRoot], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			st := openSecurityStoreValidator(t, path, nil)
			if previousRoot {
				legacySecurity(t, st, servingauth.EmptySecurity(1))
			}
			_, err := st.Apply(t.Context(), &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}, Spec: internalv1.LocalUserSpec{TOTPSecret: "legacy-unsupported-seed", Disabled: true}}, store.ApplyOptions{Manager: store.ManagerSystem})
			if err != nil {
				t.Fatal(err)
			}
			_ = st.Close()
			st = openSecurityStore(t, path)
			before, _ := st.ReadGeneration(t.Context())
			err = st.Command(t.Context(), st.Authority().Term, Initialize)
			if !errors.Is(err, servingauth.ErrUnsupported) {
				t.Fatal("legacy seed was adopted or cleared", err)
			}
			after, _ := st.ReadGeneration(t.Context())
			if !before.Generation().Same(after.Generation()) {
				t.Fatal("failed migration mutated seed or security state")
			}
		})
	}
}
