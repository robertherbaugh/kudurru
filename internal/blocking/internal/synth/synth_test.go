package synth

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestUniqueDeterministic(t *testing.T) {
	a, b := Unique(42, 5000), Unique(42, 5000)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatal("same seed produced different names")
	}
	seen := map[string]bool{}
	total := 0
	for _, d := range a {
		if seen[d] {
			t.Fatalf("duplicate %q", d)
		}
		seen[d] = true
		total += len(d)
		if strings.Contains(d, "..") || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") || strings.Contains(d, "-.") || strings.Contains(d, ".-") {
			t.Fatalf("malformed name %q", d)
		}
	}
	avg := float64(total) / float64(len(a))
	t.Logf("average name length %.1f bytes", avg)
	if avg < 12 || avg > 30 {
		t.Errorf("average length %.1f outside the realistic 12-30 range", avg)
	}
	if strings.Join(Unique(43, 100), ",") == strings.Join(a[:100], ",") {
		t.Error("different seeds produced the same names")
	}
}

func TestWrite(t *testing.T) {
	for format, want := range map[string]string{"hosts": "0.0.0.0 a.example\n", "adblock": "||a.example^\n", "domains": "a.example\n"} {
		var buf bytes.Buffer
		if err := Write(&buf, format, []string{"a.example"}); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(buf.String(), want) || !strings.HasPrefix(buf.String(), "# Title:") {
			t.Errorf("%s: %q", format, buf.String())
		}
	}
	if err := Write(&bytes.Buffer{}, "csv", nil); err == nil {
		t.Error("unknown format accepted")
	}
}

func TestRegexesCompile(t *testing.T) {
	for _, re := range Regexes(7, 50) {
		if _, err := regexp.Compile(re); err != nil {
			t.Errorf("%q: %v", re, err)
		}
	}
}
