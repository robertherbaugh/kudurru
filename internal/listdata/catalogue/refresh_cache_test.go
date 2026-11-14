package catalogue

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
)

func TestCacheReceiptOriginalSourceAndFraming(t *testing.T) {
	for _, kind := range []string{KindCatalogue, KindAdGuard} {
		t.Run(kind, func(t *testing.T) {
			body := []byte(doc(t, "2099.01.01", "original"))
			if kind == KindAdGuard {
				body = []byte(adguardFixture)
			}
			dir := t.TempDir()
			newReader := func(source string) *Refresher {
				t.Helper()
				r, err := NewRefresher(RefresherOptions{Kind: kind, URL: source, Holder: NewHolder(nil), Dir: dir, Logger: quiet()})
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			source := "https://catalog.example/source"
			r := newReader(source)
			if err := r.apply(body, source); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, r.receiptName())
			original, err := os.ReadFile(path) //nolint:gosec // G304: fixed receipt name under t.TempDir
			if err != nil {
				t.Fatal(err)
			}
			if mode, err := os.Stat(path); err != nil || mode.Mode().Perm() != 0o600 {
				t.Fatalf("receipt permissions: %v, %v", mode, err)
			}
			restarted := newReader(source)
			restarted.loadPersisted()
			if restarted.opts.Holder.Current().Fingerprint() != r.opts.Holder.Current().Fingerprint() {
				t.Fatal("restart changed accepted document")
			}
			if _, err := newReader("https://other.example/source").readReceipt(); err == nil {
				t.Fatal("adopted another URL receipt")
			}
			var good cacheReceipt
			if err := json.Unmarshal(original, &good); err != nil {
				t.Fatal(err)
			}
			changed := func(fn func(*cacheReceipt)) []byte {
				c := good
				c.Body = bytes.Clone(good.Body)
				fn(&c)
				b, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			bad := map[string][]byte{
				"body corruption": changed(func(c *cacheReceipt) { c.Body[0] ^= 1 }),
				"source":          changed(func(c *cacheReceipt) { c.Source = "https://other.example/source" }),
				"kind":            changed(func(c *cacheReceipt) { c.Kind = "other" }),
				"version":         changed(func(c *cacheReceipt) { c.Version++ }),
				"truncated":       original[:len(original)-1],
				"duplicate":       bytes.Replace(original, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
				"alias":           bytes.Replace(original, []byte(`"source"`), []byte(`"Source"`), 1),
				"missing":         bytes.Replace(original, []byte(`"version":1,`), nil, 1),
				"extra":           bytes.Replace(original, []byte(`"version":1`), []byte(`"version":1,"extra":true`), 1),
				"bound":           bytes.Repeat([]byte(" "), int(r.receiptLimit())+1),
			}
			for name, data := range bad {
				t.Run(name, func(t *testing.T) {
					if err := os.WriteFile(path, data, 0o600); err != nil {
						t.Fatal(err)
					}
					reader := newReader(source)
					before := reader.opts.Holder.Current()
					reader.loadPersisted()
					if reader.opts.Holder.Current() != before {
						t.Fatal("rejected receipt replaced held catalog")
					}
				})
			}
			if err := os.WriteFile(path, original, 0o600); err != nil { //nolint:gosec // G703: fixed receipt name under t.TempDir; content never forms the path
				t.Fatal(err)
			}
			// A crash before the atomic rename can leave an unfinished temporary
			// file; only the previous complete receipt is eligible on restart.
			if err := os.WriteFile(filepath.Join(dir, ".catalog-receipt-interrupted"), original[:50], 0o600); err != nil { //nolint:gosec // G703: fixed temporary name under t.TempDir; content never forms the path
				t.Fatal(err)
			}
			got, err := newReader(source).readReceipt()
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("interrupted replacement changed receipt: %v", err)
			}
		})
	}
}

func TestCacheIgnoresUnpairedLegacyFiles(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"catalogue.json":          doc(t, "2099.01.01", "unproven"),
		"catalogue.source":        "https://catalog.example/source\n",
		"adguard-services.json":   adguardFixture,
		"adguard-services.source": "https://catalog.example/source\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{KindCatalogue, KindAdGuard} {
		h := NewHolder(nil)
		r, err := NewRefresher(RefresherOptions{Kind: kind, URL: "https://catalog.example/source", Holder: h, Dir: dir, Logger: quiet()})
		if err != nil {
			t.Fatal(err)
		}
		r.loadPersisted()
		if h.Current() != Embedded() || h.Import() != nil {
			t.Fatal("legacy body/source pair fabricated acquisition provenance")
		}
	}
}

func TestReplicaRejectsWrongSourceBeforeDigestNoopAndFallsBack(t *testing.T) {
	srv := &docServer{}
	srv.set(http.StatusOK, doc(t, "2099.02.01", "direct"))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	old := []byte(doc(t, "2099.01.01", "old"))
	now := time.Unix(100000, 0)
	h := NewHolder(nil)
	r, err := NewRefresher(RefresherOptions{
		URL: ts.URL, Holder: h, Logger: quiet(), Now: func() time.Time { return now },
		Membership: &cluster.Static{SelfName: "b", LeaderName: "a", Members: []cluster.Peer{{Name: "a", Address: "127.0.0.1:7000"}, {Name: "b"}}},
		Pull:       func(context.Context, string) ([]byte, string, error) { return old, "https://old.example/source", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.apply(old, ts.URL); err != nil {
		t.Fatal(err)
	}
	before := h.Current()
	r.Refresh(t.Context())
	if h.Current() != before || !strings.Contains(r.Status().LastError, "source mismatch") || srv.requests != 0 {
		t.Fatal("identical leader body hid mismatched acquisition source")
	}
	now = now.Add(DefaultFallbackAfter)
	r.Refresh(t.Context())
	if !h.HasService("direct") || h.Base().Origin() != ts.URL || srv.requests != 1 {
		t.Fatal("mismatched leader prevented admitted direct acquisition")
	}
	if _, err := NewRefresher(RefresherOptions{URL: "https://example/" + strings.Repeat("a", maxSourceBytes), Holder: h}); err == nil {
		t.Fatal("accepted unbounded source URL")
	}
}
