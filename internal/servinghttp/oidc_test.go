package servinghttp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type oidcFixture struct {
	*fixture
	key    ed25519.PrivateKey
	claims map[string]any
	raw    string
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	f := &oidcFixture{}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.key = private
	f.fixture = newFixturePrepared(t, func(base *fixture, schema servingauth.Schema) {
		const issuer = "https://corp.example/issuer"
		base.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
			for _, object := range []store.Object{
				&public.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "corp", CreationTimestamp: metav1.NewTime(base.now.Add(-time.Hour))}, Spec: public.IdentityProviderSpec{Issuer: issuer, ClientID: "browser", CLIClientID: "cli", ClientSecretRef: &public.CredentialReference{Env: "TEST_CORP_SECRET"}}},
				&public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "external-read", Namespace: "default"}, Spec: public.RoleBindingSpec{Role: "viewer", Subjects: []public.BindingSubject{{Kind: "Group", Name: "corp:ops"}}}},
			} {
				if _, err := tx.Apply(ctx, object, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
					return err
				}
			}
			return nil
		})
		image, err := base.store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		cut, err := servingauth.CaptureImage(t.Context(), image, schema)
		if err != nil {
			t.Fatal(err)
		}
		sources, err := servingauth.ProviderSources(t.Context(), cut, base.deps.ConfigProviders)
		if err != nil || len(sources) != 1 {
			t.Fatal("provider source", err)
		}
		jwks, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "http", Algorithm: "EdDSA", Use: "sig"}}})
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := oidccore.NewVerifier(issuer, []string{"EdDSA"}, jwks)
		if err != nil {
			t.Fatal(err)
		}
		source := sources[0]
		base.deps.License.Providers = true
		base.deps.ProviderResources = []servingauth.ProviderResource{{Ref: source.Ref, SpecIdentity: source.SpecIdentity, PolicyIdentity: source.PolicyIdentity, Availability: servingauth.ProviderReady, TrustIdentity: "http-fixture-trust", Verifier: verifier, VerifierIdentity: verifier.Identity()}}
		f.claims = map[string]any{"iss": issuer, "sub": "http-user", "aud": "cli", "iat": base.now.Add(-time.Minute).Unix(), "exp": base.now.Add(5 * time.Minute).Unix(), "preferred_username": "bob", "groups": []string{"ops"}, "unused": strings.Repeat("x", 1200)}
		f.raw = f.sign(t, f.claims)
	})
	if len(f.raw) <= 1024 {
		t.Fatal("JWT fixture must exercise larger than API-token header bound")
	}
	return f
}

func (f *oidcFixture) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.key}, &jose.SignerOptions{ExtraHeaders: map[jose.HeaderKey]any{"kid": "http", "typ": "JWT"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(data)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *oidcFixture) requestJWT(raw string) *http.Request {
	r := f.request(http.MethodPost) // Valid local cookie must never rescue a JWT.
	r.Header.Set("Authorization", "Bearer "+raw)
	return r
}

func TestHTTPJWTProofSelectionAndBounds(t *testing.T) {
	f := newOIDCFixture(t)
	for _, test := range []struct {
		name, raw string
		status    int
	}{
		{"valid-cli", f.raw, http.StatusOK},
		{"truncated", f.raw[:len(f.raw)-1], http.StatusUnauthorized},
		{"empty-segments", "a..c", http.StatusUnauthorized},
		{"unknown-shape", "not-a-jwt", http.StatusUnauthorized},
		{"oversized-jwt", "a." + strings.Repeat("x", oidccore.MaxTokenBytes) + ".b", http.StatusUnauthorized},
		{"oversized-api-token", "kdr_" + strings.Repeat("x", 1024), http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := httptest.NewRecorder()
			f.adapter.Middleware(auth.Require(f.adapter, func(*http.Request) rbac.Attributes { return readAttrs() }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p, ok := auth.PrincipalFrom(r.Context())
				if !ok || p.Provider != "corp" || p.Name != "corp:bob" || p.AuthMethod != auth.MethodJWT || p.External == nil {
					t.Error("JWT did not use captured provider authority")
				}
				proof, err := Credential(r.Context())
				if err != nil || proof.Kind != servingauth.OIDCJWT || proof.Value != test.raw {
					t.Error("original JWT proof was not retained")
				}
				if _, ok := auth.SessionFrom(r.Context()); ok {
					t.Error("JWT became a cookie session")
				}
				w.WriteHeader(http.StatusOK)
			}))).ServeHTTP(out, f.requestJWT(test.raw))
			if out.Code != test.status {
				t.Fatalf("status %d, want %d", out.Code, test.status)
			}
		})
	}
	for _, field := range []string{"iss", "aud", "exp"} {
		t.Run("signed-wrong-"+field, func(t *testing.T) {
			claims := map[string]any{}
			for name, value := range f.claims {
				claims[name] = value
			}
			claims[field] = "untrusted"
			if field == "exp" {
				claims[field] = f.now.Unix()
			}
			out := httptest.NewRecorder()
			f.adapter.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("invalid signed JWT reached handler") })).ServeHTTP(out, f.requestJWT(f.sign(t, claims)))
			if out.Code != http.StatusUnauthorized {
				t.Fatal("signed invalid claims fell back to cookie", out.Code)
			}
		})
	}
}

func TestHTTPHeldJWTRechecksCurrentGrantsAndActualExpiry(t *testing.T) {
	f := newOIDCFixture(t)
	var clock atomic.Int64
	clock.Store(f.now.UnixNano())
	a, err := New(Options{Publisher: f.publisher, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	entered, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	out := httptest.NewRecorder()
	go a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-resume
		decision, err := a.Authorize(r.Context(), rbac.Principal{BuiltinAdmin: true}, readAttrs())
		if err != nil || !decision.Allowed {
			done <- errors.New("held JWT mixed in current grants")
			return
		}
		proof, err := Credential(r.Context())
		if err != nil {
			done <- err
			return
		}
		err = f.store.Command(r.Context(), f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, _, err := servingauth.ReauthorizeCurrent(ctx, tx, f.deps, proof, readAttrs(), f.now)
			return err
		})
		if !errors.Is(err, servingauth.ErrDenied) {
			done <- errors.New("held JWT authorized removed current grants")
			return
		}
		clock.Store(f.now.Add(5 * time.Minute).UnixNano())
		if _, err = a.Authorize(r.Context(), rbac.Principal{BuiltinAdmin: true}, readAttrs()); err == nil {
			done <- errors.New("held JWT survived actual expiry")
			return
		}
		w.WriteHeader(http.StatusOK)
		done <- nil
	})).ServeHTTP(out, f.requestJWT(f.raw))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("JWT request never captured")
	}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: "RoleBinding", Namespace: "default", Name: "external-read"}, store.DeleteOptions{Manager: store.ManagerSystem})
	})
	f.waitCurrent(t)
	reject := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("new JWT request retained removed grants") })).ServeHTTP(reject, f.requestJWT(f.raw))
	if reject.Code != http.StatusUnauthorized {
		t.Fatal("new JWT was not denied", reject.Code)
	}
	close(resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held JWT request blocked")
	}
}
