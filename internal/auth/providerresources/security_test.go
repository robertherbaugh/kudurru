package providerresources

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

func TestDiscoveryStrictConsumedFieldsBoundsAndNetworkPolicy(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	h := f.host(t, s, "test-only", true)
	for name, raw := range map[string][]byte{
		"duplicate":        []byte(`{"issuer":"x","issuer":"y","jwks_uri":"x"}`),
		"nested duplicate": []byte(`{"issuer":"x","extension":{"key":1,"key":2}}`),
		"type":             []byte(`{"issuer":true,"jwks_uri":"x"}`),
		"utf8":             {0xff}, "trailing": append(append([]byte{}, f.discovery...), []byte(` {}`)...),
		"large": []byte(strings.Repeat(" ", MaxDiscoveryBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMetadata(raw, s.Issuer, h.network); err == nil {
				t.Fatal("malformed discovery")
			}
		})
	}
	for name, change := range map[string]func(map[string]any){
		"foreign issuer":      func(m map[string]any) { m["issuer"] = "https://foreign.example" },
		"ungranted keys":      func(m map[string]any) { m["jwks_uri"] = "https://foreign.example/keys" },
		"userinfo":            func(m map[string]any) { m["jwks_uri"] = "https://user@127.0.0.1/keys" },
		"fragment":            func(m map[string]any) { m["token_endpoint"] = f.server.URL + "/token#fragment" },
		"metadata":            func(m map[string]any) { m["jwks_uri"] = "http://169.254.169.254/latest" },
		"symmetric":           func(m map[string]any) { m["id_token_signing_alg_values_supported"] = []string{"HS256", "none"} },
		"duplicate algorithm": func(m map[string]any) { m["id_token_signing_alg_values_supported"] = []string{"EdDSA", "EdDSA"} },
		"algorithm cap":       func(m map[string]any) { m["id_token_signing_alg_values_supported"] = make([]string, 65) },
	} {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			_ = json.Unmarshal(f.discovery, &m)
			change(m)
			raw, _ := json.Marshal(m)
			if _, err := parseMetadata(raw, s.Issuer, h.network); err == nil {
				t.Fatal("unsafe metadata")
			}
		})
	}
	for _, address := range []string{"169.254.169.254", "100.100.100.200", "fd00:ec2::254", "::ffff:169.254.169.254", "0.0.0.0", "::", "ff02::1"} {
		if !neverAllowed(netip.MustParseAddr(address)) {
			t.Fatal("metadata/address policy", address)
		}
	}
	n := *h.network
	n.origins = map[string]bool{"http://169.254.169.254": true}
	n.local = map[string]bool{"http://169.254.169.254": true}
	if _, err := n.endpoint("http://169.254.169.254/latest"); err == nil {
		t.Fatal("local grant admitted metadata")
	}
	noLocal := *h.network
	noLocal.local = map[string]bool{}
	if _, err := noLocal.endpoint(f.server.URL); err == nil {
		t.Fatal("loopback without grant")
	}
	// Even explicitly granted non-loopback HTTP cannot dial outside loopback.
	n.origins = map[string]bool{"http://192.0.2.1": true}
	n.local = map[string]bool{"http://192.0.2.1": true}
	client, closeClient := n.client(50 * time.Millisecond)
	defer closeClient()
	if _, err := client.Get("http://192.0.2.1"); err == nil {
		t.Fatal("non-loopback HTTP")
	}
}
func TestFetchConditionalFreshnessNoStoreAndCaps(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	h := f.host(t, s, "test-only", true)
	o, err := New(Options{Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	client, closeClient := h.network.client(time.Second)
	defer closeClient()
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	url := f.server.URL + "/.well-known/openid-configuration"
	cache, _, err := o.fetch(t.Context(), client, url, fetchCache{}, MaxDiscoveryBytes)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.conditional = true
	f.mu.Unlock()
	f.clock.Add(int64(time.Second))
	renewed, _, err := o.fetch(t.Context(), client, url, cache, MaxDiscoveryBytes)
	if err != nil || !renewed.until.After(cache.until) {
		t.Fatal("exact 304 renewal", err)
	}
	f.mu.Lock()
	f.status = 304
	f.mu.Unlock()
	if _, _, err = o.fetch(t.Context(), client, url, fetchCache{url: url, etag: cache.etag}, MaxDiscoveryBytes); !errors.Is(err, ErrInvalid) {
		t.Fatal("304 without retained body", err)
	}
	f.mu.Lock()
	f.conditional = false
	f.status = 200
	f.cache = "max-age=0"
	f.mu.Unlock()
	zero, _, err := o.fetch(t.Context(), client, url, fetchCache{}, MaxDiscoveryBytes)
	if err != nil || zero.until.After(f.now()) {
		t.Fatal("max-age zero extended", err)
	}
	for _, header := range []http.Header{{"Cache-Control": {"max-age=10"}, "Age": {"20"}}, {"Cache-Control": {"max-age=-1"}}, {"Cache-Control": {"max-age=bogus"}}, {"Cache-Control": {"NO-CACHE, max-age=0"}}} {
		if o.freshness(header) != 0 {
			t.Fatal("bad freshness extended", header)
		}
	}
	f.mu.Lock()
	f.cache = "no-store"
	f.mu.Unlock()
	private, _, err := o.fetch(t.Context(), client, url, fetchCache{}, MaxDiscoveryBytes)
	if err != nil || private.retain {
		t.Fatal("no-store retained", err)
	}
	f.mu.Lock()
	f.discovery = []byte(strings.Repeat(" ", MaxDiscoveryBytes+1))
	f.mu.Unlock()
	if _, _, err = o.fetch(t.Context(), client, url, fetchCache{}, MaxDiscoveryBytes); !errors.Is(err, ErrBounds) {
		t.Fatal("body cap", err)
	}
}
func TestTransientRefreshNeverExtendsDeadlineAndSecurityMismatchRetires(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	d := Desired{RegistryDigest: hash("registry"), Host: f.host(t, s, "test-only", true), Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o, err := New(Options{Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	a := o.acquire(t.Context(), d, s, entry{})
	if a.reason != "" {
		t.Fatal("initial acquisition", a.reason)
	}
	d.Authorities = []providerregistry.AuthorityRecord{{FormatVersion: 1, Provider: s.Ref, SpecIdentity: s.SpecIdentity, Revision: 1, Active: true, Binding: a.binding}}
	res := servingauth.ProviderResource{Ref: s.Ref, SpecIdentity: s.SpecIdentity, PolicyIdentity: s.PolicyIdentity, Availability: servingauth.ProviderReady, Verifier: a.verifier, VerifierIdentity: a.verifier.Identity(), TrustIdentity: hash(a.binding)}
	prior := entry{s, res, a.metadata, a.until}
	f.mu.Lock()
	f.status = 503
	f.mu.Unlock()
	failed := o.acquire(t.Context(), d, s, prior)
	if !failed.transient || failed.reason != "discovery" || !failed.until.IsZero() {
		t.Fatal("failure renewed authority")
	}
	d.Host = f.host(t, s, "changed", true)
	failed = o.acquire(t.Context(), d, s, prior)
	if failed.transient {
		t.Fatal("credential replacement retained prior")
	}
	// Another provider receipt with identical credential fields never counts.
	d.Host = f.host(t, s, "test-only", true)
	d.Authorities[0].Provider.UID = strings.Repeat("b", 32)
	failed = o.acquire(t.Context(), d, s, prior)
	if failed.transient {
		t.Fatal("other UID granted transient fallback")
	}
	f.mu.Lock()
	f.status = 200
	f.cache = "no-store"
	f.mu.Unlock()
	a = o.acquire(t.Context(), d, s, entry{})
	if a.reason != "" || a.metadata.discovery.body != nil || a.metadata.keys.body != nil {
		t.Fatal("no-store private document cache")
	}
}
func TestExpiredLeaseDeniesBeforeWorkerAndPublishesUnavailable(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	d := Desired{RegistryDigest: hash("registry"), Host: f.host(t, s, "test-only", true), Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o := f.owner(t, d)
	d = admit(t, o, d, candidate(t, o))
	l, err := o.Acquire(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	f.mu.Lock()
	f.status = 503
	f.mu.Unlock()
	f.clock.Add(int64(time.Minute))
	if l.Current() {
		t.Fatal("expired cut current before scheduler")
	}
	if err = o.WithCurrent(t.Context(), func(p *Pinned) error {
		if p.Resources()[0].Availability == servingauth.ProviderReady {
			t.Fatal("expired ready command admitted")
		}
		return nil
	}); err == nil && o.current.Load().entries[0].resource.Availability == servingauth.ProviderReady {
		t.Fatal("expired command admitted")
	}
	await(t, func() bool {
		next, err := o.Acquire(t.Context(), d)
		if err != nil {
			return false
		}
		defer next.Release()
		return next.Resources()[0].Availability == servingauth.ProviderUnavailable
	})
}
func TestInputCapsRedactionAndLicenseDenialWithoutAcquisition(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	h := f.host(t, s, "test-only", false)
	d := Desired{RegistryDigest: hash("registry"), Host: h, Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o := f.owner(t, d)
	time.Sleep(80 * time.Millisecond)
	if f.calls.Load() != 0 || len(o.TakeCandidates()) != 0 {
		t.Fatal("license denial did acquisition")
	}
	bad := d
	bad.Sources = make([]servingauth.ProviderDescriptor, servingauth.MaxProviders+1)
	if err := o.SetDesired(t.Context(), bad); !errors.Is(err, ErrBounds) {
		t.Fatal("source count", err)
	}
	bad = d
	bad.Authorities = nil
	if err := o.SetDesired(t.Context(), bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("omitted receipt inventory", err)
	}
	bad = d
	bad.Sources = []servingauth.ProviderDescriptor{s, s}
	if err := o.SetDesired(t.Context(), bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate source", err)
	}
	huge := cloneDescriptor(s)
	huge.Spec.ClaimValidationRules = make([]publicv1.ClaimValidationRule, 65)
	bad = d
	bad.Sources = []servingauth.ProviderDescriptor{huge}
	if err := o.SetDesired(t.Context(), bad); !errors.Is(err, ErrBounds) {
		t.Fatal("policy cap", err)
	}
	encoded, _ := json.Marshal(h)
	if strings.Contains(string(encoded), s.Issuer) || strings.Contains(string(encoded), "test-only") {
		t.Fatal("host diagnostic disclosure")
	}
	if _, err := o.Acquire(context.Background(), Desired{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("absent cut", err)
	}
}

func TestTrustAndRuntimeCounterCapsFailClosed(t *testing.T) {
	f := newResourceFixture(t)
	opts := HostOptions{Identity: "host", CredentialPolicy: "policy", EntitlementIdentity: "entitlement", FingerprintKeyID: "key", ConfigSources: []servingauth.ProviderSource{}, AllowedOrigins: []string{f.server.URL}, LocalOrigins: []string{f.server.URL}, TrustPEM: append(append([]byte{}, f.ca...), f.ca...)} //nolint:gosec // Policy/key identities are test labels, not credentials.
	if _, err := NewHost(opts); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate roots", err)
	}
	opts.TrustPEM = make([]byte, MaxTrustBytes+1)
	if _, err := NewHost(opts); !errors.Is(err, ErrBounds) {
		t.Fatal("trust byte cap", err)
	}
	opts.TrustPEM = f.ca
	opts.Algorithms = make([]string, 11)
	if _, err := NewHost(opts); !errors.Is(err, ErrBounds) {
		t.Fatal("algorithm cap", err)
	}
	if _, err := New(Options{MaxConcurrent: 5}); !errors.Is(err, ErrBounds) {
		t.Fatal("concurrency cap", err)
	}
	if _, err := New(Options{OperationTimeout: 16 * time.Second}); !errors.Is(err, ErrBounds) {
		t.Fatal("operation timeout cap", err)
	}
	h, err := NewHost(HostOptions{Identity: "host", CredentialPolicy: "policy", EntitlementIdentity: "entitlement", FingerprintKeyID: "key", ConfigSources: []servingauth.ProviderSource{}, AllowedOrigins: []string{}, LocalOrigins: []string{}})
	if err != nil {
		t.Fatal(err)
	} //nolint:gosec // Test identities only.
	o, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	d := Desired{RegistryDigest: hash("registry"), Host: h, Sources: []servingauth.ProviderDescriptor{}, Authorities: []providerregistry.AuthorityRecord{}}
	if err := o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	l, err := o.Acquire(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if err := o.WithReplacement(t.Context(), func(r *Replacement) error {
		o.revision = ^uint64(0)
		next := d
		next.RegistryDigest = hash("new-registry")
		if err := r.SetDesired(next); !errors.Is(err, ErrBounds) {
			return errors.New("runtime counter wrapped")
		}
		r.Retire()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if l.Current() {
		t.Fatal("counter exhaustion revived lease")
	}
}
