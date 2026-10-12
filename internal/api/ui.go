package api

import (
	"bytes"
	"html"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"testing/fstest"
	"time"
)

// placeholderIndex is served when no dashboard bundle is embedded (a
// build without `make web`). It loads nothing from elsewhere, so the
// strict CSP holds.
const placeholderIndex = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<base href="/">
<title>kudurru</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
</head>
<body style="font-family: system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; line-height: 1.5">
<h1>kudurru</h1>
<p>The dashboard is not part of this build. The REST API is served under
<a href="api/v1/docs">api/v1</a> (OpenAPI document: <a href="api/v1/openapi.json">api/v1/openapi.json</a>).</p>
</body>
</html>
`

var baseHrefRE = regexp.MustCompile(`(?i)<base\s+href="[^"]*"\s*/?>`)

// uiHandler serves the single-page app from an fs.FS: files by path, and
// index.html (with <base href> rewritten to the path prefix) for every
// other GET that is not an API path, so client-side routes survive a
// reload.
type uiHandler struct {
	fsys  fs.FS
	index []byte
}

// newUIHandler builds the SPA handler; a nil fsys serves the placeholder.
func newUIHandler(fsys fs.FS, prefix string) http.Handler {
	if fsys == nil {
		fsys = fstest.MapFS{"index.html": {Data: []byte(placeholderIndex)}}
	}
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		index = []byte(placeholderIndex)
	}
	base := html.EscapeString(prefix + "/")
	replaced := false
	index = baseHrefRE.ReplaceAllFunc(index, func(m []byte) []byte {
		if replaced {
			return m
		}
		replaced = true
		return []byte(`<base href="` + base + `">`)
	})
	return &uiHandler{fsys: fsys, index: index}
}

func (h *uiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, r, newError(http.StatusMethodNotAllowed, KeyMethod, r.Method+" is not allowed here", ""))
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name != "" && name != "index.html" {
		if b, err := fs.ReadFile(h.fsys, name); err == nil {
			ctype := mime.TypeByExtension(path.Ext(name))
			if ctype == "" {
				ctype = http.DetectContentType(b)
			}
			w.Header().Set("Content-Type", ctype)
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
			return
		}
		// A missing file with an extension is a 404, not the app.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(h.index))
}
