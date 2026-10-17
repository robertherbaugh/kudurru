package oidc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
)

// The dex end-to-end test runs a real dex (Apache-2.0, run as a separate
// process, never linked) with the memory store and static password users,
// and drives the full BFF login through its HTML login form.
//
// It needs a dex binary: KUDURRU_DEX_BIN=/path/to/dex, or `dex` on PATH, or
// `make test-auth-e2e`, which builds the pinned release below from source
// with CGO_ENABLED=0. Without one the test is skipped.

// dexVersion is the release `make dex` builds; keep in sync with Makefile.
const dexVersion = "v2.46.0"

const (
	dexClientID     = "kudurru"
	dexClientSecret = "kudurru-e2e-client-secret" //nolint:gosec // G101: throwaway secret of the test dex
)

func dexBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("KUDURRU_DEX_BIN"); p != "" {
		if _, err := os.Stat(p); err != nil { //nolint:gosec // G703: the developer names the dex binary
			t.Fatalf("KUDURRU_DEX_BIN=%s: %v", p, err)
		}
		return p
	}
	if p, err := exec.LookPath("dex"); err == nil {
		return p
	}
	t.Skip("dex not found: set KUDURRU_DEX_BIN or run `make test-auth-e2e` (builds dex " + dexVersion + ")")
	return ""
}

// lockedBuffer collects dex's output from its writer goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// dexConfig renders the dex configuration used by the test. It is JSON
// (a YAML subset dex accepts) so it can be generated without a YAML
// library; dev-docs/spikes/auth-oidc.md shows it as YAML.
func dexConfig(t *testing.T, issuer, listen, redirectURL string) []byte {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(e2ePassword), 10) // dex's minimum cost
	if err != nil {
		t.Fatal(err)
	}
	user := func(email, username, uid string, groups []string) map[string]any {
		return map[string]any{
			"email": email, "hash": string(hash), "username": username, "preferredUsername": username,
			"name": username, "userID": uid, "emailVerified": true, "groups": groups,
		}
	}
	cfg := map[string]any{
		"issuer":  issuer,
		"storage": map[string]any{"type": "memory"},
		"web":     map[string]any{"http": listen},
		"logger":  map[string]any{"level": "info", "format": "text"},
		"oauth2": map[string]any{
			"skipApprovalScreen": true,
			"responseTypes":      []string{"code"},
			"pkce":               map[string]any{"enforce": true, "codeChallengeMethodsSupported": []string{"S256"}},
		},
		"staticClients": []any{map[string]any{
			"id": dexClientID, "secret": dexClientSecret, "name": "kudurru", "redirectURIs": []string{redirectURL},
		}},
		"enablePasswordDB": true,
		"staticPasswords": []any{
			user(userAdmin, "admin", "6f1b4c2e-0000-4000-8000-000000000001", []string{"kudurru-admins", "staff"}),
			user(userNobody, "nobody", "6f1b4c2e-0000-4000-8000-000000000002", []string{"staff"}),
			user(userMany, "many", "6f1b4c2e-0000-4000-8000-000000000003", manyUserGroups()),
		},
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// startDex runs dex on a free loopback port and waits for discovery.
func startDex(t *testing.T, bin, redirectURL string) testIDP {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := l.Addr().String()
	_ = l.Close()
	issuer := "http://" + listen + "/dex"

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dex.json")
	if err := os.WriteFile(cfgPath, dexConfig(t, issuer, listen, redirectURL), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	cmd := exec.Command(bin, "serve", cfgPath) //nolint:gosec // G204: running the configured dex binary is the point
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dex: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("dex output:\n%s", logs.String())
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(issuer + "/.well-known/openid-configuration")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dex did not become ready: %v\n%s", err, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return testIDP{id: "dex", issuer: issuer, clientID: dexClientID, clientSecret: dexClientSecret}
}

func TestDexEndToEnd(t *testing.T) {
	bin := dexBinary(t)
	h := newHarness(t)
	start := time.Now()
	idp := startDex(t, bin, h.redirectURL("dex"))
	t.Logf("dex %s ready in %v at %s", bin, time.Since(start).Round(time.Millisecond), idp.issuer)
	h.configure(idp, keyA, nil)

	runSuite(t, h, idp)

	// Not part of the shared suite: look at what dex actually puts in the
	// ID token (claim shapes, audience, size), acting as a plain RP.
	t.Run("raw ID token shape", func(t *testing.T) {
		h.t = t
		h.clk.Reset()
		h.configure(idp, keyA, nil)
		ctx := context.Background()
		disc := struct {
			AuthURL  string `json:"authorization_endpoint"`
			TokenURL string `json:"token_endpoint"`
		}{}
		resp, err := http.Get(idp.issuer + "/.well-known/openid-configuration")
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(resp.Body).Decode(&disc)
		_ = resp.Body.Close()
		conf := oauth2.Config{
			ClientID: idp.clientID, ClientSecret: idp.clientSecret, RedirectURL: h.redirectURL("dex"),
			Endpoint: oauth2.Endpoint{AuthURL: disc.AuthURL, TokenURL: disc.TokenURL},
			Scopes:   DefaultScopes,
		}
		for _, login := range []string{userAdmin, userMany} {
			verifier := oauth2.GenerateVerifier()
			u := conf.AuthCodeURL("st", oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", "n"))
			authURL, _ := http.NewRequest(http.MethodGet, u, nil)
			cb := h.newBrowser().authenticateAtIDP(authURL.URL, login)
			tok, err := conf.Exchange(ctx, cb.Query().Get("code"), oauth2.VerifierOption(verifier))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := tok.Extra("id_token").(string)
			parts := strings.Split(raw, ".")
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			claims, err := DecodeClaims(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, isString := claims["aud"].(string); !isString {
				t.Errorf("dex aud is %T, expected a single string", claims["aud"])
			}
			groups, isArray := claims["groups"].([]any)
			if !isArray {
				t.Errorf("dex groups claim is %T", claims["groups"])
			}
			if login == userMany && len(groups) != manyGroups {
				t.Errorf("dex sent %d of %d groups", len(groups), manyGroups)
			}
			names := make([]string, 0, len(claims))
			for k := range claims {
				names = append(names, k)
			}
			sort.Strings(names)
			t.Logf("%s: id_token %d bytes, %d groups, claims %v, aud %v, refresh token issued: %v",
				login, len(raw), len(groups), names, claims["aud"], tok.RefreshToken != "")
			if login == userAdmin {
				redacted := make(map[string]any, len(claims))
				for k, v := range claims {
					redacted[k] = v
				}
				js, _ := json.Marshal(redacted)
				t.Logf("%s claims: %s", login, js)
			}
		}
	})

	t.Run("dex rejects a login without PKCE", func(t *testing.T) {
		// oauth2.pkce.enforce: true in the dex config; this guards against
		// a dex upgrade silently dropping the option.
		// dex reports it OAuth-style: a redirect back to the client with
		// error=invalid_request, after its connector-selection hop.
		h.t = t
		b := h.newBrowser()
		u := fmt.Sprintf("%s/auth?client_id=%s&redirect_uri=%s&response_type=code&scope=openid&state=s",
			idp.issuer, dexClientID, url.QueryEscape(h.redirectURL("dex")))
		for range 5 {
			resp, body := b.get(u)
			loc, err := resp.Location()
			if err != nil {
				t.Fatalf("dex answered %d without redirecting: %s", resp.StatusCode, body)
			}
			if strings.HasPrefix(loc.String(), h.redirectURL("dex")) {
				if got := loc.Query().Get("error"); got != "invalid_request" {
					t.Fatalf("dex accepted an authorization request without code_challenge: %s", loc)
				}
				t.Logf("dex without PKCE: %s (%s)", loc.Query().Get("error"), loc.Query().Get("error_description"))
				return
			}
			u = loc.String()
		}
		t.Fatal("dex never redirected back")
	})
}
