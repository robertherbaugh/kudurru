package catalogue

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/downloadpolicy"
)

func TestHTTPSOnlyCatalogueDownloads(t *testing.T) {
	for _, kind := range []string{KindCatalogue, KindAdGuard} {
		t.Run(kind, func(t *testing.T) {
			var plainHits, secureHits atomic.Int64
			var downgrade atomic.Bool
			plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				plainHits.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer plain.Close()
			body := doc(t, "2099.01.01", "alpha")
			if kind == KindAdGuard {
				body = `{"blocked_services":[{"id":"alpha","name":"Alpha","rules":["||alpha.example^"]}]}`
			}
			tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secureHits.Add(1)
				if r.URL.Path == "/first" {
					http.Redirect(w, r, "/body", http.StatusFound)
					return
				}
				if downgrade.Load() {
					http.Redirect(w, r, plain.URL+"/?token=fixture-private", http.StatusFound)
					return
				}
				if r.Header.Get("If-None-Match") == `"v1"` {
					w.WriteHeader(http.StatusNotModified)
					return
				}
				w.Header().Set("ETag", `"v1"`)
				_, _ = w.Write([]byte(body))
			}))
			defer tls.Close()
			holder := NewHolder(nil)
			refresh, err := NewRefresher(RefresherOptions{Kind: kind, URL: tls.URL + "/first", Holder: holder, Client: tls.Client(), HTTPSOnly: true, Logger: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			refresh.Refresh(t.Context())
			if refresh.Status().LastError != "" || !holder.HasService("alpha") {
				t.Fatal("verified HTTPS catalogue failed", refresh.Status().LastError)
			}
			refresh.Refresh(t.Context())
			if refresh.Status().LastError != "" {
				t.Fatal("conditional catalogue refresh failed")
			}
			downgrade.Store(true)
			before := secureHits.Load()
			refresh.Refresh(t.Context())
			if refresh.Status().LastError != downloadpolicy.ErrHTTPSRequired.Error() || !holder.HasService("alpha") {
				t.Fatal("downgrade diagnostic or last-good catalogue lost")
			}
			if plainHits.Load() != 0 || secureHits.Load() != before+2 {
				t.Fatal("downgrade contacted HTTP target or replayed request")
			}
			initial, err := NewRefresher(RefresherOptions{Kind: kind, URL: plain.URL + "/?token=fixture-private", Holder: NewHolder(nil), HTTPSOnly: true, Logger: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			if err := initial.fetch(t.Context()); !errors.Is(err, downloadpolicy.ErrHTTPSRequired) || err.Error() != downloadpolicy.ErrHTTPSRequired.Error() {
				t.Fatal("initial HTTP must give only the policy diagnostic")
			}
			if plainHits.Load() != 0 {
				t.Fatal("initial HTTP target contacted")
			}
		})
	}
}
