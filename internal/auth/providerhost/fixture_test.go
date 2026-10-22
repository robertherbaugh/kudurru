package providerhost

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	privatev1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type signedLicense struct {
	clock  atomic.Int64
	key    ed25519.PrivateKey
	ring   license.Keyring
	claims license.Claims
	raw    string
}

func newSigned(t *testing.T) *signedLicense {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	f := &signedLicense{key: key, ring: license.Keyring{"test": pub}, claims: license.Claims{Issuer: license.Issuer, ID: "host-test", Licensee: "test owner", Plan: license.PlanEnterprise, Nodes: 3, Features: []license.Feature{license.FeatureOIDC, license.FeatureRBAC}, IssuedAt: at.Add(-time.Minute).Unix(), NotBefore: at.Add(-time.Minute).Unix(), ExpiresAt: at.Add(time.Hour).Unix()}}
	f.clock.Store(at.UnixNano())
	f.raw, err = license.Sign(key, "test", f.claims)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *signedLicense) now() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
func (f *signedLicense) manager(t *testing.T) *license.Manager {
	t.Helper()
	m, err := license.New(license.Options{Keyring: f.ring, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Set(f.raw, "test"); err != nil {
		t.Fatal(err)
	}
	return m
}

var validatorOnce sync.Once
var valid *validator.Validator
var validErr error

func openStore(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	validatorOnce.Do(func() { valid, validErr = validator.New() })
	if validErr != nil {
		t.Fatal(validErr)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	kinds := []store.Kind{}
	for _, k := range runtimekinds.Private() {
		kinds = append(kinds, store.Kind(k))
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: valid, InternalKinds: kinds, InitialMode: mode, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func writer(t *testing.T, s *sqlite.Store) (context.Context, serving.TermActivation) {
	t.Helper()
	v, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	return sqlite.WithWriterToken(t.Context(), v), serving.TermActivation{Term: v.Term(), Incarnation: v.Incarnation()}
}
func command(t *testing.T, s *sqlite.Store, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	ctx, target := writer(t, s)
	if err := s.Command(ctx, target.Term, fn); err != nil {
		t.Fatal(err)
	}
}
func image(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	im, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return im
}
func registry(t *testing.T, s *sqlite.Store) *providerregistry.Registry {
	t.Helper()
	im := image(t, s)
	r, err := providerregistry.Decode(im.Payload.Security, im.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func spec() publicv1.IdentityProviderSpec {
	return publicv1.IdentityProviderSpec{Issuer: "https://issuer.example", ClientID: "browser", ClientSecretRef: &publicv1.CredentialReference{Env: "PROVIDER_HOST_TEST"}}
}
func staticInput(t *testing.T) StaticInput {
	t.Helper()
	p, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	keys := []byte(strings.Repeat("k", 32))
	totpKeys, err := totp.NewKeyring(keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	return StaticInput{Identity: "test-static", AuthKeyIdentity: "test-keys", Auth: servingauth.Dependencies{Permissions: p, CurrentKey: keys, Hasher: users.NewHasher(1), TOTPKeys: totpKeys, ConfigProviders: []servingauth.ProviderSource{}, ProviderResources: []servingauth.ProviderResource{}}, LocalProviders: []providerregistry.DesiredConfig{}, CredentialPolicy: "test-policy/v1", FingerprintKeyID: "test-fingerprint", AllowedOrigins: []string{}, LocalOrigins: []string{}, Algorithms: []string{"EdDSA"}, Current: func() bool { return true }, Release: func() {}}
}
func seed(t *testing.T, s *sqlite.Store, at time.Time) string {
	t.Helper()
	wire, id := tokens.Generate()
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := runtimeowners.Initialize(ctx, tx, at); err != nil {
			return err
		}
		if _, err := tx.Apply(ctx, &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		sa, err := tx.Apply(ctx, &publicv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "apps", CreationTimestamp: metav1.NewTime(at.Add(-time.Minute))}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		if _, err = tx.Apply(ctx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "viewer", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "ServiceAccount", Namespace: "apps", Name: "ci"}}}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &privatev1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(at.Add(-time.Minute))}, Spec: privatev1.APITokenSpec{Owner: privatev1.TokenOwner{Kind: "ServiceAccount", ID: "sa:apps/ci", Namespace: "apps", Name: "ci", UID: string(sa.GetUID())}, SecretHash: tokens.Hash(wire), Scopes: []string{"viewer"}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	return wire
}

type fixture struct {
	s           *sqlite.Store
	host        *Owner
	resources   *providerresources.Owner
	publisher   *serving.Publisher
	coordinator *providercoordinator.Coordinator
	license     *license.Manager
	f           *signedLicense
	in          StaticInput
	stop        context.CancelFunc
	keys        sync.RWMutex
	wire        string
}

func newFixture(t *testing.T, in StaticInput) *fixture {
	t.Helper()
	f := newSigned(t)
	s := openStore(t, filepath.Join(t.TempDir(), "state.db"), replication.Standalone)
	wire := seed(t, s, f.now())
	out := start(t, s, f, in)
	out.wire = wire
	return out
}
func start(t *testing.T, s *sqlite.Store, f *signedLicense, in StaticInput) *fixture {
	t.Helper()
	x := &fixture{s: s, f: f, in: in, license: f.manager(t)}
	var err error
	x.resources, err = providerresources.New(providerresources.Options{Now: f.now, RefreshInterval: time.Minute, RetryInterval: 20 * time.Millisecond, OperationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareStatic(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	im := image(t, s)
	schema := servingauth.Schema{Scheme: scheme, Kinds: im.KindManifest, SchemaVersion: store.SchemaVersion}
	buildSchema, err := servingbuild.NewSchema(scheme, valid, runtimeowners.ValidateEntry, runtimeowners.ValidateImage)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	ring, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	lists, err := listfetcher.OpenDiskCache(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := servingbuild.New(servingbuild.Options{Schema: buildSchema, Lists: lists, Authority: s.ServingAuthority, Now: f.now, Capture: func(ctx context.Context, im *replication.Image) (servingbuild.Dependencies, error) {
		r, err := providerregistry.Decode(im.Payload.Security, im.Term.ClusterID)
		if err != nil {
			return servingbuild.Dependencies{}, err
		}
		var h providercoordinator.HostCapture
		err = x.resources.WithCurrent(ctx, func(_ *providerresources.Pinned) error {
			return x.host.PinHost(ctx, func() error { var e error; h, e = x.host.Host(ctx, r); return e })
		})
		if err != nil {
			return servingbuild.Dependencies{}, err
		}
		c, err := x.coordinator.Capture(ctx, im)
		if err != nil {
			h.Release()
			return servingbuild.Dependencies{}, err
		}
		if c.HostIdentity != h.Identity {
			h.Release()
			c.Release()
			return servingbuild.Dependencies{}, ErrStale
		}
		deps := h.Auth
		deps.Identity = c.Identity
		deps.ExpectedIdentity = c.Identity
		deps.ConfigProviders = c.ConfigProviders
		deps.ProviderResources = c.ProviderResources
		return servingbuild.Dependencies{Identity: c.Identity, Host: cfg, Auth: deps, Keys: ring, KeyringIdentity: "static-tsig", Catalog: servingbuild.CatalogResource{Catalogue: catalogue.Embedded(), Source: cfg.Blocking.Catalogue}, Current: func() bool { return h.Current() && c.Current() }, Release: func() { h.Release(); c.Release() }}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := servingbuild.GuardProviderSource(s, servingbuild.ProviderSourceOptions{Resources: x.resources, PinHost: func(ctx context.Context, fn func() error) error { return x.host.PinHost(ctx, fn) }, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	x.publisher, err = serving.New(serving.Options{Source: source, Prepare: builder.Prepare, Required: serving.CompleteManifest(), RetryInterval: 10 * time.Millisecond, DependencyChanges: func() <-chan struct{} { return x.coordinator.Changes() }})
	if err != nil {
		t.Fatal(err)
	}
	x.host, err = New(Options{Store: s, Resources: x.resources, Publisher: x.publisher, License: x.license, Static: prepared, Schema: schema, Now: f.now, PinAuthKeys: func(ctx context.Context, fn func() error) error {
		if err := lock(ctx, &x.keys, false); err != nil {
			return err
		}
		defer x.keys.RUnlock()
		return fn()
	}, Withdraw: func(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
		return security.AcceptProviderAuthorities(ctx, tx, []providerregistry.DesiredAuthority{}, at)
	}})
	prepared.Release()
	if err != nil {
		t.Fatal(err)
	}
	x.coordinator, err = providercoordinator.New(providercoordinator.Options{Store: s, Resources: x.resources, Schema: schema, Publisher: x.publisher, Host: x.host.Host, HostChanges: x.host.Changes, PinHost: x.host.PinHost, Now: f.now, RetryInterval: 20 * time.Millisecond, OperationTimeout: time.Second, Admit: x.host.CheckAdmission})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	coordinatorDone, publisherDone := make(chan struct{}), make(chan struct{})
	x.stop = func() {
		cancel()
		<-coordinatorDone
		<-publisherDone
	}
	go func() { defer close(coordinatorDone); _ = x.coordinator.Run(ctx) }()
	go func() { defer close(publisherDone); _ = x.publisher.Run(ctx) }()
	t.Cleanup(func() {
		x.stop()
		_ = x.host.Close(context.Background())
		_ = x.coordinator.Close(context.Background())
		x.publisher.Close()
	})
	await(t, func() bool { return x.publisher.Status().ProtectedReady })
	return x
}
func await(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(until) {
			t.Fatal("condition timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func capture(t *testing.T, x *fixture) providercoordinator.HostCapture {
	t.Helper()
	r := registry(t, x.s)
	var h providercoordinator.HostCapture
	err := x.resources.WithReplacement(t.Context(), func(_ *providerresources.Replacement) error {
		return x.host.PinHost(t.Context(), func() error { var e error; h, e = x.host.Host(t.Context(), r); return e })
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func observe(t *testing.T, x *fixture) {
	t.Helper()
	im, token, err := x.s.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = x.host.ObserveInstalled(t.Context(), im, token); err != nil {
		t.Fatal(err)
	}
}
func authorities(t *testing.T, x *fixture, e string) []providerregistry.DesiredAuthority {
	t.Helper()
	var out = []providerregistry.DesiredAuthority{}
	command(t, x.s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		r, _, err := readCurrent(ctx, tx)
		if err != nil {
			return err
		}
		sources, err := servingauth.ProviderSourcesCurrent(ctx, tx, r.ConfigSources())
		if err != nil {
			return err
		}
		for _, s := range sources {
			out = append(out, providerregistry.DesiredAuthority{Source: s, Binding: providerregistry.AuthorityBinding{Profile: "provider-authority/v1", FingerprintKeyID: x.in.FingerprintKeyID, CredentialFingerprint: strings.Repeat("a", 64), CredentialPolicy: x.in.CredentialPolicy, NetworkPolicy: "test-network", MetadataBinding: "test-metadata", EntitlementIdentity: e}})
		}
		return nil
	})
	return out
}
func accept(t *testing.T, x *fixture, e string) {
	t.Helper()
	rs := authorities(t, x, e)
	command(t, x.s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return security.AcceptProviderAuthorities(ctx, tx, rs, x.f.now())
	})
}
func addObject(t *testing.T, x *fixture, id string, spec publicv1.IdentityProviderSpec) {
	t.Helper()
	if spec.Issuer == "https://issuer.example" {
		// Distinct actual provider authorities require distinct issuers.
		spec.Issuer += "/" + id
	}
	command(t, x.s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(x.f.now().Add(-time.Minute))}, Spec: spec}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
}
func reset(t *testing.T, x *fixture) string {
	t.Helper()
	ctx, target := writer(t, x.s)
	if err := x.host.Reset(ctx, target); err != nil {
		t.Fatal(err)
	}
	return x.host.value.Load().entitlement
}

// The only network in acceptance is this bounded, local fixture. Auth never
// makes a request; discovery/JWKS acquisition uses the real resource worker.
type providerServer struct {
	server *httptest.Server
	ca     []byte
	key    ed25519.PrivateKey
	calls  atomic.Int64
}

func newProviderServer(t *testing.T) *providerServer {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &providerServer{key: key}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	f.ca = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		if r.URL.Path == "/keys" {
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "test", Use: "sig", Algorithm: "EdDSA"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.server.URL, "jwks_uri": f.server.URL + "/keys", "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "id_token_signing_alg_values_supported": []string{"EdDSA"}})
	}))
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}
func (p *providerServer) spec() publicv1.IdentityProviderSpec {
	s := spec()
	s.Issuer = p.server.URL
	return s
}
func (p *providerServer) input(t *testing.T) StaticInput {
	t.Helper()
	in := staticInput(t)
	resolver, err := credentials.New(credentials.Options{Environment: []string{"PROVIDER_HOST_TEST"}, FingerprintKey: []byte(strings.Repeat("f", 32)), Getenv: func(string) (string, bool) { return "test-only", true }})
	if err != nil {
		t.Fatal(err)
	}
	in.ObjectCredentials = resolver
	in.AllowedOrigins = []string{p.server.URL}
	in.LocalOrigins = []string{p.server.URL}
	in.TrustPEM = p.ca
	return in
}
