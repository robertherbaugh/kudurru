package providerresources

import (
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

type browserFixture struct {
	*resourceFixture
	opMu            sync.Mutex
	response        []byte
	responseStatus  int
	compressed      bool
	redirect        bool
	entered, resume chan struct{}
	requests        atomic.Int64
	secret          atomic.Value
	badRequest      atomic.Bool
}

func newBrowserFixture(t *testing.T) *browserFixture {
	t.Helper()
	base := newResourceFixture(t)
	f := &browserFixture{resourceFixture: base, responseStatus: 200}
	f.secret.Store("test:/+ credential")
	acquire := base.server.Config.Handler
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			acquire.ServeHTTP(w, r)
			return
		}
		f.requests.Add(1)
		client, secret, basic := r.BasicAuth()
		client, e1 := url.QueryUnescape(client)
		secret, e2 := url.QueryUnescape(secret)
		if !basic || e1 != nil || e2 != nil || client != "browser:/+" || secret != "test:/+ credential" || r.Method != "POST" || r.ProtoMajor != 1 || r.ParseForm() != nil || len(r.PostForm) != 4 || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "one-shot-code" || r.PostForm.Get("redirect_uri") != "https://console.example/api/v1/auth/oidc/corp/callback" { //nolint:gosec // Synthetic credential checks Basic encoding, never a real secret.
			f.badRequest.Store(true)
		}
		if _, err := browseroidc.S256(r.PostForm.Get("code_verifier")); err != nil {
			f.badRequest.Store(true)
		}
		f.opMu.Lock()
		status, body, compressed, redirect := f.responseStatus, append([]byte{}, f.response...), f.compressed, f.redirect
		entered, resume := f.entered, f.resume
		f.opMu.Unlock()
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
		if redirect {
			http.Redirect(w, r, f.server.URL+"/token", http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if compressed {
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.WriteHeader(status)
		if compressed {
			z := gzip.NewWriter(w)
			_, _ = z.Write(body)
			_ = z.Close()
			return
		}
		_, _ = w.Write(body)
	}))
	server.TLS = base.server.TLS.Clone()
	server.StartTLS()
	t.Cleanup(server.Close)
	base.server = server
	base.discovery, _ = json.Marshal(map[string]any{"issuer": server.URL, "jwks_uri": server.URL + "/keys", "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "id_token_signing_alg_values_supported": []string{"EdDSA"}, "token_endpoint_auth_methods_supported": []string{"client_secret_basic"}, "code_challenge_methods_supported": []string{"S256"}})
	return f
}

func (f *browserFixture) source(t *testing.T, origin servingauth.ProviderOrigin) servingauth.ProviderDescriptor {
	t.Helper()
	s := f.resourceFixture.source(t, origin)
	s.Spec.ClientID = "browser:/+"
	s.Spec.RedirectURL = "https://console.example/api/v1/auth/oidc/corp/callback"
	p, err := oidccore.Compile(s.Spec, s.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.Spec, s.PolicyIdentity = p.Spec(), p.Identity()
	s.SpecIdentity = hash(struct {
		Profile   string
		Ref       servingauth.ProviderRef
		CreatedAt time.Time
		Spec      any
	}{"servingauth/provider/v1", s.Ref, s.CreatedAt, s.Spec})
	return s
}

func (f *browserFixture) host(t *testing.T, s servingauth.ProviderDescriptor) *HostCut {
	t.Helper()
	r, err := credentials.New(credentials.Options{Environment: []string{"TEST_CREDENTIAL"}, FingerprintKey: []byte(strings.Repeat("k", 32)), Getenv: func(string) (string, bool) { return f.secret.Load().(string), true }})
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHost(HostOptions{Identity: "browser-host", CredentialPolicy: "env-test/v1", EntitlementIdentity: "providers/v1", FingerprintKeyID: "key/v1", ProvidersAllowed: true, ObjectCredentials: r, //nolint:gosec // Synthetic policy and key identity labels.
		ConfigCredentials: map[string]*credentials.Resolver{"corp": r}, ConfigSources: []servingauth.ProviderSource{{Ref: servingauth.ProviderRef{ID: s.Ref.ID, UID: s.Ref.UID, Origin: servingauth.ProviderConfig}, CreatedAt: s.CreatedAt, Spec: s.Spec}},
		AllowedOrigins: []string{f.server.URL}, LocalOrigins: []string{f.server.URL}, Algorithms: []string{"EdDSA"}, TrustPEM: f.ca}) //nolint:gosec // Policy identifiers, not secrets.
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (f *browserFixture) ready(t *testing.T, origin servingauth.ProviderOrigin) (*Owner, Desired, *Lease, *Browser) {
	t.Helper()
	s := f.source(t, origin)
	d := Desired{RegistryDigest: hash("browser-registry"), Host: f.host(t, s), Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o := f.owner(t, d)
	d = admit(t, o, d, candidate(t, o))
	l, err := o.Acquire(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	id, err := servingauth.ProviderResourceIdentity(s, l.Resources()[0])
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Browser(s.Ref, id)
	if err != nil {
		t.Fatal(err)
	}
	return o, d, l, b
}

func (f *browserFixture) signedResponse(t *testing.T, nonce string) {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.private}, &jose.SignerOptions{ExtraHeaders: map[jose.HeaderKey]any{"kid": "test", "typ": "JWT"}})
	if err != nil {
		t.Fatal(err)
	}
	h := sha512.Sum512([]byte("test-access-token"))
	raw, _ := json.Marshal(map[string]any{"iss": f.server.URL, "sub": "subject", "aud": "browser:/+", "iat": f.now().Add(-time.Second).Unix(), "exp": f.now().Add(time.Minute).Unix(), "nonce": nonce, "at_hash": base64.RawURLEncoding.EncodeToString(h[:32]), "preferred_username": "alice"})
	v, err := s.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	token, err := v.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	f.opMu.Lock()
	f.response, _ = json.Marshal(map[string]any{"id_token": token, "access_token": "test-access-token", "token_type": "Bearer", "expires_in": 60, "refresh_token": "MUST-NOT-RETAIN"})
	f.opMu.Unlock()
}

func TestBrowserExactCredentialBoundOneShotAndOfflineProof(t *testing.T) {
	for _, origin := range []servingauth.ProviderOrigin{servingauth.ProviderObject, servingauth.ProviderConfig} {
		t.Run(string(origin), func(t *testing.T) {
			f := newBrowserFixture(t)
			_, _, l, b := f.ready(t, origin)
			state, _ := browseroidc.New("corp", "/", f.now())
			f.signedResponse(t, state.Nonce())
			derived, err := b.BindCurrent(func() bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			i := derived.Info()
			i.Scopes[0] = "edited"
			if b.Info().Scopes[0] == "edited" {
				t.Fatal("scope alias")
			}
			tokens, err := derived.Exchange(t.Context(), "one-shot-code", state.Verifier())
			if err != nil || f.badRequest.Load() {
				t.Fatal("exchange/profile rejected", err)
			}
			if _, err = b.Exchange(t.Context(), "one-shot-code", state.Verifier()); !errors.Is(err, ErrInvalid) || f.requests.Load() != 1 {
				t.Fatal("derived handle replayed", err)
			}
			proof := tokens.LoginProof(state.Nonce())
			before := f.requests.Load()
			verified, err := l.Resources()[0].Verifier.VerifyBrowser(t.Context(), proof.RawIDToken, proof.Browser, f.now())
			if err != nil {
				t.Fatal("signed nonce/hash proof", err)
			}
			policy, _ := oidccore.Compile(b.ref.Spec, b.ref.Ref.ID)
			if _, err = policy.Evaluate(t.Context(), verified, proof.Mode, f.now()); err != nil {
				t.Fatal(err)
			}
			proof.Browser.Nonce = "wrong"
			if _, err = l.Resources()[0].Verifier.VerifyBrowser(t.Context(), proof.RawIDToken, proof.Browser, f.now()); err == nil {
				t.Fatal("nonce mismatch")
			}
			proof = tokens.LoginProof(state.Nonce())
			proof.Browser.AccessToken = "wrong"
			if _, err = l.Resources()[0].Verifier.VerifyBrowser(t.Context(), proof.RawIDToken, proof.Browser, f.now()); err == nil {
				t.Fatal("at_hash mismatch")
			}
			if f.requests.Load() != before {
				t.Fatal("verification performed I/O")
			}
			for _, value := range []any{b, tokens, proof} {
				encoded, _ := json.Marshal(value)
				for _, text := range []string{fmt.Sprint(value), fmt.Sprintf("%#v", value), string(encoded)} {
					for _, secret := range []string{"MUST-NOT-RETAIN", "test-access-token", proof.RawIDToken, "test:/+ credential"} {
						if strings.Contains(text, secret) {
							t.Fatal("secret diagnostic disclosure")
						}
					}
				}
			}
		})
	}
}

func TestBrowserOriginalLeaseHostAndFingerprintBeforeSend(t *testing.T) {
	for _, mode := range []string{"fingerprint", "lease", "host", "expired", "regressed", "uid", "resource", "object-isolation"} {
		t.Run(mode, func(t *testing.T) {
			f := newBrowserFixture(t)
			_, _, l, b := f.ready(t, servingauth.ProviderObject)
			state, _ := browseroidc.New("corp", "/", f.now())
			f.signedResponse(t, state.Nonce())
			switch mode {
			case "fingerprint":
				f.secret.Store("rotated-not-admitted")
			case "lease":
				l.Release()
			case "host":
				b, _ = b.BindCurrent(func() bool { return true })
				b.current = func() bool { return false }
			case "expired":
				f.clock.Add(int64(time.Minute))
			case "regressed":
				f.clock.Add(-int64(time.Minute))
			case "uid":
				ref := b.Info().Provider
				ref.UID = strings.Repeat("b", 32)
				if _, err := l.Browser(ref, b.Info().ResourceIdentity); err == nil {
					t.Fatal("UID mismatch")
				}
				return
			case "resource":
				if _, err := l.Browser(b.Info().Provider, hash("other")); err == nil {
					t.Fatal("resource mismatch")
				}
				return
			case "object-isolation":
				// Captured object grant becomes unusable; same-ID config grant
				// is deliberately available but may never be used as fallback.
				deny, _ := credentials.New(credentials.Options{FingerprintKey: []byte(strings.Repeat("k", 32))})
				b.resolver = deny
			}
			if _, err := b.Exchange(t.Context(), "one-shot-code", state.Verifier()); err == nil {
				t.Fatal("stale proof sent")
			}
			if f.requests.Load() != 0 {
				t.Fatal("invalid capability performed token I/O")
			}
		})
	}
}

func TestBrowserInFlightCutReplacementReleaseHostDeadlineAndCancellation(t *testing.T) {
	for _, mode := range []string{"resource", "release", "host", "deadline", "regression", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newBrowserFixture(t)
			o, d, l, b := f.ready(t, servingauth.ProviderObject)
			state, _ := browseroidc.New("corp", "/", f.now())
			f.signedResponse(t, state.Nonce())
			var host atomic.Bool
			host.Store(true)
			b, _ = b.BindCurrent(host.Load)
			f.opMu.Lock()
			f.entered = make(chan struct{}, 1)
			f.resume = make(chan struct{})
			entered, resume := f.entered, f.resume
			f.opMu.Unlock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := b.Exchange(ctx, "one-shot-code", state.Verifier()); done <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("exchange not entered")
			}
			switch mode {
			case "resource":
				d.RegistryDigest = hash("replacement")
				if err := o.SetDesired(t.Context(), d); err != nil {
					t.Fatal(err)
				}
			case "release":
				l.Release()
			case "host":
				host.Store(false)
			case "deadline":
				f.clock.Add(int64(time.Minute))
			case "regression":
				f.clock.Add(-int64(time.Second))
			case "cancel":
				cancel()
			}
			close(resume)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("late response accepted")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("exchange did not finish")
			}
			if f.requests.Load() != 1 {
				t.Fatal("code replayed")
			}
		})
	}
}

func TestBrowserResponseCapsDuplicatesCompressionAndNoAuthRetry(t *testing.T) {
	for _, mode := range []string{"duplicate", "nested duplicate", "oversize", "gzip oversize", "redirect", "auth error", "bad type", "bad expiry", "missing token"} {
		t.Run(mode, func(t *testing.T) {
			f := newBrowserFixture(t)
			_, _, _, b := f.ready(t, servingauth.ProviderObject)
			state, _ := browseroidc.New("corp", "/", f.now())
			f.signedResponse(t, state.Nonce())
			f.opMu.Lock()
			switch mode {
			case "duplicate":
				f.response = []byte(`{"id_token":"a","id_token":"b","access_token":"x","token_type":"Bearer"}`)
			case "nested duplicate":
				f.response = []byte(`{"id_token":"a","access_token":"x","token_type":"Bearer","extension":{"a":1,"a":2}}`)
			case "oversize", "gzip oversize":
				f.response = []byte(strings.Repeat(" ", MaxTokenResponseBytes+1))
				f.compressed = mode == "gzip oversize"
			case "redirect":
				f.redirect = true
			case "auth error":
				f.responseStatus = 401
				f.response = []byte(`{"error_description":"PRIVATE-UPSTREAM-DIAGNOSTIC"}`)
			case "bad type":
				f.response = []byte(`{"id_token":123,"access_token":"x","token_type":"Bearer"}`)
			case "bad expiry":
				f.response = []byte(`{"id_token":"a","access_token":"x","token_type":"Bearer","expires_in":1.5}`)
			case "missing token":
				f.response = []byte(`{"id_token":"a","token_type":"Bearer"}`)
			}
			f.opMu.Unlock()
			if _, err := b.Exchange(t.Context(), "one-shot-code", state.Verifier()); err == nil || strings.Contains(fmt.Sprint(err), "PRIVATE-UPSTREAM-DIAGNOSTIC") {
				t.Fatal("invalid response/disclosure")
			}
			if f.requests.Load() != 1 {
				t.Fatal("one-shot request retried")
			}
		})
	}
}

func TestBrowserMetadataProfileUnavailableKeepsBearerCapability(t *testing.T) {
	f := newBrowserFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	h := f.host(t, s)
	for _, field := range []string{"token_endpoint_auth_methods_supported", "code_challenge_methods_supported", "grant_types_supported", "response_types_supported"} {
		var fields map[string]any
		_ = json.Unmarshal(f.discovery, &fields)
		fields[field] = []string{"unsupported"}
		raw, _ := json.Marshal(fields)
		m, err := parseMetadata(raw, s.Issuer, h.network)
		if err != nil || m.browser || m.jwksURL == "" || len(m.algorithms) == 0 {
			t.Fatal("unsupported browser poisoned bearer", err)
		}
		fields[field] = []string{"duplicate", "duplicate"}
		raw, _ = json.Marshal(fields)
		if _, err := parseMetadata(raw, s.Issuer, h.network); err == nil {
			t.Fatal("ambiguous browser metadata")
		}
	}
}

func TestBrowserGlobalOperationBudgetAndOriginalDeadline(t *testing.T) {
	f := newBrowserFixture(t)
	_, _, lease, first := f.ready(t, servingauth.ProviderObject)
	state, _ := browseroidc.New("corp", "/", f.now())
	f.signedResponse(t, state.Nonce())
	f.opMu.Lock()
	f.entered, f.resume = make(chan struct{}, 4), make(chan struct{})
	entered, resume := f.entered, f.resume
	f.opMu.Unlock()
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	done := make(chan error, 4)
	for range 4 {
		b, err := lease.Browser(first.Info().Provider, first.Info().ResourceIdentity)
		if err != nil {
			t.Fatal(err)
		}
		go func() { _, err := b.Exchange(t.Context(), "one-shot-code", state.Verifier()); done <- err }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("bounded exchange did not enter")
		}
	}
	extra, err := lease.Browser(first.Info().Provider, first.Info().ResourceIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extra.Exchange(t.Context(), "one-shot-code", state.Verifier()); !errors.Is(err, ErrUnavailable) || f.requests.Load() != 4 {
		t.Fatal("global budget/queue exceeded", err)
	}
	unblock()
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
