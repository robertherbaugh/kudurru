package blocking

// Benchmarks behind the §7.3 performance gate (`make bench`):
//
//   - BenchmarkLatency/*: single-threaded per-lookup latency; each lookup is
//     timed individually and reported as p50-ns / p99-ns / p999-ns. The
//     timings include the cost of two time.Now calls, reported by
//     BenchmarkLatency/timer, so they overstate the lookup itself.
//   - BenchmarkEvaluate/*: average lookup cost (ns/op) without timers.
//   - BenchmarkCompile/*: parse + compile of a 1M-entry list from memory,
//     and the unchanged-list path that stops after the checksum.
//   - BenchmarkMemory/3M: RSS of a fresh process holding a 3M-entry
//     snapshot (1M suffix, 2M exact entries), measured in a child process
//     so the generator and test harness do not count.
//
// The fixtures are synthetic (internal/synth, ~23-byte names, seeded) plus
// the StevenBlack sample from testdata.

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/blocking/internal/synth"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

const (
	benchEntries   = 1_000_000
	benchQueries   = 1 << 16
	memoryEntries  = 3_000_000
	memoryHelperOn = "KUDURRU_BLOCKING_MEMORY_HELPER"
)

// Package-level sinks keep the compiler from discarding benchmark work.
var (
	sinkDecision Decision
	sinkList     *List
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureDir != "" {
		_ = os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

// fixture is one compiled selection plus query sets.
type fixture struct {
	sel          *Selection
	hits, misses []string
}

var (
	fixtureMu   sync.Mutex
	fixtures    = map[string]*fixture{}
	bodies      = map[string][]byte{}
	domainSets  = map[uint64][]string{}
	fixtureDir  string
	memoryFiles []string
)

func domains(seed uint64, n int) []string {
	if d, ok := domainSets[seed]; ok && len(d) == n {
		return d
	}
	d := synth.Unique(seed, n)
	domainSets[seed] = d
	return d
}

func listBody(b *testing.B, format string, seed uint64, n int) []byte {
	b.Helper()
	key := fmt.Sprintf("%s/%d/%d", format, seed, n)
	if body, ok := bodies[key]; ok {
		return body
	}
	var buf bytes.Buffer
	if err := synth.Write(&buf, format, domains(seed, n)); err != nil {
		b.Fatal(err)
	}
	bodies[key] = buf.Bytes()
	return bodies[key]
}

func compileBench(b *testing.B, name string, mode Mode, format parser.Format, body []byte) *List {
	b.Helper()
	l, err := Compile(Spec{Name: name, Mode: mode, Sources: []Source{BytesSource(name, format, body)}}, nil, CompileOptions{RegexHardCap: -1})
	if err != nil {
		b.Fatal(err)
	}
	return l
}

// queries picks hit queries from entries (subdomains of them when sub is
// true, half of the time) and miss queries from an unrelated seed, and
// verifies both sets against the selection.
func queries(b *testing.B, sel *Selection, entries []string, sub bool, missSeed uint64) (hits, misses []string) {
	b.Helper()
	g := synth.New(missSeed)
	for len(hits) < benchQueries && len(entries) > 0 {
		q := entries[g.IntN(len(entries))]
		if sub && g.IntN(2) == 0 {
			q = g.Prefix() + "." + q
		}
		if sel.Evaluate(q).Blocked() {
			hits = append(hits, q)
		}
	}
	for len(misses) < benchQueries {
		q := g.Domain()
		if sel.Evaluate(q).Action == ActionPass {
			misses = append(misses, q)
		}
	}
	return hits, misses
}

func getFixture(b *testing.B, name string) *fixture {
	b.Helper()
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if f, ok := fixtures[name]; ok {
		return f
	}
	snapOf := func(lists ...*List) *Snapshot {
		s, err := NewSnapshot(1, lists...)
		if err != nil {
			b.Fatal(err)
		}
		return s
	}
	f := &fixture{}
	switch name {
	case "suffix-1M":
		l := compileBench(b, "suffix", ModeBlock, parser.FormatAdblock, listBody(b, "adblock", 1, benchEntries))
		f.sel, _ = snapOf(l).Select([]string{"suffix"}, SelectOptions{})
		f.hits, f.misses = queries(b, f.sel, domains(1, benchEntries), true, 101)
	case "exact-1M":
		l := compileBench(b, "exact", ModeBlock, parser.FormatHosts, listBody(b, "hosts", 2, benchEntries))
		f.sel, _ = snapOf(l).Select([]string{"exact"}, SelectOptions{})
		f.hits, f.misses = queries(b, f.sel, domains(2, benchEntries), false, 102)
	case "policy-3M":
		// A heavy home setup: one adblock list, two hosts/domains lists,
		// a small allow list and 100 regexes.
		suffix := compileBench(b, "suffix", ModeBlock, parser.FormatAdblock, listBody(b, "adblock", 1, benchEntries))
		hosts := compileBench(b, "exact", ModeBlock, parser.FormatHosts, listBody(b, "hosts", 2, benchEntries))
		doms := compileBench(b, "domains", ModeBlock, parser.FormatDomains, listBody(b, "domains", 3, benchEntries))
		allow := compileBench(b, "allow", ModeAllow, parser.FormatDomains, []byte(strings.Join(synth.Unique(4, 1000), "\n")))
		re := compileBench(b, "regex", ModeBlock, parser.FormatRegex, []byte(strings.Join(synth.Regexes(5, 100), "\n")))
		f.sel, _ = snapOf(suffix, hosts, doms, allow, re).Select([]string{"allow", "suffix", "exact", "domains", "regex"}, SelectOptions{})
		all := slices.Concat(domains(1, benchEntries), domains(2, benchEntries), domains(3, benchEntries))
		f.hits, f.misses = queries(b, f.sel, all, false, 103)
	case "stevenblack":
		body, err := os.ReadFile(realCorpus)
		if err != nil {
			b.Fatal(err)
		}
		l := compileBench(b, "stevenblack", ModeBlock, parser.FormatAuto, body)
		f.sel, _ = snapOf(l).Select([]string{"stevenblack"}, SelectOptions{})
		var entries []string
		_, _ = parser.Parse(bytes.NewReader(body), parser.Options{}, func(r parser.Rule) error {
			entries = append(entries, strings.Clone(r.Pattern))
			return nil
		})
		f.hits, f.misses = queries(b, f.sel, entries, false, 104)
	default:
		if n, ok := strings.CutPrefix(name, "regex-"); ok {
			count, _ := strconv.Atoi(n)
			l := compileBench(b, "regex", ModeBlock, parser.FormatRegex, []byte(strings.Join(synth.Regexes(6, count), "\n")))
			f.sel, _ = snapOf(l).Select([]string{"regex"}, SelectOptions{})
			_, f.misses = queries(b, f.sel, nil, false, 105)
			break
		}
		b.Fatalf("unknown fixture %q", name)
	}
	runtime.GC()
	fixtures[name] = f
	return f
}

// latencyHist records per-lookup latencies with 1 ns buckets up to 8 µs.
type latencyHist struct {
	counts [8192]uint64
	over   []int64
	n      uint64
}

func (h *latencyHist) add(d time.Duration) {
	ns := max(d.Nanoseconds(), 0)
	if ns < int64(len(h.counts)) {
		h.counts[ns]++
	} else {
		h.over = append(h.over, ns)
	}
	h.n++
}

func (h *latencyHist) quantile(q float64) float64 {
	if h.n == 0 {
		return 0
	}
	rank := max(uint64(math.Ceil(q*float64(h.n))), 1)
	var c uint64
	for ns, k := range h.counts {
		c += k
		if c >= rank {
			return float64(ns)
		}
	}
	slices.Sort(h.over)
	return float64(h.over[rank-c-1])
}

func (h *latencyHist) report(b *testing.B) {
	b.ReportMetric(h.quantile(0.50), "p50-ns")
	b.ReportMetric(h.quantile(0.99), "p99-ns")
	b.ReportMetric(h.quantile(0.999), "p999-ns")
}

func benchLatency(b *testing.B, sel *Selection, qs []string) {
	h := new(latencyHist)
	mask := len(qs) - 1
	i := 0
	for b.Loop() {
		q := qs[i&mask]
		i++
		start := time.Now()
		d := sel.Evaluate(q)
		h.add(time.Since(start))
		sinkDecision = d
	}
	h.report(b)
}

// BenchmarkLatency measures single-threaded per-lookup latency; the
// suffix-1M p99 is the §7.3 gate (< 2 µs).
func BenchmarkLatency(b *testing.B) {
	for _, name := range []string{"suffix-1M", "exact-1M", "policy-3M", "stevenblack"} {
		b.Run(name+"/hit", func(b *testing.B) { benchLatency(b, getFixture(b, name).sel, getFixture(b, name).hits) })
		b.Run(name+"/miss", func(b *testing.B) { benchLatency(b, getFixture(b, name).sel, getFixture(b, name).misses) })
	}
	for _, n := range []int{100, 500, 1000} {
		name := fmt.Sprintf("regex-%d", n)
		b.Run(name+"/miss", func(b *testing.B) { benchLatency(b, getFixture(b, name).sel, getFixture(b, name).misses) })
	}
	b.Run("timer", func(b *testing.B) {
		h := new(latencyHist)
		for b.Loop() {
			start := time.Now()
			h.add(time.Since(start))
		}
		h.report(b)
	})
}

// BenchmarkEvaluate measures the average cost of Selection.Evaluate.
func BenchmarkEvaluate(b *testing.B) {
	for _, name := range []string{"suffix-1M", "exact-1M", "policy-3M"} {
		for _, kind := range []string{"hit", "miss"} {
			b.Run(name+"/"+kind, func(b *testing.B) {
				f := getFixture(b, name)
				qs := f.hits
				if kind == "miss" {
					qs = f.misses
				}
				b.ReportAllocs()
				mask := len(qs) - 1
				i := 0
				for b.Loop() {
					sinkDecision = f.sel.Evaluate(qs[i&mask])
					i++
				}
			})
		}
	}
}

// BenchmarkCompile measures parse + compile of 1M-entry lists (gate: < 5 s)
// and the checksum-only path taken when a list did not change.
func BenchmarkCompile(b *testing.B) {
	for _, c := range []struct {
		name   string
		format parser.Format
		seed   uint64
	}{
		{"adblock-1M", parser.FormatAdblock, 1},
		{"hosts-1M", parser.FormatHosts, 2},
	} {
		b.Run(c.name, func(b *testing.B) {
			body := listBody(b, string(c.format), c.seed, benchEntries)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				sinkList = compileBench(b, "x", ModeBlock, c.format, body)
			}
			b.ReportMetric(float64(sinkList.Stats().Entries), "entries")
		})
	}
	b.Run("unchanged-1M", func(b *testing.B) {
		body := listBody(b, "adblock", 1, benchEntries)
		spec := Spec{Name: "x", Mode: ModeBlock, Sources: []Source{BytesSource("x", parser.FormatAdblock, body)}}
		prev := compileBench(b, "x", ModeBlock, parser.FormatAdblock, body)
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for b.Loop() {
			l, err := Compile(spec, prev, CompileOptions{RegexHardCap: -1})
			if err != nil || l != prev {
				b.Fatal("unchanged list was recompiled")
			}
		}
	})
}

// memoryFixture writes the 3M-entry lists (1M adblock suffix entries, 1M
// hosts and 1M domains exact entries) to files once per process.
func memoryFixture(b *testing.B) []string {
	b.Helper()
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if memoryFiles != nil {
		return memoryFiles
	}
	dir, err := os.MkdirTemp("", "kudurru-blocking-bench-")
	if err != nil {
		b.Fatal(err)
	}
	fixtureDir = dir
	per := memoryEntries / 3
	for i, format := range []string{"adblock", "hosts", "domains"} {
		path := filepath.Join(dir, format+".txt")
		if err := os.WriteFile(path, listBody(b, format, uint64(i+1), per), 0o600); err != nil {
			b.Fatal(err)
		}
		memoryFiles = append(memoryFiles, path)
	}
	return memoryFiles
}

type memResult struct {
	entries                     int
	rssKB, hwmKB, baseKB, heapB float64
	engineB, compileNS          float64
}

// BenchmarkMemory reports the RSS of a fresh process holding a 3M-entry
// snapshot (gate: < 150 MB). It runs TestMemoryHelper in a child process.
func BenchmarkMemory(b *testing.B) {
	if runtime.GOOS != "linux" {
		b.Skip("needs /proc/self/status")
	}
	b.Run("3M", func(b *testing.B) {
		benchMemory(b, memoryFixture(b), false)
	})
}

// benchMemory runs TestMemoryHelper over files in a child process per
// iteration and reports its memory metrics.
func benchMemory(b *testing.B, files []string, hostsSubdomainsFirst bool) {
	var r memResult
	for b.Loop() {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMemoryHelper$", "-test.v", "-test.count=1") //nolint:gosec // re-executes this test binary
		cmd.Env = append(os.Environ(), memoryHelperOn+"="+strings.Join(files, string(os.PathListSeparator)))
		if hostsSubdomainsFirst {
			cmd.Env = append(cmd.Env, memoryHelperOn+"_SUFFIX_FIRST=1")
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			b.Fatalf("helper: %v\n%s", err, out)
		}
		r = parseMemResult(b, out)
	}
	b.ReportMetric(r.rssKB/1024, "rss-MB")
	b.ReportMetric(r.hwmKB/1024, "peak-rss-MB")
	b.ReportMetric(r.baseKB/1024, "base-rss-MB")
	b.ReportMetric(r.heapB/(1<<20), "heap-MB")
	b.ReportMetric(r.engineB/float64(r.entries), "engine-B/entry")
	b.ReportMetric(r.compileNS/1e9, "compile-s")
	b.ReportMetric(float64(r.entries), "entries")
}

// externalListsEnv names real list files (hosts or adblock, separated by
// the OS path-list separator) for BenchmarkExternal. Lists are never
// committed; see dev-docs/spikes/blocking.md for the Unlicense lists used in
// the spike report.
const externalListsEnv = "KUDURRU_BLOCKING_BENCH_LISTS"

// BenchmarkExternal repeats the latency, compile and memory measurements on
// real lists named in $KUDURRU_BLOCKING_BENCH_LISTS; it is skipped when the
// variable is unset, so `make bench` does not depend on downloaded data.
// "exact" compiles every file as given; "suffix" compiles the first file
// with hosts entries matching subdomains (a real-data suffix set).
func BenchmarkExternal(b *testing.B) {
	env := os.Getenv(externalListsEnv)
	if env == "" {
		b.Skip("set " + externalListsEnv + " to real list files")
	}
	files := filepath.SplitList(env)
	load := func(b *testing.B, suffixFirst bool) (*Selection, []string, int) {
		var specs []Spec
		var names, entries []string
		for i, path := range files {
			body, err := os.ReadFile(path) //nolint:gosec // operator-supplied benchmark input
			if err != nil {
				b.Fatal(err)
			}
			name := fmt.Sprintf("%d-%s", i, filepath.Base(path))
			src := BytesSource(name, parser.FormatAuto, body)
			src.HostsSubdomains = suffixFirst && i == 0
			specs = append(specs, Spec{Name: name, Mode: ModeBlock, Sources: []Source{src}})
			names = append(names, name)
			if !suffixFirst || i == 0 {
				_, _ = parser.Parse(bytes.NewReader(body), parser.Options{}, func(r parser.Rule) error {
					entries = append(entries, strings.Clone(r.Pattern))
					return nil
				})
			}
			if suffixFirst {
				break
			}
		}
		snap, reports := Rebuild(nil, specs, CompileOptions{})
		for _, r := range reports {
			if r.Err != nil {
				b.Fatal(r.Err)
			}
		}
		sel, _ := snap.Select(names, SelectOptions{})
		return sel, entries, snap.Entries()
	}
	for _, mode := range []string{"exact", "suffix"} {
		var once sync.Once
		var f fixture
		var n int
		get := func(b *testing.B) *fixture {
			once.Do(func() {
				var entries []string
				f.sel, entries, n = load(b, mode == "suffix")
				f.hits, f.misses = queries(b, f.sel, entries, mode == "suffix", 106)
				runtime.GC()
			})
			return &f
		}
		b.Run(mode+"/hit", func(b *testing.B) {
			benchLatency(b, get(b).sel, get(b).hits)
			b.ReportMetric(float64(n), "entries")
		})
		b.Run(mode+"/miss", func(b *testing.B) {
			benchLatency(b, get(b).sel, get(b).misses)
			b.ReportMetric(float64(n), "entries")
		})
	}
	if runtime.GOOS == "linux" {
		b.Run("memory-exact", func(b *testing.B) { benchMemory(b, files, false) })
		b.Run("memory-suffix-first", func(b *testing.B) { benchMemory(b, files, true) })
	}
}

func parseMemResult(b *testing.B, out []byte) memResult {
	b.Helper()
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "KUDURRU_MEMORY ")
		if !ok {
			continue
		}
		var r memResult
		for kv := range strings.FieldsSeq(line) {
			k, v, _ := strings.Cut(kv, "=")
			f, _ := strconv.ParseFloat(v, 64)
			switch k {
			case "entries":
				r.entries = int(f)
			case "rss_kb":
				r.rssKB = f
			case "hwm_kb":
				r.hwmKB = f
			case "base_rss_kb":
				r.baseKB = f
			case "heap_inuse":
				r.heapB = f
			case "engine_bytes":
				r.engineB = f
			case "compile_ns":
				r.compileNS = f
			}
		}
		return r
	}
	b.Fatalf("no KUDURRU_MEMORY line in helper output:\n%s", out)
	return memResult{}
}

// TestMemoryHelper is the child side of BenchmarkMemory; it is skipped in
// normal test runs.
func TestMemoryHelper(t *testing.T) {
	env := os.Getenv(memoryHelperOn)
	if env == "" {
		t.Skip("helper process for BenchmarkMemory")
	}
	base := procStatusKB(t, "VmRSS")
	suffixFirst := os.Getenv(memoryHelperOn+"_SUFFIX_FIRST") != ""
	var specs []Spec
	for i, path := range filepath.SplitList(env) {
		name := fmt.Sprintf("%d-%s", i, filepath.Base(path))
		src := FileSource(name, parser.FormatAuto, path)
		src.HostsSubdomains = suffixFirst && i == 0
		specs = append(specs, Spec{Name: name, Mode: ModeBlock, Sources: []Source{src}})
	}
	start := time.Now()
	snap, reports := Rebuild(nil, specs, CompileOptions{})
	elapsed := time.Since(start)
	for _, r := range reports {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
	}
	runtime.GC()
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("KUDURRU_MEMORY entries=%d rss_kb=%d hwm_kb=%d base_rss_kb=%d heap_inuse=%d engine_bytes=%d compile_ns=%d\n",
		snap.Entries(), procStatusKB(t, "VmRSS"), procStatusKB(t, "VmHWM"), base, ms.HeapInuse, snap.Bytes(), elapsed.Nanoseconds())
	runtime.KeepAlive(snap)
}

func procStatusKB(t *testing.T, field string) int {
	t.Helper()
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(data)) {
		if v, ok := strings.CutPrefix(line, field+":"); ok {
			n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("%s not in /proc/self/status", field)
	return 0
}
