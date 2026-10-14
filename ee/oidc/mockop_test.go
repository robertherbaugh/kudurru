package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
)

// mockOP is a minimal in-process OpenID Provider for tests that cannot run
// dex: discovery and JWKS come from go-oidc's oidctest.Server; this file
// adds an authorization endpoint with a login form, a token endpoint that
// enforces client authentication, single-use codes, exact redirect_uri and
// PKCE S256, and RS256 ID tokens. It is not a general-purpose OP.
type mockOP struct {
	srv          *httptest.Server
	key          *rsa.PrivateKey
	clientID     string
	clientSecret string
	redirectURI  string

	mu      sync.Mutex
	users   map[string]mockUser // by login (email)
	pending map[string]mockAuthReq
	codes   map[string]mockAuthReq
	// tokenHook may rewrite ID token claims (audience tricks, etc.).
	tokenHook func(login string, claims map[string]any)
}

type mockUser struct {
	password string
	sub      string
	claims   map[string]any
}

type mockAuthReq struct {
	redirect, state, nonce, challenge string
	login                             string
}

const mockKeyID = "mock-key"

func newMockOP(t *testing.T, clientID, clientSecret, redirectURI string) *mockOP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	op := &mockOP{
		key: key, clientID: clientID, clientSecret: clientSecret, redirectURI: redirectURI,
		users: map[string]mockUser{}, pending: map[string]mockAuthReq{}, codes: map[string]mockAuthReq{},
	}
	disc := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: key.Public(), KeyID: mockKeyID, Algorithm: gooidc.RS256}}}
	mux := http.NewServeMux()
	mux.Handle("/", disc)
	mux.HandleFunc("GET /auth", op.authorize)
	mux.HandleFunc("POST /login", op.login)
	mux.HandleFunc("POST /token", op.token)
	op.srv = httptest.NewServer(mux)
	disc.SetIssuer(op.srv.URL)
	t.Cleanup(op.srv.Close)
	return op
}

func (op *mockOP) issuer() string { return op.srv.URL }

func (op *mockOP) addUser(login, password, sub string, claims map[string]any) {
	op.mu.Lock()
	defer op.mu.Unlock()
	op.users[login] = mockUser{password: password, sub: sub, claims: claims}
}

func (op *mockOP) setTokenHook(h func(login string, claims map[string]any)) {
	op.mu.Lock()
	op.tokenHook = h
	op.mu.Unlock()
}

func (op *mockOP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	switch {
	case q.Get("client_id") != op.clientID:
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	case q.Get("redirect_uri") != op.redirectURI:
		http.Error(w, "redirect_uri not registered", http.StatusBadRequest)
		return
	case q.Get("response_type") != "code":
		http.Error(w, "unsupported response_type", http.StatusBadRequest)
		return
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		http.Error(w, "PKCE S256 required", http.StatusBadRequest)
		return
	}
	id := rand.Text()
	op.mu.Lock()
	op.pending[id] = mockAuthReq{redirect: q.Get("redirect_uri"), state: q.Get("state"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	op.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<html><body><form method="post" action="%s">
<input name="login"><input name="password" type="password"><button>Log in</button></form></body></html>`,
		html.EscapeString("/login?req="+url.QueryEscape(id)))
}

func (op *mockOP) login(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("req")
	op.mu.Lock()
	defer op.mu.Unlock()
	req, ok := op.pending[id]
	if !ok {
		http.Error(w, "unknown request", http.StatusBadRequest)
		return
	}
	u, ok := op.users[r.PostFormValue("login")]
	if !ok || subtle.ConstantTimeCompare([]byte(u.password), []byte(r.PostFormValue("password"))) != 1 {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	delete(op.pending, id)
	req.login = r.PostFormValue("login")
	code := rand.Text()
	op.codes[code] = req
	dst, _ := url.Parse(req.redirect)
	v := dst.Query()
	v.Set("code", code)
	v.Set("state", req.state)
	dst.RawQuery = v.Encode()
	http.Redirect(w, r, dst.String(), http.StatusSeeOther)
}

func (op *mockOP) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if id != op.clientID || subtle.ConstantTimeCompare([]byte(secret), []byte(op.clientSecret)) != 1 {
		tokenError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	if r.PostFormValue("grant_type") != "authorization_code" {
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	op.mu.Lock()
	code := r.PostFormValue("code")
	req, ok := op.codes[code]
	delete(op.codes, code) // single use
	user := op.users[req.login]
	hook := op.tokenHook
	op.mu.Unlock()
	if !ok || r.PostFormValue("redirect_uri") != req.redirect {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != req.challenge {
		tokenError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	access := rand.Text()
	atSum := sha256.Sum256([]byte(access))
	now := time.Now()
	claims := map[string]any{
		"iss": op.issuer(), "sub": user.sub, "aud": op.clientID,
		"iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"nonce": req.nonce, "at_hash": base64.RawURLEncoding.EncodeToString(atSum[:16]),
	}
	for k, v := range user.claims {
		claims[k] = v
	}
	if hook != nil {
		hook(req.login, claims)
	}
	payload, _ := json.Marshal(claims)
	idToken := oidctest.SignIDToken(op.key, mockKeyID, gooidc.RS256, string(payload))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 600, "id_token": idToken,
	})
}

func tokenError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
