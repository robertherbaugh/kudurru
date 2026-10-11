package listfetcher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

func TestDiskCachePutAndReopen(t *testing.T) {
	dir := t.TempDir()
	c, err := OpenDiskCache(dir, quiet())
	if err != nil {
		t.Fatal(err)
	}
	const key = "https://lists.example/a"
	e, changed, err := c.Put(key, strings.NewReader(hostsV1), PutOptions{ETag: `"v1"`, Origin: OriginFetch})
	if err != nil || !changed || e.Digest != sha([]byte(hostsV1)) || e.Size != int64(len(hostsV1)) {
		t.Fatalf("put = %+v %v %v", e, changed, err)
	}
	if filepath.Base(c.Path(key)) != FileName(key) || len(FileName(key)) != 64 {
		t.Errorf("path = %s", c.Path(key))
	}
	// Identical body: metadata only.
	e2, changed, err := c.Put(key, strings.NewReader(hostsV1), PutOptions{ETag: `"v1b"`})
	if err != nil || changed || e2.ETag != `"v1b"` || !e2.Changed.Equal(e.Changed) {
		t.Errorf("identical put = %+v %v %v", e2, changed, err)
	}
	// Failures keep the old body.
	for name, opts := range map[string]PutOptions{
		"size cap":        {MaxSize: 10},
		"digest mismatch": {ExpectDigest: sha([]byte("other"))},
		"validation":      {Validate: func(string) (int, int, error) { return 0, 0, errors.New("nope") }},
	} {
		if _, _, err := c.Put(key, strings.NewReader(hostsV2), opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if b, _ := os.ReadFile(c.Path(key)); string(b) != hostsV1 {
			t.Errorf("%s: body replaced", name)
		}
	}
	// OpenList by digest (the internal listener's view).
	lb, err := c.OpenList(e.Digest)
	if err != nil || lb.Size != e.Size || lb.Source != key {
		t.Fatalf("OpenList = %+v %v", lb, err)
	}
	b, _ := io.ReadAll(lb.Body)
	_ = lb.Body.Close()
	if string(b) != hostsV1 {
		t.Errorf("OpenList body = %q", b)
	}
	if _, err := c.OpenList(sha([]byte("unknown"))); !errors.Is(err, internalapi.ErrListNotFound) {
		t.Errorf("unknown digest: %v", err)
	}

	// Touch (304) moves Fetched only.
	later := e.Fetched.Add(time.Hour)
	if e3, err := c.Touch(key, later, "", ""); err != nil || !e3.Fetched.Equal(later) || e3.Digest != e.Digest {
		t.Errorf("touch = %+v %v", e3, err)
	}

	// A second key, a crash leftover, a corrupted body and an orphan.
	if _, _, err := c.Put("https://lists.example/b", strings.NewReader(hostsV2), PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tmpPrefix+"123"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.Path("https://lists.example/b"), []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	orphan := FileName("https://lists.example/orphan")
	if err := os.WriteFile(filepath.Join(dir, orphan), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	c2, err := OpenDiskCache(dir, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if got := c2.Keys(); len(got) != 1 || got[0] != key {
		t.Errorf("reopened keys = %v", got)
	}
	if e4, _ := c2.Lookup(key); !e4.Fetched.Equal(later) || e4.ETag != `"v1b"` {
		t.Errorf("reopened entry = %+v", e4)
	}
	des, _ := os.ReadDir(dir)
	for _, de := range des {
		if strings.HasPrefix(de.Name(), tmpPrefix) || de.Name() == orphan || strings.HasPrefix(de.Name(), FileName("https://lists.example/b")) {
			t.Errorf("left behind: %s", de.Name())
		}
	}
	c2.Remove(key)
	if _, ok := c2.Lookup(key); ok {
		t.Error("Remove kept the entry")
	}
	if _, err := os.Stat(c2.Path(key)); !os.IsNotExist(err) {
		t.Error("Remove kept the file")
	}
}

func TestDownloaderValidation(t *testing.T) {
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	defer srv.Close()
	c, _ := OpenDiskCache(t.TempDir(), quiet())
	ctx := context.Background()
	for _, tc := range []struct {
		name, body string
		maxErrs    int
		want       error
	}{
		{"empty", "# only a comment\n", 0, ErrEmptyList},
		{"binary", "\x1f\x8b\x08\x00binary", 0, parser.ErrBinary},
		{"html", "<html><body>login</body></html>", 0, parser.ErrHTML},
		{"mostly invalid", "a b c\nd e f\ng h i\nj k l\nm n o\np q r\nok.example\n", 0, ErrTooManyErrors},
		{"mostly invalid, check disabled", "a b c\nd e f\ng h i\nj k l\nm n o\np q r\nok.example\n", -1, nil},
		{"browser list with cosmetic rules", "||ads.example^\n||b.example^\n||c.example^\n##.banner\n##.ad\n", 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body = tc.body
			d := NewDownloader(c, DownloadOptions{MaxErrorsPerSource: tc.maxErrs})
			_, err := d.Fetch(ctx, Source{Key: srv.URL + "/" + strings.ReplaceAll(tc.name, " ", "-")}, OriginFetch)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	d := NewDownloader(c, DownloadOptions{})
	if _, err := d.Fetch(ctx, Source{Key: "file:///etc/passwd"}, OriginFetch); !errors.Is(err, ErrUnsupportedURL) {
		t.Errorf("file URL: %v", err)
	}
	if _, err := d.Fetch(ctx, Source{Key: ConfigMapKey("ns", "x"), ConfigMap: &ConfigMapRef{"ns", "x"}}, OriginFetch); !errors.Is(err, ErrUnsupported) {
		t.Errorf("configMap: %v", err)
	}
	cm := NewDownloader(c, DownloadOptions{ConfigMaps: func(_ context.Context, ns, name string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("cm-" + ns + "-" + name + ".example\n")), nil
	}})
	res, err := cm.Fetch(ctx, Source{Key: ConfigMapKey("ns", "x"), ConfigMap: &ConfigMapRef{"ns", "x"}}, OriginFetch)
	if err != nil || !res.Changed || res.Entry.Rules != 1 {
		t.Errorf("configMap fetch = %+v %v", res, err)
	}
	// Credentials and query strings never reach logs or status.
	if got := redact("https://user:pw@lists.example/x?token=s3cret"); strings.Contains(got, "pw") || strings.Contains(got, "s3cret") {
		t.Errorf("redact = %q", got)
	}
}

func TestDownloaderRetries(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits < 3 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "a.example\n")
	}))
	defer srv.Close()
	c, _ := OpenDiskCache(t.TempDir(), quiet())
	d := NewDownloader(c, DownloadOptions{Attempts: 3})
	var slept []time.Duration
	d.sleep = func(_ context.Context, dur time.Duration) error { slept = append(slept, dur); return nil }
	if _, err := d.Fetch(context.Background(), Source{Key: srv.URL}, OriginFetch); err != nil {
		t.Fatalf("third attempt should succeed: %v", err)
	}
	if hits != 3 || len(slept) != 2 {
		t.Fatalf("hits %d, sleeps %v", hits, slept)
	}
	// Jittered exponential backoff: 1s±50 %, then 2s±50 %.
	if slept[0] < 500*time.Millisecond || slept[0] > 1500*time.Millisecond || slept[1] < time.Second || slept[1] > 3*time.Second {
		t.Errorf("backoff = %v", slept)
	}
	// 4xx is not retried.
	hits = 0
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; http.NotFound(w, r) }))
	defer srv404.Close()
	if _, err := d.Fetch(context.Background(), Source{Key: srv404.URL}, OriginFetch); err == nil || hits != 1 {
		t.Errorf("404: %v after %d hits", err, hits)
	}
}
