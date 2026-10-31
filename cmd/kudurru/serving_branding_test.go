package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	private "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/branding"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/blockpage"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type brandingCommandClock struct {
	calls atomic.Int64
	at    time.Time
}

func TestServingBrandingFinalClockRollbackIncludesNoop(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	defer func() { f.stop() }()
	f.patch(t, `{"branding":{"name":"Original"}}`, http.StatusOK)
	for _, at := range []time.Time{f.base.Add(time.Hour + license.Grace), f.base.Add(-time.Nanosecond)} {
		for _, name := range []string{"Changed", "Original"} {
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			script := &brandingCommandClock{at: at}
			f.final.Store(script)
			code, _ := f.request(t, http.MethodPatch, "/settings", `{"branding":{"name":"`+name+`"}}`, true)
			f.final.Store(nil)
			if script.calls.Load() < 2 || code != http.StatusPaymentRequired {
				t.Fatalf("final original branding clock not enforced: calls=%d HTTP%d", script.calls.Load(), code)
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("final license failure did not roll back entire settings command", err)
			}
		}
	}
	if got := f.publicBrand(t); got.Name != "Original" || !got.Enabled {
		t.Fatal("rolled-back command changed original settings")
	}
}

func TestServingBrandingOriginalCredentialRevocationIncludesNoop(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	defer func() { f.stop() }()
	f.patch(t, `{"branding":{"name":"Original"}}`, http.StatusOK)
	called := false
	r := httptest.NewRequest(http.MethodGet, "http://kudurru.test/api/v1/branding", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	f.adapter.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		cfg, err := f.s.Settings(r.Context())
		if err != nil {
			t.Fatal(err)
		}
		id, ok := tokens.Parse(f.token)
		if !ok {
			t.Fatal("invalid fixture token")
		}
		if err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: id}, store.DeleteOptions{Manager: store.ManagerSystem})
		}); err != nil {
			t.Fatal(err)
		}
		before, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"Original", "Revoked overwrite"} {
			cfg.Branding.Name = name
			if err := f.s.WriteSettings(r.Context(), cfg, []string{"branding.name"}); err == nil {
				t.Fatal("revoked original request authorized settings/no-op")
			}
		}
		after, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil || !before.Generation().Same(after.Generation()) {
			t.Fatal("revoked original request changed durable settings", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("original authenticated request did not enter")
	}
	f.wait(t)
	if code, _ := f.request(t, http.MethodPatch, "/settings", `{"branding":{"name":"Original"}}`, true); code != http.StatusUnauthorized {
		t.Fatalf("fresh revoked credential HTTP%d", code)
	}
}

func brandingMaxPNG(t *testing.T) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(brandingTestPNG(t), "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	// One legal ancillary chunk reaches the exact configured encoded-asset cap
	// without allocating a large decoded image or invalid trailing bytes.
	chunk := make([]byte, config.BrandingImageBytes-len(raw))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(chunk)-12)) //nolint:gosec // The entire fixture chunk is bounded by the 64 KiB PNG cap.
	copy(chunk[4:8], "teSt")
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	data := append(append(append([]byte{}, raw[:len(raw)-12]...), chunk...), raw[len(raw)-12:]...)
	if len(data) != config.BrandingImageBytes {
		t.Fatal("maximum PNG fixture has wrong size")
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

func TestServingBrandingMaximumAssetsDurableRestart(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	defer func() { f.stop() }()
	asset := brandingMaxPNG(t)
	brand := config.Branding{Name: "Retained organization", LogoLight: asset, LogoDark: asset, Favicon: asset, AccentColor: "#123456"}
	if err := brand.Validate(); err != nil {
		t.Fatal("maximum valid asset fixture rejected", err)
	}
	patch, err := json.Marshal(map[string]any{"branding": brand})
	if err != nil {
		t.Fatal(err)
	}
	f.patch(t, string(patch), http.StatusOK)
	if got := f.publicBrand(t); got.LogoLight != asset || got.LogoDark != asset || got.Favicon != asset {
		t.Fatal("maximum assets did not survive canonical auth capture")
	}
	opts := f.s.opts
	path := opts.Store.Path()
	f.server.Close()
	f.stop()
	if err := opts.Store.Close(); err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	opts.Store, err = sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: opts.Validator, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opts.Store.Close() })
	f.installKey(t, opts.License, license.PlanBusiness, []license.Feature{license.FeatureBranding}, f.base.Add(time.Hour))
	f.start(t, opts)
	if got := f.publicBrand(t); got.Enabled || got.LogoLight != "" || got.Name != "kudurru" {
		t.Fatal("restart exposed retained assets without Enterprise entitlement")
	}
	f.installKey(t, opts.License, license.PlanEnterprise, []license.Feature{license.FeatureBranding}, f.base.Add(time.Hour))
	if err := f.s.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	if got := f.publicBrand(t); !got.Enabled || got.Name != brand.Name || got.LogoLight != asset || got.LogoDark != asset || got.Favicon != asset {
		t.Fatal("restart/license restoration lost persisted desired assets")
	}
}

func TestServingBrandingPendingPublicationStandaloneFallback(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	defer func() { f.stop() }()
	f.patch(t, `{"branding":{"name":"Cached organization"}}`, http.StatusOK)
	page, err := blockpage.New(blockpage.Options{Branding: func(ctx context.Context) (*blockpage.Branding, error) {
		return f.s.captureBlockPageBranding(ctx, f.cache)
	}})
	if err != nil {
		t.Fatal(err)
	}
	listener := httptest.NewServer(page.Handler("http"))
	defer listener.Close()
	render := func(want string) {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, listener.URL+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = "blocked.example"
		response, err := listener.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("Blocked by "+want+" DNS.")) {
			t.Fatal("standalone listener did not render selected/default brand", readErr, closeErr)
		}
	}
	render("Cached organization") // Populate the retained asset cache.
	_, release, err := f.s.publisher.Pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var current public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &current); err != nil {
			return err
		}
		value := current.Spec.Settings["branding"]
		value.Raw = []byte(`{"name":"Replacement organization"}`)
		current.Spec.Settings["branding"] = value
		_, err := tx.Apply(ctx, &current, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request(t, http.MethodGet, "/branding", "", false); code != http.StatusServiceUnavailable {
		t.Fatalf("complete publication gap branding HTTP%d", code)
	}
	render("kudurru") // A populated cache cannot provide authority in a gap.
	release()
	f.wait(t)
	render("Replacement organization")
	f.installKey(t, f.s.opts.License, license.PlanBusiness, nil, f.base.Add(time.Hour))
	if err := f.s.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	render("kudurru")
}

func TestServingBrandingLegacyBlockPageFinalOriginalLicenseClock(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int64
	var armed atomic.Bool
	manager, err := license.New(license.Options{Keyring: license.Keyring{"branding": pub}, Now: func() time.Time {
		if armed.Load() && calls.Add(1) >= 3 {
			return base.Add(time.Hour + license.Grace)
		}
		return base
	}})
	if err != nil {
		t.Fatal(err)
	}
	f := &brandingHTTPFixture{key: key, base: base}
	f.installKey(t, manager, license.PlanEnterprise, nil, base.Add(time.Hour))
	cfg, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Branding.Name = "Expired during preparation"
	cfg.BlockPage.Enabled = false
	page, err := newBlockPage(cfg, config.NewReloadable(cfg), nil, nil, nil, nil, nil, manager, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	w := httptest.NewRecorder()
	page.Handler("http").ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://blocked.example/", nil))
	if calls.Load() < 3 || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Blocked by kudurru DNS.") || strings.Contains(w.Body.String(), cfg.Branding.Name) {
		t.Fatal("legacy block page retained brand after original grace deadline")
	}
}

func TestServingBrandingEntitlementDoesNotChangeNativeDNS(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	defer func() { f.stop() }()
	if err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, obj := range []store.Object{
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "branding-dns"}, Spec: public.DNSZoneSpec{ZoneName: "branding.example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.branding.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
			&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "branding-answer", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "branding-dns"}, Endpoints: []public.Endpoint{{DNSName: "www.branding.example.", RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.45"}}}}},
		} {
			if _, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	i, a, u := identify.New(nil, nil), authoritative.New(nil), upstream.NewStage(nil)
	b, err := blocking.New(blocking.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c, r := cache.New(cachedata.New(cachedata.Options{}), cache.Settings{}, u), &postprocess.Stage{}
	ctl := control.New(control.Options{})
	chain, err := pipeline.New(i, ctl, r, a, b, c, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := servingbuild.BindDNS(f.s.publisher, servingbuild.DNSStages{Chain: chain, Control: ctl, Identify: i, Authority: a, Blocking: b, Cache: c, Response: r, Resolution: u}, f.now); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	listeners, err := dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: chain, Logger: logger}, Logger: logger, Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := listeners.Add(dnsserver.Spec{Protocol: dnsserver.ProtoUDP, Address: "127.0.0.1:0"}, dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := listeners.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- listeners.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("branding DNS listeners did not join")
		}
	}()
	awaitServingRuntime(t, f.s, func() bool { ready, _ := listeners.Ready(); return ready })
	query := func() {
		t.Helper()
		for _, transport := range []struct {
			network string
			proto   dnsserver.Protocol
		}{{"udp", dnsserver.ProtoUDP}, {"tcp", dnsserver.ProtoTCP}} {
			request := new(dns.Msg)
			request.SetQuestion("www.branding.example.", dns.TypeA)
			answer, _, err := (&dns.Client{Net: transport.network, Timeout: 5 * time.Second}).Exchange(request, listeners.Addr(transport.proto).String())
			if err != nil || answer == nil || answer.Rcode != dns.RcodeSuccess || !answer.Authoritative || len(answer.Answer) != 1 {
				t.Fatal("branding entitlement changed native DNS authority", answer, err)
			}
			rr, ok := answer.Answer[0].(*dns.A)
			if !ok || rr.A.String() != "192.0.2.45" {
				t.Fatal("branding entitlement changed native DNS data")
			}
		}
	}
	f.patch(t, `{"branding":{"name":"Custom DNS presentation"}}`, http.StatusOK)
	query()
	f.installKey(t, f.s.opts.License, license.PlanBusiness, []license.Feature{license.FeatureBranding}, f.base.Add(time.Hour))
	if err := f.s.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	if f.publicBrand(t).Enabled {
		t.Fatal("Business unexpectedly enabled branding")
	}
	query()
	f.installKey(t, f.s.opts.License, license.PlanEnterprise, nil, f.base.Add(-license.Grace))
	if err := f.s.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	if f.publicBrand(t).Enabled {
		t.Fatal("expired Enterprise unexpectedly enabled branding")
	}
	query()
}

type brandingHTTPFixture struct {
	s       *servingRuntime
	server  *httptest.Server
	adapter *servinghttp.Adapter
	stop    func()
	key     ed25519.PrivateKey
	base    time.Time
	clock   atomic.Int64
	final   atomic.Pointer[brandingCommandClock]
	token   string
	cache   *branding.PageCache
}

func (f *brandingHTTPFixture) now() time.Time {
	now := time.Unix(0, f.clock.Load()).UTC()
	if script := f.final.Load(); script != nil {
		var pcs [64]uintptr
		frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs[:])])
		for {
			frame, more := frames.Next()
			// Only the settings entitlement predicate consumes this script;
			// background preparation and raw-auth clocks remain real inputs.
			if strings.Contains(frame.Function, ".(*servingRuntime).checkSettingsBranding.func1") {
				if script.calls.Add(1) >= 2 {
					return script.at
				}
				break
			}
			if !more {
				break
			}
		}
	}
	return now
}

func newBrandingHTTPFixture(t *testing.T) *brandingHTTPFixture {
	t.Helper()
	base := newServingRuntimeFixture(t)
	if err := base.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := &brandingHTTPFixture{base: time.Now().UTC().Truncate(time.Second), cache: new(branding.PageCache)}
	f.clock.Store(f.base.UnixNano())
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.key = key
	opts := base.opts
	opts.Now = f.now
	opts.License, err = license.New(license.Options{Keyring: license.Keyring{"branding": pub}, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	opts.Config, err = config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	f.installKey(t, opts.License, license.PlanEnterprise, nil, f.base.Add(time.Hour))
	var id string
	f.token, id = tokens.Generate()
	err = opts.Store.Command(t.Context(), opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			return err
		}
		created := metav1.NewTime(f.base.Add(-time.Minute))
		user, err := tx.Apply(ctx, &private.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "branding-admin", CreationTimestamp: created}, Spec: private.LocalUserSpec{BuiltinAdmin: true}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		expires := metav1.NewTime(f.base.Add(90 * 24 * time.Hour))
		_, err = tx.Apply(ctx, &private.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: created}, Spec: private.APITokenSpec{SecretHash: tokens.Hash(f.token), Owner: private.TokenOwner{Kind: private.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:branding-admin", Provider: "local"}, Scopes: []string{"admin"}, ExpiresAt: &expires}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.start(t, opts)
	return f
}

func (f *brandingHTTPFixture) installKey(t *testing.T, manager *license.Manager, plan license.Plan, features []license.Feature, expires time.Time) {
	t.Helper()
	raw, err := license.Sign(f.key, "branding", license.Claims{ID: "branding-test", Issuer: license.Issuer, Licensee: "branding test", Plan: plan, Nodes: 3, Features: features, IssuedAt: f.base.Add(-90 * 24 * time.Hour).Unix(), NotBefore: f.base.Add(-time.Minute).Unix(), ExpiresAt: expires.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Set(raw, "test"); err != nil {
		t.Fatal(err)
	}
}

func (f *brandingHTTPFixture) start(t *testing.T, opts servingRuntimeOptions) {
	t.Helper()
	s, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	f.s = s
	ctx, cancel := context.WithCancel(t.Context())
	run, writer := make(chan error, 1), make(chan error, 1)
	go func() { run <- s.Run(ctx) }()
	go func() {
		writer <- s.opts.Store.Elector("branding-test").Run(ctx, func(ctx context.Context) {
			if err := s.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}, nil)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			for _, done := range []<-chan error{writer, run} {
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("branding runtime failed to join")
				}
			}
			if err := s.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	f.stop = stop
	t.Cleanup(stop)
	f.wait(t)
	f.adapter, err = servinghttp.New(servinghttp.Options{Publisher: s.publisher, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	decoy := *opts.Config
	decoy.Branding = config.Branding{Name: "Decoy live branding"}
	reload := config.NewReloadable(&decoy)
	srv, err := api.New(api.Options{Store: opts.Store, Validator: opts.Validator, Config: &decoy, Reload: reload, CanonicalSettings: s, BrandingReads: s.Branding, Authenticate: f.adapter.Middleware, Authorizer: f.adapter, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(srv)
	t.Cleanup(f.server.Close)
}

func (f *brandingHTTPFixture) wait(t *testing.T) {
	t.Helper()
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		// Primary lifecycle admission can advance the database while preparing
		// this cut. Compare to the actual durable image, never an older target.
		image, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return v.Base().Same(image.Generation()) && f.s.publisher.Status().ProtectedReady && f.s.primary.driver.Status().Ready
	})
}

func (f *brandingHTTPFixture) request(t *testing.T, method, path, body string, authenticated bool) (int, []byte) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+api.BasePath+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authenticated {
		r.Header.Set("Authorization", "Bearer "+f.token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := f.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if path == "/branding" && response.StatusCode == http.StatusOK && response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("public branding may be cached after original expiry")
	}
	return response.StatusCode, raw
}

func (f *brandingHTTPFixture) publicBrand(t *testing.T) api.BrandingInfo {
	t.Helper()
	code, raw := f.freshRequest(t, http.MethodGet, "/branding", "", false)
	if code != http.StatusOK {
		t.Fatalf("public branding HTTP%d", code)
	}
	var out api.BrandingInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A replacement may fence a request between capture and command admission.
// Retry only by making a fresh native request; held-request tests never retry.
func (f *brandingHTTPFixture) freshRequest(t *testing.T, method, path, body string, authenticated bool) (int, []byte) {
	t.Helper()
	until := time.Now().Add(15 * time.Second)
	for {
		code, raw := f.request(t, method, path, body, authenticated)
		if code != http.StatusServiceUnavailable || time.Now().After(until) {
			return code, raw
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func brandingTestPNG(t *testing.T) string {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func (f *brandingHTTPFixture) patch(t *testing.T, patch string, status int) {
	t.Helper()
	if code, raw := f.freshRequest(t, http.MethodPatch, "/settings", patch, true); code != status {
		t.Fatalf("branding settings HTTP%d, want %d: %s", code, status, raw)
	}
	if status == http.StatusOK {
		f.wait(t)
	}
}

func TestServingBrandingActualHTTPPlansWritesAndGrace(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	defer func() { f.stop() }()
	initial := f.publicBrand(t)
	if !initial.Enabled || initial.Name != "kudurru" || initial.ValidUntil == nil || !initial.ValidUntil.Equal(f.base.Add(time.Hour+license.Grace)) {
		t.Fatal("empty customization lost finite Enterprise entitlement")
	}
	asset := brandingTestPNG(t)
	patch, err := json.Marshal(map[string]any{"branding": config.Branding{Name: "Organization A", LogoLight: asset, Favicon: asset, AccentColor: "#123456"}})
	if err != nil {
		t.Fatal(err)
	}
	f.patch(t, string(patch), http.StatusOK)
	if got := f.publicBrand(t); got.Name != "Organization A" || got.LogoLight != asset || got.Favicon != asset {
		t.Fatal("public branding did not use committed canonical assets")
	}
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.patch(t, string(patch), http.StatusOK)
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("admitted branding no-op changed durable state", err)
	}
	f.patch(t, `{"branding":{"name":null}}`, http.StatusOK)
	if got := f.publicBrand(t); got.Name != "kudurru" || got.LogoLight != asset {
		t.Fatal("reset cleared unrelated branding assets")
	}
	f.patch(t, string(patch), http.StatusOK)
	for _, plan := range []license.Plan{license.PlanPersonal, license.PlanBusiness, license.PlanTrial} {
		f.installKey(t, f.s.opts.License, plan, []license.Feature{license.FeatureBranding}, f.base.Add(time.Hour))
		if err := f.s.host.ObserveLicense(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.wait(t)
		got := f.publicBrand(t)
		if got.Enabled || got.Name != "kudurru" || got.LogoLight != "" || got.Favicon != "" || got.ValidUntil != nil {
			t.Fatal("non-Enterprise license exposed retained branding")
		}
		before, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		f.patch(t, string(patch), http.StatusPaymentRequired)
		f.patch(t, `{"branding":{"name":null}}`, http.StatusPaymentRequired)
		after, err := f.s.opts.Store.ReadGeneration(t.Context())
		if err != nil || !before.Generation().Same(after.Generation()) {
			t.Fatal("non-Enterprise branding/no-op/reset denial changed durable state", err)
		}
	}
	f.patch(t, `{"cache":{"maxEntries":27}}`, http.StatusOK)
	var stored public.Kudurru
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &stored); err != nil || !bytes.Contains(stored.Spec.Settings["branding"].Raw, []byte(asset)) {
		t.Fatal("license denial discarded persisted desired branding", err)
	}
	// Normal grace remains enabled, with its original absolute end.
	expires := f.base.Add(-license.Grace + time.Minute)
	f.installKey(t, f.s.opts.License, license.PlanEnterprise, nil, expires)
	if err := f.s.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	if got := f.publicBrand(t); !got.Enabled || got.Name != "Organization A" || got.ValidUntil == nil || !got.ValidUntil.Equal(f.base.Add(time.Minute)) {
		t.Fatal("grace did not restore original retained assets/deadline")
	}
	f.clock.Store(f.base.Add(time.Minute).UnixNano())
	if err := f.s.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wait(t)
	if got := f.publicBrand(t); got.Enabled || got.Name != "kudurru" || got.LogoLight != "" {
		t.Fatal("exact grace end leaked retained assets")
	}
	f.patch(t, string(patch), http.StatusPaymentRequired)
}

func TestServingBrandingHeldSettingsLicenseAndPreview(t *testing.T) {
	f := newBrandingHTTPFixture(t)
	defer func() { f.stop() }()
	f.patch(t, `{"branding":{"name":"Organization A","accentColor":"#123456"},"blockPage":{"title":"Original heading"},"unblockRequests":{"enabled":true}}`, http.StatusOK)
	page, err := blockpage.New(blockpage.Options{Node: "branding-node", Settings: blockpage.Settings{Title: "Decoy heading", Requests: true}, RequestsAllowed: func() bool { t.Error("captured preview consulted live requests license"); return false }})
	if err != nil {
		t.Fatal(err)
	}
	preview := servingBrandingPreview{runtime: f.s, server: page, cache: f.cache}
	var held context.Context
	called := false
	f.adapter.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called, held = true, r.Context()
		a, err := f.s.Branding(held)
		if err != nil || a.Name != "Organization A" {
			t.Fatal("initial branding read failed", err)
		}
		f.patch(t, `{"branding":{"name":"Organization B"},"blockPage":{"title":"Replacement heading"}}`, http.StatusOK)
		f.installKey(t, f.s.opts.License, license.PlanBusiness, []license.Feature{license.FeatureRequests}, f.base.Add(time.Hour))
		if err := f.s.host.ObserveLicense(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.wait(t)
		if fresh := f.publicBrand(t); fresh.Enabled {
			t.Fatal("fresh read adopted old Enterprise entitlement")
		}
		original, err := f.s.Branding(held)
		if err != nil || original.Name != a.Name || !original.Enabled || original.ValidUntil == nil || !original.ValidUntil.Equal(*a.ValidUntil) {
			t.Fatal("held read adopted successor settings/license/deadline", err)
		}
		doc, err := preview.CapturedPreview(held, "blocked.example", netip.MustParseAddr("192.0.2.5"))
		if err != nil || !bytes.Contains(doc.Body, []byte("Organization A")) || !bytes.Contains(doc.Body, []byte("Original heading")) || bytes.Contains(doc.Body, []byte("Replacement heading")) || bytes.Contains(doc.Body, []byte("Decoy heading")) || !bytes.Contains(doc.Body, []byte("fieldset disabled")) {
			t.Fatal("preview mixed original and live settings/workflow policy", err)
		}
		cfg, err := f.s.Settings(held)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Branding.Name = "Stale overwrite"
		if err := f.s.WriteSettings(held, cfg, []string{"branding.name"}); err == nil {
			t.Fatal("held read authorized a successor settings write")
		}
		f.clock.Store(a.ValidUntil.UnixNano())
		if expired, err := f.s.Branding(held); err != nil || expired.Enabled || expired.Name != "kudurru" {
			t.Fatal("held read renewed beyond its original deadline", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://kudurru.test/api/v1/branding", nil)
		r.Header.Set("Authorization", "Bearer "+f.token)
		return r
	}())
	if !called {
		t.Fatal("actual original-lease middleware did not enter")
	}
	if _, err := f.s.Branding(held); err == nil {
		t.Fatal("released original request retained branding capability")
	}
	if brand, _ := f.s.captureBlockPageBranding(t.Context(), f.cache); brand != nil {
		t.Fatal("standalone listener used old custom brand after its license deadline")
	}
}
