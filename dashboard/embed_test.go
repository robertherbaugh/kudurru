package dashboard

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// TestIndexAssetsPresent fails when index.html references bundle files
// that are not embedded, e.g. a built index.html committed by mistake
// over the placeholder while dashboard/dist/assets stays ignored.
func TestIndexAssetsPresent(t *testing.T) {
	b, err := fs.ReadFile(FS, "index.html")
	if err != nil {
		t.Fatalf("FS has no index.html: %v", err)
	}
	for _, m := range assetRef.FindAllSubmatch(b, -1) {
		name := string(m[1])
		_, err := fs.Stat(FS, name)
		if err != nil {
			_, err = fs.Stat(FS, name+".gz") // precompressed by dashboard/scripts/compress.ts
		}
		if err != nil {
			t.Errorf("index.html references %s, which is not in the embedded bundle (commit only the placeholder dashboard/dist/index.html, or run make dashboard-build before go build)", name)
		}
	}
	if !Built() && len(assetRef.FindAll(b, -1)) != 0 {
		t.Error("the placeholder index.html must not reference bundle assets")
	}
}

// TestEmbeddedBundleIsPrecompressed keeps the binary small: a built bundle
// holds its compressible files only as .gz (dashboard/scripts/compress.ts, run by
// `bun run build`), apart from index.html, whose <base href> the handler
// rewrites.
func TestEmbeddedBundleIsPrecompressed(t *testing.T) {
	if !Built() {
		t.Skip("placeholder bundle; make dashboard-build embeds the real one")
	}
	err := fs.WalkDir(FS, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || name == "index.html" {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if compressible(mime.TypeByExtension(path.Ext(name))) && info.Size() >= 1024 {
			t.Errorf("%s (%d bytes) is embedded uncompressed; bun run build stores it as %s.gz", name, info.Size(), name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

var assetRef = regexp.MustCompile(`(?:src|href)="\.?/?(assets/[^"]+)"`)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       {Data: []byte(`<!doctype html><html><head><base href="/" /><script type="module" src="./assets/app-1.js"></script></head><body><div id="root"></div></body></html>`)},
		"assets/app-1.js":  {Data: []byte(strings.Repeat("console.log('kudurru');\n", 200))},
		"assets/app-1.css": {Data: []byte("body{margin:0}")},
		"favicon.svg":      {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"brand/logo.svg":   {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
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
		{"missing top-level script", "GET", "/sw.js", 404, "", "", ""},
		{"missing nested asset", "GET", "/assets/fonts/gone.woff2", 404, "", "", ""},
		{"missing file in a bundle dir", "GET", "/brand/gone.png", 404, "", "", ""},
		{"bundle dir file", "GET", "/brand/logo.svg", 200, "image/svg+xml", "no-cache", "<svg"},
		{"fqdn route", "GET", "/zones/home.example.", 200, "text/html", "no-cache", `<div id="root">`},
		{"fqdn route below", "GET", "/zones/home.example./records", 200, "text/html", "no-cache", `<div id="root">`},
		{"route ending in .js tld", "GET", "/zones/example.js", 200, "text/html", "no-cache", `<div id="root">`},
		{"route ending in .map tld", "GET", "/queries/maps.example.map", 200, "text/html", "no-cache", `<div id="root">`},
		{"top-level trailing dot", "GET", "/home.example.", 200, "text/html", "no-cache", `<div id="root">`},
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

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestHandlerPrecompressed covers files stored only as <name>.gz (and
// <name>.br): served as stored to clients that accept the encoding,
// inflated for those that do not, one ETag per representation.
func TestHandlerPrecompressed(t *testing.T) {
	js := []byte(strings.Repeat("console.log('precompressed');\n", 100))
	css := []byte(strings.Repeat("body{margin:0}\n", 100))
	jsGz, cssGz := gzipBytes(t, js), gzipBytes(t, css)
	both := []byte(strings.Repeat("raw and gz\n", 200))
	bothGz := gzipBytes(t, both)
	fsys := testFS()
	fsys["assets/app-2.js.gz"] = &fstest.MapFile{Data: jsGz}
	fsys["assets/app-2.js.br"] = &fstest.MapFile{Data: []byte("stored brotli bytes")}
	fsys["assets/app-2.css.gz"] = &fstest.MapFile{Data: cssGz}
	fsys["third-party-licenses.txt"] = &fstest.MapFile{Data: both}
	fsys["third-party-licenses.txt.gz"] = &fstest.MapFile{Data: bothGz}
	fsys["assets/broken-4.js.gz"] = &fstest.MapFile{Data: []byte("not gzip")}
	h := Handler(Options{FS: fsys})

	get := func(method, p, accept string, hdr ...string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, p, nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	etags := map[string]bool{}
	check := func(rec *httptest.ResponseRecorder, enc string, body []byte, etagSuffix string) {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != enc {
			t.Errorf("Content-Encoding = %q, want %q", got, enc)
		}
		if rec.Header().Get("Vary") != "Accept-Encoding" {
			t.Errorf("Vary = %q", rec.Header().Get("Vary"))
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Errorf("body (%s) differs from the stored or inflated bytes", enc)
		}
		etag := rec.Header().Get("ETag")
		if !strings.HasSuffix(etag, etagSuffix+`"`) || etags[etag] {
			t.Errorf("ETag %q: want a fresh one ending in %q", etag, etagSuffix)
		}
		etags[etag] = true
	}

	rec := get(http.MethodGet, "/assets/app-2.js", "gzip, br")
	check(rec, "br", []byte("stored brotli bytes"), "-br")
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("headers %v", rec.Header())
	}
	gzRec := get(http.MethodGet, "/assets/app-2.js", "gzip")
	check(gzRec, "gzip", jsGz, "-gz")
	idRec := get(http.MethodGet, "/assets/app-2.js", "")
	check(idRec, "", js, "")
	if rec := get(http.MethodGet, "/assets/app-2.js", "br;q=0, gzip;q=0"); rec.Header().Get("Content-Encoding") != "" || !bytes.Equal(rec.Body.Bytes(), js) || rec.Header().Get("ETag") != idRec.Header().Get("ETag") {
		t.Errorf("q=0 for both codings must get the identity representation, got %v", rec.Header())
	}
	check(get(http.MethodGet, "/assets/app-2.css", "br, gzip"), "gzip", cssGz, "-gz")

	// A raw file with a stored .gz sends the stored bytes, not a re-gzip.
	check(get(http.MethodGet, "/third-party-licenses.txt", "gzip"), "gzip", bothGz, "-gz")

	if rec := get(http.MethodGet, "/assets/app-2.js", "gzip", "If-None-Match", gzRec.Header().Get("ETag")); rec.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", rec.Code)
	}
	if rec := get(http.MethodHead, "/assets/app-2.js", ""); rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != strconv.Itoa(len(js)) {
		t.Errorf("HEAD = %d, %d body bytes, Content-Length %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
	}
	if rec := get(http.MethodGet, "/assets/broken-4.js", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("corrupt .gz inflated for an identity client = %d, want 500", rec.Code)
	}
	if rec := get(http.MethodGet, "/assets/gone-5.js", "gzip"); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset = %d", rec.Code)
	}

	// Concurrent identity requests inflate once (go test -race).
	fresh := Handler(Options{FS: fsys})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			fresh.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app-2.css", nil))
			if !bytes.Equal(rec.Body.Bytes(), css) {
				t.Error("concurrent identity body differs")
			}
		}()
	}
	wg.Wait()
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
