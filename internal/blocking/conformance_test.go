package blocking

import (
	"bufio"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// The conformance corpus format is described in
// testdata/conformance/README.md.

type corpusList struct {
	name, mode, format string
	hostsSubdomains    bool
	lines              []string
}

type corpusExpect struct {
	line                      int
	query, action, list, rule string
}

type corpus struct {
	lists   []corpusList
	opts    SelectOptions
	counts  map[string]map[string]int
	expects []corpusExpect
}

var repeatMacro = regexp.MustCompile(`\{(.)\*([0-9]+)\}`)

func expandMacros(s string) string {
	return repeatMacro.ReplaceAllStringFunc(s, func(m string) string {
		sub := repeatMacro.FindStringSubmatch(m)
		n, _ := strconv.Atoi(sub[2])
		return strings.Repeat(sub[1], n)
	})
}

func readCorpus(t *testing.T, path string) corpus {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // corpus path from filepath.Glob
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	c := corpus{counts: map[string]map[string]int{}}
	section, target := "", ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := expandMacros(sc.Text())
		if strings.HasPrefix(line, "-- ") && strings.HasSuffix(line, " --") {
			f := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(line, "-- "), " --"))
			section = f[0]
			switch section {
			case "list":
				if len(f) < 4 {
					t.Fatalf("%s:%d: list header needs name, mode and format", path, n)
				}
				l := corpusList{name: f[1], mode: f[2], format: f[3]}
				l.hostsSubdomains = len(f) > 4 && f[4] == "hostsSubdomains"
				c.lists = append(c.lists, l)
			case "counts":
				target = f[1]
				c.counts[target] = map[string]int{}
			case "options", "expect":
			default:
				t.Fatalf("%s:%d: unknown section %q", path, n, section)
			}
			continue
		}
		switch section {
		case "list":
			l := &c.lists[len(c.lists)-1]
			l.lines = append(l.lines, line)
		case "options":
			for _, o := range strings.Fields(line) {
				switch o {
				case "allowOnly":
					c.opts.AllowOnly = true
				case "scope=selection":
					c.opts.ExceptionScope = ScopeSelection
				default:
					t.Fatalf("%s:%d: unknown option %q", path, n, o)
				}
			}
		case "counts":
			for _, kv := range strings.Fields(line) {
				k, v, _ := strings.Cut(kv, "=")
				i, err := strconv.Atoi(v)
				if err != nil {
					t.Fatalf("%s:%d: bad count %q", path, n, kv)
				}
				c.counts[target][k] = i
			}
		case "expect":
			if i := strings.Index(line, " #"); i >= 0 {
				line = line[:i]
			}
			f := strings.Fields(line)
			if len(f) == 0 || strings.HasPrefix(f[0], "#") {
				continue
			}
			e := corpusExpect{line: n, query: f[0]}
			if len(f) > 1 {
				e.action = f[1]
			}
			if len(f) > 2 {
				e.list = f[2]
			}
			if len(f) > 3 {
				e.rule = strings.Join(f[3:], " ")
			}
			if e.list == "-" {
				e.list = ""
			}
			if e.rule == "-" {
				e.rule = ""
			}
			c.expects = append(c.expects, e)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConformance(t *testing.T) {
	files, err := filepath.Glob("testdata/conformance/*.txtar")
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus files: %v", err)
	}
	for _, path := range files {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".txtar"), func(t *testing.T) {
			c := readCorpus(t, path)
			var specs []Spec
			var names []string
			for _, l := range c.lists {
				src := LinesSource(l.name, parser.Format(l.format), l.lines)
				src.HostsSubdomains = l.hostsSubdomains
				specs = append(specs, Spec{Name: l.name, Mode: Mode(l.mode), Sources: []Source{src}})
				names = append(names, l.name)
			}
			snap, reports := Rebuild(nil, specs, CompileOptions{})
			for _, r := range reports {
				if r.Err != nil {
					t.Fatalf("list %s: %v", r.Name, r.Err)
				}
			}
			for list, want := range c.counts {
				l := snap.List(list)
				if l == nil {
					t.Fatalf("counts for unknown list %q", list)
				}
				st := l.Stats()
				got := map[string]int{
					"rules": st.RuleCount, "exceptions": st.Exceptions, "unsupported": st.Unsupported,
					"invalid": st.Invalid, "ignored": st.Ignored, "entries": st.Entries,
				}
				for k, v := range want {
					g, ok := got[k]
					if !ok {
						t.Fatalf("unknown count key %q", k)
					}
					if g != v {
						t.Errorf("list %s: %s = %d, want %d (sources: %+v)", list, k, g, v, l.Sources())
					}
				}
			}
			sel, err := snap.Select(names, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range c.expects {
				var d Decision
				switch {
				case strings.HasPrefix(e.query, "cname:"):
					d = sel.EvaluateResponse([]string{strings.TrimPrefix(e.query, "cname:")}, nil)
				case strings.HasPrefix(e.query, "ip:"):
					a, err := netip.ParseAddr(strings.TrimPrefix(e.query, "ip:"))
					if err != nil {
						t.Fatalf("line %d: %v", e.line, err)
					}
					d = sel.EvaluateResponse(nil, []netip.Addr{a})
				default:
					d = sel.Evaluate(e.query)
				}
				if got := d.Action.String(); got != e.action || d.List != e.list || d.Rule() != e.rule {
					t.Errorf("line %d: %s -> %s %q %q (reason %s), want %s %q %q",
						e.line, e.query, got, d.List, d.Rule(), d.Reason, e.action, e.list, e.rule)
				}
				if d.Blocked() && (strings.HasPrefix(e.query, "cname:") || strings.HasPrefix(e.query, "ip:")) != d.Response {
					t.Errorf("line %d: %s: Response = %v", e.line, e.query, d.Response)
				}
			}
		})
	}
}
