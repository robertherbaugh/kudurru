package blockpage

import (
	"bytes"
	"crypto/sha256"
	_ "embed" // brand assets
	"encoding/base64"
	"html/template"
	"io"
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
footer p{margin:0 0 6px}`

// styleHash is the CSP source of css.
var styleHash = func() string {
	sum := sha256.Sum256([]byte(css))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}()

// CSP is the Content-Security-Policy of the page: nothing loads but the
// inline style (by hash), the inlined logos and the favicon, nothing runs,
// nothing can be framed, posted or re-based.
var CSP = "default-src 'none'; style-src " + styleHash + "; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// PreviewCSP is CSP with framing allowed by the dashboard's own origin, so
// the Settings page can show the preview in an iframe.
var PreviewCSP = "default-src 'none'; style-src " + styleHash + "; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<meta name="referrer" content="no-referrer">
<title>{{.Title}}{{with .Domain}}: {{.}}{{end}}</title>
<link rel="icon" href="/favicon.ico">
<style>` + css + `</style>
</head>
<body>
<main>
<div class="logo"><picture><source media="(prefers-color-scheme: dark)" srcset="{{.LogoDark}}"><img src="{{.LogoLight}}" width="180" height="60" alt="kudurru"></picture></div>
{{if .Preview}}<div class="preview">Preview</div>{{end}}
<h1>{{.Title}}</h1>
{{with .Domain}}<div class="domain">{{.}}</div>{{end}}
<p>{{.Message}}</p>
<dl>
{{- if .ShowDetails}}{{with .Reason}}
<dt>Reason</dt><dd>{{.Summary}}</dd>
{{- with .ListLabel}}<dt>{{.Label}}</dt><dd><code>{{.Value}}</code></dd>{{end}}
{{- with .Rule}}<dt>Rule</dt><dd><code>{{.}}</code></dd>{{end}}
{{- with .Policy}}<dt>Policy</dt><dd><code>{{.}}</code></dd>{{end}}
{{- end}}{{end}}
{{- with .Client}}
<dt>Your device</dt><dd><code>{{.}}</code></dd>{{end}}
{{- with .Node}}
<dt>Server</dt><dd>{{.}}</dd>{{end}}
<dt>Time</dt><dd>{{.TimeText}}</dd>
</dl>
{{if and .ShowDetails (not .Reason) .ReasonNote}}<p class="note">{{.ReasonNote}}</p>{{end}}
<footer>
{{- if .ContactURL}}
<p>Ask your administrator if you think this is a mistake: <a href="{{.ContactURL}}" rel="noreferrer">{{.Contact}}</a></p>
{{- else}}
<p>Ask your administrator if you think this is a mistake.</p>
{{- end}}
{{- if and .ShowDetails .DashboardURL}}
<p>Administrator? <a href="{{.DashboardURL}}" rel="noreferrer">See this name in the kudurru query log</a>.</p>
{{- end}}
<p>Blocked by kudurru DNS.</p>
</footer>
</main>
</body>
</html>
`

var pageTemplate = template.Must(template.New("blockpage").Parse(pageHTML))

// view adds the derived values the template reads.
type view struct {
	Page
	LogoLight template.URL
	LogoDark  template.Srcset
	TimeText  string
}

var (
	logoLightURL = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(logoLight))   //nolint:gosec // constant embedded image
	logoDarkSet  = template.Srcset("data:image/png;base64," + base64.StdEncoding.EncodeToString(logoDark)) //nolint:gosec // constant embedded image
)

// Render writes p as HTML. The template escapes every field; a hostile
// Host header or setting renders as text.
func Render(w io.Writer, p Page) error {
	if p.Title == "" {
		p.Title = DefaultTitle
	}
	if p.Message == "" {
		p.Message = DefaultMessage
	}
	if p.Time.IsZero() {
		p.Time = time.Now()
	}
	return pageTemplate.Execute(w, view{
		Page:      p,
		LogoLight: logoLightURL,
		LogoDark:  logoDarkSet,
		TimeText:  p.Time.UTC().Format("2006-01-02 15:04:05 UTC"),
	})
}

// RenderBytes is Render into a buffer.
func RenderBytes(p Page) ([]byte, error) {
	var b bytes.Buffer
	if err := Render(&b, p); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
