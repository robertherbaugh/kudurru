package blocking

import (
	"errors"
	"fmt"
	"hash/crc32"
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

// Reference the established fmt-based v3 stream independently of the optimized
// encoder. Full Parse supplies source CRCs, including format detection.
func originalV3Fingerprint(t *testing.T, spec Spec, capacity int, limit int64) uint32 {
	t.Helper()
	h := crc32.NewIEEE()
	_, _ = fmt.Fprintf(h, "kudurru-blocking-v3-lazy-dfa\x00%s\x00%d\x00%d\x00", spec.Mode, capacity, limit)
	_, _ = fmt.Fprintf(h, "external-policy=%t\x00", spec.ExternalPolicy)
	for _, src := range spec.Sources {
		rc, err := src.Open()
		if err != nil {
			t.Fatal(err)
		}
		res, err := parser.Parse(rc, parser.Options{Format: src.Format, HostsSubdomains: src.HostsSubdomains}, nil)
		closeErr := rc.Close()
		if err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
		format := src.Format
		if format == "" {
			format = parser.FormatAuto
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%t\x00%08x\x00%s\x00", src.Name, format, src.HostsSubdomains, res.CRC32, ExcludeDigest(src.Exclude))
	}
	return h.Sum32()
}

func TestCompileFingerprintRetainsV3Encoding(t *testing.T) {
	for _, format := range []parser.Format{"", parser.FormatAuto, parser.FormatAdblock, parser.FormatHosts, parser.FormatDomains, parser.FormatRegex} {
		t.Run(string(format), func(t *testing.T) {
			for _, mode := range []Mode{ModeBlock, ModeAllow} {
				for _, external := range []bool{false, true} {
					for _, capacity := range []int{-1, 0, 1000} {
						for _, limit := range []int64{1, 128 << 20} {
							empty := BytesSource("empty", format, nil)
							src := BytesSource(strings.Repeat("n", 257)+"\x00é", format, []byte("! Title: original\nordinary.example\n/ads[.]example$/\n"))
							src.HostsSubdomains = true
							src.Exclude = []string{"ordinary.example", "unused", "ordinary.example"}
							spec := Spec{Name: "encoding", Mode: mode, ExternalPolicy: external, Sources: []Source{empty, src}}
							want := originalV3Fingerprint(t, spec, capacity, limit)
							got, _, err := fingerprint(&spec, capacity, limit)
							if err != nil || got != want {
								t.Fatalf("v3 identity changed: mode=%s external=%t capacity=%d bytes=%d got=%08x want=%08x err=%v", mode, external, capacity, limit, got, want, err)
							}
						}
					}
				}
			}
		})
	}
}

func TestCompileFingerprintReuseAllocationBound(t *testing.T) {
	// The original unchanged-list gate permits eight allocations. Keep the
	// real Compile/source-open/sniff/reservation path in this smaller guard;
	// the unchanged million-entry benchmark remains the performance evidence.
	spec := Spec{Name: "reuse", Mode: ModeBlock, Sources: []Source{BytesSource("original", parser.FormatAdblock, []byte("||ads.example^\n||second.example^\n"))}}
	opts := CompileOptions{RegexHardCap: -1}
	prev, err := Compile(spec, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	var got *List
	allocs := testing.AllocsPerRun(20, func() { got, err = Compile(spec, prev, opts) })
	if err != nil || got != prev {
		t.Fatal("unchanged compilation did not preserve the original owner", err)
	}
	if allocs > 8 {
		t.Fatalf("unchanged compilation allocated %g times, original bound is eight", allocs)
	}
}
