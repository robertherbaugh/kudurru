package servingauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func testBrowserAttempt(t *testing.T) BrowserAttempt {
	t.Helper()
	s, err := browseroidc.New("corp", "/", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return BrowserAttempt{FormatVersion: 1, ID: s.ID(), Provider: ProviderRef{ID: "corp", UID: "provider-uid", Origin: ProviderObject},
		SpecIdentity: hash("spec"), ResourceIdentity: hash("resource"), ClientID: "client", RedirectURI: "https://dns.example/callback",
		IssuedAt: s.IssuedAt(), ExpiresAt: s.ExpiresAt(), PayloadHash: s.PayloadHash()}
}

func TestBrowserSecurityRequiresExactCompleteInventory(t *testing.T) {
	s := EmptySecurity(1)
	s.BrowserAttempts = []BrowserAttempt{testBrowserAttempt(t)}
	rows, err := EncodeSecurity(s)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSecurity(rows)
	if err != nil || !reflect.DeepEqual(decoded, s) {
		t.Fatal("round trip changed browser authority", err)
	}
	for _, scenario := range []string{"missing-inventory", "null-inventory", "missing-row", "unlisted-row", "duplicate-id", "unknown-field", "bad-id", "wrong-key", "long-lifetime", "version"} {
		t.Run(scenario, func(t *testing.T) {
			input := make([]replication.Entry, len(rows))
			for i, row := range rows {
				input[i] = replication.Entry{Key: row.Key, Data: bytes.Clone(row.Data)}
			}
			for i := range input {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(input[i].Data, &fields); err != nil {
					t.Fatal(err)
				}
				if input[i].Key == RootKey {
					switch scenario {
					case "missing-inventory":
						delete(fields, "browserAttempts")
					case "null-inventory":
						fields["browserAttempts"] = json.RawMessage(`null`)
					case "unlisted-row":
						fields["browserAttempts"] = json.RawMessage(`[]`)
					case "duplicate-id":
						fields["browserAttempts"], _ = json.Marshal([]string{BrowserAttemptKey(s.BrowserAttempts[0].ID), BrowserAttemptKey(s.BrowserAttempts[0].ID)})
					}
				} else {
					switch scenario {
					case "unknown-field":
						fields["refreshToken"] = json.RawMessage(`"must not persist"`)
					case "bad-id":
						fields["id"] = json.RawMessage(`"not-random-canonical-id"`)
					case "wrong-key":
						input[i].Key += "different"
					case "long-lifetime":
						fields["expiresAt"], _ = json.Marshal(s.BrowserAttempts[0].ExpiresAt.Add(time.Second))
					case "version":
						fields["formatVersion"] = json.RawMessage(`2`)
					}
				}
				input[i].Data, _ = json.Marshal(fields)
			}
			if scenario == "missing-row" {
				for i, row := range input {
					if row.Key != RootKey {
						input = append(input[:i], input[i+1:]...)
						break
					}
				}
			}
			if _, err := DecodeSecurity(input); err == nil {
				t.Fatal("incomplete or malformed pending authority accepted")
			}
		})
	}
	for range MaxBrowserAttempts {
		s.BrowserAttempts = append(s.BrowserAttempts, testBrowserAttempt(t))
	}
	if _, err := EncodeSecurity(s); !errors.Is(err, ErrBounds) {
		t.Fatal("browser inventory exceeded cap", err)
	}
}

func TestBrowserV3UpgradeIsExplicitAndAdvancesOnce(t *testing.T) {
	s := EmptySecurity(9)
	s.ProviderFloors = []ProviderFloor{{FormatVersion: 1, Provider: "corp", UID: "prior-uid", ValidAfter: time.Now().UTC()}}
	legacy := func(s Security) []replication.Entry {
		rows, err := EncodeSecurity(s)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			if rows[i].Key != RootKey {
				continue
			}
			var root map[string]json.RawMessage
			if err := json.Unmarshal(rows[i].Data, &root); err != nil {
				t.Fatal(err)
			}
			delete(root, "browserAttempts")
			root["formatVersion"] = json.RawMessage(`3`)
			rows[i].Data, _ = json.Marshal(root)
		}
		return rows
	}
	rows := legacy(s)
	if _, err := DecodeSecurity(rows); err == nil {
		t.Fatal("normal decoder silently migrated v3")
	}
	upgraded, err := UpgradeSecurityV3(rows)
	s.Epoch++
	if err != nil || !reflect.DeepEqual(upgraded, s) {
		t.Fatal("explicit migration changed prior floors", err)
	}
	current, err := EncodeSecurity(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpgradeSecurityV3(current); err == nil {
		t.Fatal("migration applied twice")
	}
	s.Epoch = math.MaxUint64
	if _, err := UpgradeSecurityV3(legacy(s)); !errors.Is(err, ErrBounds) {
		t.Fatal("epoch wrapped", err)
	}
}
