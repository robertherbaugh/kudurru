package servingbuild

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fixture struct {
	keyrings           *KeyringOwner
	store              *sqlite.Store
	builder            *Builder
	pub                *serving.Publisher
	chain              *pipeline.Chain
	token              string
	epoch              atomic.Uint64
	blocked            atomic.Bool
	captured, released atomic.Int64
	deps               Dependencies
	now                time.Time
	stop               func()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWithKeyrings(t, nil)
}

func newFixtureWithKeyrings(t *testing.T, keyrings *KeyringOwner) *fixture {
	t.Helper()
	f := &fixture{now: time.Now().UTC(), keyrings: keyrings}
	f.epoch.Store(1)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	var private []store.Kind
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	f.store, err = sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{Scheme: scheme, Validator: v, InternalKinds: private, ValidateReplicaEntry: security.ValidateEntry, ValidateReplicaImage: security.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	schema, err := NewSchema(scheme, v, security.ValidateEntry, security.ValidateImage)
	if err != nil {
		t.Fatal(err)
	}
	host, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	f.deps = Dependencies{Identity: "test-host-v1", Host: host, Catalog: CatalogResource{Catalogue: catalogue.Embedded(), Source: host.Blocking.Catalogue}, Keys: keys, Auth: servingauth.Dependencies{ConfigProviders: []servingauth.ProviderSource{}, ProviderResources: []servingauth.ProviderResource{}, Identity: "test-auth-v1", ExpectedIdentity: "test-auth-v1", Permissions: permissions, License: servingauth.LicensePolicy{RBAC: true}, CurrentKey: bytes.Repeat([]byte{3}, 32)}}
	lists, err := listfetcher.OpenDiskCache(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.builder, err = New(Options{Schema: schema, Lists: lists, Authority: f.store.ServingAuthority, Now: func() time.Time { return f.now }, Capture: func(context.Context, *replication.Image) (Dependencies, error) {
		d := f.deps
		epoch := f.epoch.Load()
		d.Current = func() bool { return f.epoch.Load() == epoch && !f.blocked.Load() }
		if keyrings != nil {
			cut := keyrings.Capture()
			d.Keys = cut.Keys()
			d.Identity += "/" + cut.Identity()
			d.Current = func() bool { return f.epoch.Load() == epoch && !f.blocked.Load() && cut.Current() }
		}
		f.captured.Add(1)
		d.Release = func() { f.released.Add(1) }
		return d, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	f.token, id = tokens.Generate()
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := security.InitializeOwners(ctx, tx); err != nil {
			return err
		}
		u, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice", CreationTimestamp: metav1.NewTime(f.now.Add(-time.Hour))}, Spec: internalv1.LocalUserSpec{Groups: []string{"staff"}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		for _, object := range []store.Object{
			settings("192.0.2.11"),
			&public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "read", Namespace: "default"}, Spec: public.RoleBindingSpec{Role: "viewer", Subjects: []public.BindingSubject{{Kind: "Group", Name: "staff"}}}},
			&internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(f.now.Add(-time.Hour))}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(f.token), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: "alice", UID: string(u.GetUID()), ID: "local:alice", Provider: "local"}, Scopes: []string{"viewer"}}},
			&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "deny", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "inline", Inline: []string{"blocked.example"}}}}},
			&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"deny"}}},
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "uncommitted"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "uncommitted.example."}},
		} {
			if _, err := tx.Apply(ctx, object, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		return nil
	})
	options := serving.Options{Source: f.store, Required: serving.CompleteManifest(), Prepare: f.builder.Prepare, RetryInterval: time.Millisecond}
	if keyrings != nil {
		options.Source, err = keyrings.GuardSource(f.store)
		if err != nil {
			t.Fatal(err)
		}
		options.DependencyChanges = keyrings.Changes
	}
	f.pub, err = serving.New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = f.pub.Run(ctx) }()
	f.stop = func() { cancel(); <-done; f.pub.Close() }
	t.Cleanup(f.stop)
	select {
	case <-f.pub.Initialized():
	case <-time.After(10 * time.Second):
		t.Fatalf("publisher did not initialize: %+v", f.pub.Status())
	}
	f.chain = newChain(t, f.pub)
	return f
}

func settings(ip string) *public.Kudurru {
	return &public.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: public.KudurruSpec{Settings: map[string]extensions.JSON{"blocking": {Raw: []byte(`{"defaultResponse":"customIP","customIP":{"v4":"` + ip + `"}}`)}}}}
}

func (f *fixture) command(t *testing.T, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	if err := f.store.Command(t.Context(), f.store.Authority().Term, fn); err != nil {
		t.Fatal(err)
	}
}

func newChain(t *testing.T, p *serving.Publisher) *pipeline.Chain {
	t.Helper()
	i, a, u := identify.New(nil, nil), authoritative.New(nil), upstream.NewStage(nil)
	b, err := blocking.New(blocking.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(cachedata.New(cachedata.Options{}), cache.Settings{}, u)
	r := &postprocess.Stage{}
	chain, err := pipeline.New(i, r, a, b, c, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := BindDNS(p, DNSStages{Chain: chain, Identify: i, Authority: a, Blocking: b, Cache: c, Response: r, Resolution: u}, nil); err != nil {
		t.Fatal(err)
	}
	return chain
}

func query(t *testing.T, chain *pipeline.Chain, pinned *serving.Lease, name string) *pipeline.Response {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, dns.TypeA)
	req := &pipeline.Request{Ctx: t.Context(), Msg: m, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:12345"), Proto: pipeline.ProtoUDP}}
	if pinned != nil {
		clone, err := pinned.Clone()
		if err != nil {
			t.Fatal(err)
		}
		req.Serving = servingdns.Wrap(clone)
		t.Cleanup(req.Serving.Release)
	}
	response, err := chain.Serve(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(response.Close)
	return response
}

func wantIP(t *testing.T, response *pipeline.Response, ip string) {
	t.Helper()
	if response == nil || response.Msg == nil || len(response.Msg.Answer) != 1 {
		t.Fatalf("expected blocked IP %s: %+v", ip, response)
	}
	a, ok := response.Msg.Answer[0].(*dns.A)
	if !ok || a.A.String() != ip {
		t.Fatalf("expected IP %s, got %s", ip, response.Msg)
	}
}

func (f *fixture) waitCurrent(t *testing.T) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		changed := f.pub.Subscribe()
		lease, err := f.pub.CaptureHTTP(t.Context())
		if err == nil {
			lease.Release()
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("publication stayed pending: %+v", f.pub.Status())
		}
	}
}

func (f *fixture) auth(t *testing.T, lease *serving.Lease) *servingauth.View {
	t.Helper()
	value, ok := lease.Bundle().Component("auth")
	if !ok {
		t.Fatal("missing auth owner")
	}
	return value.(*servingauth.View)
}

func TestCompleteBundleKeepsDNSAndAuthOnOneHeldImage(t *testing.T) {
	f := newFixture(t)
	old, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	base := old.Bundle().Identity().Base
	for _, name := range serving.CompleteManifest() {
		if value, ok := old.Bundle().Component(name); !ok || value == nil {
			t.Fatal("missing real owner", name)
		}
	}
	proof := servingauth.Credential{Kind: servingauth.APIToken, Value: f.token}
	if _, err := f.auth(t, old).Authenticate(t.Context(), proof, f.now); err != nil {
		t.Fatal(err)
	}
	wantIP(t, query(t, f.chain, old, "blocked.example."), "192.0.2.11")
	if got := query(t, f.chain, old, "host.uncommitted.example."); got.Msg.Rcode != dns.RcodeServerFailure {
		t.Fatal("desired primary became serving authority")
	}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		u := &internalv1.LocalUser{}
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "alice"}, u); err != nil {
			return err
		}
		u.Spec.Disabled = true
		if _, err := tx.Apply(ctx, u, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, settings("192.0.2.22"), store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	f.waitCurrent(t)
	next, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if base.Same(next.Bundle().Identity().Base) {
		t.Fatal("new image was not published")
	}
	if _, err := f.auth(t, next).Authenticate(t.Context(), proof, f.now); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("new account state not enforced", err)
	}
	if _, err := f.auth(t, old).Authenticate(t.Context(), proof, f.now); err != nil {
		t.Fatal("held read mixed new auth state", err)
	}
	wantIP(t, query(t, f.chain, old, "blocked.example."), "192.0.2.11")
	wantIP(t, query(t, f.chain, next, "blocked.example."), "192.0.2.22")
	// All held owners are detached from both SQLite and their preparation input.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	wantIP(t, query(t, f.chain, old, "blocked.example."), "192.0.2.11")
	if _, err := f.auth(t, old).Authenticate(t.Context(), proof, f.now); err != nil {
		t.Fatal(err)
	}
}

func TestMissingBodyRetainsWholeBundleAndGatesHTTP(t *testing.T) {
	f := newFixture(t)
	old, err := f.pub.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := tx.Apply(ctx, settings("192.0.2.22"), store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Name: "url", URL: "https://lists.example/missing"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	image, token, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.builder.Prepare(t.Context(), serving.Candidate{Image: image, Token: token}); !errors.Is(err, listfetcher.ErrIncomplete) {
		t.Fatal("incomplete candidate admitted", err)
	}
	if lease, err := f.pub.CaptureHTTP(t.Context()); !errors.Is(err, serving.ErrPending) {
		if lease != nil {
			lease.Release()
		}
		t.Fatal("HTTP did not gate ahead of publication", err)
	}
	wantIP(t, query(t, f.chain, nil, "blocked.example."), "192.0.2.11")
	adapter, err := servinghttp.New(servinghttp.Options{Publisher: f.pub, Cookie: session.CookieConfig{Secure: session.SecureAlways}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	request.Header.Set("Authorization", "Bearer "+f.token)
	adapter.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("pending auth reached handler") })).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatal("pending auth status", recorder.Code)
	}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: "BlockList", Namespace: "default", Name: "missing"}, store.DeleteOptions{})
	})
	f.waitCurrent(t)
	wantIP(t, query(t, f.chain, nil, "blocked.example."), "192.0.2.22")
}

func TestDependencyChangeRejectsWholeCandidateAndReleasesOwners(t *testing.T) {
	f := newFixture(t)
	held, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.stop()
	image, token, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	opts := f.builder.opts
	before := f.released.Load()
	capture := opts.Capture
	opts.Capture = func(ctx context.Context, image *replication.Image) (Dependencies, error) {
		deps, err := capture(ctx, image)
		f.epoch.Add(1) // immutable source changed after its capture, before CAS
		return deps, err
	}
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if prepared, err := b.Prepare(t.Context(), serving.Candidate{Image: image, Token: token}); !errors.Is(err, serving.ErrDependency) || len(prepared.Components) != 0 {
		t.Fatal("stale dependency cut admitted", err)
	}
	if f.released.Load() != before+1 {
		t.Fatal("failed candidate did not release its owned dependency reference")
	}
	view := f.auth(t, held)
	proof := servingauth.Credential{Kind: servingauth.APIToken, Value: f.token}
	if _, err := view.Authenticate(t.Context(), proof, f.now); err != nil {
		t.Fatal("publisher close retired leased auth resource", err)
	}
	held.Release()
	if _, err := view.Authenticate(t.Context(), proof, f.now); err == nil {
		t.Fatal("retired auth resource remained usable")
	}
	if f.released.Load() != f.captured.Load() {
		t.Fatal("owner references leaked", f.captured.Load(), f.released.Load())
	}
}
