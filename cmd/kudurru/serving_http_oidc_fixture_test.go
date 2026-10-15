package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
)

// servingBrowserOP is a real TLS/JWKS/signed-JWT code-exchange test peer. Distinct
// /<id> issuers let root exercise BOTH object/config origins without duplicate
// issuers. No real provider or legacy Stack participates.
type servingBrowserOP struct {
	Server                 *httptest.Server
	CA                     []byte
	ClientID, ClientSecret string
	key                    ed25519.PrivateKey
	mu                     sync.Mutex
	flows                  map[string]servingBrowserOPFlow
	mode                   string
	entered, resume        chan struct{}
	requests               atomic.Int64
}
type servingBrowserOPFlow struct {
	issuer, client, redirect, nonce, challenge string
	used                                       bool
}

func newServingBrowserOP(t *testing.T) *servingBrowserOP {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := &servingBrowserOP{ClientID: "browser", ClientSecret: "browser-test-client-secret", key: key, flows: map[string]servingBrowserOPFlow{}}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "browser test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	p.CA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	p.Server = httptest.NewUnstartedServer(http.HandlerFunc(p.serve))
	p.Server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	p.Server.StartTLS()
	t.Cleanup(p.Server.Close)
	return p
}

func (p *servingBrowserOP) Issuer(id string) string { return p.Server.URL + "/" + id }

// Complete validates the captured authorize redirect and returns a real callback
// URL with an independent, one-use code. Never print its returned private query.
func (p *servingBrowserOP) Complete(t *testing.T, authorization string) string {
	t.Helper()
	u, err := url.Parse(authorization)
	if err != nil {
		t.Fatal("invalid authorize redirect")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != p.ClientID || q.Get("state") == "" || q.Get("nonce") == "" || len(q.Get("code_challenge")) != 43 || !strings.HasSuffix(u.Path, "/authorize") {
		t.Fatal("invalid captured authorization profile")
	}
	issuer := strings.TrimSuffix(u.Scheme+"://"+u.Host+u.Path, "/authorize")
	callback, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || callback.Host == "" {
		t.Fatal("invalid callback target")
	}
	var random [32]byte
	_, _ = rand.Read(random[:])
	code := base64.RawURLEncoding.EncodeToString(random[:])
	p.mu.Lock()
	p.flows[code] = servingBrowserOPFlow{issuer: issuer, client: q.Get("client_id"), redirect: q.Get("redirect_uri"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	p.mu.Unlock()
	values := callback.Query()
	values.Set("code", code)
	values.Set("state", q.Get("state"))
	values.Set("iss", issuer)
	callback.RawQuery = values.Encode()
	return callback.String()
}

func (p *servingBrowserOP) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "max-age=60")
	// Issuer path is /<id>; all test routes contain that bounded segment.
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		w.WriteHeader(404)
		return
	}
	issuer := p.Issuer(parts[0])
	switch {
	case strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration"):
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"EdDSA"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"client_secret_basic"}})
	case strings.HasSuffix(r.URL.Path, "/keys"):
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: p.key.Public(), KeyID: "browser-test", Algorithm: "EdDSA", Use: "sig"}}})
	case strings.HasSuffix(r.URL.Path, "/token"):
		p.exchange(w, r, issuer)
	default:
		w.WriteHeader(404)
	}
}

func (p *servingBrowserOP) exchange(w http.ResponseWriter, r *http.Request, issuer string) {
	p.requests.Add(1)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	client, secret, basic := r.BasicAuth()
	client, e1 := url.QueryUnescape(client)
	secret, e2 := url.QueryUnescape(secret)
	if r.Method != "POST" || !basic || e1 != nil || e2 != nil || client != p.ClientID || secret != p.ClientSecret || r.ParseForm() != nil || len(r.PostForm) != 4 || r.PostForm.Get("grant_type") != "authorization_code" {
		w.WriteHeader(401)
		return
	}
	challenge, err := browseroidc.S256(r.PostForm.Get("code_verifier"))
	if err != nil {
		w.WriteHeader(400)
		return
	}
	p.mu.Lock()
	flow, ok := p.flows[r.PostForm.Get("code")]
	if !ok || flow.used || flow.issuer != issuer || flow.client != client || flow.redirect != r.PostForm.Get("redirect_uri") || flow.challenge != challenge {
		p.mu.Unlock()
		w.WriteHeader(400)
		return
	}
	flow.used = true
	p.flows[r.PostForm.Get("code")] = flow
	mode, entered, resume := p.mode, p.entered, p.resume
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if resume != nil {
		select {
		case <-resume:
		case <-r.Context().Done():
			return
		}
	}
	if mode == "upstream-error" {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error_description":"NEVER-EXPOSE-UPSTREAM"}`))
		return
	}
	if mode == "oversize" {
		_, _ = w.Write([]byte(strings.Repeat(" ", 65537)))
		return
	}
	now := time.Now().UTC()
	hash := sha512.Sum512([]byte("browser-test-access-token"))
	claims := map[string]any{"iss": issuer, "sub": "browser-subject", "aud": client, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "nonce": flow.nonce, "at_hash": base64.RawURLEncoding.EncodeToString(hash[:32]), "preferred_username": "alice", "name": "Alice", "groups": []string{"dns"}}
	if mode == "nonce" {
		claims["nonce"] = "wrong"
	}
	if mode == "at_hash" {
		claims["at_hash"] = "wrong"
	}
	if mode == "expired" {
		claims["iat"] = now.Add(-time.Minute).Unix()
		claims["exp"] = now.Add(-time.Second).Unix()
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "browser-test"))
	if err != nil {
		w.WriteHeader(500)
		return
	}
	raw, _ := json.Marshal(claims)
	signed, err := signer.Sign(raw)
	if err != nil {
		w.WriteHeader(500)
		return
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		w.WriteHeader(500)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id_token": token, "access_token": "browser-test-access-token", "token_type": "Bearer", "expires_in": 60, "refresh_token": "NEVER-RETAIN-REFRESH"}) //nolint:gosec // Deliberately synthetic tokens test nonretention, never real credentials.
}
