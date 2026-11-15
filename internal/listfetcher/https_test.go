package listfetcher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/downloadpolicy"
)

func TestHTTPSOnlyDownloads(t *testing.T) {
	var plainHits, secureHits atomic.Int64
	var downgrade atomic.Bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		_, _ = io.WriteString(w, "plain.example\n")
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secureHits.Add(1)
		if r.URL.Path == "/first" {
			http.Redirect(w, r, "/list", http.StatusTemporaryRedirect)
			return
		}
		if downgrade.Load() {
			http.Redirect(w, r, plain.URL+"/?token=fixture-private", http.StatusTemporaryRedirect)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, "secure.example\n")
	}))
	defer tls.Close()
	cache, err := OpenDiskCache(t.TempDir(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	client := tls.Client() // Verifies the TLS server with its trusted test CA.
	originalTransport := client.Transport
	var redirects atomic.Int64
	client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		redirects.Add(1)
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	dl := NewDownloader(cache, DownloadOptions{Client: client, HTTPSOnly: true, AllowLocalAddresses: true, Attempts: 3})
	dl.sleep = func(_ context.Context, _ time.Duration) error { t.Fatal("policy failure retried"); return nil }
	src := Source{Key: tls.URL + "/first", Format: parser.FormatDomains}
	first, err := dl.Fetch(t.Context(), src, "test")
	if err != nil || !first.Changed {
		t.Fatal("verified HTTPS download failed", err)
	}
	next, err := dl.Fetch(t.Context(), src, "test")
	if err != nil || !next.NotModified || next.Entry.Digest != first.Entry.Digest {
		t.Fatal("HTTPS conditional revalidation failed", err)
	}
	downgrade.Store(true)
	before := secureHits.Load()
	if _, err := dl.Fetch(t.Context(), src, "test"); !errors.Is(err, downloadpolicy.ErrHTTPSRequired) || err.Error() != downloadpolicy.ErrHTTPSRequired.Error() {
		t.Fatal("downgrade must return only the credential-free policy diagnostic")
	}
	if plainHits.Load() != 0 || secureHits.Load() != before+2 {
		t.Fatal("downgrade was contacted or automatically retried")
	}
	cached, ok := cache.Lookup(src.Key)
	if !ok || cached.Digest != first.Entry.Digest {
		t.Fatal("policy refusal replaced the last good body")
	}
	if _, err := dl.Fetch(t.Context(), Source{Key: plain.URL + "/?token=fixture-private", Format: parser.FormatDomains}, "test"); !errors.Is(err, downloadpolicy.ErrHTTPSRequired) || err.Error() != downloadpolicy.ErrHTTPSRequired.Error() {
		t.Fatal("initial HTTP policy diagnostic lost")
	}
	if plainHits.Load() != 0 {
		t.Fatal("initial HTTP target was contacted")
	}
	if client.Transport != originalTransport || redirects.Load() == 0 {
		t.Fatal("supplied client was mutated or redirect callback discarded")
	}
	// The opt-in must not change another caller's client or the default behavior.
	permissive := NewDownloader(cache, DownloadOptions{Client: client, AllowLocalAddresses: true})
	if _, err := permissive.Fetch(t.Context(), Source{Key: plain.URL, Format: parser.FormatDomains}, "test"); err != nil || plainHits.Load() != 1 {
		t.Fatal("default HTTP download changed", err)
	}
}
