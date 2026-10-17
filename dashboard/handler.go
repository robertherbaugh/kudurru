package dashboard

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DefaultCSP is the Content-Security-Policy sent with every dashboard
// response. Scripts load only from the bundle (no inline or eval'd script,
// no CDN) and the SPA talks only to its own origin. 'unsafe-inline' for
// styles is needed because Astryx installs its popover position-try rules
// and React sets per-element geometry through style attributes; no script
// source is relaxed.
const DefaultCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; worker-src 'self'; " +
	"manifest-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

// Options configures Handler.
type Options struct {
	// BasePath is the URL path prefix the dashboard is served under: "" or
	// "/" at the root, "/kudurru" behind a reverse proxy that keeps the
	// prefix. The <base href> of index.html is rewritten to it, which is
	// how the bundle finds its assets and the API (/kudurru/api/v1). The
	// handler itself expects request paths with the prefix already
	// stripped (http.StripPrefix).
	BasePath string
	// FS replaces the embedded bundle (tests, development).
	FS fs.FS
	// ContentSecurityPolicy replaces DefaultCSP when non-empty.
	ContentSecurityPolicy string
}

// Handler serves the dashboard: files of the bundle with long-lived caching
// for the content-hashed assets/ directory, index.html (no-cache) for every
// other path that is not a missing bundle file (missingFile) so client-side
// routes survive a reload, compressed bodies when the client accepts them,
// and strict security headers. Paths under api/ are never answered with the
// SPA; mount the API before this handler.
//
// The build stores compressible files only as <name>.gz (and optionally
// <name>.br; dashboard/scripts/compress.ts), so the binary embeds the compressed
// bytes. Such a file is served as is to clients that accept the encoding
// and inflated once for those that do not; a file stored raw is gzipped on
// the fly once.
func Handler(o Options) http.Handler {
	f := o.FS
	if f == nil {
		f = FS
	}
	csp := o.ContentSecurityPolicy
	if csp == "" {
		csp = DefaultCSP
	}
	h := &handler{fs: f, csp: csp, files: map[string]*file{}}
	h.index = h.buildIndex(NormalizeBasePath(o.BasePath))
	return h
}

// NormalizeBasePath returns p with exactly one leading and one trailing
// slash ("" → "/", "kudurru" → "/kudurru/").
func NormalizeBasePath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "/"
	}
	return "/" + p + "/"
}

type handler struct {
	fs    fs.FS
	csp   string
	index *file

	mu    sync.Mutex
	files map[string]*file
}

// file is one response body, prepared once (the bundle is immutable). raw
// is nil for a file stored only precompressed until a client without gzip
// asks for it. etag names the content; each encoding adds a suffix.
type file struct {
	ctype string
	etag  string
	// stored: gz came from the bundle (<name>.gz), not from gzipped.
	stored bool
	br     []byte // precompressed brotli, optional

	rawOnce sync.Once
	raw     []byte
	rawErr  error

	gzOnce sync.Once
	gz     []byte
}

var baseHref = regexp.MustCompile(`(?i)<base\s+href="[^"]*"\s*/?>`)

func (h *handler) buildIndex(base string) *file {
	b, err := fs.ReadFile(h.fs, "index.html")
	if err != nil {
		b = []byte("<!doctype html><title>kudurru</title><p>The dashboard bundle is missing index.html.</p>")
	}
	tag := `<base href="` + html.EscapeString(base) + `" />`
	replaced := false
	b = baseHref.ReplaceAllFunc(b, func(m []byte) []byte {
		if replaced {
			return m
		}
		replaced = true
		return []byte(tag)
	})
	return newFile("text/html; charset=utf-8", b)
}

func newFile(ctype string, b []byte) *file {
	return &file{ctype: ctype, raw: b, etag: etagOf(b)}
}

// newPrecompressed is a file stored only as gzip (gz) and optionally brotli.
// Both encode the same content, so the gzip bytes name it.
func newPrecompressed(ctype string, gz, br []byte) *file {
	return &file{ctype: ctype, gz: gz, stored: true, br: br, etag: etagOf(gz)}
}

func etagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	setSecurityHeaders(w.Header(), h.csp)

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	switch {
	case name == "" || name == "index.html":
		h.serve(w, r, h.index, "no-cache")
		return
	case name == "api" || strings.HasPrefix(name, "api/"):
		http.NotFound(w, r)
		return
	}
	if f := h.lookup(name); f != nil {
		cache := "no-cache"
		if strings.HasPrefix(name, "assets/") {
			cache = "public, max-age=31536000, immutable"
		}
		h.serve(w, r, f, cache)
		return
	}
	// A missing file is a 404, never index.html: a stale script URL must
	// not get HTML back. Every other path is a client-side route, dots
	// included (/zones/home.example., /zones/example.js).
	if h.missingFile(name) {
		http.NotFound(w, r)
		return
	}
	h.serve(w, r, h.index, "no-cache")
}

// missingFile reports whether name, which is not in the bundle, is a
// bundle file by its location: anything under assets/ or another
// directory of the bundle, or a top-level name with an extension
// (favicon.ico, robots.txt). Deeper paths are client routes whatever their
// last segment looks like: zone and domain names end in a trailing dot or
// in TLDs that read like extensions (.js, .map, .zip).
func (h *handler) missingFile(name string) bool {
	dir, _, nested := strings.Cut(name, "/")
	if !nested {
		return len(path.Ext(name)) > 1
	}
	if dir == "assets" {
		return true
	}
	st, err := fs.Stat(h.fs, dir)
	return err == nil && st.IsDir()
}

func (h *handler) lookup(name string) *file {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f, ok := h.files[name]; ok {
		return f
	}
	raw := h.read(name)
	gz, br := h.read(name+".gz"), h.read(name+".br")
	var f *file
	switch {
	case raw != nil:
		f = newFile(contentType(name, raw), raw)
		f.gz, f.stored, f.br = gz, gz != nil, br
	case gz != nil:
		f = newPrecompressed(mime.TypeByExtension(path.Ext(name)), gz, br)
		if f.ctype == "" {
			b, err := f.identity()
			if err != nil {
				return nil
			}
			f.ctype = http.DetectContentType(b)
		}
	default:
		return nil
	}
	h.files[name] = f
	return f
}

// read returns the bundle file name, or nil when it is missing or a
// directory.
func (h *handler) read(name string) []byte {
	st, err := fs.Stat(h.fs, name)
	if err != nil || st.IsDir() {
		return nil
	}
	b, err := fs.ReadFile(h.fs, name)
	if err != nil {
		return nil
	}
	return b
}

func contentType(name string, b []byte) string {
	if ctype := mime.TypeByExtension(path.Ext(name)); ctype != "" {
		return ctype
	}
	return http.DetectContentType(b)
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request, f *file, cache string) {
	hdr := w.Header()
	hdr.Set("Content-Type", f.ctype)
	hdr.Set("Cache-Control", cache)
	body, etag, enc := []byte(nil), f.etag, ""
	if f.compressed() {
		hdr.Add("Vary", "Accept-Encoding")
		switch {
		case f.br != nil && accepts(r, "br"):
			body, enc = f.br, "br"
		case accepts(r, "gzip"):
			body, enc = f.gzipped(), "gzip"
		}
	}
	if enc == "" {
		b, err := f.identity()
		if err != nil {
			http.Error(w, "corrupt bundle file", http.StatusInternalServerError)
			return
		}
		body = b
	} else {
		etag = strings.TrimSuffix(f.etag, `"`) + "-" + map[string]string{"gzip": "gz", "br": "br"}[enc] + `"`
		hdr.Set("Content-Encoding", enc)
	}
	hdr.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// compressed reports whether the file is sent compressed to clients that
// accept it: it is stored precompressed, or it is compressible and at
// least 1 KiB.
func (f *file) compressed() bool {
	if f.stored || f.br != nil {
		return true
	}
	return compressible(f.ctype) && len(f.raw) >= 1024
}

// identity is the uncompressed body, inflated once from the gzip bytes of a
// file stored only precompressed.
func (f *file) identity() ([]byte, error) {
	f.rawOnce.Do(func() {
		if f.raw != nil {
			return
		}
		zr, err := gzip.NewReader(bytes.NewReader(f.gz))
		if err != nil {
			f.rawErr = err
			return
		}
		f.raw, f.rawErr = io.ReadAll(zr)
	})
	return f.raw, f.rawErr
}

func (f *file) gzipped() []byte {
	f.gzOnce.Do(func() {
		if f.stored {
			return
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(f.raw)
		_ = zw.Close()
		f.gz = buf.Bytes()
	})
	return f.gz
}

func setSecurityHeaders(h http.Header, csp string) {
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
}

func compressible(ctype string) bool {
	mt, _, _ := mime.ParseMediaType(ctype)
	switch {
	case strings.HasPrefix(mt, "text/"):
		return true
	case mt == "application/javascript", mt == "application/json", mt == "image/svg+xml",
		mt == "application/manifest+json", mt == "application/wasm":
		return true
	}
	return false
}

// accepts reports whether the request's Accept-Encoding lists coding
// without q=0.
func accepts(r *http.Request, coding string) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(enc), coding) {
			continue
		}
		return strings.ReplaceAll(strings.TrimSpace(params), " ", "") != "q=0"
	}
	return false
}
