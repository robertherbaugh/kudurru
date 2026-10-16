package blockpage

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrandingCallbackFallbackAndOncePerRequest(t *testing.T) {
	custom, err := PrepareBranding(BrandingInput{Name: "Acme", AccentColor: "#123456"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		brand *Branding
		err   error
		want  string
	}{
		{"selected", custom, nil, "Acme"},
		{"missing", nil, nil, "kudurru"},
		{"zero", &Branding{}, nil, "kudurru"},
		{"unavailable", nil, errors.New("unavailable owner"), "kudurru"},
		{"error with pointer", custom, errors.New("license ended"), "kudurru"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s, err := New(Options{Branding: func(context.Context) (*Branding, error) {
				calls++
				return tc.brand, tc.err
			}})
			if err != nil {
				t.Fatal(err)
			}
			h := s.Handler("http")
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				w := do(h, method, "ads.example", "/", "192.0.2.1:53")
				if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Strict-Transport-Security") != "" {
					t.Fatal("unavailable presentation must still serve safely")
				}
				if method == http.MethodGet {
					if !strings.Contains(w.Body.String(), "Blocked by "+tc.want+" DNS.") {
						t.Fatal("wrong selected/fallback brand")
					}
					checkBrandingDocument(t, Document{Body: w.Body.Bytes(), CSP: w.Header().Get("Content-Security-Policy")}, false)
				} else if w.Body.Len() != 0 {
					t.Fatal("HEAD wrote a page body")
				}
			}
			if calls != 2 {
				t.Fatalf("GET/HEAD branding calls=%d", calls)
			}
			doc := s.PreviewDocument(context.Background(), "ads.example", netip.Addr{})
			checkBrandingDocument(t, doc, true)
			if calls != 3 {
				t.Fatalf("preview branding calls=%d", calls)
			}
			for _, path := range []string{"/health", "/robots.txt"} {
				if w := do(h, http.MethodGet, "ads.example", path, "192.0.2.1:53"); w.Code != http.StatusOK {
					t.Fatalf("static path status=%d", w.Code)
				}
			}
			if calls != 3 {
				t.Fatal("static paths must not acquire presentation")
			}
		})
	}
}

func TestBrandingFaviconMIMECacheAndInlineCut(t *testing.T) {
	raw := brandingTestPNG(t, 7, 11, color.RGBA{G: 127, A: 255})
	custom, err := PrepareBranding(BrandingInput{Name: "Acme", Favicon: raw})
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[Branding]
	current.Store(custom)
	calls := 0
	s, err := New(Options{Branding: func(context.Context) (*Branding, error) {
		calls++
		return current.Load(), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler("http")
	page := do(h, http.MethodGet, "ads.example", "/", "192.0.2.1:53")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := do(h, method, "ads.example", "/favicon.ico", "192.0.2.1:53")
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("custom favicon headers: %v", w.Header())
		}
		if (method == http.MethodGet && !bytes.Equal(w.Body.Bytes(), raw)) || (method == http.MethodHead && w.Body.Len() != 0) {
			t.Fatal("wrong favicon body")
		}
	}
	if calls != 3 {
		t.Fatalf("favicon/HTML branding calls=%d", calls)
	}
	current.Store(nil) // license/dependency unavailable on the next request
	icon := do(h, http.MethodGet, "ads.example", "/favicon.ico", "192.0.2.1:53")
	if icon.Header().Get("Content-Type") != "image/x-icon" || icon.Header().Get("Cache-Control") != "no-store" || !bytes.Equal(icon.Body.Bytes(), favicon) {
		t.Fatal("unavailable branding must use built-in favicon without stale caching")
	}
	if !strings.Contains(page.Body.String(), `href="`+pngURL(raw)+`"`) || strings.Contains(page.Body.String(), `href="/favicon.ico"`) {
		t.Fatal("held page favicon must remain inline from its own selection")
	}
	legacy, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	w := do(legacy.Handler("http"), http.MethodGet, "ads.example", "/favicon.ico", "192.0.2.1:53")
	if w.Header().Get("Cache-Control") != "public, max-age=86400" || !bytes.Equal(w.Body.Bytes(), favicon) {
		t.Fatal("legacy built-in favicon caching changed")
	}
}

type brandingPanicFinder struct{}

func (brandingPanicFinder) Find(context.Context, netip.Addr, string, time.Time) (Reason, bool, string) {
	panic("pure preview read query history")
}

func TestPreviewDocumentWithSettingsUsesOnlySuppliedPresentation(t *testing.T) {
	brand, err := PrepareBranding(BrandingInput{Name: "Original", AccentColor: "#35a866"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s, err := New(Options{
		Node: "node-a", Now: func() time.Time { return now },
		Settings:        Settings{Title: "successor title", Message: "successor message"},
		Branding:        func(context.Context) (*Branding, error) { panic("pure preview selected live branding") },
		RequestsAllowed: func() bool { panic("pure preview sampled live license") },
		Finder:          brandingPanicFinder{},
		Policy:          func(netip.Addr, time.Time) string { panic("pure preview sampled policy") },
	})
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{Title: "Original title", Message: "Original message", ShowDetails: true, Contact: "it@example.net", ContactURL: "mailto:it@example.net", DashboardURL: "https://original.example", Requests: true}
	client := netip.MustParseAddr("192.0.2.1")
	doc := s.PreviewDocumentWithSettings(context.Background(), "ADS.Example.", client, settings, brand)
	body := string(doc.Body)
	for _, want := range []string{"Original title", "Original message", "Blocked by Original DNS.", "ads.example", "192.0.2.1", "node-a", "2026-10-16 12:00:00 UTC", "default/example-list", "<fieldset disabled>", "mailto:it@example.net", "https://original.example/queries?domain=ads.example"} {
		if !strings.Contains(body, want) {
			t.Errorf("pure preview lacks %q", want)
		}
	}
	if strings.Contains(body, "successor") || !strings.Contains(doc.CSP, "form-action 'none'") || !strings.Contains(doc.CSP, "frame-ancestors 'self'") {
		t.Fatal("pure preview mixed live state or enabled form submissions")
	}
	s.Reconfigure(Settings{Title: "a newer title", ShowDetails: false, Requests: false})
	next := s.PreviewDocumentWithSettings(context.Background(), "ADS.Example.", client, settings, brand)
	if !bytes.Equal(doc.Body, next.Body) || doc.CSP != next.CSP {
		t.Fatal("held settings changed after live reconfiguration")
	}
	settings.Requests, settings.ShowDetails = false, false
	settings.DashboardURL = ""
	without := s.PreviewDocumentWithSettings(context.Background(), "<script>", netip.Addr{}, settings, nil)
	checkBrandingDocument(t, without, true)
	if strings.Contains(string(without.Body), "<form") || strings.Contains(string(without.Body), "example-list") || !strings.Contains(string(without.Body), "blocked.example") || without.CSP != PreviewCSP {
		t.Fatal("supplied request entitlement/details/default projection not respected")
	}
}

type brandingHoldKey struct{}

func TestBrandingHeldSelectionAndConcurrentReplacement(t *testing.T) {
	first, err := PrepareBranding(BrandingInput{Name: "Original", AccentColor: "#123456"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := PrepareBranding(BrandingInput{Name: "Successor", AccentColor: "#654321"})
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[Branding]
	current.Store(first)
	captured, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(resume) }) })
	s, err := New(Options{Branding: func(ctx context.Context) (*Branding, error) {
		selected := current.Load()
		if ctx.Value(brandingHoldKey{}) != nil {
			close(captured)
			<-resume
		}
		return selected, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler("http")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, "http://ads.example/", nil)
		r = r.WithContext(context.WithValue(r.Context(), brandingHoldKey{}, true))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		done <- w
	}()
	select {
	case <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter branding callback")
	}
	current.Store(second)
	release.Do(func() { close(resume) })
	select {
	case w := <-done:
		if w.Header().Get("Content-Security-Policy") != first.csp || !strings.Contains(w.Body.String(), "Blocked by Original DNS.") || strings.Contains(w.Body.String(), "Successor") {
			t.Fatal("held response mixed a successor appearance or CSP")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held request did not finish")
	}
	fresh := do(h, http.MethodGet, "ads.example", "/", "192.0.2.1:53")
	if fresh.Header().Get("Content-Security-Policy") != second.csp || !strings.Contains(fresh.Body.String(), "Blocked by Successor DNS.") {
		t.Fatal("fresh request did not adopt successor")
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 12 {
				if i%2 == 0 {
					current.Store(first)
				} else {
					current.Store(second)
				}
				w := do(h, http.MethodGet, "ads.example", "/", "192.0.2.1:53")
				body := w.Body.String()
				brand := first
				if strings.Contains(body, "Blocked by Successor DNS.") {
					brand = second
				} else if !strings.Contains(body, "Blocked by Original DNS.") {
					t.Error("concurrent request lost selected brand")
				}
				if w.Header().Get("Content-Security-Policy") != brand.csp {
					t.Error("concurrent request mixed body and CSP")
				}
			}
		})
	}
	wg.Wait()
}

type brandingBarrierFinder struct {
	entered chan struct{}
	resume  <-chan struct{}
}

func (f brandingBarrierFinder) Find(ctx context.Context, _ netip.Addr, _ string, _ time.Time) (Reason, bool, string) {
	close(f.entered)
	select {
	case <-f.resume:
		return Reason{Kind: KindList, List: "default/ads"}, true, ""
	case <-ctx.Done():
		return Reason{}, false, noteNotFound
	}
}

func TestBrandingExpiryDuringDetailLookupUsesDefault(t *testing.T) {
	custom, err := PrepareBranding(BrandingInput{Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(deadline.Add(-time.Second).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	entered, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(resume) }) })
	var calls atomic.Int32
	s, err := New(Options{
		Now: now, Settings: Settings{ShowDetails: true},
		Finder: brandingBarrierFinder{entered: entered, resume: resume},
		Branding: func(context.Context) (*Branding, error) {
			calls.Add(1)
			if !now().Before(deadline) {
				return nil, errors.New("license expired")
			}
			return custom, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- do(s.Handler("http"), http.MethodGet, "ads.example", "/", "192.0.2.1:53")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("detail lookup did not start")
	}
	if calls.Load() != 0 {
		t.Error("branding selected before detail lookup finished")
	}
	clock.Store(deadline.UnixNano()) // exact expiry while the detail lookup is held
	release.Do(func() { close(resume) })
	select {
	case w := <-done:
		if calls.Load() != 1 || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Blocked by kudurru DNS.") || strings.Contains(w.Body.String(), "Acme") || w.Header().Get("Content-Security-Policy") != CSP {
			t.Fatal("rendering used a brand selected before lookup-time expiry")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("detail lookup did not finish")
	}
}
