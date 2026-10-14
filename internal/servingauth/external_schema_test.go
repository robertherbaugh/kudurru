package servingauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"

	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func oldSecurityRows(t *testing.T, s Security, version int) []replication.Entry {
	t.Helper()
	rows, err := EncodeSecurity(s)
	if err != nil {
		t.Fatal(err)
	}
	for j := range rows {
		if rows[j].Key != RootKey {
			continue
		}
		var root map[string]json.RawMessage
		if json.Unmarshal(rows[j].Data, &root) != nil {
			t.Fatal("root")
		}
		root["formatVersion"], _ = json.Marshal(version)
		delete(root, "externalIdentities")
		delete(root, "externalSessions")
		if version == 1 {
			delete(root, "totp")
		}
		rows[j].Data, _ = json.Marshal(root)
	}
	return rows
}
func TestExternalSecurityExplicitV2UpgradePreservesAllAuthority(t *testing.T) {
	s := EmptySecurity(42)
	s.GrantFloors = []GrantFloor{{FormatVersion: 1, Grant: "viewer", ValidAfter: time.Unix(100, 0)}}
	a := AccountRef{Kind: "LocalUser", Name: "alice", UID: "old-uid"}
	s.AccountFloors = []AccountFloor{{FormatVersion: 1, Account: a, ValidAfter: time.Unix(101, 0)}}
	s.ProviderFloors = []ProviderFloor{{FormatVersion: 1, Provider: "corp", UID: "provider-uid", ValidAfter: time.Unix(102, 0)}}
	s.Sessions = []SessionRecord{{FormatVersion: 1, ID: "abcdefghijklmnop", Account: a, Provider: "local", Epoch: 7, IssuedAt: time.Unix(103, 0), ExpiresAt: time.Unix(200, 0), IdleExpiresAt: time.Unix(190, 0), Grants: []string{"viewer"}, CSRFHash: CSRFHash("csrf"), Revoked: true}}
	s.TOTP = []totp.Record{{FormatVersion: 1, Account: totp.Account{Name: "alice", UID: "old-uid"}, Enabled: false, HasConsumed: true, LastConsumedStep: 7}}
	// Obtain an actual valid disabled replay record using the established codec.
	s.TOTP[0].SeedVersion = 1
	s.TOTP[0].SeedID = "0123456789abcdef0123456789abcdef"
	rows := oldSecurityRows(t, s, 2)
	before, _ := json.Marshal(rows)
	if version, err := SecurityRootVersion(rows); err != nil || version != 2 {
		t.Fatal(version, err)
	}
	if _, err := DecodeSecurity(rows); err == nil {
		t.Fatal("normal decoder fell back")
	}
	upgraded, err := UpgradeSecurityV2(rows)
	if err != nil {
		t.Fatal(err)
	}
	expected := s
	expected.Epoch++
	gotValues, _ := json.Marshal(upgraded)
	wantValues, _ := json.Marshal(expected)
	if !bytes.Equal(gotValues, wantValues) {
		t.Fatal("migration changed existing authority")
	}
	after, _ := json.Marshal(rows)
	if !bytes.Equal(before, after) {
		t.Fatal("migration mutated input")
	}
	encoded, err := EncodeSecurity(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = UpgradeSecurityV2(encoded); err == nil {
		t.Fatal("double upgrade")
	}
	s.Epoch = math.MaxUint64
	if _, err = UpgradeSecurityV2(oldSecurityRows(t, s, 2)); !errors.Is(err, ErrBounds) {
		t.Fatal("overflow", err)
	}
}
