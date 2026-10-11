package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestScalarBundleIntegrity(t *testing.T) {
	zr, err := gzip.NewReader(bytes.NewReader(scalarGz))
	if err != nil {
		t.Fatal(err)
	}
	js, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum384(js)
	if got := "sha384-" + base64.StdEncoding.EncodeToString(sum[:]); got != scalarIntegrity {
		t.Errorf("embedded bundle %s, scalarIntegrity %s", got, scalarIntegrity)
	}
	if !bytes.Contains(js[:1024], []byte("@scalar/api-reference "+scalarVersion)) {
		t.Errorf("embedded bundle is not @scalar/api-reference %s", scalarVersion)
	}
}

func TestDocsSelfHosted(t *testing.T) {
	for _, prefix := range []string{"", "/kudurru"} {
		h := newHarness(t, func(o *Options) { o.Config.Listeners.API.PathPrefix = prefix })
		r := h.do(http.MethodGet, prefix+"/api/v1/docs", "", nil).want(http.StatusOK)
		body := r.Body.String()
		for _, want := range []string{
			`data-url="` + prefix + `/api/v1/openapi.json"`,
			`<script src="` + prefix + `/api/v1/docs/scalar-` + scalarVersion + `.js" integrity="` + scalarIntegrity + `">`,
			`&#34;withDefaultFonts&#34;:false`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("prefix %q: docs page lacks %q:\n%s", prefix, want, body)
			}
		}
		if strings.Contains(body, "unpkg.com") || strings.Contains(body, "https://") {
			t.Errorf("prefix %q: docs page loads from another origin:\n%s", prefix, body)
		}
		csp := r.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self' 'unsafe-eval'") || !strings.Contains(csp, "connect-src 'self'") ||
			strings.Contains(csp, "unpkg") || strings.Contains(csp, "https:") {
			t.Errorf("prefix %q: CSP %q", prefix, csp)
		}

		js := prefix + "/api/v1/docs/scalar-" + scalarVersion + ".js"
		gz := h.do(http.MethodGet, js, "", nil, "Accept-Encoding", "gzip, deflate, br").want(http.StatusOK)
		if gz.Header().Get("Content-Encoding") != "gzip" || !bytes.Equal(gz.Body.Bytes(), scalarGz) {
			t.Errorf("prefix %q: gzip bundle: encoding %q, %d bytes", prefix, gz.Header().Get("Content-Encoding"), gz.Body.Len())
		}
		if ct := gz.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("bundle content type %q", ct)
		}
		plain := h.do(http.MethodGet, js, "", nil, "Accept-Encoding", "gzip;q=0").want(http.StatusOK)
		if plain.Header().Get("Content-Encoding") != "" || !bytes.Contains(plain.Body.Bytes()[:1024], []byte("@scalar/api-reference")) {
			t.Errorf("prefix %q: identity bundle: encoding %q", prefix, plain.Header().Get("Content-Encoding"))
		}
	}
}

func TestAcceptsGzip(t *testing.T) {
	for v, want := range map[string]bool{
		"":                  false,
		"gzip":              true,
		"br, gzip;q=0.5":    true,
		"GZIP":              true,
		"gzip;q=0":          false,
		"identity":          false,
		"*":                 true,
		"deflate, gzip;q=0": false,
	} {
		if got := acceptsGzip(v); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", v, got, want)
		}
	}
}
