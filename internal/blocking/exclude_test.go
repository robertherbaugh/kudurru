package blocking

import (
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

func TestCompileExclude(t *testing.T) {
	body := "0.0.0.0 ads.example tracker.example\n||cdn.example^\n/^x[0-9]+\\.example$/\n@@||ok.example^\n"
	base := BytesSource("s", parser.FormatAuto, []byte(body))
	full := mustCompile(t, Spec{Name: "l", Mode: ModeBlock, Sources: []Source{base}})

	src := base
	src.Exclude = []string{"tracker.example", "||cdn.example^", "/^x[0-9]+\\.example$/", "@@||ok.example^", "gone.example", "cdn.example"}
	l, err := Compile(Spec{Name: "l", Mode: ModeBlock, Sources: []Source{src}}, full, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if l == full {
		t.Fatal("an exclude change did not recompile")
	}
	if _, ok := l.Match("ads.example"); !ok {
		t.Error("ads.example: want still blocked")
	}
	for _, n := range []string{"tracker.example", "cdn.example", "a.cdn.example", "x1.example"} {
		if _, ok := l.Match(n); ok {
			t.Errorf("%s: excluded but still matches", n)
		}
	}
	if _, ok := l.MatchException("ok.example"); ok {
		t.Error("the excluded exception still matches")
	}
	st := l.Sources()[0]
	if st.Excluded != 4 || st.Rules != 1 || st.Exceptions != 0 {
		t.Errorf("excluded %d rules %d exceptions %d, want 4, 1, 0", st.Excluded, st.Rules, st.Exceptions)
	}
	// "cdn.example" is not how the source writes the rule (||cdn.example^).
	if want := []string{"cdn.example", "gone.example"}; !slices.Equal(st.StaleExcludes, want) {
		t.Errorf("stale %v, want %v", st.StaleExcludes, want)
	}
	if l.Stats().RuleCount != 1 {
		t.Errorf("ruleCount %d, want 1", l.Stats().RuleCount)
	}
	again, _ := Compile(Spec{Name: "l", Mode: ModeBlock, Sources: []Source{src}}, l, CompileOptions{})
	if again != l {
		t.Error("an unchanged exclude set recompiled")
	}
}

func TestExcludeKey(t *testing.T) {
	for in, want := range map[string]string{
		"a.example": "a.example", "||a.example^": "a.example", "|a.example^": "a.example", "||*.a.example^": "a.example",
		"*.a.example": "a.example", "/re/": "re", "@@||a.example^": "a.example",
	} {
		if got := ExcludeKey(in); got != want {
			t.Errorf("ExcludeKey(%q) = %q, want %q", in, got, want)
		}
	}
}
