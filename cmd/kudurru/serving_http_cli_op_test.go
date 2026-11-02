package main

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
)

func (p *servingBrowserOP) authorizeCLI(w http.ResponseWriter, r *http.Request, issuer string) {
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil || r.Method != "GET" || q.Get("client_id") != "kudurru-cli" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || len(q.Get("state")) != 43 || len(q.Get("nonce")) != 43 || len(q.Get("code_challenge")) != 43 {
		w.WriteHeader(400)
		return
	}
	cb, e := url.Parse(q.Get("redirect_uri"))
	if e != nil || (cb.Host != "127.0.0.1:8000" && cb.Host != "127.0.0.1:18000") || cb.Path != "/callback" {
		w.WriteHeader(400)
		return
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	code := base64.RawURLEncoding.EncodeToString(b[:])
	p.mu.Lock()
	p.flows[code] = servingBrowserOPFlow{issuer: issuer, client: "kudurru-cli", redirect: cb.String(), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	p.mu.Unlock()
	v := url.Values{"state": {q.Get("state")}, "code": {code}, "iss": {issuer}}
	cb.RawQuery = v.Encode()
	// #nosec G710 -- fixture accepts only the two exact loopback callback hosts/path above.
	http.Redirect(w, r, cb.String(), http.StatusFound)
}
func (p *servingBrowserOP) exchangeCLI(w http.ResponseWriter, r *http.Request, issuer string) {
	p.requests.Add(1)
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.Method != "POST" || r.ParseForm() != nil || len(r.PostForm) != 5 || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("client_id") != "kudurru-cli" {
		w.WriteHeader(401)
		return
	}
	challenge, e := browseroidc.S256(r.PostForm.Get("code_verifier"))
	if e != nil {
		w.WriteHeader(400)
		return
	}
	p.mu.Lock()
	flow, ok := p.flows[r.PostForm.Get("code")]
	if !ok || flow.used || flow.client != "kudurru-cli" || flow.issuer != issuer || flow.challenge != challenge || flow.redirect != r.PostForm.Get("redirect_uri") {
		p.mu.Unlock()
		w.WriteHeader(400)
		return
	}
	flow.used = true
	p.flows[r.PostForm.Get("code")] = flow
	mode := p.mode
	p.mu.Unlock()
	if mode == "upstream-error" {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error_description":"NEVER-EXPOSE-CLI-UPSTREAM"}`))
		return
	}
	now := time.Now()
	sum := sha512.Sum512([]byte("cli-test-access"))
	claims := map[string]any{"iss": issuer, "sub": "cli-subject", "aud": "kudurru-cli", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "nonce": flow.nonce, "at_hash": base64.RawURLEncoding.EncodeToString(sum[:32]), "preferred_username": "alice", "groups": []string{"dns"}}
	switch mode {
	case "nonce":
		claims["nonce"] = "wrong"
	case "at_hash":
		claims["at_hash"] = "wrong"
	case "audience":
		claims["aud"] = "browser"
	case "azp":
		claims["azp"] = "browser"
	case "expired":
		claims["exp"] = now.Add(-time.Second).Unix()
	case "multi":
		claims["aud"] = []string{"kudurru-cli", "other"}
	}
	signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "browser-test"))
	if e != nil {
		w.WriteHeader(500)
		return
	}
	b, _ := json.Marshal(claims)
	signed, e := signer.Sign(b)
	if e != nil {
		w.WriteHeader(500)
		return
	}
	token, e := signed.CompactSerialize()
	if e != nil {
		w.WriteHeader(500)
		return
	}
	p.mu.Lock()
	p.lastToken = token
	p.mu.Unlock()
	out := map[string]any{"access_token": "cli-test-access", "id_token": token, "refresh_token": "NEVER-RETAIN-CLI-REFRESH", "token_type": "Bearer"}
	if mode == "access-only" {
		delete(out, "id_token")
	}
	_ = json.NewEncoder(w).Encode(out)
}
