package blockpage

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/unblock"
)

type requestFixture struct {
	srv  *Server
	h    http.Handler
	book *unblock.Service
	set  config.UnblockRequests
	reg  *prometheus.Registry
	now  time.Time
}

func newRequestFixture(t *testing.T) *requestFixture {
	t.Helper()
	f := &requestFixture{set: unblock.Defaults, reg: prometheus.NewRegistry(), now: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return f.now }
	f.book = unblock.New(unblock.Options{Store: memory.New(), Node: "node-a", Now: clock, Settings: func() config.UnblockRequests { return f.set }})
	finder := &fixedFinder{r: Reason{Kind: KindList, List: "default/ads", Rule: "||ads.example^"}, ok: true}
	srv, err := New(Options{
		Node: "node-a", Finder: finder, Requests: f.book, Registerer: f.reg, Now: clock,
		Settings: Settings{ShowDetails: true, Requests: true, RequestRate: 5, BlockTTL: 10 * time.Minute},
		Policy:   func(netip.Addr, time.Time) string { return "default/kids" },
		Membership: func(netip.Addr, time.Time) ([]string, []string) {
			return []string{"default/kids"}, []string{"default/kids"}
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.srv, f.h = srv, srv.Handler("http")
	return f
}

// post submits the form as a browser on host would.
func (f *requestFixture) post(form url.Values, remote string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://placeholder"+RequestPath, strings.NewReader(form.Encode()))
	r.Host = "games.example"
	r.RemoteAddr = remote
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://games.example")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *requestFixture) page(path, remote string) string {
	return do(f.h, http.MethodGet, "games.example", path, remote).Body.String()
}

func wantRedirect(t *testing.T, w *httptest.ResponseRecorder, flash string) {
	t.Helper()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/?"+flashParam+"="+flash {
		t.Fatalf("%d %q, want 303 to flash %s: %s", w.Code, w.Header().Get("Location"), flash, w.Body.String())
	}
}

func TestRequestFormFlow(t *testing.T) {
	f := newRequestFixture(t)
	page := f.page("/", "10.0.20.5:4000")
	for _, want := range []string{`<form method="post" action="/.kudurru/request">`, `name="reason"`, `name="website"`, "Request access</button>"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
	w := f.post(url.Values{"name": {"Sam"}, "contact": {"sam@example.net"}, "reason": {"homework\r\nplease"}}, "10.0.20.5:4000")
	wantRedirect(t, w, flashSent)
	r := f.book.Lookup(netip.MustParseAddr("10.0.20.5"), "games.example")
	if r == nil || r.Spec.Domain != "games.example" || r.Spec.Reason != "homework\nplease" || r.Spec.RequesterName != "Sam" ||
		r.Spec.Block == nil || r.Spec.Block.List != "default/ads" || r.Spec.Block.Policy != "default/kids" || len(r.Spec.Groups) != 1 {
		t.Fatalf("stored %+v", r)
	}
	page = f.page("/?"+flashParam+"=sent", "10.0.20.5:4000")
	for _, want := range []string{"Your request was sent to the administrator.", "has not decided yet", "Update request</button>"} {
		if !strings.Contains(page, want) {
			t.Errorf("pending page lacks %q", want)
		}
	}
	// Another client of the same name sees a fresh form.
	if p := f.page("/", "10.0.20.6:4000"); strings.Contains(p, "has not decided yet") {
		t.Error("another client sees the request")
	}

	// Approved: no form, the retry hint.
	f.now = f.now.Add(time.Minute)
	if _, err := f.book.Approve(context.Background(), "default", r.Name, unblock.Decision{Duration: time.Hour, Note: "until <b>5</b>"}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(3 * time.Minute)
	page = f.page("/", "10.0.20.5:4000")
	if !strings.Contains(page, "An administrator allowed this name until") || !strings.Contains(page, "up to 7 more minutes") ||
		strings.Contains(page, "<form") || !strings.Contains(page, "until &lt;b&gt;5&lt;/b&gt;") {
		t.Fatalf("approved page:\n%s", page)
	}
	// Denied with a note: shown escaped, the form back.
	if _, err := f.book.Deny(context.Background(), "default", r.Name, `<script>alert("x")</script>`, "user:admin"); err != nil {
		t.Fatal(err)
	}
	page = f.page("/", "10.0.20.5:4000")
	if !strings.Contains(page, "declined your request") || !strings.Contains(page, "&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;") ||
		strings.Contains(page, "<script") || !strings.Contains(page, "Request access</button>") {
		t.Fatalf("denied page:\n%s", page)
	}
	if got := testutil.ToFloat64(f.srv.unblockRequests.WithLabelValues(flashSent)); got != 1 {
		t.Errorf("sent metric %v", got)
	}
}

func TestRequestFormRefusals(t *testing.T) {
	f := newRequestFixture(t)
	form := url.Values{"reason": {"x"}}
	// Cross-site posts.
	for _, hdr := range [][]string{{"Origin", "http://evil.example"}, {"Origin", "null"}, {"Sec-Fetch-Site", "cross-site"}} {
		if w := f.post(form, "10.0.0.1:1", hdr...); w.Code != http.StatusForbidden {
			t.Errorf("%v: %d, want 403", hdr, w.Code)
		}
	}
	// Not a form, too long, invalid.
	r := httptest.NewRequest(http.MethodPost, "http://placeholder"+RequestPath, strings.NewReader(`{"reason":"x"}`))
	r.Host, r.RemoteAddr = "games.example", "10.0.0.1:1"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("json: %d", w.Code)
	}
	wantRedirect(t, f.post(url.Values{"reason": {strings.Repeat("a", maxReasonLen+1)}}, "10.0.0.1:1"), flashInvalid)
	wantRedirect(t, f.post(url.Values{"name": {"\xff"}}, "10.0.0.1:1"), flashInvalid)
	if w := f.post(url.Values{"reason": {strings.Repeat("a", maxRequestBody)}}, "10.0.0.1:1"); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge body: %d", w.Code)
	}
	// The honeypot: looks sent, stores nothing.
	wantRedirect(t, f.post(url.Values{"reason": {"x"}, "website": {"http://spam.example"}}, "10.0.0.2:1"), flashSent)
	if f.book.Lookup(netip.MustParseAddr("10.0.0.2"), "games.example") != nil || f.book.Counts().Pending != 0 {
		t.Fatal("the honeypot post was stored")
	}
	// Other methods and paths.
	if w := do(f.h, http.MethodPost, "games.example", "/elsewhere", "10.0.0.1:1"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST elsewhere: %d", w.Code)
	}
	if w := do(f.h, http.MethodPut, "games.example", RequestPath, "10.0.0.1:1"); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Errorf("PUT: %d %q", w.Code, w.Header().Get("Allow"))
	}
	if w := do(f.h, http.MethodGet, "games.example", RequestPath, "10.0.0.1:1"); w.Code != http.StatusOK {
		t.Errorf("GET of the request path: %d", w.Code)
	}
	// No domain.
	r = httptest.NewRequest(http.MethodPost, "http://placeholder"+RequestPath, strings.NewReader("reason=x"))
	r.Host, r.RemoteAddr = "192.0.2.1", "10.0.0.1:1"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("IP host: %d", w.Code)
	}
	// Off.
	f.srv.Reconfigure(Settings{Requests: false})
	if w := f.post(form, "10.0.0.1:1"); w.Code != http.StatusNotFound {
		t.Errorf("off: %d", w.Code)
	}
	if strings.Contains(f.page("/", "10.0.0.1:1"), "<form") {
		t.Error("form shown while requests are off")
	}
}

func TestRequestRateLimitAndCap(t *testing.T) {
	f := newRequestFixture(t)
	// Five per hour per client; IPv6 per /64.
	for i := range 5 {
		wantRedirect(t, f.post(url.Values{"reason": {"x"}}, "[2001:db8:1:2::"+string(rune('a'+i))+"]:1"), flashSent)
	}
	wantRedirect(t, f.post(url.Values{"reason": {"x"}}, "[2001:db8:1:2::ff]:1"), flashLimited)
	f.now = f.now.Add(13 * time.Minute) // one token back
	wantRedirect(t, f.post(url.Values{"reason": {"x"}}, "[2001:db8:1:2::ff]:1"), flashSent)

	// The cap: a new client is told requests are paused; the form is gone.
	f.set.MaxPending = f.book.Counts().Pending
	wantRedirect(t, f.post(url.Values{"reason": {"x"}}, "10.9.9.9:1"), flashPaused)
	if p := f.page("/", "10.9.9.9:1"); strings.Contains(p, "<form") || !strings.Contains(p, "Requests are paused") {
		t.Errorf("paused page:\n%s", p)
	}
}

func TestPreviewShowsAnInertForm(t *testing.T) {
	f := newRequestFixture(t)
	p := string(f.srv.Preview(context.Background(), "x.example", netip.MustParseAddr("10.0.0.1")))
	if !strings.Contains(p, "<fieldset disabled>") || !strings.Contains(p, "Request access</button>") {
		t.Fatalf("preview:\n%s", p)
	}
}

func TestFormText(t *testing.T) {
	for _, c := range []struct {
		in, want  string
		multiline bool
	}{
		{"  Sam  ", "Sam", false},
		{"a\r\nb", "a b", false},
		{"a\r\nb", "a\nb", true},
		{"x‮y\x00z\tw", "xyz w", false},
	} {
		if got, ok := formText(c.in, 100, c.multiline); !ok || got != c.want {
			t.Errorf("formText(%q) = %q %v, want %q", c.in, got, ok, c.want)
		}
	}
	if _, ok := formText(strings.Repeat("é", 6), 5, false); ok {
		t.Error("6 characters accepted under a 5-character limit")
	}
	if _, ok := formText(strings.Repeat("é", 5), 5, false); !ok {
		t.Error("the limit counts characters, not bytes")
	}
}
