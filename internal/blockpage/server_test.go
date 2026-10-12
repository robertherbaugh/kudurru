package blockpage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// fixedFinder answers one reason for every lookup.
type fixedFinder struct {
	r    Reason
	ok   bool
	note string
	got  []string
}

func (f *fixedFinder) Find(_ context.Context, client netip.Addr, domain string, _ time.Time) (Reason, bool, string) {
	f.got = append(f.got, client.String()+" "+domain)
	return f.r, f.ok, f.note
}

func newTestServer(t *testing.T, st Settings, f Finder) (*Server, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	s, err := New(Options{Node: "node-a", Finder: f, Settings: st, Registerer: reg,
		Policy: func(netip.Addr, time.Time) string { return "default/kids" }})
	if err != nil {
		t.Fatal(err)
	}
	return s, reg
}

func do(h http.Handler, method, host, path, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://placeholder"+path, nil)
	r.Host = host
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandlerServesThePage(t *testing.T) {
	f := &fixedFinder{r: Reason{Kind: KindList, List: "default/ads", Rule: "||ads.example^"}, ok: true}
	s, reg := newTestServer(t, Settings{ShowDetails: true, DashboardURL: "https://192.0.2.53:8080"}, f)
	h := s.Handler("http")
	w := do(h, http.MethodGet, "Ads.Example.:80", "/some/path?q=1", "10.0.0.5:51000")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`<div class="domain">ads.example</div>`, "default/ads", "default/kids", "10.0.0.5", "node-a",
		`href="https://192.0.2.53:8080/queries?domain=ads.example"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if len(f.got) != 1 || f.got[0] != "10.0.0.5 ads.example" {
		t.Errorf("finder asked %v", f.got)
	}
	hd := w.Header()
	for k, v := range map[string]string{
		"Content-Type": "text/html; charset=utf-8", "Content-Security-Policy": CSP, "X-Frame-Options": "DENY",
		"X-Content-Type-Options": "nosniff", "Cache-Control": "no-store", "X-Kudurru-Block": "1", "Referrer-Policy": "same-origin",
	} {
		if got := hd.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if hd.Get("Strict-Transport-Security") != "" {
		t.Error("the block page must never send HSTS")
	}

	head := do(h, http.MethodHead, "ads.example", "/", "10.0.0.5:51000")
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD: %d with %d bytes", head.Code, head.Body.Len())
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, "CONNECT", http.MethodOptions} {
		w := do(h, m, "ads.example", "/", "10.0.0.5:51000")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: %d Allow=%q", m, w.Code, w.Header().Get("Allow"))
		}
	}
	if got := testutil.ToFloat64(s.requests.WithLabelValues("http")); got != 7 {
		t.Errorf("kudurru_blockpage_requests_total{scheme=http} = %v", got)
	}
	if n, err := testutil.GatherAndCount(reg, "kudurru_blockpage_requests_total"); err != nil || n != 1 {
		t.Errorf("metric registered: %d %v", n, err)
	}
}

func TestHandlerStaticPaths(t *testing.T) {
	s, _ := newTestServer(t, Settings{}, nil)
	h := s.Handler("http")
	w := do(h, http.MethodGet, "ads.example", "/robots.txt", "10.0.0.5:1")
	if w.Code != 200 || w.Body.String() != "User-agent: *\nDisallow: /\n" {
		t.Errorf("robots.txt: %d %q", w.Code, w.Body.String())
	}
	w = do(h, http.MethodGet, "ads.example", "/favicon.ico", "10.0.0.5:1")
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/x-icon" || w.Body.Len() != len(favicon) || len(favicon) == 0 {
		t.Errorf("favicon: %d %s %d", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
	}
	w = do(h, http.MethodGet, "ads.example", "/health", "10.0.0.5:1")
	if w.Code != 200 || w.Body.String() != "ok\n" {
		t.Errorf("health: %d %q", w.Code, w.Body.String())
	}
}

// TestHandlerHostileHost: the Host header is the attacker-controlled
// input. Anything that is not a DNS name renders as "no domain"; nothing
// of it reaches the page or the finder.
func TestHandlerHostileHost(t *testing.T) {
	f := &fixedFinder{}
	s, _ := newTestServer(t, Settings{ShowDetails: true, DashboardURL: "https://192.0.2.53:8080"}, f)
	h := s.Handler("http")
	for _, host := range []string{
		`<script>alert(1)</script>`,
		`ads.example"><script>alert(1)</script>`,
		`ads.example/<svg/onload=alert(1)>`,
		`javascript:alert(1)`,
		`ads.example%3Cscript%3E`,
		"192.0.2.1", "[2001:db8::1]:80", "2001:db8::1", "1.2.3.4.5", "-bad.example", "a..b", strings.Repeat("a", 64) + ".example",
	} {
		w := do(h, http.MethodGet, host, "/", "10.0.0.5:1")
		body := w.Body.String()
		if w.Code != http.StatusOK {
			t.Errorf("%q: status %d", host, w.Code)
		}
		if strings.Contains(body, "<script>") || strings.Contains(body, "<svg") || strings.Contains(body, `class="domain"`) {
			t.Errorf("%q: hostile Host reached the page", host)
		}
		if !strings.Contains(body, noteNoDomain) {
			t.Errorf("%q: no 'no domain' note", host)
		}
		if strings.Contains(body, "queries?domain") {
			t.Errorf("%q: dashboard link without a domain", host)
		}
	}
	if len(f.got) != 0 {
		t.Errorf("finder consulted for hostile hosts: %v", f.got)
	}
}

func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{
		"Ads.Example":           "ads.example",
		"ads.example.":          "ads.example",
		"ads.example:8080":      "ads.example",
		"_dmarc.example":        "_dmarc.example",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
		"localhost":             "localhost",
		"":                      "",
		"10.0.0.1":              "",
		"10.0.0.1:80":           "",
		"[::1]:80":              "",
		"::1":                   "",
		"bücher.example":        "",
		"a b.example":           "",
		"example.123":           "",
	} {
		if got := normalizeDomain(in); got != want {
			t.Errorf("normalizeDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandlerTLSServerName(t *testing.T) {
	s, _ := newTestServer(t, Settings{}, nil)
	r := httptest.NewRequest(http.MethodGet, "https://placeholder/", nil)
	r.Host = "192.0.2.80"
	r.TLS.ServerName = "sni.example"
	w := httptest.NewRecorder()
	s.Handler("https").ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `<div class="domain">sni.example</div>`) {
		t.Error("the TLS server name names the domain when Host is an address")
	}
}

func TestRateLimit(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	reg := prometheus.NewRegistry()
	s, err := New(Options{Settings: Settings{RateLimit: 3}, Registerer: reg, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler("http")
	codes := func(remote string, n int) []int {
		var out []int
		for range n {
			out = append(out, do(h, http.MethodGet, "ads.example", "/", remote).Code)
		}
		return out
	}
	if got := codes("10.0.0.5:1", 4); got[2] != 200 || got[3] != http.StatusTooManyRequests {
		t.Errorf("burst of 3 then 429: %v", got)
	}
	if w := do(h, http.MethodGet, "ads.example", "/", "10.0.0.5:2"); w.Header().Get("Retry-After") != "60" {
		t.Errorf("429 without Retry-After: %v", w.Header())
	}
	if got := codes("10.0.0.6:1", 1); got[0] != 200 {
		t.Errorf("another client is not limited: %v", got)
	}
	// IPv6 clients share a bucket per /64.
	codes("[2001:db8::1]:1", 3)
	if got := codes("[2001:db8::2]:1", 1); got[0] != http.StatusTooManyRequests {
		t.Errorf("same /64: %v", got)
	}
	// /health is never limited.
	if w := do(h, http.MethodGet, "", "/health", "10.0.0.5:1"); w.Code != 200 {
		t.Errorf("health limited: %d", w.Code)
	}
	// Tokens come back at 3 per minute.
	now = now.Add(20 * time.Second)
	if got := codes("10.0.0.5:1", 2); got[0] != 200 || got[1] != http.StatusTooManyRequests {
		t.Errorf("after 20s: %v", got)
	}
	if got := testutil.ToFloat64(s.rateLimited.WithLabelValues("http")); got < 3 {
		t.Errorf("kudurru_blockpage_rate_limited_total = %v", got)
	}
	// 0 disables the limit.
	s.Reconfigure(Settings{RateLimit: 0})
	if got := codes("10.0.0.5:1", 10); got[9] != 200 {
		t.Errorf("rateLimit 0: %v", got)
	}
}

func TestLimiterSweep(t *testing.T) {
	now := time.Now()
	l := newLimiter(1, time.Minute, func() time.Time { return now })
	for i := range maxLimited {
		l.allow(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}))
	}
	if len(l.clients) != maxLimited {
		t.Fatalf("%d clients", len(l.clients))
	}
	l.allow(netip.MustParseAddr("192.0.2.1"))
	if len(l.clients) != 1 {
		t.Errorf("a full table of active clients resets: %d", len(l.clients))
	}
	now = now.Add(2 * time.Minute)
	l.allow(netip.MustParseAddr("192.0.2.2"))
	if len(l.clients) != 2 {
		t.Errorf("%d clients", len(l.clients))
	}
}

func TestPreview(t *testing.T) {
	f := &fixedFinder{note: noteNotFound}
	s, _ := newTestServer(t, Settings{ShowDetails: true}, f)
	body := string(s.Preview(context.Background(), "Tracker.Example", netip.MustParseAddr("192.0.2.10")))
	for _, want := range []string{`<div class="preview">Preview</div>`, `<div class="domain">tracker.example</div>`,
		"<code>default/example-list</code>", "<code>||tracker.example^</code>", "<code>192.0.2.10</code>"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	if len(f.got) != 0 {
		t.Errorf("the preview must not consult the query log or policies: %v", f.got)
	}
	if body := string(s.Preview(context.Background(), "<b>", netip.Addr{})); !strings.Contains(body, "blocked.example") || strings.Contains(body, "<b>") {
		t.Error("an invalid preview domain falls back to blocked.example")
	}
	s.Reconfigure(Settings{ShowDetails: false})
	if body := string(s.Preview(context.Background(), "x.example", netip.Addr{})); strings.Contains(body, "example-list") {
		t.Error("showDetails false hides the example reason too")
	}
}

func TestSettingsFrom(t *testing.T) {
	cfg, err := config.Load("", []string{
		"KUDURRU_BLOCKPAGE__ENABLED=true", "KUDURRU_BLOCKPAGE__IPV4=192.0.2.80", "KUDURRU_BLOCKPAGE__CONTACT=it@example.net",
		"KUDURRU_LISTENERS__API__ADDRESS=0.0.0.0:8443", "KUDURRU_LISTENERS__API__PATHPREFIX=/kudurru",
	})
	if err != nil {
		t.Fatal(err)
	}
	st := SettingsFrom(cfg)
	if st.ContactURL != "mailto:it@example.net" || !st.ShowDetails || st.Title == "" || st.RateLimit != 120 {
		t.Errorf("settings: %+v", st)
	}
	if st.DashboardURL != "https://192.0.2.80:8443/kudurru" {
		t.Errorf("derived dashboard URL %q", st.DashboardURL)
	}
	cfg.Listeners.API.TLS.Auto = config.APITLSOff
	cfg.Listeners.API.PathPrefix = ""
	if got := SettingsFrom(cfg).DashboardURL; got != "http://192.0.2.80:8443" {
		t.Errorf("plain-HTTP API: %q", got)
	}
	cfg.BlockPage.DashboardURL = "https://dns.example/admin/"
	if got := SettingsFrom(cfg).DashboardURL; got != "https://dns.example/admin" {
		t.Errorf("configured: %q", got)
	}
	cfg.BlockPage.DashboardURL, cfg.BlockPage.IPv4 = "", ""
	if got := SettingsFrom(cfg).DashboardURL; got != "" {
		t.Errorf("no usable host: %q", got)
	}
}

// TestRunServesHTTPAndHTTPS binds both listeners on loopback with a
// generated certificate and fetches the page and /health over each.
func TestRunServesHTTPAndHTTPS(t *testing.T) {
	certFile, keyFile := selfSignedForTest(t)
	reg := prometheus.NewRegistry()
	s, err := New(Options{HTTP: "127.0.0.1:0", HTTPS: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile, Registerer: reg, Node: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	httpAddr, httpsAddr := s.Addrs()
	client := &http.Client{Timeout: 5 * time.Second, Transport: insecureTransport()}
	for _, u := range []string{"http://" + httpAddr + "/", "https://" + httpsAddr + "/"} {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Host = "blocked.example"
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), "blocked.example") {
			t.Errorf("%s: %d", u, resp.StatusCode)
		}
	}
	if got := testutil.ToFloat64(s.requests.WithLabelValues("https")); got != 1 {
		t.Errorf("https requests = %v", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
}
