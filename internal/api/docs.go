package api

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The Scalar API reference (MIT, internal/api/scalar/README.md) is served
// from the binary instead of unpkg.com, so /api/v1/docs works offline,
// below listeners.api.pathPrefix and under a CSP that allows scripts from
// this origin only.
const (
	scalarVersion   = "1.44.20"
	scalarIntegrity = "sha384-tMz7GAo6dMy55x9tLFtH+sHtogji6Scmb+feBR31TAHmvSPRUTboK9H3M5NFaP4R"
	docsPath        = "/docs"
	scalarPath      = docsPath + "/scalar-" + scalarVersion + ".js"
)

//go:embed scalar/standalone.js.gz
var scalarGz []byte

// docsCSP replaces the API's default CSP on the docs page: the bundle
// comes from 'self'; Scalar compiles code at runtime ('unsafe-eval', as
// huma's own renderer allows) and injects its styles inline. connect-src
// 'self' keeps the "try it" client and Scalar's telemetry and request
// proxy (both disabled in the configuration) on this origin.
var docsCSP = strings.Join([]string{
	"default-src 'none'",
	"base-uri 'none'",
	"connect-src 'self'",
	"font-src 'self' data:",
	"form-action 'none'",
	"frame-ancestors 'none'",
	"img-src 'self' data:",
	"script-src 'self' 'unsafe-eval'",
	"style-src 'self' 'unsafe-inline'",
	"sandbox allow-same-origin allow-scripts allow-popups allow-popups-to-escape-sandbox allow-downloads",
}, "; ")

// scalarConfig is Scalar's data-configuration: no web fonts from
// fonts.scalar.com, no telemetry and no AI agent (which queries
// api.scalar.com through proxy.scalar.com; the CSP would block it anyway).
const scalarConfig = `{"withDefaultFonts":false,"telemetry":false,"agent":{"disabled":true}}`

// docsPage renders the reference page for the path prefix.
func docsPage(prefix string) []byte {
	base := prefix + BasePath
	return []byte(`<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="referrer" content="no-referrer">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>kudurru API Reference</title>
  </head>
  <body>
    <script id="api-reference" data-url="` + html.EscapeString(base+"/openapi.json") + `" data-configuration="` + html.EscapeString(scalarConfig) + `"></script>
    <script src="` + html.EscapeString(base+scalarPath) + `" integrity="` + scalarIntegrity + `"></script>
  </body>
</html>
`)
}

// registerDocs mounts GET /api/v1/docs and the bundle on the mux. Both are
// public like the OpenAPI document (registerMeta declares them).
func (s *Server) registerDocs() {
	page := docsPage(s.prefix)
	s.mux.HandleFunc("GET "+BasePath+docsPath, func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", docsCSP)
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		_, _ = w.Write(page)
	})
	s.mux.HandleFunc("GET "+BasePath+scalarPath, serveScalar)
}

// serveScalar sends the gzipped bundle as is to clients that accept gzip
// (every browser) and decompresses it for the rest. The versioned path
// makes it immutable.
func serveScalar(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Vary", "Accept-Encoding")
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h.Set("Content-Encoding", "gzip")
		h.Set("Content-Length", strconv.Itoa(len(scalarGz)))
		_, _ = w.Write(scalarGz)
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(scalarGz))
	if err != nil {
		http.Error(w, "docs bundle unavailable", http.StatusInternalServerError)
		return
	}
	_, _ = io.Copy(w, zr) //nolint:gosec // G110: the embedded, known bundle, not user input
}

// acceptsGzip reports whether an Accept-Encoding value allows gzip.
func acceptsGzip(v string) bool {
	for part := range strings.SplitSeq(v, ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		enc = strings.ToLower(strings.TrimSpace(enc))
		if enc != "gzip" && enc != "*" {
			continue
		}
		q := strings.TrimSpace(params)
		if q, ok := strings.CutPrefix(q, "q="); ok {
			if f, err := strconv.ParseFloat(q, 64); err == nil && f == 0 {
				continue
			}
		}
		return true
	}
	return false
}
