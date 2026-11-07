package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baselineText = `goos: linux
goarch: amd64
pkg: github.com/robertherbaugh/kudurru/internal/blocking
cpu: Test CPU @ 1GHz
BenchmarkLatency/suffix-1M/hit-8   1000  800 ns/op  600 p50-ns  1300 p99-ns  1900 p999-ns  0 B/op  0 allocs/op
BenchmarkLatency/suffix-1M/hit-8   1000  820 ns/op  610 p50-ns  1320 p99-ns  2400 p999-ns  0 B/op  0 allocs/op
BenchmarkLatency/suffix-1M/hit-8   1000  810 ns/op  605 p50-ns  1310 p99-ns  2100 p999-ns  0 B/op  0 allocs/op
BenchmarkCompile/adblock-1M-8      2  900000000 ns/op  30.0 MB/s  1000000 entries  121457176 B/op  141141 allocs/op
BenchmarkMemory/3M-8               1  2400000000 ns/op  91.6 rss-MB  147.5 peak-rss-MB
BenchmarkTiny-8                    100000000  10.0 ns/op
PASS
ok  	github.com/robertherbaugh/kudurru/internal/blocking	39.022s
`

const limitsText = `# comment
limit  ^BenchmarkLatency/suffix-1M/(hit|miss)$  p99-ns  2000
limit  ^BenchmarkMemory/3M$                     rss-MB  150
ignore ^BenchmarkMemory/                        ns/op
ignore .                                        entries
`

func mustParse(t *testing.T, s string) *Results {
	t.Helper()
	r, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParse(t *testing.T) {
	r := mustParse(t, baselineText)
	if r.CPU != "Test CPU @ 1GHz" || len(r.Failures) != 0 {
		t.Fatalf("CPU %q failures %v", r.CPU, r.Failures)
	}
	if m, ok := r.Median("BenchmarkLatency/suffix-1M/hit", "p99-ns"); !ok || m != 1310 {
		t.Errorf("median p99 = %v %v, want 1310", m, ok)
	}
	if m, _ := r.Median("BenchmarkLatency/suffix-1M/hit", "p999-ns"); m != 2100 {
		t.Errorf("median p999 = %v", m)
	}
	if _, ok := r.Median("BenchmarkNope", "ns/op"); ok {
		t.Error("median of a missing benchmark")
	}
	two := mustParse(t, "BenchmarkX-2 1 10 ns/op\nBenchmarkX-2 1 20 ns/op\n")
	if m, _ := two.Median("BenchmarkX", "ns/op"); m != 15 {
		t.Errorf("even median = %v", m)
	}
	f := mustParse(t, "--- FAIL: BenchmarkX\npanic: boom\nFAIL\tpkg\t1s\nBenchmarkEcho\n")
	if len(f.Failures) != 3 || len(f.Samples) != 0 {
		t.Errorf("failures %v samples %v", f.Failures, f.Samples)
	}
	if _, err := Parse(strings.NewReader("BenchmarkX-2 1 abc ns/op\n")); err == nil {
		t.Error("bad value accepted")
	}
}

func TestParseRules(t *testing.T) {
	rules, err := ParseRules(strings.NewReader(limitsText))
	if err != nil || len(rules) != 4 || rules[0].Max != 2000 || !rules[2].Ignore {
		t.Fatalf("rules %+v, %v", rules, err)
	}
	for _, bad := range []string{"limit x p99-ns", "limit x p99-ns abc", "ignore ( ns/op", "bogus"} {
		if _, err := ParseRules(strings.NewReader(bad)); err == nil {
			t.Errorf("ParseRules(%q) accepted", bad)
		}
	}
}

func statuses(fs []Finding) map[string]string {
	m := map[string]string{}
	for _, f := range fs {
		m[f.Name+" "+f.Unit] = f.Status
	}
	return m
}

func TestCompare(t *testing.T) {
	base := mustParse(t, baselineText)
	rules, _ := ParseRules(strings.NewReader(limitsText))

	same := Compare(base, base, rules, 0.20)
	for _, f := range same {
		if f.Failed() || f.Status == "regression" {
			t.Errorf("identical run failed: %+v", f)
		}
	}
	if s := statuses(same); s["BenchmarkMemory/3M ns/op"] != "" || s["BenchmarkCompile/adblock-1M entries"] != "" {
		t.Error("ignored metrics compared")
	}

	worse := strings.NewReplacer(
		"1300 p99-ns", "1700 p99-ns", // +30 % in every run: regression
		"1320 p99-ns", "1720 p99-ns",
		"1310 p99-ns", "1710 p99-ns",
		"605 p50-ns", "620 p50-ns", // +2.5 %: ok
		"30.0 MB/s", "20.0 MB/s", // throughput -33 %: regression
		"91.6 rss-MB", "160.0 rss-MB", // over the 150 MB limit and +75 %
		"10.0 ns/op", "14.0 ns/op", // +40 % but below the 25 ns floor: ok
		"141141 allocs/op", "100000 allocs/op", // -29 %: improved
	).Replace(baselineText)
	s := statuses(Compare(base, mustParse(t, worse), rules, 0.20))
	want := map[string]string{
		"BenchmarkLatency/suffix-1M/hit p99-ns": "regression",
		"BenchmarkLatency/suffix-1M/hit p50-ns": "ok",
		"BenchmarkCompile/adblock-1M MB/s":      "regression",
		"BenchmarkMemory/3M rss-MB":             "limit",
		"BenchmarkTiny ns/op":                   "ok",
		"BenchmarkCompile/adblock-1M allocs/op": "improved",
	}
	for k, v := range want {
		if s[k] != v {
			t.Errorf("%s: status %q, want %q", k, s[k], v)
		}
	}

	allocs := strings.ReplaceAll(baselineText, "0 B/op  0 allocs/op", "48 B/op  1 allocs/op")
	if s := statuses(Compare(base, mustParse(t, allocs), nil, 0.20)); s["BenchmarkLatency/suffix-1M/hit allocs/op"] != "regression" {
		t.Errorf("new allocation not flagged: %v", s)
	}

	missing := mustParse(t, "BenchmarkNew-8 10 5 ns/op\n")
	s = statuses(Compare(base, missing, nil, 0.20))
	if s["BenchmarkTiny ns/op"] != "missing" || s["BenchmarkNew ns/op"] != "new" {
		t.Errorf("missing/new = %v", s)
	}

	limitsOnly := Compare(nil, base, rules, 0.20)
	if len(limitsOnly) != 0 {
		t.Errorf("limits-only run on a passing baseline = %+v", limitsOnly)
	}
}

func TestRun(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := write("base.txt", baselineText)
	limits := write("limits.txt", limitsText)
	slow := write("slow.txt", strings.NewReplacer("1300 p99-ns", "1700 p99-ns", "1320 p99-ns", "1720 p99-ns", "1310 p99-ns", "1710 p99-ns").Replace(baselineText))
	overLimit := write("over.txt", strings.NewReplacer("1300 p99-ns", "2500 p99-ns", "1320 p99-ns", "2520 p99-ns", "1310 p99-ns", "2510 p99-ns").Replace(baselineText))
	otherCPU := write("cpu.txt", strings.Replace(baselineText, "Test CPU", "Other CPU", 1))
	empty := write("empty.txt", "PASS\n")

	tests := []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"-baseline", base, "-limits", limits, base}, 0, "benchgate: ok"},
		{[]string{"-baseline", base, "-limits", limits, slow}, 1, "regression"},
		{[]string{"-limits", limits, slow}, 0, "benchgate: ok"}, // +30 % but under the 2000 ns limit
		{[]string{"-limits", limits, overLimit}, 1, "exceeds the limit 2000"},
		{[]string{"-baseline", base, otherCPU}, 0, "WARNING baseline CPU"},
		{[]string{"-baseline", base, "-strict-cpu", otherCPU}, 1, "WARNING"},
		{[]string{empty}, 1, "no benchmark results"},
		{[]string{}, 2, ""},
		{[]string{"-baseline", filepath.Join(dir, "nope"), base}, 2, ""},
		{[]string{"-limits", filepath.Join(dir, "nope"), base}, 2, ""},
		{[]string{"-bogus"}, 2, ""},
	}
	for _, tc := range tests {
		var stdout, stderr bytes.Buffer
		code := run(tc.args, &stdout, &stderr)
		if code != tc.code || !strings.Contains(stdout.String(), tc.out) {
			t.Errorf("run(%v) = %d\n%s%s\nwant %d containing %q", tc.args, code, stdout.String(), stderr.String(), tc.code, tc.out)
		}
	}
}
