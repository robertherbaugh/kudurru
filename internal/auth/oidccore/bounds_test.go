package oidccore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestBoundedJSONRejectsBeforeAllocationGrowth(t *testing.T) {
	for _, raw := range []string{
		`{"a":` + strings.Repeat("[", MaxJSONDepth+1) + "0" + strings.Repeat("]", MaxJSONDepth+1) + "}",
		`{"a":[` + strings.Repeat("0,", MaxJSONMembers) + "0]}",
		`{"a":"` + strings.Repeat("x", MaxStringBytes+1) + `"}`,
	} {
		if _, err := parseObject([]byte(raw), MaxClaimsBytes); !errors.Is(err, ErrBounds) {
			t.Fatal("JSON bound not enforced", err)
		}
	}
	if _, err := parseObject([]byte(strings.Repeat(" ", MaxClaimsBytes+1)), MaxClaimsBytes); !errors.Is(err, ErrBounds) {
		t.Fatal("byte cap ignored", err)
	}
	if _, err := parseObject([]byte(`{"a":"\ud83d\ude00","b":"\\ud800"}`), MaxClaimsBytes); err != nil {
		t.Fatal("valid surrogate/literal escape rejected", err)
	}
	for _, raw := range []string{`{"a":"\ud800x"}`, `{"a":"\ud800\ud800"}`, `{"a":"\udfff"}`, `{"a":1} trailing`} {
		if _, err := parseObject([]byte(raw), MaxClaimsBytes); err == nil {
			t.Fatal("invalid Unicode/trailing JSON accepted")
		}
	}
	// A broad nested input can fit each container's cap while exceeding the
	// total node budget. The parser must still stop before the full tree grows.
	row := "[" + strings.Repeat("0,", 1023) + "0]"
	if _, err := parseObject([]byte(`{"v":[`+row+","+row+","+row+","+row+`]}`), MaxClaimsBytes); !errors.Is(err, ErrBounds) {
		t.Fatal("aggregate JSON node budget ignored", err)
	}
}

func TestPolicyAggregateAndEscapedByteBudgets(t *testing.T) {
	spec := testSpec()
	spec.ClaimValidationRules = make([]publicv1.ClaimValidationRule, 64)
	for i := range spec.ClaimValidationRules {
		spec.ClaimValidationRules[i] = publicv1.ClaimValidationRule{Claim: "required", RequiredValue: strings.Repeat("<", 2048)}
	}
	if _, err := Compile(spec, "corp"); !errors.Is(err, ErrBounds) {
		t.Fatal("aggregate escaped policy byte budget ignored", err)
	}
	for i := range spec.ClaimValidationRules {
		spec.ClaimValidationRules[i] = publicv1.ClaimValidationRule{Expression: "true"}
	}
	spec.ClaimMappings.Username.Expression = "'alice'"
	if _, err := Compile(spec, "corp"); !errors.Is(err, ErrBounds) {
		t.Fatal("total expression count ignored mappings", err)
	}
	// Ordinary empty public mappings remain valid defaults, not expressions.
	spec = testSpec()
	if _, err := Compile(spec, "corp"); err != nil {
		t.Fatal("empty mapping defaults rejected", err)
	}
}

func TestJWTAndJWKSByteCountAndMappedOutputCaps(t *testing.T) {
	v := testVerifier(t)
	claims := testClaims()
	claims["sub"] = strings.Repeat("x", MaxSubjectBytes+1)
	if _, err := v.Verify(t.Context(), signRSA(t, claims), testNow); err == nil {
		t.Fatal("subject cap ignored")
	}
	claims = testClaims()
	aud := make([]string, MaxAudiences+1)
	for i := range aud {
		aud[i] = fmt.Sprintf("aud-%d", i)
	}
	claims["aud"] = aud
	if _, err := v.Verify(t.Context(), signRSA(t, claims), testNow); !errors.Is(err, ErrBounds) {
		t.Fatal("audience count cap ignored", err)
	}
	claims = testClaims()
	groups := make([]string, MaxGroups+1)
	for i := range groups {
		groups[i] = fmt.Sprintf("group-%d", i)
	}
	claims["groups"] = groups
	if _, err := evaluateClaims(t, testPolicy(t, testSpec()), claims); !errors.Is(err, ErrBounds) {
		t.Fatal("mapped group cap ignored", err)
	}
	if _, err := NewVerifier(testIssuer, nil, []byte(strings.Repeat("x", MaxJWKSBytes+1))); !errors.Is(err, ErrBounds) {
		t.Fatal("JWKS byte cap ignored", err)
	}
	key := jose.JSONWebKey{Key: &testRSA(t).PublicKey, KeyID: "key", Algorithm: "RS256"}
	keys := make([]jose.JSONWebKey, MaxKeys+1)
	for i := range keys {
		keys[i] = key
	}
	if _, err := NewVerifier(testIssuer, nil, testJWKS(t, keys...)); !errors.Is(err, ErrBounds) {
		t.Fatal("JWKS key count cap ignored", err)
	}
	payload := []byte(strings.Repeat(" ", MaxClaimsBytes+1))
	if _, err := v.Verify(t.Context(), signRaw(t, []byte(`{"alg":"RS256","kid":"rsa"}`), payload), testNow); !errors.Is(err, ErrBounds) {
		t.Fatal("decoded payload cap ignored", err)
	}
	header := []byte(`{"alg":"RS256","kid":"rsa","typ":"` + strings.Repeat("x", MaxHeaderBytes) + `"}`)
	body, _ := json.Marshal(testClaims())
	if _, err := v.Verify(t.Context(), signRaw(t, header, body), testNow); !errors.Is(err, ErrBounds) {
		t.Fatal("decoded header cap ignored", err)
	}
}

func TestCanonicalBase64AndSafeDiagnostics(t *testing.T) {
	for _, raw := range []string{"YQ==", "YR", "YQ\n", "+w", "_x"} {
		if _, err := decodeSegment(raw, 8); err == nil {
			t.Fatal("noncanonical base64url accepted")
		}
	}
	if b, err := decodeSegment("YQ", 8); err != nil || string(b) != "a" {
		t.Fatal("canonical base64url rejected", err)
	}
	token, err := testVerifier(t).Verify(t.Context(), signRSA(t, testClaims()), testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{token, token.Facts(), BrowserProof{Nonce: "secret-nonce", AccessToken: "secret-access"}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			s := fmt.Sprintf(format, value)
			if strings.Contains(s, "stable:subject") || strings.Contains(s, "secret-") {
				t.Fatal("credential material disclosed by formatting")
			}
		}
		raw, err := json.Marshal(value)
		if err != nil || string(raw) != `"redacted"` {
			t.Fatal("credential material disclosed by JSON", err)
		}
	}
	if _, err := testVerifier(t).Verify(t.Context(), "secret-invalid-token", testNow); err == nil || strings.Contains(err.Error(), "secret-") {
		t.Fatal("unsafe error diagnostic")
	}
	// Strict decoding rejects nonzero trailing padding bits, independently of
	// encoding/json or the signature library's more tolerant decoders.
	if _, err := base64.RawURLEncoding.Strict().DecodeString("YR"); err == nil {
		t.Fatal("fixture is canonical")
	}
}
