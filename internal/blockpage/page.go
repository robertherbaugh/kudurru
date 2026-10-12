package blockpage

import (
	"crypto/sha256"
	_ "embed" // brand assets
	"encoding/base64"
	"html"
	"io"
	"strings"
	"time"
)

// Brand assets: downscaled copies of docs/brand/assets
// (kudurru-logo-mixed{,-paper}.png as web/src/assets has them, and
// favicon.ico), written by web/scripts/brand-assets.py. The logos are
// inlined as data: URIs so the page makes no request but /favicon.ico.
var (
	//go:embed assets/logo-light.png
	logoLight []byte
	//go:embed assets/logo-dark.png
	logoDark []byte
	//go:embed assets/favicon.ico
	favicon []byte
)

// Defaults for empty blockPage.title and blockPage.message.
const (
	DefaultTitle   = "This site is blocked"
	DefaultMessage = "The administrator of this network has blocked access to this domain."
)

// Page is what one rendering shows. Every string is plain text: the
// template escapes it for its context (element text, attribute, URL).
type Page struct {
	Title   string
	Message string
	// Domain is the blocked name from the Host header (or TLS SNI); empty
	// when the request named no valid domain (an IP literal, garbage).
	Domain string
	// Client is the requesting address, Node this node's name, Time the
	// request time.
	Client string
	Node   string
	Time   time.Time
	// ShowDetails shows Reason and the dashboard link.
	ShowDetails bool
	// Reason is why the name is blocked, nil when unknown; ReasonNote says
	// why no reason is shown (query log privacy, no recent query).
	Reason     *Reason
	ReasonNote string
	// Contact is the administrator contact as configured and ContactURL
	// its validated mailto: or http(s) link (config.BlockPageContactURL).
	Contact    string
	ContactURL string
	// DashboardURL links administrators to the query log for Domain;
	// empty omits the link.
	DashboardURL string
	// Preview marks the admin preview (GET /api/v1/blockpage/preview).
	Preview bool
	// Request is the unblock request section; nil omits it.
	Request *RequestView
}

// css is the page's only style sheet, inlined and allowed by its hash in
// the Content-Security-Policy (no 'unsafe-inline'). The colours are the
// brand tokens of docs/brand/tokens.css.
const css = `:root{--ink:#182522;--clay:#B65C3A;--paper:#F7F3EA;--stone:#D9D1C2;--bg:var(--paper);--fg:var(--ink);--card:#FFFFFF;--muted:#4A5552;--line:var(--stone);--accent:#9A4A2C;color-scheme:light dark}
@media (prefers-color-scheme:dark){:root{--bg:#101917;--fg:var(--paper);--card:var(--ink);--muted:#B9B3A6;--line:#2E3B38;--accent:#E08A66}}
*{box-sizing:border-box}
html,body{margin:0;padding:0}
body{min-height:100vh;display:flex;align-items:center;justify-content:center;background:var(--bg);color:var(--fg);font:16px/1.5 Inter,ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;padding:24px}
main{width:100%;max-width:640px;background:var(--card);border:1px solid var(--line);border-top:4px solid var(--clay);border-radius:12px;padding:32px}
.logo img{display:block;width:180px;height:60px;margin:0 0 24px -6px}
h1{font-size:1.6rem;line-height:1.25;margin:0 0 12px}
p{margin:0 0 16px}
.domain{font:600 1.05rem/1.4 "JetBrains Mono",ui-monospace,SFMono-Regular,Consolas,monospace;word-break:break-all;background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:10px 14px;margin:0 0 20px}
dl{display:grid;grid-template-columns:max-content 1fr;gap:6px 16px;margin:0 0 20px;font-size:.95rem}
dt{color:var(--muted)}
dd{margin:0;word-break:break-word}
code{font-family:"JetBrains Mono",ui-monospace,SFMono-Regular,Consolas,monospace;font-size:.9em}
.note{color:var(--muted);font-size:.9rem}
.preview{display:inline-block;font-size:.8rem;font-weight:600;letter-spacing:.04em;text-transform:uppercase;color:var(--card);background:var(--clay);border-radius:4px;padding:2px 8px;margin:0 0 16px}
a{color:var(--accent)}
footer{border-top:1px solid var(--line);margin-top:8px;padding-top:16px;font-size:.9rem;color:var(--muted)}
footer p{margin:0 0 6px}
.request{border-top:1px solid var(--line);margin:4px 0 16px;padding-top:16px}
h2{font-size:1.1rem;margin:0 0 8px}
.flash{background:var(--bg);border:1px solid var(--line);border-left:4px solid var(--clay);border-radius:8px;padding:10px 14px}
label{display:block;font-size:.9rem;color:var(--muted);margin:0 0 4px}
input,textarea{display:block;width:100%;font:inherit;color:var(--fg);background:var(--bg);border:1px solid var(--line);border-radius:8px;padding:8px 10px;margin:0 0 12px}
textarea{min-height:5em;resize:vertical}
button{font:inherit;font-weight:600;color:#FFFFFF;background:var(--accent);border:0;border-radius:8px;padding:9px 18px;cursor:pointer}
button:disabled{opacity:.6;cursor:default}
fieldset{border:0;margin:0;padding:0}
.hp{position:absolute;left:-10000px;width:1px;height:1px;overflow:hidden}`

// styleHash is the CSP source of css.
var styleHash = func() string {
	sum := sha256.Sum256([]byte(css))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}()

// CSP is the Content-Security-Policy of the page: nothing loads but the
// inline style (by hash), the inlined logos and the favicon, nothing runs,
// nothing can be framed or re-based, and the only form posts to the page's
// own origin (the unblock request).
var CSP = "default-src 'none'; style-src " + styleHash + "; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// PreviewCSP is CSP with framing allowed by the dashboard's own origin, so
// the Settings page can show the preview in an iframe.
var PreviewCSP = "default-src 'none'; style-src " + styleHash + "; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"

// timeLayout formats the request time on the page.
const timeLayout = "2006-01-02 15:04:05 UTC"

// The logos as data: URIs: constants of the binary, never escaped.
var (
	logoLightURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(logoLight)
	logoDarkURL  = "data:image/png;base64," + base64.StdEncoding.EncodeToString(logoDark)
)

// head is everything up to the logo; it holds no value.
const head = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<meta name="referrer" content="same-origin">
`

// Render writes p as HTML. Every value is written through esc (element
// text and quoted attribute values alike) and links pass safeURL, so a
// hostile Host header, setting, list name, rule or request text renders
// as text. It is hand-written rather than an html/template: executing a
// template makes the linker keep every exported method of the binary
// (reflect.Value.MethodByName), which costs megabytes.
func Render(w io.Writer, p Page) error {
	_, err := io.WriteString(w, renderString(p))
	return err
}

// esc escapes s for element text and double-quoted attribute values.
func esc(s string) string { return html.EscapeString(s) }

// safeURL returns u when it is an http(s) or mailto: URL, else "".
func safeURL(u string) string {
	l := strings.ToLower(u)
	if strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "mailto:") {
		return u
	}
	return ""
}

func renderString(p Page) string {
	if p.Title == "" {
		p.Title = DefaultTitle
	}
	if p.Message == "" {
		p.Message = DefaultMessage
	}
	if p.Time.IsZero() {
		p.Time = time.Now()
	}
	var b strings.Builder
	b.Grow(len(head) + len(css) + len(logoLightURL) + len(logoDarkURL) + 4096)
	w := func(parts ...string) {
		for _, s := range parts {
			b.WriteString(s)
		}
	}
	w(head, "<title>", esc(p.Title))
	if p.Domain != "" {
		w(": ", esc(p.Domain))
	}
	w("</title>\n<link rel=\"icon\" href=\"/favicon.ico\">\n<style>", css, "</style>\n</head>\n<body>\n<main>\n")
	w(`<div class="logo"><picture><source media="(prefers-color-scheme: dark)" srcset="`, logoDarkURL, `"><img src="`, logoLightURL,
		`" width="180" height="60" alt="kudurru"></picture></div>`, "\n")
	if p.Preview {
		w(`<div class="preview">Preview</div>`, "\n")
	}
	w("<h1>", esc(p.Title), "</h1>\n")
	if p.Domain != "" {
		w(`<div class="domain">`, esc(p.Domain), "</div>\n")
	}
	w("<p>", esc(p.Message), "</p>\n<dl>")
	if r := p.Reason; p.ShowDetails && r != nil {
		w("\n<dt>Reason</dt><dd>", esc(r.Summary()), "</dd>")
		if lv := r.ListLabel(); lv != nil {
			w("<dt>", esc(lv.Label), "</dt><dd><code>", esc(lv.Value), "</code></dd>")
		}
		if r.Rule != "" {
			w("<dt>Rule</dt><dd><code>", esc(r.Rule), "</code></dd>")
		}
		if r.Policy != "" {
			w("<dt>Policy</dt><dd><code>", esc(r.Policy), "</code></dd>")
		}
	}
	if p.Client != "" {
		w("\n<dt>Your device</dt><dd><code>", esc(p.Client), "</code></dd>")
	}
	if p.Node != "" {
		w("\n<dt>Server</dt><dd>", esc(p.Node), "</dd>")
	}
	w("\n<dt>Time</dt><dd>", p.Time.UTC().Format(timeLayout), "</dd>\n</dl>\n")
	if p.ShowDetails && p.Reason == nil && p.ReasonNote != "" {
		w(`<p class="note">`, esc(p.ReasonNote), "</p>\n")
	}
	if rq := p.Request; rq != nil {
		renderRequest(&b, rq)
	}
	w("<footer>\n")
	if u := safeURL(p.ContactURL); u != "" {
		w(`<p>Ask your administrator if you think this is a mistake: <a href="`, esc(u), `" rel="noreferrer">`, esc(p.Contact), "</a></p>\n")
	} else {
		w("<p>Ask your administrator if you think this is a mistake.</p>\n")
	}
	if u := safeURL(p.DashboardURL); p.ShowDetails && u != "" {
		w(`<p>Administrator? <a href="`, esc(u), `" rel="noreferrer">See this name in the kudurru query log</a>.</p>`, "\n")
	}
	w("<p>Blocked by kudurru DNS.</p>\n</footer>\n</main>\n</body>\n</html>\n")
	return b.String()
}

// renderRequest writes the unblock request section: the outcome of the
// last post, the status of the client's request and the form (a plain
// POST to RequestPath; no script).
func renderRequest(b *strings.Builder, rq *RequestView) {
	b.WriteString(`<section class="request">` + "\n<h2>Request access</h2>\n")
	if rq.Flash != "" {
		b.WriteString(`<p class="flash" role="status">` + esc(rq.Flash) + "</p>\n")
	}
	if rq.Status != "" {
		b.WriteString("<p>" + esc(rq.Status) + "</p>\n")
	}
	if rq.Note != "" {
		b.WriteString("<p>Note from the administrator: " + esc(rq.Note) + "</p>\n")
	}
	if rq.Form {
		disabled, button := "", "Request access"
		if rq.Disabled {
			disabled = " disabled"
		}
		if rq.Update {
			button = "Update request"
		}
		b.WriteString(`<form method="post" action="` + esc(RequestPath) + `">` + "\n<fieldset" + disabled + ">\n" +
			`<label for="kr-name">Your name (optional)</label>` + "\n" +
			`<input id="kr-name" name="` + fieldName + `" maxlength="100" autocomplete="name">` + "\n" +
			`<label for="kr-contact">How to reach you (optional)</label>` + "\n" +
			`<input id="kr-contact" name="` + fieldContact + `" maxlength="254" autocomplete="email">` + "\n" +
			`<label for="kr-reason">Why do you need this site?</label>` + "\n" +
			`<textarea id="kr-reason" name="` + fieldReason + `" maxlength="500"></textarea>` + "\n" +
			`<div class="hp" aria-hidden="true"><label for="kr-website">Leave this empty</label><input id="kr-website" name="` + fieldHoneypot + `" tabindex="-1" autocomplete="off"></div>` + "\n" +
			`<button type="submit">` + button + "</button>\n</fieldset>\n</form>\n")
	}
	b.WriteString("</section>\n")
}

// RenderBytes is Render into a buffer.
func RenderBytes(p Page) ([]byte, error) {
	return []byte(renderString(p)), nil
}
