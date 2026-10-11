package blocking

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata")

const realCorpus = "testdata/corpus/stevenblack/hosts"

// TestRealCorpus compiles the StevenBlack sample (MIT, see PROVENANCE.md),
// compares its statistics with a golden file, and checks that every
// compiled entry matches exactly and that hosts semantics keep subdomains
// unblocked.
func TestRealCorpus(t *testing.T) {
	body, err := os.ReadFile(realCorpus)
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]bool{}
	if _, err := parser.Parse(strings.NewReader(string(body)), parser.Options{}, func(r parser.Rule) error {
		ref[r.Pattern] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	l := mustCompile(t, Spec{Name: "stevenblack", Mode: ModeBlock, Sources: []Source{BytesSource("hosts", parser.FormatAuto, body)}})
	st, src := l.Stats(), l.Sources()[0]
	got := fmt.Sprintf("format=%s\ntitle=%s\nlines=%d\nrules=%d\nentries=%d\nduplicates=%d\ninvalid=%d\nignored=%d\nunsupported=%d\ncrc32=%08x\n",
		src.Format, src.Title, src.Lines, st.RuleCount, st.Entries, st.Duplicates, st.Invalid, st.Ignored, st.Unsupported, src.CRC32)

	golden := filepath.Join(filepath.Dir(realCorpus), "golden.txt")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden) //nolint:gosec // fixed testdata path
	if err != nil {
		t.Fatalf("%v (run go test -run TestRealCorpus -update)", err)
	}
	if got != string(want) {
		t.Errorf("corpus statistics changed:\n%s\nwant:\n%s", got, want)
	}
	if st.Entries != len(ref) {
		t.Errorf("Entries = %d, reference set has %d names", st.Entries, len(ref))
	}

	sel, _ := mustSnapshot(t, l).Select([]string{"stevenblack"}, SelectOptions{})
	for name := range ref {
		if d := sel.Evaluate(name); !d.Blocked() || d.Rule() != name {
			t.Fatalf("Evaluate(%q) = %+v", name, d)
		}
		sub := "kudurru-probe." + name
		if d := sel.Evaluate(sub); d.Blocked() != ref[sub] {
			t.Fatalf("Evaluate(%q) = %+v: hosts entries must not cover subdomains", sub, d)
		}
	}
}

func mustSnapshot(t *testing.T, lists ...*List) *Snapshot {
	t.Helper()
	s, err := NewSnapshot(1, lists...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
