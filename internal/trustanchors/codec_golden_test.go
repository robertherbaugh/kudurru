package trustanchors

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pure documents under testdata were written on November 8, 2026 by the
// recovery-format commit's encoder (pure-format1.json is a quarantined
// format-1 owner, pure-format2.json its explicit recovery). The format-1
// bytes were also decoded and re-encoded unchanged by the pre-recovery codec
// of be4446fc. These files are compatibility evidence: never regenerate them.
// A codec change that alters their bytes or digests needs a new format, and a
// new format needs a store schema or required-feature bump (D370).
var pureGoldens = []struct {
	file          string
	format        int
	generation    uint64
	digest        string
	recoveries    int
	quarantined   bool
	containsField string
}{
	{"pure-format1.json", FormatVersion, 2, "e4f7de991ad94e228f0c40f1ba90c61d01235f4c48e60bc276b9b0ae79f65e36", 0, true, ""},
	{"pure-format2.json", RecoveryFormatVersion, 3, "e1e06f5665d0827324ee92b7b547e6c5a22e78f05f22bb5c0557350daeec5388", 1, false, `"recoveries":[`},
}

func readPureGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(b, []byte("\n"))
}

func TestCodecGoldenFormatsAreByteStable(t *testing.T) {
	for _, g := range pureGoldens {
		t.Run(g.file, func(t *testing.T) {
			raw := readPureGolden(t, g.file)
			s, err := Decode(raw)
			if err != nil {
				t.Fatal("golden document no longer decodes", err)
			}
			encoded, err := Encode(s)
			if err != nil || !bytes.Equal(encoded, raw) {
				t.Fatal("decode then encode changed golden bytes", err)
			}
			v, err := s.Version()
			if err != nil || v.Generation != g.generation || v.Digest != g.digest || v.Digest != digest(raw) {
				t.Fatalf("golden version changed: %+v %v", v, err)
			}
			doc := s.Inspect()
			if doc.Format != g.format || len(doc.Recoveries) != g.recoveries || doc.Quarantined != g.quarantined {
				t.Fatal("golden document changed meaning", doc.Format, len(doc.Recoveries), doc.Quarantined)
			}
			if g.containsField != "" && !bytes.Contains(raw, []byte(g.containsField)) || g.containsField == "" && bytes.Contains(raw, []byte(`"recoveries"`)) {
				t.Fatal("format 1 must omit recoveries and format 2 must carry them")
			}
		})
	}
	// The recovery receipt binds the exact format-1 digest it replaced.
	recovered, err := Decode(readPureGolden(t, "pure-format2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r := recovered.Inspect().Recoveries[0]; r.PreviousDigest != pureGoldens[0].digest || r.Generation != 3 {
		t.Fatal("format-2 receipt no longer binds the format-1 golden", r.PreviousDigest)
	}
	if !recovered.RetainsRecoveryHistory(recovered) {
		t.Fatal("format-2 golden does not retain its own recovery history")
	}
}

// An unknown pure format is refused, and neither known format can be relabeled
// as the other: format 1 carries no recovery records and format 2 needs one.
func TestCodecRejectsUnknownFormat(t *testing.T) {
	one, two := readPureGolden(t, "pure-format1.json"), readPureGolden(t, "pure-format2.json")
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"format3-from-2", bytes.Replace(two, []byte(`{"format":2,`), []byte(`{"format":3,`), 1)},
		{"format3-from-1", bytes.Replace(one, []byte(`{"format":1,`), []byte(`{"format":3,`), 1)},
		{"format0", bytes.Replace(one, []byte(`{"format":1,`), []byte(`{"format":0,`), 1)},
		{"format-string", bytes.Replace(one, []byte(`{"format":1,`), []byte(`{"format":"1",`), 1)},
		{"format1-with-recoveries", bytes.Replace(two, []byte(`{"format":2,`), []byte(`{"format":1,`), 1)},
		{"format2-without-recoveries", bytes.Replace(one, []byte(`{"format":1,`), []byte(`{"format":2,`), 1)},
		{"unknown-field", bytes.Replace(two, []byte(`"recoveries":[`), []byte(`"recoveryFormat":3,"recoveries":[`), 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if bytes.Equal(tc.raw, one) || bytes.Equal(tc.raw, two) || !strings.HasPrefix(string(tc.raw), `{"format":`) {
				t.Fatal("fixture mutation did not apply")
			}
			if _, err := Decode(tc.raw); err == nil {
				t.Fatal("unknown or relabeled pure format decoded")
			}
		})
	}
}
