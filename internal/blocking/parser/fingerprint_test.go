package parser

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// Retain serialized callback values so later buffer reuse cannot make two
// different candidate streams appear equal through shared strings/slices.
func fingerprintCandidates(out *[]string) func(UnsupportedRule) error {
	return func(u UnsupportedRule) error {
		if strings.Contains(u.Text, "$") {
			*out = append(*out, fmt.Sprintf("%#v", u))
		}
		return ErrUnsupportedSlow
	}
}

func TestFingerprintMatchesFullParserCandidatesAndCRC(t *testing.T) {
	corpora := map[string]string{
		"mixed": "\xef\xbb\xbf! Title: fixture\r\n[Adblock Plus 2.0]\n\n||ADS.example^\r\n0.0.0.0 one.example two.example # note\n" +
			"||ads.example^$badfilter\n@@||ads.example^$client=192.0.2.0/24,dnstype=A,badfilter\n" +
			"/^ads\\.example$/$client=192.0.2.0/24,badfilter\n" +
			`/a\/$b/$badfilter` + "\n" + `/[/$]x/$badfilter` + "\n" + `/\Q/$\E/$badfilter` + "\n" +
			"||bad.example^$unknown\n||bad.example^$badfilter=yes\n0.0.0.0 host.example # $badfilter\n" +
			"example##$badfilter\n/ads/$badfilter,client='Fixture PC'\nno valid domain\nlast.example",
		"detect-boundary": strings.Repeat("0.0.0.0 fixture.example\n", detectSampleLines) + "||fixture.example^$badfilter\n",
		"overlong":        strings.Repeat("a", maxLineLen+1) + "$badfilter\n||short.example^$badfilter\n" + strings.Repeat("z", maxLineLen+5),
		"empty":           "",
		"regex":           "^first\\.example$\n/second/\n^third$;querytype=A\n^fourth;invert\ninvalid(\n",
	}
	for name, body := range corpora {
		for _, format := range []Format{"", FormatAuto, FormatHosts, FormatDomains, FormatAdblock, FormatRegex} {
			t.Run(name+"/"+string(format), func(t *testing.T) {
				var want, got []string
				opts := Options{Format: format, HostsSubdomains: true, KeepUnsupported: -1, KeepInvalid: -1, EmitSlow: fingerprintCandidates(&want)}
				full, err := Parse(strings.NewReader(body), opts, nil)
				if err != nil {
					t.Fatal("full parser fixture", err)
				}
				opts.EmitSlow = fingerprintCandidates(&got)
				crc, err := Fingerprint(iotest.OneByteReader(strings.NewReader(body)), opts)
				if err != nil || crc != full.CRC32 || !reflect.DeepEqual(got, want) {
					t.Fatalf("fingerprint changed parser contract: crc %08x/%08x err %v\ncandidates %#v\nwant %#v", crc, full.CRC32, err, got, want)
				}
			})
		}
	}
}

func TestFingerprintDoesNotTreatEveryDollarAsBadfilter(t *testing.T) {
	body := "||ok.example^$badfilter\n/^ads\\.example$/$badfilter,client=192.0.2.0/24\n" +
		"/^literal\\$badfilter$/\n0.0.0.0 plain.example # $badfilter\n" +
		"||wrong.example^$unknown,badfilter\n! ||comment.example^$badfilter\n"
	var patterns []string
	_, err := Fingerprint(strings.NewReader(body), Options{Format: FormatAdblock, EmitSlow: func(u UnsupportedRule) error {
		for _, m := range u.Modifiers {
			if m.Name == "badfilter" {
				patterns = append(patterns, strings.Clone(u.Pattern))
			}
		}
		return ErrUnsupportedSlow
	}})
	if err != nil || !reflect.DeepEqual(patterns, []string{"||ok.example^", `/^ads\.example$/`}) {
		t.Fatal("fingerprinting bypassed actual modifier parsing", patterns, err)
	}
}

func TestFingerprintPreservesSniffReadAndCallbackRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		format     Format
		want       error
	}{
		{"html", "<!doctype html><html>list</html>", FormatAuto, ErrHTML},
		{"binary", "one.example\x00two.example", FormatDomains, ErrBinary},
		{"format", "one.example", "unknown", ErrUnknownFormat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crc, err := Fingerprint(strings.NewReader(tc.body), Options{Format: tc.format})
			if !errors.Is(err, tc.want) || crc != 0 {
				t.Fatal("source refusal became a reusable fingerprint", crc, err)
			}
		})
	}
	stop := errors.New("original input interrupted")
	for _, prefix := range []string{"", strings.Repeat("ordinary.example\n", 5000)} {
		reader := func() io.Reader { return io.MultiReader(strings.NewReader(prefix), iotest.ErrReader(stop)) }
		_, fullErr := Parse(reader(), Options{}, nil)
		crc, err := Fingerprint(reader(), Options{})
		if !errors.Is(fullErr, stop) || !errors.Is(err, stop) || crc != 0 {
			t.Fatal("partial read error was swallowed", crc, fullErr, err)
		}
	}
	for _, result := range []error{nil, ErrInvalidSlow, ErrUnsupportedSlow, stop} {
		calls := 0
		crc, err := Fingerprint(strings.NewReader("ordinary.example\n||one.example^$badfilter\n||two.example^$badfilter"), Options{EmitSlow: func(UnsupportedRule) error { calls++; return result }})
		if result == stop {
			if !errors.Is(err, stop) || calls != 1 || crc != 0 {
				t.Fatal("fatal modifier/resource refusal did not stop original stream", calls, crc, err)
			}
		} else if err != nil || calls != 2 {
			t.Fatal("candidate disposition changed the original stream", calls, err)
		}
	}
}

func TestFingerprintVisitsAllModifiersBeyondDiagnosticLimits(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 2501; i++ {
		fmt.Fprintf(&body, "||h%d.example^$badfilter\n", i)
	}
	for _, limit := range []int{-1, 0, 1, 1000} {
		calls := 0
		_, err := Fingerprint(strings.NewReader(body.String()), Options{KeepUnsupported: limit, EmitSlow: func(u UnsupportedRule) error {
			if u.Line != calls+1 || u.Pattern != fmt.Sprintf("||h%d.example^", calls) {
				t.Fatal("source order/line identity changed", u.Line, u.Pattern)
			}
			calls++
			return ErrUnsupportedSlow
		}})
		if err != nil || calls != 2501 {
			t.Fatal("diagnostic limit truncated fingerprint validation", limit, calls, err)
		}
	}
}
