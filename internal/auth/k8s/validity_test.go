package k8s_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/auth/k8s"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/stack"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

const (
	reviewAccept int32 = iota
	reviewDeletedSA
	reviewDeletedPod
	reviewOutage
	reviewWrongUID
	reviewWrongAudience
	reviewWrongSubject
)

// projectedAPI is an authoritative Kubernetes review fixture with real signed
// JWTs/JWKS. It never logs requests, tokens or response bodies.
type projectedAPI struct {
	server         *httptest.Server
	key            *ecdsa.PrivateKey
	clock          atomic.Int64
	mode           atomic.Int32
	reviews        atomic.Int32
	keysRead       atomic.Int32
	expireOnKeys   atomic.Bool
	expireOnReview atomic.Bool
	expireOnSAR    atomic.Bool
	discoveryFail  atomic.Bool
	start          time.Time
	logs           bytes.Buffer
}

func newProjectedAPI(t *testing.T) *projectedAPI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("generate signing key")
	}
	f := &projectedAPI{key: key, start: time.Now().UTC().Truncate(time.Second)}
	f.set(f.start)
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *projectedAPI) now() time.Time    { return time.Unix(f.clock.Load(), 0).UTC() }
func (f *projectedAPI) set(now time.Time) { f.clock.Store(now.Unix()) }
func (f *projectedAPI) cfg() *rest.Config {
	return &rest.Config{Host: f.server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}}
}
func (f *projectedAPI) claims() map[string]any {
	return map[string]any{"iss": f.server.URL, "sub": "system:serviceaccount:default:reader", "aud": []string{k8s.DefaultAudience},
		"iat": f.start.Unix(), "exp": f.start.Add(k8s.MaxTokenLifetime).Unix(),
		"kubernetes.io": map[string]any{"namespace": "default", "serviceaccount": map[string]string{"name": "reader", "uid": "sa-original"}, "pod": map[string]string{"name": "job", "uid": "pod-original"}}}
}
func (f *projectedAPI) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal("encode claims")
	}
	return f.signBytes(t, raw)
}
func (f *projectedAPI) signBytes(t *testing.T, payload []byte) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: f.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "fixture"))
	if err != nil {
		t.Fatal("create signer")
	}
	jws, err := s.Sign(payload)
	if err != nil {
		t.Fatal("sign proof")
	}
	raw, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal("serialize proof")
	}
	return raw
}
func (f *projectedAPI) verifier(t *testing.T, review bool) *k8s.Authenticator {
	t.Helper()
	a, err := k8s.New(k8s.Options{Config: f.cfg(), Now: f.now, DisableJWKS: review, Logger: slog.New(slog.NewTextHandler(&f.logs, nil))})
	if err != nil {
		t.Fatal("create authenticator")
	}
	return a
}
func (f *projectedAPI) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		if f.discoveryFail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": f.server.URL})
	case "/openid/v1/jwks":
		f.keysRead.Add(1)
		if f.mode.Load() == reviewOutage {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if f.expireOnKeys.Load() {
			f.set(f.start.Add(k8s.MaxTokenLifetime))
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "fixture", Algorithm: "ES256", Use: "sig"}}})
	case "/apis/authentication.k8s.io/v1/tokenreviews":
		f.reviews.Add(1)
		var tr authenticationv1.TokenReview
		if json.NewDecoder(io.LimitReader(r.Body, auth.MaxBearerBytes*2)).Decode(&tr) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.mode.Load() == reviewOutage {
			// Hostile error text must not reach error causes or auth logs.
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonServiceUnavailable, Code: 503, Message: "private-review-detail " + tr.Spec.Token})
			return
		}
		if f.expireOnReview.Load() {
			f.set(f.start.Add(k8s.MaxTokenLifetime))
		}
		st := authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{k8s.DefaultAudience}, User: authenticationv1.UserInfo{
			Username: "system:serviceaccount:default:reader", UID: "sa-original", Groups: []string{"system:serviceaccounts", "system:serviceaccounts:default", "system:authenticated"}}}
		signed, err := jose.ParseSignedCompact(tr.Spec.Token, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			st.Authenticated = false
		} else if _, err = signed.Verify(&f.key.PublicKey); err != nil {
			st.Authenticated = false
		}
		switch f.mode.Load() {
		case reviewDeletedSA, reviewDeletedPod:
			st.Authenticated, st.Error = false, "private-review-detail "+tr.Spec.Token
		case reviewWrongUID:
			st.User.UID = "replacement-account"
		case reviewWrongAudience:
			st.Audiences = []string{"other-service"}
		case reviewWrongSubject:
			st.User.Username = "system:serviceaccount:default:another"
		}
		_ = json.NewEncoder(w).Encode(authenticationv1.TokenReview{TypeMeta: metav1.TypeMeta{APIVersion: "authentication.k8s.io/v1", Kind: "TokenReview"}, Status: st})
	case "/apis/authorization.k8s.io/v1/subjectaccessreviews":
		if f.expireOnSAR.Load() {
			f.set(f.start.Add(k8s.MaxTokenLifetime))
		}
		_ = json.NewEncoder(w).Encode(authorizationv1.SubjectAccessReview{TypeMeta: metav1.TypeMeta{APIVersion: "authorization.k8s.io/v1", Kind: "SubjectAccessReview"}, Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestProjectedLifetimeAdmission(t *testing.T) {
	for _, review := range []bool{false, true} {
		name := "jwks"
		if review {
			name = "review"
		}
		t.Run(name, func(t *testing.T) {
			f := newProjectedAPI(t)
			a := f.verifier(t, review)
			cases := []struct {
				name string
				edit func(map[string]any)
			}{
				{"missing-iat", func(c map[string]any) { delete(c, "iat") }},
				{"null-iat", func(c map[string]any) { c["iat"] = nil }},
				{"string-iat", func(c map[string]any) { c["iat"] = "0" }},
				{"fractional-iat", func(c map[string]any) { c["iat"] = float64(f.start.Unix()) + 0.5 }},
				{"exponent-iat", func(c map[string]any) { c["iat"] = json.Number("1e9") }},
				{"negative-iat", func(c map[string]any) { c["iat"] = -1 }},
				{"overflow-iat", func(c map[string]any) { c["iat"] = json.Number("99999999999999999999999") }},
				{"future-iat", func(c map[string]any) { c["iat"] = f.start.Add(time.Second).Unix() }},
				{"missing-exp", func(c map[string]any) { delete(c, "exp") }},
				{"null-exp", func(c map[string]any) { c["exp"] = nil }},
				{"expired", func(c map[string]any) { c["iat"] = f.start.Add(-time.Minute).Unix(); c["exp"] = f.start.Unix() }},
				{"reversed", func(c map[string]any) { c["exp"] = f.start.Add(-time.Second).Unix() }},
				{"empty-lifetime", func(c map[string]any) { c["exp"] = f.start.Unix() }},
				{"overlong", func(c map[string]any) { c["exp"] = f.start.Add(k8s.MaxTokenLifetime + time.Second).Unix() }},
				{"ordinary-hour", func(c map[string]any) { c["exp"] = f.start.Add(time.Hour).Unix() }},
				{"future-nbf", func(c map[string]any) { c["nbf"] = f.start.Add(time.Second).Unix() }},
				{"invalid-nbf", func(c map[string]any) { c["nbf"] = "invalid" }},
				{"nbf-at-exp", func(c map[string]any) { c["nbf"] = c["exp"] }},
				{"missing-sa", func(c map[string]any) { delete(c, "kubernetes.io") }},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					c := f.claims()
					tc.edit(c)
					if _, err := a.VerifyJWT(t.Context(), f.sign(t, c)); err == nil {
						t.Fatal("invalid projected credential accepted")
					}
				})
			}
			c, err := json.Marshal(f.claims())
			if err != nil {
				t.Fatal("encode fixture")
			}
			duplicate := append([]byte(`{"iat":0,`), c[1:]...)
			for _, raw := range []string{f.signBytes(t, duplicate), "legacy-opaque-token", strings.Repeat("x", auth.MaxBearerBytes+1)} {
				if _, err := a.VerifyJWT(t.Context(), raw); err == nil {
					t.Fatal("ambiguous, legacy or oversized proof accepted")
				}
			}
			if f.reviews.Load() != 0 || f.keysRead.Load() != 0 {
				t.Fatal("invalid admission input reached credential acquisition")
			}
			raw := f.sign(t, f.claims())
			p, err := a.VerifyJWT(t.Context(), raw)
			if err != nil || p.Kind != auth.KindKubernetesServiceAccount || p.UID != "sa-original" || !p.CredentialIssuedAt.Equal(f.start) {
				t.Fatal("bounded projected credential rejected")
			}
			parts := strings.Split(raw, ".")
			parts[1] = base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(string(c), "pod-original", "pod-tampered")))
			if _, err := a.VerifyJWT(t.Context(), strings.Join(parts, ".")); err == nil {
				t.Fatal("changed signed proof accepted")
			}
		})
	}
}

func TestProjectedDeletionCacheAndOutage(t *testing.T) {
	for _, mode := range []int32{reviewDeletedSA, reviewDeletedPod, reviewOutage} {
		t.Run(map[int32]string{reviewDeletedSA: "serviceaccount-deleted", reviewDeletedPod: "pod-deleted", reviewOutage: "api-outage"}[mode], func(t *testing.T) {
			f := newProjectedAPI(t)
			a := f.verifier(t, true)
			raw := f.sign(t, f.claims())
			p, err := a.VerifyJWT(t.Context(), raw)
			if err != nil {
				t.Fatal("initial authoritative review failed")
			}
			p.Groups[0] = "forged-group"
			f.mode.Store(mode)
			f.set(f.start.Add(k8s.MaxCacheTTL - time.Second))
			cached, err := a.VerifyJWT(t.Context(), raw)
			if err != nil || cached.Groups[0] == "forged-group" || f.reviews.Load() != 1 {
				t.Fatal("bounded cache changed or aliased")
			}
			f.set(f.start.Add(k8s.MaxCacheTTL))
			_, err = a.VerifyJWT(t.Context(), raw)
			if err == nil || f.reviews.Load() != 2 {
				t.Fatal("expired review cache retained authority")
			}
			if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "private-review-detail") || strings.Contains(f.logs.String(), raw) {
				t.Fatal("review refusal leaked private response/proof")
			}
			if _, err := a.VerifyJWT(t.Context(), raw); err == nil || f.reviews.Load() != 2 {
				t.Fatal("negative cache or fail-closed review changed")
			}
			f.set(f.start.Add(k8s.MaxCacheTTL + 10*time.Second))
			if _, err := a.VerifyJWT(t.Context(), raw); err == nil || f.reviews.Load() != 3 {
				t.Fatal("negative cache expiry failed")
			}
		})
	}
	for _, review := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline-deletion-bounded", true: "review-original-expiry"}[review], func(t *testing.T) {
			f := newProjectedAPI(t)
			a := f.verifier(t, review)
			raw := f.sign(t, f.claims())
			p, err := a.VerifyJWT(t.Context(), raw)
			if err != nil {
				t.Fatal("initial verification failed")
			}
			held := auth.WithPrincipal(t.Context(), p)
			if !review {
				f.mode.Store(reviewDeletedPod)
			}
			f.set(f.start.Add(k8s.MaxTokenLifetime - time.Second))
			if _, err := a.VerifyJWT(t.Context(), raw); err != nil {
				t.Fatal("valid bounded original proof rejected before expiry")
			}
			f.set(f.start.Add(k8s.MaxTokenLifetime))
			if _, err := a.VerifyJWT(t.Context(), raw); err == nil {
				t.Fatal("original proof expiry extended")
			}
			if _, ok := auth.PrincipalFrom(held); ok {
				t.Fatal("held principal escaped exclusive original expiry")
			}
			if !review && f.reviews.Load() != 0 {
				t.Fatal("offline verification used a live review fallback")
			}
		})
	}
}

func TestProjectedReviewBindingAndAcquisitionDeadline(t *testing.T) {
	for _, mode := range []int32{reviewWrongUID, reviewWrongAudience, reviewWrongSubject} {
		f := newProjectedAPI(t)
		f.mode.Store(mode)
		if _, err := f.verifier(t, true).VerifyJWT(t.Context(), f.sign(t, f.claims())); err == nil {
			t.Fatal("mismatched authoritative review accepted")
		}
	}
	for _, review := range []bool{false, true} {
		f := newProjectedAPI(t)
		f.expireOnKeys.Store(!review)
		f.expireOnReview.Store(review)
		if _, err := f.verifier(t, review).VerifyJWT(t.Context(), f.sign(t, f.claims())); err == nil {
			t.Fatal("credential expired during acquisition was accepted")
		}
	}
	f := newProjectedAPI(t)
	a := f.verifier(t, true)
	c := f.claims()
	c["iat"] = f.start.Add(-time.Minute).Unix()
	c["exp"] = f.start.Add(time.Minute).Unix()
	raw := f.sign(t, c)
	if _, err := a.VerifyJWT(t.Context(), raw); err != nil {
		t.Fatal("initial short proof rejected")
	}
	f.set(f.start.Add(-time.Second))
	if _, err := a.VerifyJWT(t.Context(), raw); err == nil {
		t.Fatal("clock rollback reused cached authority")
	}
	f.set(f.start.Add(time.Minute))
	if _, err := a.VerifyJWT(t.Context(), raw); err == nil {
		t.Fatal("cache outlived original short expiry")
	}
}

func TestProjectedActualStackHeldAndFinalExpiry(t *testing.T) {
	for _, point := range []string{"normal", "held", "delegated-final", "review-normal", "review-held", "review-final"} {
		t.Run(point, func(t *testing.T) {
			f := newProjectedAPI(t)
			f.discoveryFail.Store(strings.HasPrefix(point, "review-"))
			cfg, err := config.Load("", nil)
			if err != nil {
				t.Fatal("load config")
			}
			s, err := stack.New(t.Context(), stack.Options{Config: cfg, Store: authtest.Memory(t), Keys: keys.Keys{Current: make([]byte, 32)}, Kube: f.cfg(), Now: f.now,
				DisableBootstrap: true, Getenv: func(string) string { return "" }, Out: io.Discard, Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				t.Fatal("mount identity stack")
			}
			var applied atomic.Int32
			next := auth.Require(s.Authorizer(), auth.ClusterAttrs("zones", "read"), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { applied.Add(1) }))
			mounted := s.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if point == "held" || point == "review-held" {
					f.set(f.start.Add(k8s.MaxTokenLifetime))
				}
				if point == "delegated-final" || point == "review-final" {
					f.expireOnSAR.Store(true)
				}
				next.ServeHTTP(w, r)
			}))
			r := httptest.NewRequest(http.MethodGet, "/api/v1/zones", nil)
			r.Header.Set("Authorization", "Bearer "+f.sign(t, f.claims()))
			w := httptest.NewRecorder()
			mounted.ServeHTTP(w, r) // one request, never replayed
			want := http.StatusUnauthorized
			if point == "normal" || point == "review-normal" {
				want = http.StatusOK
			}
			if w.Code != want || (point == "normal" || point == "review-normal") != (applied.Load() == 1) {
				t.Fatalf("mounted request status=%d expected=%d handlerCalls=%d", w.Code, want, applied.Load())
			}
		})
	}
}

// Compile-time check keeps the fixture on the mounted production interface.
var _ auth.JWTVerifier = (*k8s.Authenticator)(nil)

func TestProjectedActualRESTFinalExpiry(t *testing.T) {
	for _, point := range []string{"normal", "held", "delegated-final", "review-normal", "review-held", "review-final"} {
		t.Run(point, func(t *testing.T) {
			f := newProjectedAPI(t)
			f.discoveryFail.Store(strings.HasPrefix(point, "review-"))
			cfg, err := config.Load("", nil)
			if err != nil {
				t.Fatal("load config")
			}
			st := authtest.Memory(t)
			s, err := stack.New(t.Context(), stack.Options{Config: cfg, Store: st, Keys: keys.Keys{Current: make([]byte, 32)}, Kube: f.cfg(), Now: f.now,
				DisableBootstrap: true, Getenv: func(string) string { return "" }, Out: io.Discard, Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				t.Fatal("mount identity stack")
			}
			rc := cache.New(cache.Options{MaxEntries: 1000})
			message := new(dns.Msg)
			message.SetQuestion("retained.example.", dns.TypeA)
			wire, err := message.Pack()
			if err != nil {
				t.Fatal("pack cache fixture")
			}
			rc.Set(cache.MakeKey("retained.example.", dns.TypeA, dns.ClassINET, false, false, netip.Prefix{}), wire, cache.Positive, time.Hour)
			authenticate := func(next http.Handler) http.Handler {
				return s.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if point == "held" || point == "review-held" {
						f.set(f.start.Add(k8s.MaxTokenLifetime))
					}
					if point == "delegated-final" || point == "review-final" {
						f.expireOnSAR.Store(true)
					}
					next.ServeHTTP(w, r)
				}))
			}
			server, err := api.New(api.Options{Store: st, Validator: authtest.Validator(t), Config: cfg, Now: f.now, Authenticate: authenticate, Authorizer: s.Authorizer(), ResponseCache: rc, Logger: slog.New(slog.DiscardHandler)})
			if err != nil {
				t.Fatal("mount REST API")
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/cache/flush", nil)
			r.Header.Set("Authorization", "Bearer "+f.sign(t, f.claims()))
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r) // exactly one actual challenged request
			want, remaining := http.StatusUnauthorized, 1
			if point == "normal" || point == "review-normal" {
				want, remaining = http.StatusOK, 0
			}
			if w.Code != want || rc.Len() != remaining {
				t.Fatalf("REST status=%d expected=%d retainedEntries=%d expected=%d", w.Code, want, rc.Len(), remaining)
			}
		})
	}
}

func TestProjectedOfflineOutageAndFallback(t *testing.T) {
	f := newProjectedAPI(t)
	a := f.verifier(t, false)
	raw := f.sign(t, f.claims())
	if _, err := a.VerifyJWT(t.Context(), raw); err != nil {
		t.Fatal("initial offline verification failed")
	}
	f.mode.Store(reviewOutage)
	f.set(f.start.Add(k8s.MaxCacheTTL))
	if _, err := a.VerifyJWT(t.Context(), raw); err != nil {
		t.Fatal("known offline key lost bounded authority during outage")
	}
	if f.keysRead.Load() != 1 || f.reviews.Load() != 0 {
		t.Fatal("known offline proof unexpectedly fetched or reviewed")
	}
	if _, err := f.verifier(t, false).VerifyJWT(t.Context(), raw); err == nil {
		t.Fatal("unavailable verification keys granted authority")
	}
	f.set(f.start.Add(k8s.MaxTokenLifetime))
	if _, err := a.VerifyJWT(t.Context(), raw); err == nil {
		t.Fatal("outage extended original expiry")
	}

	g := newProjectedAPI(t)
	g.discoveryFail.Store(true)
	b := g.verifier(t, false)
	raw = g.sign(t, g.claims())
	if _, err := b.Fallback().VerifyJWT(t.Context(), raw); err != nil {
		t.Fatal("authoritative fallback failed")
	}
	g.mode.Store(reviewDeletedSA)
	g.set(g.start.Add(k8s.MaxCacheTTL))
	if _, err := b.Fallback().VerifyJWT(t.Context(), raw); err == nil {
		t.Fatal("fallback cache retained deleted account authority")
	}
}

func TestProjectedConcurrentCacheOwnsDetachedEvidence(t *testing.T) {
	f := newProjectedAPI(t)
	a := f.verifier(t, false)
	raw := f.sign(t, f.claims())
	var workers sync.WaitGroup
	failures := make(chan bool, 8)
	for range 8 {
		workers.Go(func() {
			for range 10 {
				p, err := a.VerifyJWT(t.Context(), raw)
				if err != nil || p.Groups[0] != "system:serviceaccounts" {
					failures <- true
					return
				}
				p.Groups[0] = "caller-mutated"
			}
		})
	}
	workers.Wait()
	close(failures)
	if len(failures) != 0 {
		t.Fatal("concurrent verification shared mutable cache evidence")
	}
}

func TestProjectedCachedVerificationRechecksOriginalExpiry(t *testing.T) {
	f := newProjectedAPI(t)
	var armed atomic.Bool
	var calls atomic.Int32
	a, err := k8s.New(k8s.Options{Config: f.cfg(), Now: func() time.Time {
		if armed.Load() && calls.Add(1) > 1 {
			return f.start.Add(k8s.MaxTokenLifetime)
		}
		return f.start
	}})
	if err != nil {
		t.Fatal("create authenticator")
	}
	raw := f.sign(t, f.claims())
	if _, err := a.VerifyJWT(t.Context(), raw); err != nil {
		t.Fatal("initial proof rejected")
	}
	armed.Store(true)
	if _, err := a.VerifyJWT(t.Context(), raw); err == nil {
		t.Fatal("cache hit skipped original final expiry check")
	}
}

// reviewDeadlineLimiter exposes the context at the rate-limiter boundary,
// which client-go reaches before its REST request timeout is installed.
type reviewDeadlineLimiter struct {
	started chan time.Time
}

func (*reviewDeadlineLimiter) TryAccept() bool { return false }
func (*reviewDeadlineLimiter) Accept()         {}
func (*reviewDeadlineLimiter) Stop()           {}
func (*reviewDeadlineLimiter) QPS() float32    { return 1 }
func (l *reviewDeadlineLimiter) Wait(ctx context.Context) error {
	deadline, _ := ctx.Deadline()
	l.started <- deadline
	<-ctx.Done()
	return ctx.Err()
}

func TestProjectedTokenReviewBoundsRateLimitAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		clientTimeout, callerTimeout time.Duration
	}{
		{name: "default"},
		{name: "clamped-client", clientTimeout: time.Hour},
		{name: "shorter-client", clientTimeout: 50 * time.Millisecond},
		{name: "shorter-caller", callerTimeout: 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientTimeout := tc.clientTimeout
			f := newProjectedAPI(t)
			cfg := f.cfg()
			cfg.Timeout = clientTimeout
			limiter := &reviewDeadlineLimiter{started: make(chan time.Time, 1)}
			cfg.RateLimiter = limiter
			a, err := k8s.New(k8s.Options{Config: cfg, DisableJWKS: true, Now: f.now})
			if err != nil {
				t.Fatal("create bounded authenticator")
			}
			ctx, cancel := context.WithCancel(t.Context())
			if tc.callerTimeout > 0 {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), tc.callerTimeout)
			}
			defer cancel()
			raw := f.sign(t, f.claims())
			done := make(chan error, 1)
			began := time.Now()
			go func() { _, err := a.VerifyJWT(ctx, raw); done <- err }()
			var deadline time.Time
			select {
			case deadline = <-limiter.started:
			case <-time.After(2 * time.Second):
				t.Fatal("review did not reach rate limiter")
			}
			bound := 10 * time.Second
			if clientTimeout > 0 && clientTimeout < bound {
				bound = clientTimeout
			}
			if tc.callerTimeout > 0 && tc.callerTimeout < bound {
				bound = tc.callerTimeout
			}
			if deadline.IsZero() || deadline.After(time.Now().Add(bound)) || deadline.Before(began) {
				t.Error("review throttle did not receive the bounded original context")
			}
			// The shorter configured deadline must expire by itself. The other
			// cases prove caller cancellation beats the ten-second upper bound.
			if clientTimeout != 50*time.Millisecond && tc.callerTimeout == 0 {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("canceled/throttled review authenticated")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("review ignored cancellation/deadline while throttled")
			}
			if f.reviews.Load() != 0 {
				t.Fatal("throttled review unexpectedly reached API")
			}
		})
	}
}

func TestProjectedTokenReviewHTTPCancellation(t *testing.T) {
	for _, callerCancel := range []bool{false, true} {
		t.Run(fmt.Sprint(callerCancel), func(t *testing.T) {
			f := newProjectedAPI(t)
			started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-release:
				}
			}))
			t.Cleanup(func() { close(release); server.Close() })
			cfg := f.cfg()
			cfg.Host, cfg.Timeout = server.URL, 2*time.Second
			a, err := k8s.New(k8s.Options{Config: cfg, DisableJWKS: true, Now: f.now})
			if err != nil {
				t.Fatal("create authenticator")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			raw := f.sign(t, f.claims())
			done := make(chan error, 1)
			go func() { _, err := a.VerifyJWT(ctx, raw); done <- err }()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("review did not reach HTTP handler")
			}
			if callerCancel {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("unfinished review authenticated")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP review ignored original client/caller bound")
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("review request context was not canceled")
			}
		})
	}
}
