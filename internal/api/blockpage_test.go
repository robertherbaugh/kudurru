package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/blockpage"
)

func TestBlockPagePreview(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodGet, "/api/v1/blockpage/preview", "admin", nil).want(http.StatusServiceUnavailable)

	bp, err := blockpage.New(blockpage.Options{Node: "node-a", Settings: blockpage.Settings{Title: "Nope", ShowDetails: true}})
	if err != nil {
		t.Fatal(err)
	}
	h = newHarness(t, func(o *Options) { o.BlockPage = bp })
	r := h.do(http.MethodGet, "/api/v1/blockpage/preview?domain=Ads.Example", "viewer", nil).want(http.StatusOK)
	body := r.Body.String()
	for _, want := range []string{"<h1>Nope</h1>", `<div class="domain">ads.example</div>`, `<div class="preview">Preview</div>`, "<code>192.0.2.10</code>"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	for k, v := range map[string]string{
		"Content-Type": "text/html; charset=utf-8", "Content-Security-Policy": blockpage.PreviewCSP,
		"X-Frame-Options": "SAMEORIGIN", "Cache-Control": "no-store",
	} {
		if got := r.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	h.do(http.MethodGet, "/api/v1/blockpage/preview", "", nil).want(http.StatusUnauthorized)
	h.do(http.MethodGet, "/api/v1/blockpage/preview?domain="+strings.Repeat("a", 300), "admin", nil).want(http.StatusBadRequest)
}
