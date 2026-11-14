package providercoordinator

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
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
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

type providerServer struct {
	server  *httptest.Server
	ca      []byte
	private ed25519.PrivateKey
	calls   atomic.Int64
	status  atomic.Int64
}

func newProviderServer(t *testing.T) *providerServer {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &providerServer{private: key}
	f.status.Store(200)
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "delegated test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	f.ca = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		w.WriteHeader(int(f.status.Load()))
		if f.status.Load() != 200 {
			return
		}
		if r.URL.Path == "/keys" {
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "k1", Use: "sig", Algorithm: "EdDSA"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.server.URL, "jwks_uri": f.server.URL + "/keys", "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "id_token_signing_alg_values_supported": []string{"EdDSA"}})
	}))
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}
func (f *providerServer) spec() publicv1.IdentityProviderSpec {
	return publicv1.IdentityProviderSpec{Issuer: f.server.URL, ClientID: "browser", ClientSecretRef: &publicv1.CredentialReference{Env: "COORDINATOR_TEST"}}
}
func (f *providerServer) token(t *testing.T, at time.Time) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.private}, &jose.SignerOptions{ExtraHeaders: map[jose.HeaderKey]any{"kid": "k1", "typ": "JWT"}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"iss": f.server.URL, "sub": "subject", "aud": "browser", "iat": at.Unix(), "exp": at.Add(time.Hour).Unix(), "preferred_username": "alice"})
	v, err := s.Sign(data)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := v.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type hostGeneration struct {
	id       string
	allowed  bool
	secret   string
	deadline time.Time
	config   []providerregistry.DesiredConfig
}
type rootHost struct {
	gate        sync.RWMutex
	state       atomic.Pointer[hostGeneration]
	changesMu   sync.Mutex
	changes     chan struct{}
	memoMu      sync.Mutex
	memo        map[string]*providerresources.HostCut
	permissions *rbac.Permissions
	servers     []*providerServer
	clock       *atomic.Int64
	refs        atomic.Int64
}

func (h *rootHost) now() time.Time { return time.Unix(0, h.clock.Load()).UTC() }
func (h *rootHost) pin(ctx context.Context, fn func() error) error {
	for !h.gate.TryRLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	defer h.gate.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}
func (h *rootHost) subscribe() <-chan struct{} {
	h.changesMu.Lock()
	defer h.changesMu.Unlock()
	return h.changes
}
func (h *rootHost) install(g *hostGeneration) {
	h.gate.Lock()
	h.state.Store(g)
	h.gate.Unlock()
	h.changesMu.Lock()
	close(h.changes)
	h.changes = make(chan struct{})
	h.changesMu.Unlock()
}
func (h *rootHost) auth(g *hostGeneration) servingauth.Dependencies {
	return servingauth.Dependencies{Identity: g.id, ExpectedIdentity: g.id, Permissions: h.permissions, License: servingauth.LicensePolicy{RBAC: true, Providers: g.allowed}, CurrentKey: []byte(strings.Repeat("k", 32)), ConfigProviders: []servingauth.ProviderSource{}, ProviderResources: []servingauth.ProviderResource{}}
}
func (h *rootHost) capture(_ context.Context, r *providerregistry.Registry) (HostCapture, error) {
	g := h.state.Load()
	config := []servingauth.ProviderSource{}
	for _, local := range g.config {
		for _, canonical := range r.ConfigSources() {
			if local.ID == canonical.Ref.ID {
				canonical.Spec = local.Spec
				config = append(config, canonical)
			}
		}
	}
	encoded, _ := json.Marshal(config)
	memoKey := g.id + string(encoded)
	h.memoMu.Lock()
	p := h.memo[memoKey]
	if p == nil {
		resolver, err := credentials.New(credentials.Options{Environment: []string{"COORDINATOR_TEST"}, FingerprintKey: []byte(strings.Repeat("k", 32)), Getenv: func(string) (string, bool) { return g.secret, true }})
		if err != nil {
			h.memoMu.Unlock()
			return HostCapture{}, err
		}
		origins := []string{}
		ca := []byte{}
		for _, s := range h.servers {
			origins = append(origins, s.server.URL)
			ca = append(ca, s.ca...)
		}
		byID := map[string]*credentials.Resolver{}
		for _, local := range config {
			byID[local.Ref.ID] = resolver
		}
		p, err = providerresources.NewHost(providerresources.HostOptions{Identity: g.id, CredentialPolicy: "test-delegation/v1", EntitlementIdentity: "root-test/" + g.id, FingerprintKeyID: "test-key", ProvidersAllowed: g.allowed, ConfigSources: config, ObjectCredentials: resolver, ConfigCredentials: byID, AllowedOrigins: origins, LocalOrigins: origins, TrustPEM: ca, Algorithms: []string{"EdDSA"}})
		if err != nil {
			h.memoMu.Unlock()
			return HostCapture{}, err
		}
		h.memo[memoKey] = p
	}
	h.memoMu.Unlock()
	h.refs.Add(1)
	var once sync.Once
	return HostCapture{Identity: g.id, Providers: p, Auth: h.auth(g), ValidUntil: g.deadline, Current: func() bool { return h.state.Load() == g && (g.deadline.IsZero() || h.now().Before(g.deadline)) }, Release: func() { once.Do(func() { h.refs.Add(-1) }) }}, nil
}

var validatorOnce sync.Once
var localValidator *validator.Validator
var validatorErr error

func openStore(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	validatorOnce.Do(func() { localValidator, validatorErr = validator.New() })
	if validatorErr != nil {
		t.Fatal(validatorErr)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	private := []store.Kind{}
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InternalKinds: private, Validator: localValidator, InitialMode: mode, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func command(t *testing.T, s *sqlite.Store, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	if err := s.Command(t.Context(), s.Authority().Term, fn); err != nil {
		t.Fatal(err)
	}
}
func writer(t *testing.T, s *sqlite.Store) (context.Context, serving.TermActivation) {
	t.Helper()
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	return sqlite.WithWriterToken(t.Context(), token), serving.TermActivation{Term: token.Term(), Incarnation: token.Incarnation()}
}
func wait(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(6 * time.Second)
	for !fn() {
		if time.Now().After(until) {
			t.Fatal("condition timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type fixture struct {
	store       *sqlite.Store
	host        *rootHost
	resources   *providerresources.Owner
	coordinator *Coordinator
	publisher   *serving.Publisher
	schema      servingauth.Schema
	clock       atomic.Int64
	wire        string
	stop        context.CancelFunc
}

func newFixture(t *testing.T, servers ...*providerServer) *fixture {
	t.Helper()
	f := &fixture{}
	f.clock.Store(time.Now().UTC().UnixNano())
	f.store = openStore(t, filepath.Join(t.TempDir(), "state.db"), replication.Standalone)
	command(t, f.store, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := runtimeowners.Initialize(ctx, tx, time.Unix(0, f.clock.Load())); err != nil {
			return err
		}
		created := metav1.NewTime(time.Unix(0, f.clock.Load()).Add(-time.Hour))
		if _, err := tx.Apply(ctx, &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		for i, server := range servers {
			if _, err := tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("corp%d", i), CreationTimestamp: created}, Spec: server.spec()}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		sa, err := tx.Apply(ctx, &publicv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "apps", CreationTimestamp: created}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		subjects := []publicv1.BindingSubject{{Kind: "ServiceAccount", Name: "ci", Namespace: "apps"}}
		for i := range servers {
			subjects = append(subjects, publicv1.BindingSubject{Kind: "User", Name: fmt.Sprintf("corp%d:alice", i)})
		}
		if _, err = tx.Apply(ctx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "view", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: subjects}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		wire, id := tokens.Generate()
		f.wire = wire
		_, err = tx.Apply(ctx, &privatev1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: created}, Spec: privatev1.APITokenSpec{Owner: privatev1.TokenOwner{Kind: "ServiceAccount", ID: "sa:apps/ci", Namespace: "apps", Name: "ci", UID: string(sa.GetUID())}, SecretHash: tokens.Hash(wire), Scopes: []string{"viewer"}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	return startFixture(t, f, servers...)
}

func startFixture(t *testing.T, f *fixture, servers ...*providerServer) *fixture {
	t.Helper()
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	f.host = &rootHost{changes: make(chan struct{}), memo: map[string]*providerresources.HostCut{}, permissions: permissions, servers: servers, clock: &f.clock}
	f.host.install(&hostGeneration{id: "installation-1", allowed: true, secret: "test-only"})
	image, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, _ := runtimekinds.NewScheme()
	f.schema = servingauth.Schema{Scheme: scheme, Kinds: image.KindManifest, SchemaVersion: store.SchemaVersion}
	f.resources, err = providerresources.New(providerresources.Options{Now: f.host.now, RefreshInterval: 30 * time.Second, RetryInterval: 20 * time.Millisecond, OperationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	buildSchema, err := servingbuild.NewSchema(scheme, localValidator, runtimeowners.ValidateEntry, runtimeowners.ValidateImage)
	if err != nil {
		t.Fatal(err)
	}
	hostConfig, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	lists, err := listfetcher.OpenDiskCache(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := servingbuild.New(servingbuild.Options{Schema: buildSchema, Lists: lists, Authority: f.store.ServingAuthority, Now: f.host.now, Capture: func(ctx context.Context, image *replication.Image) (servingbuild.Dependencies, error) {
		if f.coordinator == nil {
			return servingbuild.Dependencies{}, ErrInvalid
		}
		g := f.host.state.Load()
		capture, err := f.coordinator.Capture(ctx, image)
		if err != nil {
			return servingbuild.Dependencies{}, err
		}
		if capture.HostIdentity != g.id {
			capture.Release()
			return servingbuild.Dependencies{}, ErrStale
		}
		auth := f.host.auth(g)
		auth.Identity = capture.Identity
		auth.ExpectedIdentity = capture.Identity
		auth.ConfigProviders = capture.ConfigProviders
		auth.ProviderResources = capture.ProviderResources
		return servingbuild.Dependencies{Identity: capture.Identity, Host: hostConfig, Auth: auth, Keys: keys, KeyringIdentity: "test-keyring", Catalog: servingbuild.CatalogResource{Catalogue: catalogue.Embedded(), Source: hostConfig.Blocking.Catalogue}, Current: func() bool { return f.host.state.Load() == g && capture.Current() }, Release: capture.Release}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := servingbuild.GuardProviderSource(f.store, servingbuild.ProviderSourceOptions{Resources: f.resources, PinHost: f.host.pin, Now: f.host.now})
	if err != nil {
		t.Fatal(err)
	}
	f.publisher, err = serving.New(serving.Options{Source: source, Prepare: builder.Prepare, Required: serving.CompleteManifest(), RetryInterval: 10 * time.Millisecond, DependencyChanges: func() <-chan struct{} { return f.coordinator.Changes() }})
	if err != nil {
		t.Fatal(err)
	}
	f.coordinator, err = New(Options{Store: f.store, Resources: f.resources, Schema: f.schema, Publisher: f.publisher, Host: f.host.capture, HostChanges: f.host.subscribe, PinHost: f.host.pin, Now: f.host.now, RetryInterval: 30 * time.Millisecond, OperationTimeout: time.Second, Admit: func(_ context.Context, _ *sqlite.CommandTx, a *Admission) error {
		if a.HostIdentity() != f.host.state.Load().id || a.RegistryDigest() == "" {
			return ErrAdmission
		}
		for _, r := range a.Receipts() {
			if r.Binding.Profile != "provider-authority/v1" || r.Binding.EntitlementIdentity != "root-test/"+a.HostIdentity() {
				return ErrAdmission
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	f.stop = cancel
	go func() { _ = f.coordinator.Run(ctx) }()
	go func() { _ = f.publisher.Run(ctx) }()
	t.Cleanup(func() { cancel(); _ = f.coordinator.Close(context.Background()); f.publisher.Close() })
	wait(t, func() bool { return f.publisher.Status().ProtectedReady })
	return f
}
func (f *fixture) capture(t *testing.T) Capture {
	t.Helper()
	image, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	capture, err := f.coordinator.Capture(t.Context(), image)
	if err != nil {
		t.Fatal(err)
	}
	return capture
}
func (f *fixture) view(t *testing.T) *servingauth.View {
	t.Helper()
	lease, err := f.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lease.Release)
	v, ok := lease.Bundle().Component("auth")
	if !ok {
		t.Fatal("missing auth owner")
	}
	return v.(*servingauth.View)
}
func (f *fixture) registry(t *testing.T) *providerregistry.Registry {
	t.Helper()
	image, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r, err := providerregistry.Decode(image.Payload.Security, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func isDenied(err error) bool {
	return errors.Is(err, servingauth.ErrCredentials) || errors.Is(err, servingauth.ErrDenied)
}
