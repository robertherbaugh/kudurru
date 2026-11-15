package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	jose "github.com/go-jose/go-jose/v4"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

func bearerProvider(t *testing.T) (*Provider, *mockOP) {
	t.Helper()
	op := newMockOP(t, "kudurru", "secret", "http://127.0.0.1:1/api/v1/auth/oidc/corp/callback")
	key := make([]byte, 32)
	sealed, err := session.NewSealed(session.Options{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProvider(ProviderConfig{
		ID: "corp", Issuer: op.issuer(), ClientID: "kudurru", ClientSecret: "secret", CLIClientID: "kudurru-cli",
		RedirectURL:   "http://127.0.0.1:1/api/v1/auth/oidc/corp/callback",
		ClaimMappings: ClaimMappings{Username: PrefixedClaim{Prefix: "corp:"}, Groups: PrefixedClaim{Prefix: "corp:"}},
	}, Options{Key: key, Sessions: sealed, Resolver: BindingResolver{Bindings: func() []rbac.Binding { return nil }}})
	if err != nil {
		t.Fatal(err)
	}
	return p, op
}

func (op *mockOP) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return oidctest.SignIDToken(op.key, mockKeyID, gooidc.RS256, string(payload))
}

// signTyp signs claims with a typ header (access tokens use at+jwt).
func (op *mockOP) signTyp(t *testing.T, typ string, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: op.key},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)).WithHeader("kid", mockKeyID))
	if err != nil {
		t.Fatal(err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBearerIDTokenAudienceRules(t *testing.T) {
	p, op := bearerProvider(t)
	ctx := context.Background()
	now := time.Now()
	base := func(over map[string]any) map[string]any {
		c := map[string]any{
			"iss": op.issuer(), "sub": "u-1", "aud": "kudurru-cli", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			"preferred_username": "alice", "groups": []string{"dns-admins"},
		}
		for k, v := range over {
			if v == nil {
				delete(c, k)
				continue
			}
			c[k] = v
		}
		return c
	}
	for _, tc := range []struct {
		name   string
		claims map[string]any
		ok     bool
	}{
		{"cli audience", base(nil), true},
		{"web client audience", base(map[string]any{"aud": "kudurru"}), true},
		{"several audiences with our azp", base(map[string]any{"aud": []string{"kudurru-cli", "other"}, "azp": "kudurru-cli"}), true},
		{"several audiences without azp", base(map[string]any{"aud": []string{"kudurru-cli", "other"}}), false},
		{"several audiences, foreign azp", base(map[string]any{"aud": []string{"kudurru-cli", "other"}, "azp": "other"}), false},
		{"another relying party", base(map[string]any{"aud": "grafana"}), false},
		{"single audience, foreign azp", base(map[string]any{"azp": "grafana"}), false},
		{"expired", base(map[string]any{"exp": now.Add(-time.Minute).Unix()}), false},
		{"wrong issuer", base(map[string]any{"iss": "https://evil.example"}), false},
		{"no subject", base(map[string]any{"sub": nil}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := op.sign(t, tc.claims)
			pr, err := p.VerifyJWT(ctx, raw)
			if tc.ok != (err == nil) {
				t.Fatalf("VerifyJWT = %+v, %v; want ok=%v", pr, err, tc.ok)
			}
			if err != nil {
				if ae := auth.AsError(err); ae.Status != http.StatusUnauthorized {
					t.Fatalf("status %d", ae.Status)
				}
				return
			}
			if pr.Name != "corp:alice" || len(pr.Groups) != 1 || pr.Groups[0] != "corp:dns-admins" || pr.AuthMethod != auth.MethodJWT || pr.Kind != auth.KindUser {
				t.Fatalf("principal %+v", pr)
			}
		})
	}
	if _, err := p.VerifyJWT(ctx, op.signTyp(t, "at+jwt", base(nil))); err == nil {
		t.Fatal("an access token (typ at+jwt) was accepted as an ID token")
	}
	if _, err := p.VerifyJWT(ctx, op.signTyp(t, "JWT", base(map[string]any{"sub": "u-typ"}))); err != nil {
		t.Fatalf("typ JWT: %v", err)
	}
	if !p.AcceptsIssuer(ctx, op.issuer()) || p.AcceptsIssuer(ctx, op.issuer()+"/") {
		t.Fatal("issuer routing must be exact")
	}
}

func TestBearerCacheAndChainRouting(t *testing.T) {
	p, op := bearerProvider(t)
	reg, err := NewRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	if reg.ByIssuer(op.issuer()) != p || reg.ByIssuer("https://other") != nil {
		t.Fatal("ByIssuer")
	}
	now := time.Now()
	raw := op.sign(t, map[string]any{"iss": op.issuer(), "sub": "u-2", "aud": "kudurru", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "email": "x@example.com"})
	step := auth.BearerJWT{Verifiers: []auth.JWTVerifier{p}}
	mw := auth.Middleware(auth.Options{Steps: []auth.Authenticator{step}})
	var got auth.Principal
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = auth.PrincipalFrom(r.Context()) }))
	for range 2 { // the second call is served from the cache
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/zones", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		h.ServeHTTP(&discard{}, req)
		if got.Name != "corp:u-2" { // unverified email: username falls back to sub
			t.Fatalf("principal %+v", got)
		}
	}
	if len(p.bearer.m) != 1 {
		t.Fatalf("cache holds %d entries", len(p.bearer.m))
	}
}

func TestFromIdentityProviderDefaults(t *testing.T) {
	empty := ""
	idp, err := ParseConfigEntry(map[string]any{
		"id": "corp", "issuer": "https://sso.example", "clientID": "kudurru", "clientSecretFile": "/nonexistent",
		"claimMappings": map[string]any{"groups": map[string]any{"claim": "roles", "prefix": ""}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if idp.Name != "corp" || idp.Spec.ClaimMappings.Groups.Prefix == nil || *idp.Spec.ClaimMappings.Groups.Prefix != empty {
		t.Fatalf("parsed %+v", idp.Spec)
	}
	if _, err := ParseConfigEntry(map[string]any{"id": "corp", "issuer": "https://sso.example", "clientID": "k", "clientSecretFiel": "x"}); err == nil {
		t.Fatal("a typo in an identity provider key must be an error")
	}
	secret := func(_ context.Context, ref publicv1.CredentialReference) (string, error) {
		return ref.SecretRef.Name + "/" + ref.SecretRef.Key, nil
	}
	obj := &internalv1.IdentityProvider{Spec: internalv1.IdentityProviderSpec{Issuer: "https://sso.example", ClientID: "k",
		ClientSecretRef: &publicv1.CredentialReference{SecretRef: &publicv1.CredentialSecretRef{Name: "oidc", Key: "secret"}}}}
	obj.Name = "sso"
	cfg, err := FromIdentityProvider(context.Background(), obj, secret)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaimMappings.Username.Prefix != "sso:" || cfg.ClaimMappings.Groups.Prefix != "sso:" || cfg.ClientSecret != "oidc/secret" {
		t.Fatalf("defaults %+v", cfg)
	}
	if _, err := FromIdentityProvider(context.Background(), obj, nil); err == nil {
		t.Fatal("clientSecretRef without the kube driver must fail")
	}
	if _, err := FromIdentityProvider(context.Background(), idp, nil); err == nil {
		t.Fatal("a missing clientSecretFile must fail")
	}
}

type discard struct{ h http.Header }

func (d *discard) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}
