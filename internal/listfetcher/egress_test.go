package listfetcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// TestFetchRefusesLocalAddresses is the regression test of the M5 security
// finding "blocklist URLs reach node-local services (SSRF)": a BlockList
// writer (operator role) could point a source at 127.0.0.1 (dnsdist's
// console and webserver, sidecars) or 169.254.169.254 (cloud metadata) and
// have the leader fetch it.
func TestFetchRefusesLocalAddresses(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("0.0.0.0 ads.example\n"))
	}))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]

	cache, err := OpenDiskCache(t.TempDir(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(cache, DownloadOptions{Attempts: 3})
	d.sleep = func(context.Context, time.Duration) error { return nil }
	for _, raw := range []string{
		srv.URL + "/list.txt",                      // 127.0.0.1 literal
		"http://localhost" + port + "/list.txt",    // a name for loopback
		"http://[::1]" + port + "/list.txt",        // IPv6 loopback
		"http://[::ffff:127.0.0.1]" + port + "/l",  // IPv4-mapped loopback
		"http://0.0.0.0" + port + "/list.txt",      // connects to the local host
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://[fd00:ec2::254]/latest/meta-data/", // AWS IMDS over IPv6
	} {
		_, err := d.Fetch(context.Background(), Source{Key: raw, Format: parser.FormatHosts}, "test")
		if !errors.Is(err, ErrLocalAddress) {
			t.Errorf("Fetch(%s) = %v, want ErrLocalAddress", raw, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the local server was reached %d times", n)
	}

	// The opt-in restores the previous behaviour.
	open := NewDownloader(cache, DownloadOptions{AllowLocalAddresses: true})
	if _, err := open.Fetch(context.Background(), Source{Key: srv.URL + "/list.txt", Format: parser.FormatHosts}, "test"); err != nil {
		t.Fatalf("Fetch with AllowLocalAddresses: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

// TestGuardedDialerChecksResolvedAddress covers names that resolve to a
// local address (DNS rebinding, redirect targets): the dialer checks the
// resolved address of every connection, not the URL.
func TestGuardedDialerChecksResolvedAddress(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	// The dialer alone: the transport under the URL check, which would
	// refuse "localhost" before any dial.
	dialOnly := &http.Client{Transport: guardedClient().Transport.(guardTransport).base}
	resp, err := dialOnly.Get("http://localhost" + port + "/secret")
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrLocalAddress) {
		t.Fatalf("Get = %v, want ErrLocalAddress", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the local server was reached")
	}
}

// TestGuardedClientChecksRedirectsThroughProxy: through an HTTP proxy the
// dialer only sees the proxy, so a list server that redirects to cloud
// metadata must be refused by the URL check of the redirect itself (the
// proxy would fetch it otherwise).
func TestGuardedClientChecksRedirectsThroughProxy(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.RequestURI)
		mu.Unlock()
		if r.URL.Host == "lists.example.test" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("0.0.0.0 ads.example\n"))
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL) // proxyAddrs: the dialer may reach it
	c := guardedClient()
	pu, _ := url.Parse(proxy.URL)
	c.Transport.(guardTransport).base.(*http.Transport).Proxy = http.ProxyURL(pu)

	cache, err := OpenDiskCache(t.TempDir(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(cache, DownloadOptions{Client: c, Attempts: 1})
	_, err = d.Fetch(context.Background(), Source{Key: "http://lists.example.test/list.txt", Format: parser.FormatHosts}, "test")
	if !errors.Is(err, ErrLocalAddress) {
		t.Errorf("Fetch = %v, want ErrLocalAddress", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "http://lists.example.test/list.txt" {
		t.Fatalf("the proxy was asked for %q, want only the list URL", seen)
	}
}

func TestLocalAddress(t *testing.T) {
	for s, want := range map[string]bool{
		"127.0.0.1": true, "127.8.9.10": true, "::1": true, "0.0.0.0": true, "0.1.2.3": true, "::": true,
		"169.254.169.254": true, "fe80::1": true, "224.0.0.1": true, "ff02::1": true, "::ffff:127.0.0.1": true,
		"100.100.100.200": true, "fd00:ec2::254": true,
		"10.0.0.5": false, "192.168.1.10": false, "172.16.0.1": false, "fd12:3456::1": false, "93.184.215.14": false,
		"2606:4700::1111": false, "100.64.0.1": false,
	} {
		if got := localAddress(netip.MustParseAddr(s)); got != want {
			t.Errorf("localAddress(%s) = %v, want %v", s, got, want)
		}
	}
}
