package oidccore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
)

func evidenceFixture(t *testing.T) (*EvidenceKeyring, *Policy, *VerifiedToken, EvidenceBinding, EvidenceSource) {
	t.Helper()
	k, err := NewEvidenceKeyring(bytes.Repeat([]byte{1}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	v := testVerifier(t)
	token, err := v.Verify(t.Context(), signRSA(t, testClaims()), testNow)
	if err != nil {
		t.Fatal(err)
	}
	p := testPolicy(t, testSpec())
	b := EvidenceBinding{Kind: SessionEvidence, Provider: "corp", ProviderUID: "provider-uid", Origin: "object", Issuer: testIssuer, Subject: testClaims()["sub"].(string), IdentityUID: strings.Repeat("a", 32), RecordID: "abcdefghijklmnop"}
	source := EvidenceSource{strings.Repeat("a", 64), p.Identity(), strings.Repeat("c", 64)}
	return k, p, token, b, source
}
func TestEvidenceSeparateSessionAndExpiringRequestAuthority(t *testing.T) {
	k, p, token, b, source := evidenceFixture(t)
	sealed, err := k.SealVerified(b, source, token, BearerIDToken, testNow)
	if err != nil {
		t.Fatal(err)
	}
	e, err := k.Open(b, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.EvaluateEnrollment(t.Context(), e, testNow.Add(2*time.Hour)); err != nil {
		t.Fatal("enrolled evidence treated as expired JWT", err)
	}
	if _, err = p.EvaluateRequest(t.Context(), e, testNow); err == nil {
		t.Fatal("session evidence became request proof")
	}
	changed := testSpec()
	changed.ClaimValidationRules = nil
	// Current mapping policy still re-evaluates enrolled claims after expiry.
	changed.ClaimMappings.Username.Expression = `"changed"`
	changed.ClaimMappings.Username.Claim = ""
	policy := testPolicy(t, changed)
	mapped, err := policy.EvaluateEnrollment(t.Context(), e, testNow.Add(2*time.Hour))
	if err != nil || mapped.Username != "corp:changed" {
		t.Fatal("current mapping ignored", err)
	}
	b.Kind = RequestEvidence
	b.RecordID = ""
	sealed, err = k.SealVerified(b, source, token, BearerIDToken, testNow)
	if err != nil {
		t.Fatal(err)
	}
	e, err = k.Open(b, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.EvaluateRequest(t.Context(), e, testNow); err != nil {
		t.Fatal(err)
	}
	if _, err = p.EvaluateRequest(t.Context(), e, testNow.Add(2*time.Hour)); !errors.Is(err, ErrTime) {
		t.Fatal("request expiry lost", err)
	}
	if _, err = p.EvaluateEnrollment(t.Context(), e, testNow); err == nil {
		t.Fatal("request evidence became enrollment")
	}
	if _, err = k.SealVerified(b, source, token, BearerIDToken, testNow.Add(2*time.Hour)); err == nil {
		t.Fatal("expired JWT enrolled")
	}
	if _, err = k.SealVerified(b, source, token, BrowserIDToken, testNow); err == nil {
		t.Fatal("browser marker bypass")
	}
	if _, err = p.Evaluate(t.Context(), token, BearerIDToken, testNow.Add(2*time.Hour)); !errors.Is(err, ErrTime) {
		t.Fatal("strict Evaluate changed", err)
	}
}
func TestEvidenceExactBindingPurposeRotationCapsAndDiagnostics(t *testing.T) {
	k, p, token, b, source := evidenceFixture(t)
	sealed, err := k.SealVerified(b, source, token, BearerIDToken, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*EvidenceBinding){func(b *EvidenceBinding) { b.ProviderUID = "other" }, func(b *EvidenceBinding) { b.Origin = "config" }, func(b *EvidenceBinding) { b.IdentityUID = strings.Repeat("b", 32) }, func(b *EvidenceBinding) { b.RecordID = "qrstuvwxyzabcdef" }, func(b *EvidenceBinding) { b.Subject = "other" }} {
		changed := b
		change(&changed)
		if _, err = k.Open(changed, sealed); err == nil {
			t.Fatal("binding swap")
		}
	}
	wrong, _ := session.NewSealer(session.PurposeSession, bytes.Repeat([]byte{1}, 32), nil)
	raw, _, _ := k.sealer.Open(sealed)
	if _, err = k.Open(b, wrong.Seal(raw)); err == nil {
		t.Fatal("purpose confusion")
	}
	rotated, err := NewEvidenceKeyring(bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rotated.Open(b, sealed); err != nil {
		t.Fatal("previous key rejected", err)
	}
	retired, _ := NewEvidenceKeyring(bytes.Repeat([]byte{2}, 32), nil)
	if _, err = retired.Open(b, sealed); err == nil {
		t.Fatal("retired key accepted")
	}
	if _, err = k.Open(b, strings.Repeat("x", MaxEvidenceSealedBytes+1)); err == nil {
		t.Fatal("cap")
	}
	for _, malformed := range []string{strings.Replace(string(raw), `"formatVersion":1`, `"formatVersion":99`, 1), strings.Replace(string(raw), `"formatVersion":1`, `"formatVersion":1,"FormatVersion":1`, 1), strings.Replace(string(raw), `"kind":"session"`, `"kind":"session","Kind":"session"`, 1)} {
		if _, err = k.Open(b, k.sealer.Seal([]byte(malformed))); err == nil {
			t.Fatal("strict evidence schema")
		}
	}
	e, _ := k.Open(b, sealed)
	facts := e.Facts()
	facts.Audiences[0] = "injected"
	if _, err = p.EvaluateEnrollment(t.Context(), e, testNow); err != nil {
		t.Fatal("facts alias", err)
	}
	for _, value := range []any{k, e, token, browserProofForTest()} {
		dump := fmt.Sprintf("%v %#v", value, value)
		j, _ := json.Marshal(value)
		if strings.Contains(dump, "stable:subject") || strings.Contains(string(j), "stable:subject") || strings.Contains(dump, sealed) {
			t.Fatal("evidence disclosure")
		}
	}
	if _, err = k.SealVerified(b, source, &VerifiedToken{}, BearerIDToken, testNow); err == nil {
		t.Fatal("zero verified token accepted")
	}
}
func browserProofForTest() BrowserProof {
	return BrowserProof{Nonce: "private-nonce", AccessToken: "private-access-token"}
}
func TestRoutingIssuerIsBoundedAndUnverified(t *testing.T) {
	raw := signRSA(t, testClaims())
	issuer, err := RoutingIssuer(raw)
	if err != nil || issuer != testIssuer || testVerifier(t).Issuer() != issuer {
		t.Fatal("routing", err)
	}
	if _, err = RoutingIssuer(strings.Repeat("x", MaxTokenBytes+1)); !errors.Is(err, ErrBounds) {
		t.Fatal(err)
	}
	if _, err = RoutingIssuer(signRaw(t, []byte(`{"alg":"RS256"}`), []byte(`{"iss":"a","iss":"b"}`))); err == nil {
		t.Fatal("duplicate routing claim")
	}
}
