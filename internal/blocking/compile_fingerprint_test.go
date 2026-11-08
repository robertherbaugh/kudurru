package blocking

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

func TestCompileFingerprintCrossSourceBadfilterAndExclusionReuse(t *testing.T) {
	target := "||ads.example^\n/^regex\\.example$/\nordinary.example\n"
	directives := "||ads.example^$badfilter\n/^regex\\.example$/$badfilter\n"
	aOpens, bOpens := 0, 0
	spec := Spec{Name: "original", Mode: ModeBlock, Sources: []Source{
		countingSource("targets", parser.FormatAuto, &target, &aOpens),
		countingSource("directives", parser.FormatAdblock, &directives, &bOpens),
	}}
	first := mustCompile(t, spec)
	check := func(l *List, ads, regex bool) {
		t.Helper()
		snapshot, err := NewSnapshot(1, l)
		if err != nil {
			t.Fatal(err)
		}
		selected, err := snapshot.Select([]string{"original"}, SelectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]bool{"ads.example": ads, "regex.example": regex, "ordinary.example": true} {
			if got := selected.Evaluate(name).Blocked(); got != want {
				t.Fatalf("%s blocked=%v want=%v", name, got, want)
			}
		}
	}
	check(first, false, false)
	if aOpens != 2 || bOpens != 2 {
		t.Fatal("initial compile did not use original two-pass sources", aOpens, bOpens)
	}
	// The original caller sees the same owner on unchanged normalized content.
	target = "! comment changed\n" + target
	aOpens, bOpens = 0, 0
	again, err := Compile(spec, first, CompileOptions{})
	if err != nil || again != first || aOpens != 1 || bOpens != 1 {
		t.Fatal("unchanged source lost exact owner reuse", err, aOpens, bOpens)
	}
	spec.Sources[1].Exclude = []string{"||ads.example^$badfilter"}
	excluded, err := Compile(spec, first, CompileOptions{})
	if err != nil || excluded == first || excluded.Fingerprint() == first.Fingerprint() {
		t.Fatal("exclusion failed to rebuild original target", err)
	}
	check(excluded, true, false)
	check(first, false, false)
	directives = "! comment-only directive source\n"
	removed, err := Compile(spec, excluded, CompileOptions{})
	if err != nil || removed == excluded {
		t.Fatal("removed badfilters reused the canceled owner", err)
	}
	check(removed, true, true)
}

type fingerprintTrackedReader struct {
	io.Reader
	closed *int
}

func (r fingerprintTrackedReader) Close() error { *r.closed++; return nil }

func TestCompileFingerprintSniffFailureCannotReusePrevious(t *testing.T) {
	body, opens, closes := "||valid.example^\n", 0, 0
	spec := Spec{Name: "original", Mode: ModeBlock, Sources: []Source{{Name: "source", Format: parser.FormatAuto, Open: func() (io.ReadCloser, error) {
		opens++
		return fingerprintTrackedReader{Reader: strings.NewReader(body), closed: &closes}, nil
	}}}}
	first := mustCompile(t, spec)
	if closes != opens || opens != 2 {
		t.Fatal("initial source handles not closed", opens, closes)
	}
	body = "<html>||valid.example^</html>"
	invalid, err := Compile(spec, first, CompileOptions{})
	if !errors.Is(err, parser.ErrHTML) || invalid != nil || opens != 4 || closes != opens {
		t.Fatal("non-list body reused previous owner or leaked source", err, opens, closes)
	}
	if _, ok := first.Match("valid.example"); !ok {
		t.Fatal("failed candidate mutated original owner")
	}
}
