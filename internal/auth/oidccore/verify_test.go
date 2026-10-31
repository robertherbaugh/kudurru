package oidccore

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

var rsaOnce sync.Once
var rsaKey *rsa.PrivateKey
var rsaError error

func testRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	rsaOnce.Do(func() { rsaKey, rsaError = rsa.GenerateKey(rand.Reader, 2048) })
	if rsaError != nil {
		t.Fatal(rsaError)
	}
	return rsaKey
}

var testNow = time.Unix(1800000000, 0).UTC()

const testIssuer = "https://issuer.example/tenant"

func testClaims() map[string]any {
	return map[string]any{"iss": testIssuer, "sub": "stable:subject /+", "aud": "browser", "iat": testNow.Add(-time.Minute).Unix(), "exp": testNow.Add(time.Hour).Unix(), "preferred_username": "alice", "groups": []string{"ops", "readers"}}
}

func testJWKS(t *testing.T, keys ...jose.JSONWebKey) []byte {
	t.Helper()
	raw, err := json.Marshal(jose.JSONWebKeySet{Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testVerifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := NewVerifier(testIssuer, nil, testJWKS(t, jose.JSONWebKey{Key: &testRSA(t).PublicKey, KeyID: "rsa", Algorithm: "RS256", Use: "sig"}))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func signToken(t *testing.T, key any, algorithm jose.SignatureAlgorithm, header map[jose.HeaderKey]any, claims any) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: algorithm, Key: key}, &jose.SignerOptions{ExtraHeaders: header})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := s.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	token, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func signRSA(t *testing.T, claims any) string {
	return signToken(t, testRSA(t), jose.RS256, map[jose.HeaderKey]any{"kid": "rsa", "typ": "JWT"}, claims)
}

// Sign exact malformed JSON to prove strict checks apply even to valid signatures.
func signRaw(t *testing.T, header, claims []byte) string {
	t.Helper()
	raw := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	h := sha256.Sum256([]byte(raw))
	sig, err := rsa.SignPKCS1v15(rand.Reader, testRSA(t), crypto.SHA256, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return raw + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestVerifierAllApprovedAsymmetricAlgorithms(t *testing.T) {
	for _, alg := range []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512, jose.ES256, jose.ES384, jose.ES512, jose.EdDSA} {
		t.Run(string(alg), func(t *testing.T) {
			var key, pub any
			switch alg {
			case jose.ES256, jose.ES384, jose.ES512:
				curve := elliptic.P256()
				if alg == jose.ES384 {
					curve = elliptic.P384()
				}
				if alg == jose.ES512 {
					curve = elliptic.P521()
				}
				k, err := ecdsa.GenerateKey(curve, rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				key, pub = k, &k.PublicKey
			case jose.EdDSA:
				p, k, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				key, pub = k, p
			default:
				key, pub = testRSA(t), &testRSA(t).PublicKey
			}
			v, err := NewVerifier(testIssuer, []string{string(alg)}, testJWKS(t, jose.JSONWebKey{Key: pub, KeyID: "test", Algorithm: string(alg)}))
			if err != nil {
				t.Fatal(err)
			}
			tok, err := v.Verify(t.Context(), signToken(t, key, alg, map[jose.HeaderKey]any{"kid": "test"}, testClaims()), testNow)
			if err != nil || tok.Facts().Algorithm != string(alg) || tok.Facts().VerifierIdentity != v.Identity() {
				t.Fatalf("approved algorithm/resource rejected: %v", err)
			}
		})
	}
}

func TestVerifierTimesRecheckedAndClaimsDetached(t *testing.T) {
	v := testVerifier(t)
	raw := signRSA(t, testClaims())
	token, err := v.Verify(t.Context(), raw, testNow)
	if err != nil {
		t.Fatal(err)
	}
	facts := token.Facts()
	facts.Audiences[0] = "injected"
	claims := token.Claims()
	claims["iss"] = "injected"
	claims["groups"].([]any)[0] = "admin"
	if token.Facts().Audiences[0] != "browser" || token.Claims()["groups"].([]any)[0] != "ops" {
		t.Fatal("caller changed immutable proof")
	}
	for _, now := range []time.Time{testNow.Add(time.Hour), testNow.Add(-2 * time.Minute), {}, time.Unix(-1, 0)} {
		if _, err = v.Verify(t.Context(), raw, now); !errors.Is(err, ErrTime) {
			t.Fatalf("verification cached time verdict: %v", err)
		}
		if err = token.ValidAt(now); !errors.Is(err, ErrTime) {
			t.Fatalf("held facts survived invalid time: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = v.Verify(ctx, raw, testNow); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation", err)
	}
	if _, err = (&Verifier{}).Verify(t.Context(), raw, testNow); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty verifier accepted", err)
	}
}

func TestStrictSignedSecurityClaims(t *testing.T) {
	v := testVerifier(t)
	for _, field := range []string{"iss", "sub", "aud", "iat", "exp"} {
		claims := testClaims()
		delete(claims, field)
		if _, err := v.Verify(t.Context(), signRSA(t, claims), testNow); err == nil {
			t.Fatalf("missing %s accepted", field)
		}
	}
	cases := []struct {
		field string
		value any
	}{
		{"iss", "https://other.example"}, {"sub", ""}, {"sub", 7}, {"sub", "nul\x00value"},
		{"aud", nil}, {"aud", []any{}}, {"aud", []any{"browser", 1}}, {"aud", []string{"browser", "browser"}},
		{"azp", nil}, {"azp", ""}, {"azp", false}, {"iat", "1800000000"}, {"iat", json.Number("1800000000.0")},
		{"iat", json.Number("18e8")}, {"iat", -1}, {"iat", testNow.Add(time.Second).Unix()},
		{"exp", testNow.Unix()}, {"exp", testNow.Add(-2 * time.Minute).Unix()}, {"exp", json.Number("999999999999999999999999")},
		{"nbf", testNow.Add(time.Second).Unix()}, {"nbf", nil}, {"nonce", false}, {"at_hash", "bad"},
		{"email_verified", nil}, {"email_verified", 1}, {"email_verified", "True"},
	}
	for i, c := range cases {
		t.Run(strconvIndex(i), func(t *testing.T) {
			claims := testClaims()
			claims[c.field] = c.value
			if _, err := v.Verify(t.Context(), signRSA(t, claims), testNow); err == nil {
				t.Fatalf("invalid %s accepted", c.field)
			}
		})
	}
	claims := testClaims()
	claims["nbf"] = testNow.Unix()
	if _, err := v.Verify(t.Context(), signRSA(t, claims), testNow); err != nil {
		t.Fatal("exact nbf should pass", err)
	}
}

func strconvIndex(i int) string { return fmt.Sprintf("case-%d", i) }

func TestStrictSignedJSONAndHeader(t *testing.T) {
	v := testVerifier(t)
	header := []byte(`{"alg":"RS256","kid":"rsa"}`)
	good, _ := json.Marshal(testClaims())
	for _, body := range [][]byte{
		[]byte(`{"iss":"x","iss":"y"}`), []byte(`{"nested":{"x":1,"x":2}}`),
		append(slices.Clone(good), []byte(` {}`)...), []byte(`null`), []byte(`[]`),
		[]byte(`{"x":"\ud800"}`), []byte(`{"x":"\udc00"}`), {'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'},
	} {
		if _, err := v.Verify(t.Context(), signRaw(t, header, body), testNow); err == nil {
			t.Fatal("malformed signed claims accepted")
		}
	}
	for _, bad := range []string{
		`{"alg":"RS256","alg":"RS256","kid":"rsa"}`, `{"alg":"none","kid":"rsa"}`,
		`{"alg":"RS256","kid":"other"}`, `{"alg":"RS256","kid":null}`,
		`{"alg":"RS256","kid":"rsa","crit":[]}`, `{"alg":"RS256","kid":"rsa","b64":true}`,
		`{"alg":"RS256","kid":"rsa","jku":"https://attacker.invalid"}`, `{"alg":"RS256","kid":"rsa","jwk":{}}`,
		`{"alg":"RS256","kid":"rsa","x5u":"https://attacker.invalid"}`, `{"alg":"RS256","kid":"rsa","typ":"at+jwt"}`,
		`{"alg":"RS256","kid":"rsa","typ":"APPLICATION/AT+JWT"}`, `{"alg":"RS256","kid":"rsa","typ":null}`,
	} {
		if _, err := v.Verify(t.Context(), signRaw(t, []byte(bad), good), testNow); err == nil {
			t.Fatal("unsupported signed header accepted")
		}
	}
	raw := signRSA(t, testClaims())
	parts := strings.Split(raw, ".")
	for _, bad := range []string{raw + ".extra", raw + "=", " " + raw, parts[0] + "=." + parts[1] + "." + parts[2], parts[0] + "." + parts[1] + ".", "{}", strings.Repeat("x", MaxTokenBytes+1)} {
		if _, err := v.Verify(t.Context(), bad, testNow); err == nil {
			t.Fatal("malformed compact token accepted")
		}
	}
	parts[2] = strings.Repeat("A", len(parts[2]))
	if _, err := v.Verify(t.Context(), strings.Join(parts, "."), testNow); !errors.Is(err, ErrSignature) {
		t.Fatal("invalid signature accepted", err)
	}
}

func TestVerifierResourceIdentityDetachedOrderAndSelection(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := jose.JSONWebKey{Key: &testRSA(t).PublicKey, KeyID: "rsa", Algorithm: "RS256"}
	b := jose.JSONWebKey{Key: pub, KeyID: "ed", Algorithm: "EdDSA"}
	raw := testJWKS(t, a, b)
	v, err := NewVerifier(testIssuer, []string{"RS256", "EdDSA"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewVerifier(testIssuer, []string{"EdDSA", "RS256"}, testJWKS(t, b, a))
	if err != nil || v.Identity() != other.Identity() {
		t.Fatal("order-dependent resource identity", err)
	}
	clear(raw)
	if _, err = v.Verify(t.Context(), signRSA(t, testClaims()), testNow); err != nil {
		t.Fatal("JWKS aliased caller", err)
	}
	if _, err = v.Verify(t.Context(), signToken(t, testRSA(t), jose.RS256, nil, testClaims()), testNow); !errors.Is(err, ErrSignature) {
		t.Fatal("ambiguous missing kid accepted", err)
	}
	old := testVerifier(t)
	if v.Identity() == old.Identity() {
		t.Fatal("key-set change lost resource identity")
	}
	if _, err = old.Verify(t.Context(), signToken(t, testRSA(t), jose.RS256, nil, testClaims()), testNow); err != nil {
		t.Fatal("single-key missing kid rejected", err)
	}
}

func TestStrictJWKSRejectsUnsafeAndAmbiguousKeys(t *testing.T) {
	key := jose.JSONWebKey{Key: &testRSA(t).PublicKey, KeyID: "rsa", Algorithm: "RS256", Use: "sig"}
	raw, _ := json.Marshal(key)
	var base map[string]any
	_ = json.Unmarshal(raw, &base)
	for _, mutation := range []map[string]any{
		{"d": "AQ"}, {"k": "AQ"}, {"kty": "oct"}, {"alg": "HS256"}, {"alg": "ES256"},
		{"kid": nullValue()}, {"kid": ""}, {"use": "enc"}, {"key_ops": []string{"sign"}},
		{"x5u": "https://attacker.invalid"}, {"n": "AQ"}, {"e": "Ag"}, {"extra": true}, {"x": "AQ"},
	} {
		m := cloneJSON(base).(map[string]any)
		for k, v := range mutation {
			m[k] = v
		}
		body, _ := json.Marshal(map[string]any{"keys": []any{m}})
		if _, err := NewVerifier(testIssuer, nil, body); err == nil {
			t.Fatal("unsafe JWK accepted")
		}
	}
	for _, body := range [][]byte{testJWKS(t, key, key), []byte(`{"keys":[],"keys":[]}`), []byte(`{"keys":[]}`), []byte(`{"keys":null}`), testJWKS(t, jose.JSONWebKey{Key: testRSA(t), KeyID: "rsa"}), []byte(`{"keys":[],"extra":true}`)} {
		if _, err := NewVerifier(testIssuer, nil, body); err == nil {
			t.Fatal("malformed/ambiguous JWKS accepted")
		}
	}
	for _, algorithms := range [][]string{{"none"}, {"HS256"}, {"RS256", "RS256"}, {"unknown"}} {
		if _, err := NewVerifier(testIssuer, algorithms, testJWKS(t, key)); err == nil {
			t.Fatal("unsafe allowlist accepted")
		}
	}
	for _, issuer := range []string{"", "http://issuer.example", "https://user:secret@issuer.example", "https://issuer.example?query=x", "https://issuer.example#fragment"} {
		if _, err := NewVerifier(issuer, nil, testJWKS(t, key)); err == nil {
			t.Fatal("invalid issuer accepted")
		}
	}
}

func nullValue() any { return nil }

func TestBrowserNonceAndAccessTokenHash(t *testing.T) {
	v := testVerifier(t)
	claims := testClaims()
	claims["nonce"] = "expected-nonce"
	h := sha256.Sum256([]byte("exchanged-access-token"))
	claims["at_hash"] = base64.RawURLEncoding.EncodeToString(h[:16])
	raw := signRSA(t, claims)
	proof := BrowserProof{Nonce: "expected-nonce", AccessToken: "exchanged-access-token"}
	token, err := v.VerifyBrowser(t.Context(), raw, proof, testNow)
	if err != nil || !token.browser {
		t.Fatal("browser proof rejected", err)
	}
	for _, p := range []BrowserProof{{Nonce: "wrong", AccessToken: proof.AccessToken}, {Nonce: proof.Nonce, AccessToken: "wrong"}, {Nonce: proof.Nonce}, {}, {Nonce: proof.Nonce, AccessToken: strings.Repeat("x", MaxTokenBytes+1)}} {
		if _, err = v.VerifyBrowser(t.Context(), raw, p, testNow); !errors.Is(err, ErrBrowser) {
			t.Fatal("invalid browser proof accepted", err)
		}
	}
	delete(claims, "at_hash")
	if _, err = v.VerifyBrowser(t.Context(), signRSA(t, claims), BrowserProof{Nonce: proof.Nonce}, testNow); err != nil {
		t.Fatal("optional absent at_hash rejected", err)
	}
	delete(claims, "nonce")
	if _, err = v.VerifyBrowser(t.Context(), signRSA(t, claims), proof, testNow); !errors.Is(err, ErrBrowser) {
		t.Fatal("absent signed nonce accepted", err)
	}
}
