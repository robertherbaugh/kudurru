package blocking

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// countingSource wraps a body and counts Open calls.
func countingSource(name string, format parser.Format, body *string, opens *int) Source {
	return Source{Name: name, Format: format, Open: func() (io.ReadCloser, error) {
		*opens++
		return io.NopCloser(strings.NewReader(*body)), nil
	}}
}

func mustCompile(t *testing.T, spec Spec) *List {
	t.Helper()
	l, err := Compile(spec, nil, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestCompileChecksumSkip(t *testing.T) {
	body := "! Title: one\n! Last modified: Mon\n||ads.example^\n0.0.0.0 tracker.example\n"
	opens := 0
	spec := Spec{Name: "ads", Mode: ModeBlock, Sources: []Source{countingSource("s", parser.FormatAuto, &body, &opens)}}

	first, err := Compile(spec, nil, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if opens != 2 {
		t.Fatalf("first compile opened the source %d times, want 2 (checksum + parse)", opens)
	}

	body = "! Title: one\n! Last modified: Tue\n\n||ads.example^   \n0.0.0.0 tracker.example\n"
	opens = 0
	second, err := Compile(spec, first, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second != first || opens != 1 {
		t.Fatalf("comment-only change recompiled (same=%v, opens=%d)", second == first, opens)
	}

	if third, _ := Compile(spec, first, CompileOptions{RegexHardCap: 7}); third == first {
		t.Fatal("an options change did not recompile")
	}
	modeSpec := spec
	modeSpec.Mode = ModeAllow
	if fourth, _ := Compile(modeSpec, first, CompileOptions{}); fourth == first {
		t.Fatal("a mode change did not recompile")
	}

	body += "||new.example^\n"
	fifth, err := Compile(spec, first, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fifth == first || fifth.Fingerprint() == first.Fingerprint() {
		t.Fatal("a rule change did not recompile")
	}
	if _, ok := fifth.Match("x.new.example"); !ok {
		t.Fatal("new rule not compiled")
	}
}

func TestCompileValidation(t *testing.T) {
	src := LinesSource("s", parser.FormatAuto, []string{"a.example"})
	tests := []struct {
		spec Spec
		want error
	}{
		{Spec{Mode: ModeBlock, Sources: []Source{src}}, ErrNoName},
		{Spec{Name: "x", Mode: "deny", Sources: []Source{src}}, ErrBadMode},
		{Spec{Name: "x", Mode: ModeBlock}, ErrNoSources},
		{Spec{Name: "x", Mode: ModeBlock, Sources: []Source{{Name: "nil"}}}, nil},
		{Spec{Name: "x", Mode: ModeBlock, Sources: []Source{{Name: "f", Format: "csv", Open: src.Open}}}, parser.ErrUnknownFormat},
	}
	for _, tc := range tests {
		_, err := Compile(tc.spec, nil, CompileOptions{})
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("Compile(%+v) = %v, want %v", tc.spec, err, tc.want)
		}
	}
}

func TestCompileSourceFailures(t *testing.T) {
	good := LinesSource("good", parser.FormatAdblock, []string{"||ads.example^"})
	html := BytesSource("html", parser.FormatAuto, []byte("<!DOCTYPE html><html>captive portal</html>"))
	boom := errors.New("connection reset")
	broken := Source{Name: "broken", Open: func() (io.ReadCloser, error) { return nil, boom }}

	l, err := Compile(Spec{Name: "mixed", Mode: ModeBlock, Sources: []Source{good, html}}, nil, CompileOptions{})
	if !errors.Is(err, parser.ErrHTML) || l == nil {
		t.Fatalf("Compile = (%v, %v), want a partial list and ErrHTML", l, err)
	}
	if _, ok := l.Match("ads.example"); !ok {
		t.Fatal("good source missing from partial list")
	}
	srcs := l.Sources()
	if srcs[1].Err == nil || srcs[0].Err != nil || len(l.Warnings()) == 0 {
		t.Fatalf("source errors not reported: %+v %q", srcs, l.Warnings())
	}

	if l, err := Compile(Spec{Name: "dead", Mode: ModeBlock, Sources: []Source{broken}}, nil, CompileOptions{}); l != nil || !errors.Is(err, boom) {
		t.Fatalf("all-failed Compile = (%v, %v)", l, err)
	}

	// A partial list is never reused by the checksum skip.
	again, _ := Compile(Spec{Name: "mixed", Mode: ModeBlock, Sources: []Source{good, html}}, l, CompileOptions{})
	if again == l {
		t.Fatal("partial list reused")
	}
}

func TestCompileStats(t *testing.T) {
	l := mustCompile(t, Spec{Name: "s", Mode: ModeBlock, Sources: []Source{
		LinesSource("a", parser.FormatAdblock, []string{
			"||ads.example^", "||ads.example^", "*.ads.example", "dup.example", "dup.example",
			"@@||ok.ads.example^", "/^re[0-9]\\./", "/^re[0-9]\\./", "||x.example^$important",
			"example.org##.ad", "127.0.0.1 localhost", "192.0.2.1",
		}),
		LinesSource("b", parser.FormatHosts, []string{"0.0.0.0 dup.example other.example"}),
	}})
	st := l.Stats()
	want := Stats{
		Entries: 6, RuleCount: 11, Exceptions: 1, Duplicates: 5,
		Invalid: 1, Unsupported: 1, Ignored: 1, Regexes: 1, RegexUnindexed: 1,
	}
	st.Bytes, st.CompileTime = 0, 0
	if st != want {
		t.Errorf("Stats = %+v\nwant    %+v", st, want)
	}
	if l.Stats().Bytes <= 0 || l.Stats().CompileTime <= 0 {
		t.Error("Bytes or CompileTime not set")
	}
	u := l.Unsupported()
	if len(u) != 1 || u[0].Modifiers[0].Name != "important" {
		t.Errorf("Unsupported() = %+v", u)
	}
	if l.Name() != "s" || l.Mode() != ModeBlock {
		t.Error("Name/Mode")
	}
}

// TestCompiledEntriesDoNotAlias compiles a list whose first rules are
// followed by enough lines to overwrite the parser's read buffer many times
// and checks that the compiled entries, regex sources included, are intact.
func TestCompiledEntriesDoNotAlias(t *testing.T) {
	body := "/^kept-regex[0-9]\\./\n||kept.example^\nkept-exact.example\n" + strings.Repeat("0.0.0.0 filler-line-overwriting-the-buffer.example\n", 20000)
	l := mustCompile(t, Spec{Name: "a", Mode: ModeBlock, Sources: []Source{BytesSource("s", parser.FormatAdblock, []byte(body))}})
	for name, rule := range map[string]string{
		"kept-regex1.example": `/^kept-regex[0-9]\./`,
		"x.kept.example":      "||kept.example^",
		"kept-exact.example":  "kept-exact.example",
	} {
		if m, ok := l.Match(name); !ok || m.Rule() != rule {
			t.Errorf("Match(%q) = %q, %v; want %q", name, m.Rule(), ok, rule)
		}
	}
}

func TestRegexCapAndWarnings(t *testing.T) {
	var lines []string
	for i := range 600 {
		lines = append(lines, fmt.Sprintf("/^r%d\\./", i))
	}
	lines = append(lines, "@@/^ok\\./")
	spec := Spec{Name: "re", Mode: ModeBlock, Sources: []Source{LinesSource("s", parser.FormatAdblock, lines)}}

	l, err := Compile(spec, nil, CompileOptions{RegexHardCap: 550})
	if err != nil {
		t.Fatal(err)
	}
	if st := l.Stats(); st.Regexes != 550 || st.RegexDropped != 51 {
		t.Fatalf("Regexes = %d, RegexDropped = %d, want 550, 51", st.Regexes, st.RegexDropped)
	}
	w := strings.Join(l.Warnings(), "\n")
	if !strings.Contains(w, "550 regex rules (more than 500)") || !strings.Contains(w, "51 regex rules dropped") {
		t.Errorf("warnings = %q", w)
	}
	if _, ok := l.Match("r549.example"); !ok {
		t.Error("regex under the cap not compiled")
	}
	if _, ok := l.Match("r550.example"); ok {
		t.Error("regex over the cap compiled")
	}

	unlimited, _ := Compile(spec, nil, CompileOptions{RegexHardCap: -1})
	if st := unlimited.Stats(); st.Regexes != 601 || st.RegexDropped != 0 {
		t.Fatalf("unlimited: Regexes = %d, RegexDropped = %d", st.Regexes, st.RegexDropped)
	}
}

func TestUnindexedRegexWarning(t *testing.T) {
	var lines []string
	for i := range 60 {
		lines = append(lines, fmt.Sprintf(`^[a-z]{%d}\.`, i+1))
	}
	lines = append(lines, `^long-literal[0-9]+\.`)
	l := mustCompile(t, Spec{Name: "re", Mode: ModeBlock, Sources: []Source{LinesSource("s", parser.FormatRegex, lines)}})
	if st := l.Stats(); st.Regexes != 61 || st.RegexUnindexed != 60 {
		t.Fatalf("Regexes = %d, RegexUnindexed = %d", st.Regexes, st.RegexUnindexed)
	}
	if w := strings.Join(l.Warnings(), "\n"); !strings.Contains(w, "60 regex rules have no literal") {
		t.Errorf("warnings = %q", w)
	}
}

func testSnapshot(t *testing.T) *Snapshot {
	t.Helper()
	ads := mustCompile(t, Spec{Name: "ads", Mode: ModeBlock, Sources: []Source{LinesSource("s", parser.FormatAdblock, []string{
		"||ads.example^", "@@||ok.ads.example^", "/^banner[0-9]*\\./", "192.0.2.66",
	})}})
	malware := mustCompile(t, Spec{Name: "malware", Mode: ModeBlock, Sources: []Source{LinesSource("s", parser.FormatHosts, []string{
		"0.0.0.0 evil.example", "0.0.0.0 ok.ads.example",
	})}})
	allow := mustCompile(t, Spec{Name: "allow", Mode: ModeAllow, Sources: []Source{LinesSource("s", parser.FormatDomains, []string{
		"good.ads.example",
	})}})
	s, err := NewSnapshot(7, ads, malware, allow, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshotContains(t *testing.T) {
	s := testSnapshot(t)
	if s.Generation() != 7 || len(s.Lists()) != 3 || s.Entries() != 7 || s.Bytes() <= 0 {
		t.Fatalf("generation %d, lists %d, entries %d", s.Generation(), len(s.Lists()), s.Entries())
	}
	if s.List("ads") == nil || s.List("nope") != nil {
		t.Fatal("List lookup")
	}
	tests := []struct {
		name  string
		lists []string
		want  map[string]string
	}{
		{"x.ads.example", nil, map[string]string{"ads": "||ads.example^"}},
		{"ok.ads.example", nil, map[string]string{"malware": "ok.ads.example"}},
		{"banner12.example", nil, map[string]string{"ads": `/^banner[0-9]*\./`}},
		{"evil.example.", nil, map[string]string{"malware": "evil.example"}},
		{"x.ads.example", []string{"malware", "unknown"}, nil},
		{"clean.example", nil, nil},
	}
	for _, tc := range tests {
		got := s.Contains(tc.name, tc.lists...)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("Contains(%q, %v) = %v, want %v", tc.name, tc.lists, got, tc.want)
		}
	}
	allow := s.ContainsAllow("ok.ads.example")
	if fmt.Sprint(allow) != fmt.Sprint(map[string]string{"ads": "@@||ok.ads.example^"}) {
		t.Errorf("ContainsAllow = %v", allow)
	}
	if got := s.ContainsAllow("good.ads.example"); got["allow"] != "good.ads.example" {
		t.Errorf("ContainsAllow(good) = %v", got)
	}
	if _, err := NewSnapshot(1, s.List("ads"), s.List("ads")); err == nil {
		t.Error("duplicate list names accepted")
	}
}

func TestSelectionEvaluate(t *testing.T) {
	s := testSnapshot(t)
	sel, err := s.Select([]string{"ads", "malware", "allow", "ads"}, SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sel.BlockLists(), sel.AllowLists()) != "[ads malware] [allow]" {
		t.Fatalf("lists = %v %v", sel.BlockLists(), sel.AllowLists())
	}
	tests := []struct {
		name   string
		action Action
		reason Reason
		list   string
		rule   string
		status pipeline.Status
	}{
		{"x.ads.example", ActionBlock, ReasonDeny, "ads", "||ads.example^", pipeline.StatusBlockedList},
		{"X.ADS.EXAMPLE.", ActionBlock, ReasonDeny, "ads", "||ads.example^", pipeline.StatusBlockedList},
		{"banner3.example", ActionBlock, ReasonDeny, "ads", `/^banner[0-9]*\./`, pipeline.StatusBlockedRegex},
		{"ok.ads.example", ActionBlock, ReasonDeny, "malware", "ok.ads.example", pipeline.StatusBlockedList},
		{"www.ok.ads.example", ActionAllow, ReasonException, "ads", "@@||ok.ads.example^", pipeline.StatusUnknown},
		{"good.ads.example", ActionAllow, ReasonAllowList, "allow", "good.ads.example", pipeline.StatusUnknown},
		{"clean.example", ActionPass, ReasonNone, "", "", pipeline.StatusUnknown},
	}
	for _, tc := range tests {
		d := sel.Evaluate(tc.name)
		if d.Action != tc.action || d.Reason != tc.reason || d.List != tc.list || d.Rule() != tc.rule || d.Status() != tc.status {
			t.Errorf("Evaluate(%q) = %s/%s %q %q %s, want %s/%s %q %q %s", tc.name,
				d.Action, d.Reason, d.List, d.Rule(), d.Status(), tc.action, tc.reason, tc.list, tc.rule, tc.status)
		}
		if d.Blocked() != (tc.action == ActionBlock) {
			t.Errorf("Blocked() mismatch for %q", tc.name)
		}
	}

	only, _ := s.Select([]string{"allow", "ads"}, SelectOptions{AllowOnly: true})
	if d := only.Evaluate("clean.example"); d.Action != ActionBlock || d.Reason != ReasonAllowOnly || d.Rule() != "" || d.Status() != pipeline.StatusBlockedList {
		t.Errorf("allow-only = %+v", d)
	}
	if only.Options() != (SelectOptions{AllowOnly: true}) {
		t.Error("Options()")
	}

	if _, err := s.Select([]string{"ads", "missing"}, SelectOptions{}); !errors.Is(err, ErrUnknownList) || !strings.Contains(err.Error(), "missing") {
		t.Errorf("Select with a missing list = %v", err)
	}
}

func TestEvaluateResponse(t *testing.T) {
	s := testSnapshot(t)
	sel, _ := s.Select([]string{"ads", "malware", "allow"}, SelectOptions{})
	d := sel.EvaluateResponse([]string{"cdn.clean.example.", "edge.ads.example."}, nil)
	if !d.Blocked() || !d.Response || d.Name != "edge.ads.example" || d.Status() != pipeline.StatusBlockedCNAME {
		t.Errorf("CNAME chain = %+v", d)
	}
	if d := sel.EvaluateResponse([]string{"good.ads.example"}, nil); d.Action != ActionPass {
		t.Errorf("allow-listed CNAME = %+v", d)
	}
	d = sel.EvaluateResponse(nil, []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.66")})
	if !d.Blocked() || d.Addr.String() != "192.0.2.66" || d.Rule() != "192.0.2.66" || d.Status() != pipeline.StatusBlockedCNAME {
		t.Errorf("address = %+v", d)
	}
	if d := sel.EvaluateResponse(nil, nil); d != (Decision{}) {
		t.Errorf("empty response = %+v", d)
	}
}

func TestEvaluateIPExceptionScopes(t *testing.T) {
	a := mustCompile(t, Spec{Name: "a", Mode: ModeBlock, Sources: []Source{LinesSource("s", parser.FormatAdblock, []string{"192.0.2.1", "@@192.0.2.2"})}})
	b := mustCompile(t, Spec{Name: "b", Mode: ModeBlock, Sources: []Source{LinesSource("s", parser.FormatAdblock, []string{"192.0.2.2"})}})
	w := mustCompile(t, Spec{Name: "w", Mode: ModeAllow, Sources: []Source{LinesSource("s", parser.FormatAdblock, []string{"192.0.2.3"})}})
	s, _ := NewSnapshot(1, a, b, w)
	ip := func(s string) []netip.Addr { return []netip.Addr{netip.MustParseAddr(s)} }

	sel, _ := s.Select([]string{"a", "b", "w"}, SelectOptions{})
	if d := sel.EvaluateResponse(nil, ip("192.0.2.2")); !d.Blocked() || d.List != "b" {
		t.Errorf("list scope = %+v", d)
	}
	if d := sel.EvaluateResponse(nil, ip("192.0.2.3")); d.Action != ActionPass {
		t.Errorf("allow-listed address = %+v", d)
	}
	wide, _ := s.Select([]string{"a", "b"}, SelectOptions{ExceptionScope: ScopeSelection})
	if d := wide.EvaluateResponse(nil, ip("192.0.2.2")); d.Blocked() {
		t.Errorf("selection scope = %+v", d)
	}
	if d := wide.EvaluateResponse(nil, ip("192.0.2.1")); !d.Blocked() || d.List != "a" {
		t.Errorf("selection scope deny = %+v", d)
	}
	if m, ok := a.MatchIP(netip.MustParseAddr("192.0.2.1")); !ok || m.Rule() != "192.0.2.1" {
		t.Errorf("MatchIP = %+v %v", m, ok)
	}
	if m, ok := a.MatchException("whatever"); ok {
		t.Errorf("MatchException = %+v", m)
	}
}

func TestEvaluateDoesNotAllocate(t *testing.T) {
	s := testSnapshot(t)
	sel, _ := s.Select([]string{"allow", "ads", "malware"}, SelectOptions{})
	names := []string{"x.ads.example", "clean.example", "www.ok.ads.example", "good.ads.example", "banner1.example", "deep.sub.clean.example"}
	addrs := []netip.Addr{netip.MustParseAddr("192.0.2.1")}
	allocs := testing.AllocsPerRun(1000, func() {
		for _, n := range names {
			sel.Evaluate(n)
		}
		sel.EvaluateResponse(names[1:2], addrs)
	})
	if allocs != 0 {
		t.Fatalf("Evaluate allocated %.1f times per run, want 0", allocs)
	}
}

func TestRebuild(t *testing.T) {
	body := "||ads.example^\n"
	spec := func(name, b string) Spec {
		return Spec{Name: name, Mode: ModeBlock, Sources: []Source{BytesSource("s", parser.FormatAdblock, []byte(b))}}
	}
	s1, rep := Rebuild(nil, []Spec{spec("ads", body), spec("gone", "a.example\n")}, CompileOptions{})
	if s1.Generation() != 1 || len(rep) != 2 || rep[0].Reused || rep[0].Err != nil || rep[0].Stats.Entries != 1 {
		t.Fatalf("first rebuild: gen %d, %+v", s1.Generation(), rep)
	}

	html := Spec{Name: "ads", Mode: ModeBlock, Sources: []Source{BytesSource("s", parser.FormatAuto, []byte("<html>"))}}
	s2, rep := Rebuild(s1, []Spec{html, spec("new", "b.example\n"), spec("new", "dup\n")}, CompileOptions{})
	if s2.Generation() != 2 {
		t.Fatalf("generation %d", s2.Generation())
	}
	if !rep[0].KeptOld || rep[0].Err == nil || s2.List("ads") != s1.List("ads") {
		t.Errorf("failed list not kept: %+v", rep[0])
	}
	if s2.List("gone") != nil {
		t.Error("removed list still present")
	}
	if rep[2].Err == nil {
		t.Error("duplicate spec not reported")
	}

	s3, rep := Rebuild(s2, []Spec{spec("ads", body), spec("new", "b.example\n")}, CompileOptions{})
	if !rep[0].Reused || !rep[1].Reused || s3.List("ads") != s1.List("ads") {
		t.Errorf("unchanged lists not reused: %+v", rep)
	}

	// A list that fails on its first compile is simply absent.
	s4, rep := Rebuild(nil, []Spec{html}, CompileOptions{})
	if s4.List("ads") != nil || rep[0].KeptOld || rep[0].Err == nil {
		t.Errorf("first-compile failure: %+v", rep)
	}
}

// TestConcurrentSwap runs lookups while the owner swaps snapshots through
// an atomic.Pointer, the pattern the blocking stage uses; run with -race.
func TestConcurrentSwap(t *testing.T) {
	mk := func(gen int) *Snapshot {
		s, _ := Rebuild(nil, []Spec{{Name: "ads", Mode: ModeBlock, Sources: []Source{
			LinesSource("s", parser.FormatAdblock, []string{fmt.Sprintf("||gen%d.example^", gen), "||always.example^"}),
		}}}, CompileOptions{})
		return s
	}
	var cur atomic.Pointer[Snapshot]
	cur.Store(mk(0))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := cur.Load()
				sel, _ := snap.Select([]string{"ads"}, SelectOptions{})
				if d := sel.Evaluate("x.always.example"); !d.Blocked() {
					t.Error("stable rule not matched")
					return
				}
			}
		})
	}
	for gen := 1; gen <= 20; gen++ {
		cur.Store(mk(gen))
	}
	close(stop)
	wg.Wait()
}

func TestFileSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(path, []byte("0.0.0.0 file.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := mustCompile(t, Spec{Name: "f", Mode: ModeBlock, Sources: []Source{FileSource("f", parser.FormatAuto, path)}})
	if _, ok := l.Match("file.example"); !ok {
		t.Fatal("file source not compiled")
	}
	if l.Sources()[0].Format != parser.FormatHosts {
		t.Errorf("detected format %q", l.Sources()[0].Format)
	}
	_, err := Compile(Spec{Name: "f", Mode: ModeBlock, Sources: []Source{FileSource("f", "", filepath.Join(t.TempDir(), "missing"))}}, nil, CompileOptions{})
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file = %v", err)
	}
}

func TestSpecFromAPIAndSelectPolicy(t *testing.T) {
	bl := v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{
		{Inline: []string{"||inline.example^"}},
		{URL: "https://lists.example/hosts.txt", Format: "hosts"},
		{ConfigMapRef: &v1alpha1.ObjectRef{Name: "extra"}, Name: "named"},
	}}
	var opened []string
	open := func(i int, _ v1alpha1.ListSource) (func() (io.ReadCloser, error), error) {
		opened = append(opened, fmt.Sprint(i))
		body := "0.0.0.0 fetched" + fmt.Sprint(i) + ".example\n"
		return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }, nil
	}
	spec, err := SpecFromAPI("ns/ads", &bl, open)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Mode != ModeBlock || len(spec.Sources) != 3 || fmt.Sprint(opened) != "[1 2]" {
		t.Fatalf("spec = %+v, opened %v", spec, opened)
	}
	names := []string{spec.Sources[0].Name, spec.Sources[1].Name, spec.Sources[2].Name}
	if fmt.Sprint(names) != "[inline-0 https://lists.example/hosts.txt named]" {
		t.Errorf("source names = %v", names)
	}
	if _, err := SpecFromAPI("x", &bl, nil); err == nil {
		t.Error("URL source without an opener accepted")
	}
	failing := func(int, v1alpha1.ListSource) (func() (io.ReadCloser, error), error) {
		return nil, errors.New("no digest yet")
	}
	if _, err := SpecFromAPI("x", &bl, failing); err == nil {
		t.Error("opener error swallowed")
	}

	allowSpec, _ := SpecFromAPI("ns/allow", &v1alpha1.BlockListSpec{Mode: "allow", Sources: []v1alpha1.ListSource{{Inline: []string{"fetched1.example"}}}}, nil)
	snap, rep := Rebuild(nil, []Spec{spec, allowSpec}, CompileOptions{})
	if rep[0].Err != nil || rep[1].Err != nil {
		t.Fatal(rep)
	}
	sel, err := snap.SelectPolicy(&v1alpha1.PolicySpec{BlockListRefs: []string{"ns/ads"}, AllowListRefs: []string{"ns/allow"}}, ScopeList)
	if err != nil {
		t.Fatal(err)
	}
	if d := sel.Evaluate("fetched1.example"); d.Action != ActionAllow {
		t.Errorf("allow ref = %+v", d)
	}
	if d := sel.Evaluate("fetched2.example"); !d.Blocked() || d.List != "ns/ads" {
		t.Errorf("block ref = %+v", d)
	}
	_, err = snap.SelectPolicy(&v1alpha1.PolicySpec{BlockListRefs: []string{"ns/allow", "gone"}, AllowListRefs: []string{"ns/ads"}, AllowOnly: true}, ScopeList)
	if !errors.Is(err, ErrModeMismatch) || !errors.Is(err, ErrUnknownList) {
		t.Errorf("SelectPolicy errors = %v", err)
	}
}

func TestStrings(t *testing.T) {
	for v, want := range map[fmt.Stringer]string{
		ActionPass: "pass", ActionAllow: "allow", ActionBlock: "block", Action(9): "unknown",
		ReasonNone: "none", ReasonAllowList: "allowlist", ReasonAllowOnly: "allow_only",
		ReasonException: "exception", ReasonDeny: "deny", Reason(9): "unknown",
	} {
		if v.String() != want {
			t.Errorf("%v.String() = %q, want %q", v, v.String(), want)
		}
	}
	if (Match{}).Rule() != "" {
		t.Error("zero Match renders a rule")
	}
	if NormalizeName("A.B.") != "a.b" || NormalizeName("") != "" {
		t.Error("NormalizeName")
	}
}
