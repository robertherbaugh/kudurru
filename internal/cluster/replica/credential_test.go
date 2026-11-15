package replica

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestCredentialReferenceBindsExactTypedProof(t *testing.T) {
	wire, id := tokens.Generate()
	proof, err := NewCredentialProof(servingauth.Credential{Kind: servingauth.APIToken, Value: wire})
	if err != nil {
		t.Fatal(err)
	}
	if ref, err := proof.Reference(); err != nil || ref != id {
		t.Fatal("API token reference lost actual token ID", err)
	}
	var absent *CredentialProof
	if _, err := absent.Reference(); err == nil {
		t.Fatal("absent proof gained reference")
	}
	seen := map[string]bool{}
	for _, raw := range []servingauth.Credential{
		{Kind: servingauth.Cookie, Value: "same-original-bytes"},
		{Kind: servingauth.OIDCJWT, Value: "same-original-bytes"},
		{Kind: servingauth.OIDCJWT, Value: "changed-original-bytes"},
		{Kind: servingauth.OIDCJWT, Value: "same-original-bytes", ProviderID: "original-provider"},
	} {
		proof, err := NewCredentialProof(raw)
		if err != nil {
			t.Fatal(err)
		}
		reference, err := proof.Reference()
		if err != nil || !strings.HasPrefix(reference, "sha256:") || seen[reference] || strings.Contains(reference, raw.Value) {
			t.Fatal("reference lost original proof identity or exposed bytes", err)
		}
		seen[reference] = true
		encoded, err := json.Marshal(proof)
		if err != nil {
			t.Fatal(err)
		}
		var decoded CredentialProof
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if got, err := decoded.Reference(); err != nil || got != reference {
			t.Fatal("wire round trip changed original reference", err)
		}
	}
}

func TestCredentialProofWireBoundsAndRedaction(t *testing.T) {
	for _, kind := range []servingauth.CredentialKind{servingauth.APIToken, servingauth.Cookie, servingauth.OIDCJWT} {
		raw := servingauth.Credential{Kind: kind, Value: "private-wire-proof"}
		if kind == servingauth.OIDCJWT {
			raw.ProviderID = "original-provider"
		}
		proof, err := NewCredentialProof(raw)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(proof)
		if err != nil || !bytes.Contains(wire, []byte(raw.Value)) {
			t.Fatal("member wire lost raw proof", err)
		}
		var received CredentialProof
		if err := json.Unmarshal(wire, &received); err != nil {
			t.Fatal(err)
		}
		got, err := received.Raw()
		if err != nil || got != raw {
			t.Fatal("member wire changed original proof")
		}
		got.Value = "replacement"
		if again, _ := received.Raw(); again != raw {
			t.Fatal("raw projection aliases wire proof")
		}
		var log bytes.Buffer
		slog.New(slog.NewJSONHandler(&log, nil)).Info("test", "proof", proof, "command", Command{Credential: proof, Payload: json.RawMessage(`{"secret":"private-payload"}`)})
		printed := log.String() + fmt.Sprintf("%v %+v %#v", proof, proof, proof) + fmt.Sprintf("%v %+v %#v", Command{Credential: proof}, Command{Credential: proof}, Command{Credential: proof})
		if strings.Contains(printed, raw.Value) || strings.Contains(printed, "private-payload") {
			t.Fatal("diagnostics exposed command secrets")
		}
	}
	for _, raw := range []servingauth.Credential{
		{}, {Kind: servingauth.Password, Value: "password"},
		{Kind: servingauth.APIToken, Value: "token", Username: "user"},
		{Kind: servingauth.Cookie, Value: "cookie", TOTPCode: "123456"},
		{Kind: servingauth.Cookie, Value: "cookie", ProviderID: "foreign"},
		{Kind: servingauth.APIToken, Value: "white space"},
		{Kind: servingauth.OIDCJWT, Value: strings.Repeat("x", MaxCredentialBytes+1)},
		{Kind: servingauth.OIDCJWT, Value: "token", ProviderID: strings.Repeat("x", 257)},
	} {
		if _, err := NewCredentialProof(raw); err == nil {
			t.Fatal("invalid proof accepted")
		}
	}
	if _, err := NewCredentialProof(servingauth.Credential{Kind: servingauth.OIDCJWT, Value: strings.Repeat("x", MaxCredentialBytes)}); err != nil {
		t.Fatal("exact credential bound refused", err)
	}
	for _, wire := range []string{
		`null`, `{}`, `{"kind":"api-token","value":"one","value":"two"}`,
		`{"kind":"api-token","value":"one","principal":{"admin":true}}`,
		`{"kind":"api-token","value":3}`, `{"kind":"api-token","value":"one"} {}`,
	} {
		proof, _ := NewCredentialProof(servingauth.Credential{Kind: servingauth.APIToken, Value: "original"})
		if err := proof.UnmarshalJSON([]byte(wire)); err == nil {
			t.Fatal("invalid wire accepted")
		}
		if _, err := proof.Raw(); err == nil {
			t.Fatal("failed decode retained prior raw proof")
		}
	}
}

func TestCommandCredentialVersionAndFingerprint(t *testing.T) {
	s := openStore(t, replication.Primary)
	legacy := commandFor(s, "credential-version")
	wire, err := json.Marshal(legacy)
	if err != nil || bytes.Contains(wire, []byte(`"credential"`)) {
		t.Fatal("version 1 wire changed", err)
	}
	proof, _ := NewCredentialProof(servingauth.Credential{Kind: servingauth.Cookie, Value: "original-cookie"})
	command := legacy
	command.Credential = proof
	if err := command.validate(time.Now(), DefaultCommandLifetime); err == nil {
		t.Fatal("version 1 accepted a raw-proof extension")
	}
	command.Version = 2
	if err := command.validate(time.Now(), DefaultCommandLifetime); err != nil {
		t.Fatal(err)
	}
	wire, err = json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	var received Command
	if err := decodeStrict(wire, &received); err != nil {
		t.Fatal(err)
	}
	original, err := command.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	actual, err := received.fingerprint()
	if err != nil || actual != original {
		t.Fatal("wire changed immutable command fingerprint")
	}
	received.Credential, _ = NewCredentialProof(servingauth.Credential{Kind: servingauth.Cookie, Value: "different-cookie"})
	changed, err := received.fingerprint()
	if err != nil || changed == original {
		t.Fatal("dedup ignored substituted raw proof")
	}
	for _, mutate := range []func(*Command){
		func(c *Command) { c.Version = 3 },
		func(c *Command) { c.Credential = nil },
		func(c *Command) { c.Subject.CredentialKind = "kubernetes" },
		func(c *Command) { c.Subject.CredentialKind = "api-token" },
	} {
		bad := command
		mutate(&bad)
		if err := bad.validate(time.Now(), DefaultCommandLifetime); err == nil {
			t.Fatal("invalid credential/version pairing accepted")
		}
	}
}
