package deploy_test

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	yamlv3 "go.yaml.in/yaml/v3"
)

// update rewrites generated assets (the Grafana dashboard, the chart's
// copies of deploy/ files, the kustomize bases rendered from the chart)
// instead of comparing them: go test ./deploy -update.
var update = flag.Bool("update", false, "rewrite generated deployment assets")

// repoRoot is the module root (the parent of this package's directory).
func repoRoot(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

var (
	metricsOnce sync.Once
	metricsSet  map[string]bool
	metricsErr  error
)

// metricNameRE matches a full metric name literal in Go source.
var metricNameRE = regexp.MustCompile(`"(kudurru_[a-z0-9_]*[a-z0-9])"`)

// codeMetrics returns every kudurru_* metric name that appears as a string
// literal in the non-test Go sources under internal/ and cmd/: the names the
// binary registers with prometheus.New*Opts{Name: ...} or NewDesc.
func codeMetrics(t testing.TB) map[string]bool {
	t.Helper()
	root := repoRoot(t)
	metricsOnce.Do(func() {
		metricsSet = map[string]bool{}
		for _, dir := range []string{"internal", "cmd"} {
			err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
					return nil
				}
				b, err := os.ReadFile(p) //nolint:gosec // repository sources
				if err != nil {
					return err
				}
				for _, m := range metricNameRE.FindAllSubmatch(b, -1) {
					metricsSet[string(m[1])] = true
				}
				return nil
			})
			if err != nil {
				metricsErr = err
				return
			}
		}
	})
	if metricsErr != nil {
		t.Fatal(metricsErr)
	}
	return metricsSet
}

// exprMetricRE finds metric-looking identifiers in a PromQL expression.
var exprMetricRE = regexp.MustCompile(`\b(kudurru_[a-z0-9_]+)`)

// unknownMetrics returns the kudurru_* names in expr that the code does not
// register; histogram series (_bucket, _sum, _count) resolve to their base.
func unknownMetrics(t testing.TB, expr string) []string {
	t.Helper()
	known := codeMetrics(t)
	var out []string
	for _, m := range exprMetricRE.FindAllStringSubmatch(expr, -1) {
		name := m[1]
		if known[name] {
			continue
		}
		base := name
		for _, suf := range []string{"_bucket", "_sum", "_count"} {
			base = strings.TrimSuffix(base, suf)
		}
		if base != name && known[base] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// decodeDocs splits a multi-document YAML stream into generic maps,
// skipping empty documents.
func decodeDocs(t testing.TB, b []byte) []map[string]any {
	t.Helper()
	dec := yamlv3.NewDecoder(bytes.NewReader(b))
	var docs []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode YAML: %v", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

// dig walks nested maps (and list indexes given as int) and returns the
// value or nil.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// writeOrCompare writes want to path under -update, otherwise fails when the
// file differs.
func writeOrCompare(t *testing.T, path string, want []byte, regen string) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path) //nolint:gosec // repository file
	if err != nil {
		t.Fatalf("%v (regenerate with %s)", err, regen)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date; regenerate with %s", path, regen)
	}
}
