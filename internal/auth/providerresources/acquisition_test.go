package providerresources

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

type resourceFixture struct {
	server          *httptest.Server
	ca              []byte
	private         ed25519.PrivateKey
	mu              sync.Mutex
	discovery, keys []byte
	status          int
	etag, cache     string
	conditional     bool
	active, peak    atomic.Int64
	calls           atomic.Int64
	clock           atomic.Int64
	entered         chan struct{}
	block           chan struct{}
}

func newResourceFixture(t *testing.T) *resourceFixture {
	t.Helper()
	f := &resourceFixture{status: 200, etag: `"v1"`, cache: "max-age=60"}
	f.clock.Store(time.Now().UTC().UnixNano())
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.private = key
	tlsPub, tlsKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, tlsPub, tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	f.ca = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		active := f.active.Add(1)
		defer f.active.Add(-1)
		for {
			peak := f.peak.Load()
			if active <= peak || f.peak.CompareAndSwap(peak, active) {
				break
			}
		}
		f.mu.Lock()
		status, cache, etag, conditional := f.status, f.cache, f.etag, f.conditional
		body := slices.Clone(f.discovery)
		if r.URL.Path == "/keys" {
			body = slices.Clone(f.keys)
		}
		entered, block := f.entered, f.block
		f.mu.Unlock()
		if entered != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		if block != nil {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Cache-Control", cache)
		w.Header().Set("ETag", etag)
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		if conditional && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		_, _ = w.Write(body)
	}))
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: tlsKey}}}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	f.discovery, _ = json.Marshal(map[string]any{"issuer": f.server.URL, "jwks_uri": f.server.URL + "/keys", "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "id_token_signing_alg_values_supported": []string{"EdDSA"}})
	f.keys, _ = json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "test", Algorithm: "EdDSA", Use: "sig"}}})
	return f
}
func (f *resourceFixture) now() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
func (f *resourceFixture) source(t *testing.T, origin servingauth.ProviderOrigin) servingauth.ProviderDescriptor {
	t.Helper()
	ref := servingauth.ProviderRef{ID: "corp", UID: strings.Repeat("a", 32), Origin: origin}
	spec := publicv1.IdentityProviderSpec{Issuer: f.server.URL, ClientID: "browser", ClientSecretRef: &publicv1.CredentialReference{Env: "TEST_CREDENTIAL"}}
	created := f.now().Add(-time.Hour)
	p, err := oidccore.Compile(spec, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	identity := hash(struct {
		Profile   string
		Ref       servingauth.ProviderRef
		CreatedAt time.Time
		Spec      publicv1.IdentityProviderSpec
	}{"servingauth/provider/v1", ref, created, spec})
	return servingauth.ProviderDescriptor{Ref: ref, CreatedAt: created, Issuer: spec.Issuer, SpecIdentity: identity, PolicyIdentity: p.Identity(), Spec: p.Spec()}
}
func (f *resourceFixture) host(t *testing.T, source servingauth.ProviderDescriptor, secret string, allowed bool) *HostCut {
	t.Helper()
	resolver, err := credentials.New(credentials.Options{Environment: []string{"TEST_CREDENTIAL"}, FingerprintKey: []byte(strings.Repeat("k", 32)), Getenv: func(string) (string, bool) { return secret, true }})
	if err != nil {
		t.Fatal(err)
	}
	// The host input is the raw local source spec, before normalized defaults.
	raw := publicv1.IdentityProviderSpec{Issuer: source.Issuer, ClientID: "browser", ClientSecretRef: &publicv1.CredentialReference{Env: "TEST_CREDENTIAL"}}
	h, err := NewHost(HostOptions{Identity: "host-v1", CredentialPolicy: "env-test/v1", EntitlementIdentity: "providers/v1", FingerprintKeyID: "key/v1", ProvidersAllowed: allowed, ObjectCredentials: resolver, ConfigCredentials: map[string]*credentials.Resolver{"corp": resolver}, ConfigSources: []servingauth.ProviderSource{{Ref: servingauth.ProviderRef{ID: "corp", UID: source.Ref.UID, Origin: servingauth.ProviderConfig}, CreatedAt: source.CreatedAt, Spec: raw}}, AllowedOrigins: []string{f.server.URL}, LocalOrigins: []string{f.server.URL}, Algorithms: []string{"EdDSA"}, TrustPEM: f.ca}) //nolint:gosec // Test policy identities are not credential material.
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func (f *resourceFixture) owner(t *testing.T, d Desired) *Owner {
	t.Helper()
	o, err := New(Options{Now: f.now, OperationTimeout: time.Second, RefreshInterval: 30 * time.Second, RetryInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err = o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	go func() { _ = o.Run(t.Context()) }()
	return o
}
func await(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("condition timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func candidate(t *testing.T, o *Owner) *AuthorityCandidate {
	t.Helper()
	var c *AuthorityCandidate
	await(t, func() bool {
		cs := o.TakeCandidates()
		if len(cs) > 0 {
			c = cs[0]
		}
		return c != nil
	})
	return c
}
func admit(t *testing.T, o *Owner, d Desired, c *AuthorityCandidate) Desired {
	t.Helper()
	if err := c.Match(d); err != nil {
		t.Fatal(err)
	}
	r := c.Receipt()
	d.RegistryDigest = hash(struct {
		Old     string
		Receipt providerregistry.DesiredAuthority
	}{d.RegistryDigest, r})
	d.Authorities = []providerregistry.AuthorityRecord{{FormatVersion: 1, Provider: r.Source.Ref, SpecIdentity: r.Source.SpecIdentity, Revision: 1, Active: true, Binding: r.Binding}}
	if err := o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		l, err := o.Acquire(t.Context(), d)
		if err != nil {
			return false
		}
		defer l.Release()
		return l.Resources()[0].Availability == servingauth.ProviderReady
	})
	return d
}
func (f *resourceFixture) token(t *testing.T, kid string) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.private}, &jose.SignerOptions{ExtraHeaders: map[jose.HeaderKey]any{"kid": kid, "typ": "JWT"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"iss": f.server.URL, "sub": "subject", "aud": "browser", "iat": f.now().Add(-time.Minute).Unix(), "exp": f.now().Add(time.Hour).Unix(), "preferred_username": "alice", "nonce": "expected"})
	signed, err := signer.Sign(raw)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func TestRealTLSAcquisitionAdmissionOfflineJWTAndDetachedCuts(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	d := Desired{RegistryDigest: hash("registry"), Host: f.host(t, s, "test-only", true), Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o := f.owner(t, d)
	old, err := o.Acquire(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if old.Resources()[0].Availability != servingauth.ProviderUnavailable {
		t.Fatal("unadmitted authority ready")
	}
	c := candidate(t, o)
	copyReceipt := c.Receipt()
	copyReceipt.Source.Spec.ClientID = "edited"
	if c.Receipt().Source.Spec.ClientID != "browser" {
		t.Fatal("candidate alias")
	}
	if err = c.Match(d); err != nil {
		t.Fatal(err)
	}
	d = admit(t, o, d, c)
	if old.Current() {
		t.Fatal("old unavailable cut current")
	}
	held, err := o.Acquire(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	resource := held.Resources()[0]
	policy, err := oidccore.Compile(s.Spec, s.Ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := f.calls.Load()
	v, err := resource.Verifier.VerifyBrowser(t.Context(), f.token(t, "test"), oidccore.BrowserProof{Nonce: "expected"}, f.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.Evaluate(t.Context(), v, oidccore.BrowserIDToken, f.now()); err != nil {
		t.Fatal(err)
	}
	if _, err = resource.Verifier.Verify(t.Context(), f.token(t, "missing-kid"), f.now()); err == nil {
		t.Fatal("unknown key")
	}
	if f.calls.Load() != before {
		t.Fatal("network on verify")
	}
	detached := held.Resources()
	detached[0].TrustIdentity = "edited"
	if held.Resources()[0].TrustIdentity == "edited" {
		t.Fatal("resource alias")
	}
	if len(held.Endpoints()) != 1 || held.Endpoints()[0].TokenURL != f.server.URL+"/token" {
		t.Fatal("endpoint cut")
	}
	// Changing the actual delegated credential denies immediately and cannot
	// activate until the new exact receipt is admitted (root floors separately).
	next := d
	next.Host = f.host(t, s, "replacement", true)
	if err = o.SetDesired(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if held.Current() {
		t.Fatal("credential rotation retained old cut")
	}
	await(t, func() bool {
		l, err := o.Acquire(t.Context(), next)
		if err != nil {
			return false
		}
		defer l.Release()
		return l.Resources()[0].Availability == servingauth.ProviderUnavailable
	})
	c2 := candidate(t, o)
	if c2.Receipt().Binding.CredentialFingerprint == c.Receipt().Binding.CredentialFingerprint {
		t.Fatal("credential receipt unchanged")
	}
	if err = c.Match(next); !errors.Is(err, ErrStale) {
		t.Fatal("old provenance reused", err)
	}
	// Held verifier remains immutable but lease eligibility never revives.
	if _, err = resource.Verifier.VerifyBrowser(t.Context(), f.token(t, "test"), oidccore.BrowserProof{Nonce: "expected"}, f.now()); err != nil {
		t.Fatal(err)
	}
	if err = o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if held.Current() {
		t.Fatal("ABA revived held cut")
	}
}
func TestConfigLocalMismatchAndObjectCredentialIsolation(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderConfig)
	h := f.host(t, s, "test-only", true)
	d := Desired{RegistryDigest: hash("registry"), Host: h, Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o := f.owner(t, d)
	d = admit(t, o, d, candidate(t, o))
	mismatched := *h
	mismatched.serial = hostSerial.Add(1)
	mismatched.configSpecs = map[string]string{"corp": hash("different-local-spec")}
	d.Host = &mismatched
	if err := o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	calls := f.calls.Load()
	await(t, func() bool {
		l, err := o.Acquire(t.Context(), d)
		if err != nil {
			return false
		}
		defer l.Release()
		return l.Resources()[0].Availability == servingauth.ProviderUnavailable
	})
	time.Sleep(80 * time.Millisecond)
	if f.calls.Load() != calls {
		t.Fatal("mismatched local config performed acquisition")
	}
	// An object with the same name/ref path cannot use the config resolver.
	object := f.source(t, servingauth.ProviderObject)
	isolated := *h
	isolated.serial = hostSerial.Add(1)
	isolated.objects = nil
	d.Host = &isolated
	d.Sources = []servingauth.ProviderDescriptor{object}
	d.Authorities = []providerregistry.AuthorityRecord{}
	if err := o.SetDesired(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if f.calls.Load() != calls {
		t.Fatal("object inherited config credentials")
	}
}
func TestLateCompletionCancellationAndCloseJoin(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	f.mu.Lock()
	f.block = make(chan struct{})
	f.entered = make(chan struct{}, 1)
	entered := f.entered
	f.mu.Unlock()
	d := Desired{RegistryDigest: hash("registry"), Host: f.host(t, s, "test-only", true), Sources: []servingauth.ProviderDescriptor{s}, Authorities: []providerregistry.AuthorityRecord{}}
	o := f.owner(t, d)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	empty := d
	empty.Sources = []servingauth.ProviderDescriptor{}
	if err := o.SetDesired(t.Context(), empty); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool {
		l, err := o.Acquire(t.Context(), empty)
		if err != nil {
			return false
		}
		defer l.Release()
		return len(l.Resources()) == 0
	})
	if len(o.TakeCandidates()) != 0 {
		t.Fatal("late candidate installed")
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Acquire(context.Background(), empty); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestBoundedConcurrentJobsAndCompleteQueueReplacement(t *testing.T) {
	f := newResourceFixture(t)
	s := f.source(t, servingauth.ProviderObject)
	h := f.host(t, s, "test-only", true)
	f.mu.Lock()
	f.block = make(chan struct{})
	f.entered = make(chan struct{}, 4)
	entered := f.entered
	f.mu.Unlock()
	d := Desired{RegistryDigest: hash("registry"), Host: h, Sources: []servingauth.ProviderDescriptor{}, Authorities: []providerregistry.AuthorityRecord{}}
	for i := 0; i < servingauth.MaxProviders; i++ {
		source := cloneDescriptor(s)
		source.Ref.ID = fmt.Sprintf("provider-%d", i)
		source.Spec.Issuer = fmt.Sprintf("%s/provider-%d", f.server.URL, i)
		source.Issuer = source.Spec.Issuer
		p, err := oidccore.Compile(source.Spec, source.Ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		source.PolicyIdentity = p.Identity()
		source.Spec = p.Spec()
		source.SpecIdentity = hash(struct {
			Ref  servingauth.ProviderRef
			Spec publicv1.IdentityProviderSpec
		}{source.Ref, source.Spec})
		d.Sources = append(d.Sources, source)
	}
	o := f.owner(t, d)
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("bounded batch did not start")
		}
	}
	if f.peak.Load() > 4 {
		t.Fatal("worker concurrency exceeded cap")
	}
	empty := d
	empty.Sources = []servingauth.ProviderDescriptor{}
	if err := o.SetDesired(t.Context(), empty); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return f.active.Load() == 0 })
	if f.peak.Load() > 4 || len(o.TakeCandidates()) != 0 {
		t.Fatal("obsolete jobs/candidates survived complete replacement")
	}
}
func TestRealJWTKeyRefreshDoesNotMergeKeysOrWriteDurableAuthority(t *testing.T) {
	f, s, o, d := realOwner(t)
	old, err := o.Acquire(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	oldToken := f.token(t, "test")
	oldResource := old.Resources()[0]
	before, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "test", Algorithm: "EdDSA", Use: "sig"}}})
	f.mu.Lock()
	f.keys = keys
	f.mu.Unlock()
	f.private = key
	f.clock.Add(int64(15 * time.Second))
	var fresh *Lease
	await(t, func() bool {
		l, err := o.Acquire(t.Context(), d)
		if err != nil {
			return false
		}
		if l.Resources()[0].VerifierIdentity == oldResource.VerifierIdentity {
			l.Release()
			return false
		}
		fresh = l
		return true
	})
	defer fresh.Release()
	if old.Current() {
		t.Fatal("old key cut current")
	}
	calls := f.calls.Load()
	if _, err := fresh.Resources()[0].Verifier.Verify(t.Context(), oldToken, f.now()); err == nil {
		t.Fatal("new key set merged old keys")
	}
	if _, err := fresh.Resources()[0].Verifier.Verify(t.Context(), f.token(t, "test"), f.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := oldResource.Verifier.Verify(t.Context(), oldToken, f.now()); err != nil {
		t.Fatal("old public key cut mutated", err)
	}
	if f.calls.Load() != calls {
		t.Fatal("network on unknown signature/verify")
	}
	after, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !before.Generation().Same(after.Generation()) || len(o.TakeCandidates()) != 0 {
		t.Fatal("routine key refresh fabricated durable authority/floors")
	}
}
