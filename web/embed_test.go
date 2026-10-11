package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestFSServesIndex(t *testing.T) {
	b, err := fs.ReadFile(FS, "index.html")
	if err != nil {
		t.Fatalf("FS has no index.html: %v", err)
	}
	if !bytes.Contains(b, []byte("<base href=")) {
		t.Fatalf("index.html has no <base href>; the handler cannot apply the path prefix")
	}

	rec := httptest.NewRecorder()
	Handler(Options{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "kudurru") {
		t.Fatalf("GET / did not return the dashboard index: %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("no Content-Security-Policy header")
	}
}

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       {Data: []byte(`<!doctype html><html><head><base href="/" /><script type="module" src="./assets/app-1.js"></script></head><body><div id="root"></div></body></html>`)},
		"assets/app-1.js":  {Data: []byte(strings.Repeat("console.log('kudurru');\n", 200))},
		"assets/app-1.css": {Data: []byte("body{margin:0}")},
		"favicon.svg":      {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	}
}

func TestHandlerRoutes(t *testing.T) {
	h := Handler(Options{FS: testFS(), BasePath: "/kudurru"})
	cases := []struct {
		name, method, path string
		code               int
		ctype, cache, body string
	}{
		{"root", "GET", "/", 200, "text/html", "no-cache", `<base href="/kudurru/" />`},
		{"index", "GET", "/index.html", 200, "text/html", "no-cache", `<base href="/kudurru/" />`},
		{"spa route", "GET", "/queries", 200, "text/html", "no-cache", `<div id="root">`},
		{"nested spa route", "GET", "/zones/home-arpa/records", 200, "text/html", "no-cache", `<base href="/kudurru/" />`},
		{"asset", "GET", "/assets/app-1.css", 200, "text/css", "public, max-age=31536000, immutable", "body{margin:0}"},
		{"root file", "GET", "/favicon.svg", 200, "image/svg+xml", "no-cache", "<svg"},
		{"missing asset", "GET", "/assets/gone-2.js", 404, "", "", ""},
		{"missing file", "GET", "/robots.txt", 404, "", "", ""},
		{"api is not the SPA", "GET", "/api/v1/nope", 404, "", "", ""},
		{"dot-dot stays inside", "GET", "/../../etc/passwd", 200, "text/html", "no-cache", `<div id="root">`},
		{"head", "HEAD", "/", 200, "text/html", "no-cache", ""},
		{"post", "POST", "/", 405, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.code {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.code)
			}
			if tc.ctype != "" && !strings.HasPrefix(rec.Header().Get("Content-Type"), tc.ctype) {
				t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), tc.ctype)
			}
			if tc.cache != "" && rec.Header().Get("Cache-Control") != tc.cache {
				t.Errorf("Cache-Control = %q, want %q", rec.Header().Get("Cache-Control"), tc.cache)
			}
			if tc.body != "" && !strings.Contains(rec.Body.String(), tc.body) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), tc.body)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("missing nosniff")
			}
		})
	}
}

func TestHandlerGzipAndETag(t *testing.T) {
	h := Handler(Options{FS: testFS()})
	req := httptest.NewRequest(http.MethodGet, "/assets/app-1.js", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(plain), "console.log") {
		t.Fatalf("gunzipped body = %q", plain[:20])
	}
	etag := rec.Header().Get("ETag")
	if etag == "" || !strings.HasSuffix(etag, `-gz"`) {
		t.Fatalf("ETag = %q", etag)
	}

	req = httptest.NewRequest(http.MethodGet, "/assets/app-1.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/assets/app-1.js", nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("gzip;q=0 still got Content-Encoding %q", rec.Header().Get("Content-Encoding"))
	}
}

func TestNormalizeBasePath(t *testing.T) {
	for in, want := range map[string]string{"": "/", "/": "/", "kudurru": "/kudurru/", "/kudurru/": "/kudurru/", " /a/b ": "/a/b/"} {
		if got := NormalizeBasePath(in); got != want {
			t.Errorf("NormalizeBasePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseHrefIsEscaped(t *testing.T) {
	h := Handler(Options{FS: testFS(), BasePath: `/a"><script>`})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "<script>alert") || strings.Contains(rec.Body.String(), `"><script>`) {
		t.Fatalf("base path not escaped: %s", rec.Body.String())
	}
}

func TestBuiltDetectsPlaceholder(t *testing.T) {
	if isBuilt(fstest.MapFS{"index.html": {Data: []byte("<html>" + placeholderMarker + "</html>")}}) {
		t.Fatal("placeholder reported as built")
	}
	if !isBuilt(testFS()) {
		t.Fatal("bundle reported as placeholder")
	}
	if isBuilt(fstest.MapFS{}) {
		t.Fatal("empty FS reported as built")
	}
	_ = Built()
}
