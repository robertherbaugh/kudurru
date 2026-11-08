package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Malformed evidence must not become an apparent improvement or silently
// satisfy an absolute ceiling. None of these inputs represents a native sample.
func TestRejectInvalidMeasurementEvidence(t *testing.T) {
	for _, value := range []string{"NaN", "+Inf", "-Inf", "-1", "1e400"} {
		t.Run(value, func(t *testing.T) {
			if _, err := Parse(strings.NewReader("BenchmarkX-6 1 " + value + " ns/op\n")); err == nil {
				t.Fatal("invalid measurement admitted")
			}
			if _, err := ParseRules(strings.NewReader("limit . ns/op " + value)); err == nil {
				t.Fatal("invalid limit admitted")
			}
		})
	}
	for _, line := range []string{
		"BenchmarkX-6 0 10 ns/op", "BenchmarkX-6 -1 10 ns/op",
		"BenchmarkX-6 nope 10 ns/op", "BenchmarkX-0 1 10 ns/op",
		"BenchmarkX-6 1 10 ns/op 20 ns/op",
		"BenchmarkX-6 1 10 ns/op trailing", "BenchmarkX-6 1",
		"cpu: first\ncpu: second\nBenchmarkX-6 1 10 ns/op",
		"cpu: \nBenchmarkX-6 1 10 ns/op",
	} {
		if _, err := Parse(strings.NewReader(line)); err == nil {
			t.Errorf("invalid evidence accepted: %q", line)
		}
	}
	one := mustParse(t, "BenchmarkX 1 0 ns/op 0 allocs/op\n")
	if !one.Procs[1] {
		t.Fatal("native single-processor spelling lost")
	}
}

func TestRequiredMeasurementShape(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	line := "BenchmarkX-6 1 100 ns/op 0 allocs/op\n"
	full := "cpu: stable\n" + strings.Repeat(line, 5)
	baseline := write("baseline", full)
	cases := []struct {
		name, body, message string
		want                int
	}{
		{"complete", full, "benchgate: ok", 0},
		{"one-sample", "cpu: stable\n" + line, "has 1 samples; expected 5", 1},
		{"six-samples", full + line, "has 6 samples; expected 5", 1},
		{"partial-metric", strings.Replace(full, " 0 allocs/op", "", 1), "allocs/op has 4 samples", 1},
		{"mixed-procs", strings.Replace(full, "BenchmarkX-6", "BenchmarkX-2", 1), "expected GOMAXPROCS=6", 1},
		{"missing-cpu", strings.TrimPrefix(full, "cpu: stable\n"), "requires CPU headers", 1},
		{"different-cpu", strings.Replace(full, "stable", "different", 1), "WARNING baseline CPU", 1},
		{"actual-regression", strings.ReplaceAll(full, "100 ns/op", "200 ns/op"), "regression", 1},
		{"native-failure", full + "FAIL: go test exited non-zero\n", "benchmark run failed", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := write(tc.name, tc.body)
			var out, errOut bytes.Buffer
			code := run([]string{"-baseline", baseline, "-strict-cpu", "-samples", "5", "-procs", "6", path}, &out, &errOut)
			if code != tc.want || !strings.Contains(out.String(), tc.message) {
				t.Fatalf("exit %d, want %d containing %q: %s%s", code, tc.want, tc.message, &out, &errOut)
			}
		})
	}
	for _, args := range [][]string{
		{"-threshold", "NaN"}, {"-threshold", "+Inf"}, {"-threshold", "-1"},
		{"-samples", "-1"}, {"-procs", "-1"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(append(args, baseline), &out, &errOut); code != 2 {
				t.Fatalf("invalid control admitted: exit %d, %s%s", code, &out, &errOut)
			}
		})
	}
}

func TestRequiredBaselineShape(t *testing.T) {
	line := "BenchmarkX-6 1 100 ns/op 0 allocs/op\n"
	full := "cpu: stable\n" + strings.Repeat(line, 5)
	for _, tc := range []struct {
		name, body, message string
		controls            bool
		want                int
	}{
		{"complete", full, "benchgate: ok", true, 0},
		{"empty", "cpu: stable\n", "no baseline benchmark results", false, 1},
		{"native-failure", full + "FAIL: go test exited non-zero\n", "baseline run failed", false, 1},
		{"panic", full + "panic: helper failed\n", "baseline run failed", false, 1},
		{"one-sample", "cpu: stable\n" + line, "baseline BenchmarkX allocs/op has 1 samples", true, 1},
		{"six-samples", full + line, "has 6 samples; expected 5", true, 1},
		{"partial-metric", strings.Replace(full, " 0 allocs/op", "", 1), "allocs/op has 4 samples", true, 1},
		{"different-procs", strings.ReplaceAll(full, "BenchmarkX-6", "BenchmarkX-2"), "expected GOMAXPROCS=6 on every baseline result", true, 1},
		{"mixed-procs", strings.Replace(full, "BenchmarkX-6", "BenchmarkX-2", 1), "expected GOMAXPROCS=6 on every baseline result", true, 1},
		{"legacy-without-shape-controls", "cpu: stable\n" + line, "benchgate: ok", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			baseline, current := filepath.Join(dir, "baseline"), filepath.Join(dir, "current")
			for path, body := range map[string]string{baseline: tc.body, current: full} {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"-baseline", baseline, "-strict-cpu"}
			if tc.controls {
				args = append(args, "-samples", "5", "-procs", "6")
			}
			var out, errOut bytes.Buffer
			code := run(append(args, current), &out, &errOut)
			if code != tc.want || !strings.Contains(out.String(), tc.message) {
				t.Fatalf("exit %d, want %d containing %q: %s%s", code, tc.want, tc.message, &out, &errOut)
			}
		})
	}
}

func TestSocketThroughputDirection(t *testing.T) {
	baseline := mustParse(t, "BenchmarkUDPSockets/sockets=6-6 100 1000 q/s 0 lost\n")
	for _, tc := range []struct {
		throughput, lost, status string
	}{
		{"700", "0", "regression"},
		{"1300", "0", "improved"},
		{"1000", "1", "ok"},
	} {
		current := mustParse(t, "BenchmarkUDPSockets/sockets=6-6 100 "+tc.throughput+" q/s "+tc.lost+" lost\n")
		findings := statuses(Compare(baseline, current, nil, 0.20))
		if findings["BenchmarkUDPSockets/sockets=6 q/s"] != tc.status {
			t.Fatalf("throughput %s: %v", tc.throughput, findings)
		}
		if tc.lost != "0" && findings["BenchmarkUDPSockets/sockets=6 lost"] != "regression" {
			t.Fatal("packet loss did not remain a lower-is-better metric", findings)
		}
	}
}
