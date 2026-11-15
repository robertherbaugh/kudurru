package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestCapturedSourceIndexKeepsParserFiltersAndOriginalBody(t *testing.T) {
	body := "# comments keep physical line numbers\n0.0.0.0 alpha.example beta.example\n0.0.0.0 gamma.example\n"
	bl := sourceTestList()
	bl.Spec.Sources[0] = public.ListSource{Name: "original", URL: "https://example.test/list", Format: "hosts", Exclude: []string{"beta.example", "gone.example"}}
	bl.Status.Sources = []public.SourceStatus{{Name: "original", Digest: sourceTestDigest(body)}}
	h, objects, bodies := newSourceReadFixture(t, bl, body)
	var first SourceEntries
	h.do(http.MethodGet, sourceTestPath+"?limit=1", "viewer", nil).want(http.StatusOK).json(&first)
	if first.Items[0].Line != 2 || first.Total != 3 || first.Excluded != 1 {
		t.Fatal("incorrect cold parsed page", first)
	}
	key := sourceIndexKey{sourceTestDigest(body), parser.FormatHosts, false}
	if h.srv.sourceIndexes.get(key) == nil {
		t.Fatal("parsed digest not indexed")
	}
	// The seam promises verified bytes, so a warm read must still open it but
	// must not parse it again. The production opener rehashes the actual file.
	bodies.open = func(context.Context, string, string, string) (io.ReadCloser, string, int64, error) {
		return sourceFixtureBody{failSourceIndexRead{t}, func() { bodies.closed++ }}, key.digest, int64(len(body)), nil
	}
	var second SourceEntries
	h.do(http.MethodGet, sourceTestPath+"?limit=1&continue="+first.Continue, "viewer", nil).want(http.StatusOK).json(&second)
	if second.Items[0].Value != "beta.example" || second.Items[0].Line != 2 || second.Items[0].Enabled || second.Total != 3 || second.Excluded != 1 || !reflect.DeepEqual(second.StaleExcludes, []string{"gone.example"}) || bodies.calls != 2 || bodies.closed != 2 {
		t.Fatal("warm original page changed semantics or skipped body verification", second)
	}
	var filtered SourceEntries
	h.do(http.MethodGet, sourceTestPath+"?q=BETA&enabled=false", "viewer", nil).want(http.StatusOK).json(&filtered)
	if filtered.Matched != 1 || filtered.Items[0].Value != "beta.example" {
		t.Fatal("warm filtering diverged", filtered)
	}
	changed := bl.DeepCopy()
	changed.Spec.Sources[0].Exclude = []string{"gamma.example"}
	sourceTestPrepare(t, objects, changed)
	h.do(http.MethodGet, sourceTestPath+"?enabled=false", "viewer", nil).want(http.StatusOK).json(&filtered)
	if filtered.Matched != 1 || filtered.Items[0].Value != "gamma.example" {
		t.Fatal("index retained old exclusions", filtered)
	}
	h.do(http.MethodGet, sourceTestPath+"?limit=1&continue="+first.Continue, "viewer", nil).want(http.StatusConflict)
	bodies.open = func(context.Context, string, string, string) (io.ReadCloser, string, int64, error) {
		return nil, "", 0, io.ErrUnexpectedEOF
	}
	h.do(http.MethodGet, sourceTestPath, "viewer", nil).want(http.StatusServiceUnavailable)
}

type failSourceIndexRead struct{ t *testing.T }

func (r failSourceIndexRead) Read([]byte) (int, error) {
	r.t.Error("warm index reparsed the body")
	return 0, io.ErrUnexpectedEOF
}

func TestSourceIndexBoundsEvictionAndConcurrentReaders(t *testing.T) {
	var cache sourceIndexCache
	index := &sourceIndex{}
	for i := range 4000 {
		if !index.add(fmt.Sprintf("d%d.example", i), i+2, maxSourceIndexBytes) {
			t.Fatal("small index exceeded budget")
		}
	}
	before := index.count
	if index.add("new", 5000, index.bytes-1) || index.count != before {
		t.Fatal("budget refusal changed retained index")
	}
	base := sourceIndexKey{"first", parser.FormatHosts, false}
	cache.put(base, index)
	pinned := cache.get(base)
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 50 {
				key := sourceIndexKey{fmt.Sprint(worker, ":", i), parser.FormatDomains, false}
				cache.put(key, index)
				cache.get(key)
				var out SourceEntries
				pg := &entryPager{start: 3900, limit: 100, out: &out}
				if err := pinned.page(t.Context(), pg, nil); err != nil || out.Total != 4000 || len(out.Items) != 100 || out.Items[99].Line != 4001 || out.Continue != "" {
					t.Error("eviction affected a pinned page")
					return
				}
			}
		})
	}
	wg.Wait()
	if len(cache.entries) > maxSourceIndexes || cache.bytes > maxSourceIndexesBytes || cache.get(base) != nil {
		t.Fatal("cache failed its eviction bounds")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if pinned.page(ctx, &entryPager{q: "example", out: &SourceEntries{}}, nil) == nil {
		t.Fatal("canceled index scan succeeded")
	}
}

func TestSourceIndexParserIdentityAndBufferReuse(t *testing.T) {
	var cache sourceIndexCache
	body := strings.Repeat("0.0.0.0 alpha.example beta.example\n", 5000)
	for _, subdomains := range []bool{false, true} {
		index := &sourceIndex{}
		_, err := parser.Parse(strings.NewReader(body), parser.Options{Format: parser.FormatHosts, HostsSubdomains: subdomains}, func(rule parser.Rule) error {
			if !index.add(rule.String(), rule.Line, maxSourceIndexBytes) {
				t.Fatal("index over budget")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		cache.put(sourceIndexKey{"same-body", parser.FormatHosts, subdomains}, index)
	}
	plain := cache.get(sourceIndexKey{"same-body", parser.FormatHosts, false})
	suffix := cache.get(sourceIndexKey{"same-body", parser.FormatHosts, true})
	if plain == suffix || cache.get(sourceIndexKey{"same-body", parser.FormatDomains, false}) != nil {
		t.Fatal("parser identity aliased")
	}
	for _, i := range []int{0, 1, 2047, 9999} {
		value, line := plain.at(i)
		want := "alpha.example"
		if i%2 == 1 {
			want = "beta.example"
		}
		if value != want || line != i/2+1 {
			t.Fatal("retained parser buffer or wrong line", i, value, line)
		}
	}
	// Rule.String is the same canonical exclusion value for exact and suffix
	// hosts rules; the indexes must still keep parser identities distinct.
	if suffix.count != plain.count {
		t.Fatal("subdomain variant lost parsed entries")
	}
}

type fileIndexBodies struct {
	path, digest string
	size         int64
}

func (f fileIndexBodies) OpenSourceBody(string, public.ListSource) (io.ReadCloser, SourceBody, error) {
	body, err := os.Open(f.path)
	return body, SourceBody{Digest: f.digest, Size: f.size}, err
}

func TestLegacySourceIndexVerifiedFileAndCursor(t *testing.T) {
	body := "alpha.example\nbeta.example\ngamma.example\n"
	path := filepath.Join(t.TempDir(), "body")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := fileIndexBodies{path, sourceTestDigest(body), int64(len(body))}
	h := newHarness(t, func(o *Options) { o.ListBodies = provider })
	bl := sourceTestList()
	bl.Spec.Sources[0] = public.ListSource{Name: "original", URL: "https://example.test/body", Format: "domains"}
	h.seed(store.ManagerAPI, bl)
	var first, second SourceEntries
	h.do(http.MethodGet, sourceTestPath+"?limit=1", "viewer", nil).want(http.StatusOK).json(&first)
	h.do(http.MethodGet, sourceTestPath+"?limit=1&continue="+first.Continue, "viewer", nil).want(http.StatusOK).json(&second)
	if first.Items[0].Value != "alpha.example" || second.Items[0].Value != "beta.example" || second.Total != 3 || second.Continue == "" {
		t.Fatal("legacy index cursor changed", first, second)
	}
	var third SourceEntries
	h.do(http.MethodGet, sourceTestPath+"?limit=1&continue="+second.Continue, "viewer", nil).want(http.StatusOK).json(&third)
	if third.Items[0].Value != "gamma.example" || third.Continue != "" {
		t.Fatal("legacy continuation missing", third)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "alpha", "wrong")), 0o600); err != nil {
		t.Fatal(err)
	}
	h.do(http.MethodGet, sourceTestPath, "viewer", nil).want(http.StatusServiceUnavailable)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.do(http.MethodGet, sourceTestPath, "viewer", nil).want(http.StatusServiceUnavailable)
}

func TestSourceIndexBudgetFallbackAndFailedParse(t *testing.T) {
	s := &Server{}
	body := strings.Repeat("alpha.example\nbeta.example\n", 2000)
	key := sourceIndexKey{sourceTestDigest(body), parser.FormatDomains, false}
	pg := &entryPager{start: 3998, limit: 2, out: &SourceEntries{}}
	if err := s.scanIndexedSource(t.Context(), key, strings.NewReader(body), pg, nil, 1); err != nil {
		t.Fatal(err)
	}
	if s.sourceIndexes.get(key) != nil || pg.out.Total != 4000 || len(pg.out.Items) != 2 || pg.out.Items[0].Value != "alpha.example" || pg.out.Items[1].Line != 4000 {
		t.Fatal("budget fallback lost page or cached partial index", pg.out)
	}
	bad := sourceIndexKey{"bad", parser.FormatDomains, false}
	pg = &entryPager{limit: 2, out: &SourceEntries{}}
	if err := s.scanIndexedSource(t.Context(), bad, strings.NewReader("<html>invalid</html>"), pg, nil, maxSourceIndexBytes); err == nil || s.sourceIndexes.get(bad) != nil {
		t.Fatal("invalid body published an index")
	}
}

func BenchmarkSourceIndexTwoMillion(b *testing.B) {
	var source strings.Builder
	for i := range 2_000_000 {
		fmt.Fprintf(&source, "d%d.example\n", i)
	}
	body := source.String()
	path := filepath.Join(b.TempDir(), "two-million-domains")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		b.Fatal(err)
	}
	index := &sourceIndex{}
	parse := func(emit func(parser.Rule) error) {
		b.Helper()
		if _, err := parser.Parse(strings.NewReader(body), parser.Options{Format: parser.FormatDomains}, emit); err != nil {
			b.Fatal(err)
		}
	}
	parse(func(rule parser.Rule) error {
		if !index.add(rule.String(), rule.Line, maxSourceIndexBytes) {
			b.Fatal("two-million-rule index exceeded budget")
		}
		return nil
	})
	if index.count != 2_000_000 {
		b.Fatal("incorrect large index")
	}
	b.Logf("indexed_rules=%d accounted_index_bytes=%d source_bytes=%d", index.count, index.bytes, len(body))
	b.Run("cold-parse-page", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(index.bytes), "index-bytes")
		for b.Loop() {
			pg := &entryPager{start: 1_999_900, limit: 100, out: &SourceEntries{}}
			parse(func(rule parser.Rule) error {
				pg.add(SourceEntry{Value: rule.String(), Line: rule.Line, Enabled: true})
				return nil
			})
			if len(pg.out.Items) != 100 {
				b.Fatal("page count")
			}
		}
	})
	b.Run("warm-verified-index-page", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(index.bytes), "index-bytes")
		for b.Loop() {
			hash := sha256.New()
			file, err := os.Open(path) //nolint:gosec // benchmark-owned temporary fixture
			if err != nil {
				b.Fatal(err)
			}
			_, err = io.Copy(hash, file)
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				b.Fatal("verify body read failed")
			}
			pg := &entryPager{start: 1_999_900, limit: 100, out: &SourceEntries{}}
			if err := index.page(b.Context(), pg, nil); err != nil || len(pg.out.Items) != 100 {
				b.Fatal("verified page count")
			}
		}
	})
	b.Run("warm-index-page", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(float64(index.bytes), "index-bytes")
		for b.Loop() {
			pg := &entryPager{start: 1_999_900, limit: 100, out: &SourceEntries{}}
			if err := index.page(b.Context(), pg, nil); err != nil || len(pg.out.Items) != 100 || pg.out.Total != 2_000_000 {
				b.Fatal("page count")
			}
		}
	})
}
