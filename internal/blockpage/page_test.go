package blockpage

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, p Page) string {
	t.Helper()
	b, err := RenderBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRenderShowsTheFacts(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	html := render(t, Page{
		Domain: "ads.example", Client: "10.0.0.5", Node: "kudurru-0", Time: at, ShowDetails: true,
		Reason:  &Reason{Kind: KindList, List: "default/ads", Rule: "||ads.example^", Policy: "default/default"},
		Contact: "it@example.net", ContactURL: "mailto:it@example.net",
		DashboardURL: "https://10.0.0.53:8080/queries?domain=ads.example",
	})
	for _, want := range []string{
		"<title>This site is blocked: ads.example</title>",
		`<div class="domain">ads.example</div>`,
		DefaultMessage,
		"The name is on a block list.",
		"<dt>List</dt><dd><code>default/ads</code></dd>",
		"<dt>Rule</dt><dd><code>||ads.example^</code></dd>",
		"<dt>Policy</dt><dd><code>default/default</code></dd>",
		"<code>10.0.0.5</code>", "kudurru-0", "2026-10-11 09:30:00 UTC",
		`<a href="mailto:it@example.net" rel="noreferrer">it@example.net</a>`,
		`href="https://10.0.0.53:8080/queries?domain=ads.example"`,
		`<img src="data:image/png;base64,`, `srcset="data:image/png;base64,`,
		`<link rel="icon" href="/favicon.ico">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(html, "Preview") {
		t.Error("only the preview carries the preview badge")
	}
}

func TestRenderHidesDetails(t *testing.T) {
	html := render(t, Page{
		Domain: "ads.example", Client: "10.0.0.5", ShowDetails: false,
		Reason:       &Reason{Kind: KindList, List: "default/ads", Rule: "||ads.example^"},
		ReasonNote:   noteNotFound,
		DashboardURL: "https://10.0.0.53:8080/queries?domain=ads.example",
	})
	for _, leak := range []string{"default/ads", "||ads.example^", "queries?domain", noteNotFound} {
		if strings.Contains(html, leak) {
			t.Errorf("showDetails false shows %q", leak)
		}
	}
	if !strings.Contains(html, "Ask your administrator if you think this is a mistake.") {
		t.Error("the contact line without a contact")
	}
}

func TestReasonSummaries(t *testing.T) {
	for _, tc := range []struct {
		r          Reason
		summary    string
		label, val string
	}{
		{Reason{Kind: KindService, List: "service:tiktok"}, "blocked service (tiktok)", "Service", "tiktok"},
		{Reason{Kind: KindSafeSearch, List: "safesearch:google"}, "Safe search", "Search engine", "google"},
		{Reason{Kind: KindPolicy}, "allow list", "", ""},
		{Reason{Kind: KindCNAME, List: "default/ads"}, "points to another name", "List", "default/ads"},
	} {
		html := render(t, Page{Domain: "x.example", ShowDetails: true, Reason: &tc.r})
		if !strings.Contains(html, tc.summary) {
			t.Errorf("%s: summary lacks %q", tc.r.Kind, tc.summary)
		}
		if tc.label != "" && !strings.Contains(html, "<dt>"+tc.label+"</dt><dd><code>"+tc.val+"</code></dd>") {
			t.Errorf("%s: no %s row", tc.r.Kind, tc.label)
		}
	}
}

// TestRenderEscapes: every value is text. Settings, the Host-derived
// domain, list names and rules cannot inject markup, and an unsafe
// contact URL is neutralised by html/template even if it got past the
// config validation.
func TestRenderEscapes(t *testing.T) {
	evil := `"><script>alert(1)</script><img src=x onerror=alert(2)>`
	html := render(t, Page{
		Title: evil, Message: evil, Domain: evil, Client: evil, Node: evil, ShowDetails: true,
		Reason:  &Reason{Kind: KindList, List: evil, Rule: evil, Policy: evil},
		Contact: evil, ContactURL: "javascript:alert(3)",
		DashboardURL: "javascript:alert(4)",
	})
	for _, bad := range []string{"<script>", "<img src=x", "javascript:alert"} {
		if strings.Contains(html, bad) {
			t.Errorf("unescaped %q in the page", bad)
		}
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the hostile text should appear escaped")
	}
	if n := strings.Count(html, "<script"); n != 0 {
		t.Errorf("%d script elements", n)
	}
}

// TestCSPHashMatchesStyle: the style element is exactly the hashed CSS, so
// the strict CSP admits it, and the page has no inline script, handler or
// external URL.
func TestCSPHashMatchesStyle(t *testing.T) {
	html := render(t, Page{Domain: "ads.example"})
	m := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no style element")
	}
	sum := sha256.Sum256([]byte(m[1]))
	if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(CSP, want) {
		t.Errorf("CSP %q does not allow the style (%s)", CSP, want)
	}
	for _, directive := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(CSP, directive) {
			t.Errorf("CSP lacks %s", directive)
		}
	}
	if strings.Contains(CSP, "unsafe-inline") || strings.Contains(CSP, "script-src") {
		t.Errorf("CSP relaxes scripts or inline styles: %s", CSP)
	}
	for _, bad := range []string{"<script", " onload=", " onerror=", "http://", "https://", "style=\""} {
		if strings.Contains(html, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	if !strings.Contains(PreviewCSP, "frame-ancestors 'self'") {
		t.Errorf("preview CSP: %s", PreviewCSP)
	}
}
