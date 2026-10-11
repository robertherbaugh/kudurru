package parser

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// parseAll parses src and returns the emitted rules and the result.
func parseAll(t *testing.T, src string, opts Options) ([]Rule, *Result) {
	t.Helper()
	var rules []Rule
	res, err := Parse(strings.NewReader(src), opts, func(r Rule) error {
		r.Pattern = strings.Clone(r.Pattern) // Pattern is only valid during the callback
		rules = append(rules, r)
		return nil
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return rules, res
}

// TestLines checks the classification of single lines in adblock format.
func TestLines(t *testing.T) {
	type want struct {
		rule        string // rendered rule, "" when no rule
		kind        Kind
		unsupported string
		invalid     string
		ignored     bool
	}
	tests := []struct {
		line string
		want want
	}{
		// adblock suffix rules
		{"||example.org^", want{rule: "||example.org^", kind: KindSuffix}},
		{"||Example.ORG^", want{rule: "||example.org^", kind: KindSuffix}},
		{"||example.org", want{rule: "||example.org^", kind: KindSuffix}},
		{"||example.org^|", want{rule: "||example.org^", kind: KindSuffix}},
		{"||example.org.^", want{rule: "||example.org^", kind: KindSuffix}},
		{"||example.org^ # trailing comment", want{rule: "||example.org^", kind: KindSuffix}},
		{"@@||example.org^", want{rule: "@@||example.org^", kind: KindSuffix}},
		{"||*.example.org^", want{rule: "||*.example.org^", kind: KindSuffix}},
		{"||zip^", want{rule: "||zip^", kind: KindSuffix}},
		// exact rules
		{"|example.org^", want{rule: "|example.org^", kind: KindExact}},
		{"|example.org|", want{rule: "|example.org^", kind: KindExact}},
		{"example.org", want{rule: "example.org", kind: KindExact}},
		{"example.org.", want{rule: "example.org", kind: KindExact}},
		{"EXAMPLE.org", want{rule: "example.org", kind: KindExact}},
		{"bücher.example", want{rule: "xn--bcher-kva.example", kind: KindExact}},
		{"BÜCHER.example", want{rule: "xn--bcher-kva.example", kind: KindExact}},
		{"_dmarc.example.org", want{rule: "_dmarc.example.org", kind: KindExact}},
		// wildcards
		{"*.example.org", want{rule: "*.example.org", kind: KindSuffix}},
		{"*.example.org^", want{rule: "*.example.org", kind: KindSuffix}},
		{"||ad*.example.org^", want{unsupported: ReasonWildcard}},
		{"||example.*^", want{unsupported: ReasonWildcard}},
		{"*.*.example.org", want{unsupported: ReasonWildcard}},
		{"||*^", want{unsupported: ReasonWildcard}},
		{"ad*.example.org", want{unsupported: ReasonWildcard}},
		{"example.org^", want{unsupported: ReasonUnanchored}},
		{"ample.org|", want{unsupported: ReasonUnanchored}},
		{"|example", want{unsupported: ReasonPrefix}},
		// regex
		{"/^ad[sx]?\\./", want{rule: "/^ad[sx]?\\./", kind: KindRegex}},
		{"@@/^safe\\./", want{rule: "@@/^safe\\./", kind: KindRegex}},
		{"/ads$/", want{rule: "/ads$/", kind: KindRegex}},
		{"/(/", want{invalid: ReasonInvalidRegex}},
		{"/", want{invalid: ReasonInvalidRegex}},
		{"//", want{invalid: ReasonInvalidRegex}},
		{"/ads/banner.js", want{invalid: ReasonURL}},
		{"/example.*/$important", want{unsupported: ReasonModifiers}},
		{"/example.*/$script", want{invalid: ReasonUnknownModifier}},
		// modifiers: every DNS modifier is kept as unsupported
		{"||example.org^$important", want{unsupported: ReasonModifiers}},
		{"||example.org^$badfilter", want{unsupported: ReasonModifiers}},
		{"||example.org^$dnstype=AAAA", want{unsupported: ReasonModifiers}},
		{"||example.org^$dnstype=~A|~CNAME", want{unsupported: ReasonModifiers}},
		{"||example.com^$dnsrewrite=NOERROR;A;1.2.3.4", want{unsupported: ReasonModifiers}},
		{"||example.com^$dnsrewrite=NOERROR;HTTPS;32 example.com alpn=h3", want{unsupported: ReasonModifiers}},
		{"||example.org^$client='Frank\\'s laptop'", want{unsupported: ReasonModifiers}},
		{"||example.org^$client=~Mom|~Dad|Kids", want{unsupported: ReasonModifiers}},
		{"||example.org^$ctag=device_pc|device_phone", want{unsupported: ReasonModifiers}},
		{"*$denyallow=com|net", want{unsupported: ReasonModifiers}},
		{"||*^$respgeo=US", want{unsupported: ReasonModifiers}},
		{"@@||example.org^$important", want{unsupported: ReasonModifiers}},
		{"$dnstype=AAAA,denyallow=example.org,dnsrewrite=NOERROR;;", want{unsupported: ReasonModifiers}},
		{"||example.org^$third-party", want{invalid: ReasonUnknownModifier}},
		{"||example.org^$~third-party", want{invalid: ReasonUnknownModifier}},
		{"||example.org^$important,script", want{invalid: ReasonUnknownModifier}},
		{"||example.org^$", want{invalid: ReasonMalformedModifiers}},
		{"||example.org^$client='unterminated", want{invalid: ReasonMalformedModifiers}},
		{"||exa mple.org^$important", want{invalid: ReasonWhitespace}},
		{"||bad_label!.org^$important", want{invalid: ReasonInvalidDomain}},
		// browser-only and garbage
		{"example.org##.banner", want{invalid: ReasonCosmetic}},
		{"example.org#@#.banner", want{invalid: ReasonCosmetic}},
		{"example.org#?#div:has(> a)", want{invalid: ReasonCosmetic}},
		{"example.org$$script[data-ad]", want{invalid: ReasonCosmetic}},
		{"||example.org/ads/*", want{invalid: ReasonURL}},
		{"https://example.org/", want{invalid: ReasonURL}},
		{"example.org:8080", want{invalid: ReasonURL}},
		{"foo bar", want{invalid: ReasonWhitespace}},
		{"@@", want{invalid: ReasonEmptyPattern}},
		{"exa!mple.org", want{invalid: ReasonInvalidDomain}},
		{"-.", want{rule: "-", kind: KindExact}},
		{"a..b", want{invalid: ReasonInvalidDomain}},
		{"example.123", want{invalid: ReasonInvalidDomain}},
		{strings.Repeat("a", 64) + ".org", want{invalid: ReasonInvalidDomain}},
		{strings.Repeat("abcdefghi.", 26) + "org", want{invalid: ReasonInvalidDomain}},
		// IPs and CIDRs
		{"192.0.2.1", want{rule: "192.0.2.1", kind: KindIP}},
		{"||192.0.2.1^", want{rule: "192.0.2.1", kind: KindIP}},
		{"2001:db8::1", want{rule: "2001:db8::1", kind: KindIP}},
		{"192.0.2.0/24", want{unsupported: ReasonCIDR}},
		// boilerplate
		{"localhost", want{ignored: true}},
	}
	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			rules, res := parseAll(t, tc.line+"\n", Options{Format: FormatAdblock})
			got := want{ignored: res.Ignored > 0}
			if len(rules) > 1 {
				t.Fatalf("got %d rules, want at most 1", len(rules))
			}
			if len(rules) == 1 {
				got.rule, got.kind = rules[0].String(), rules[0].Kind
			}
			if len(res.UnsupportedRules) > 0 {
				got.unsupported = res.UnsupportedRules[0].Reason
			}
			if len(res.InvalidSamples) > 0 {
				got.invalid = res.InvalidSamples[0].Reason
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v (invalid detail %+v)", got, tc.want, res.InvalidSamples)
			}
		})
	}
}

func TestHostsLines(t *testing.T) {
	src := strings.Join([]string{
		"# Title: Test hosts",
		"127.0.0.1 localhost",
		"127.0.0.1 localhost.localdomain local",
		"255.255.255.255 broadcasthost",
		"::1 localhost ip6-localhost ip6-loopback",
		"fe80::1%lo0 localhost",
		"0.0.0.0 0.0.0.0",
		"0.0.0.0 ads.example.com",
		"0.0.0.0\tTracker.Example.NET   # inline comment",
		"127.0.0.1 one.example two.example three.example",
		":: v6sink.example",
		"0:0:0:0:0:0:0:0 v6long.example",
		"127.0.0.2 loop2.example",
		"192.168.1.10 nas.lan",
		"0.0.0.0 bad!name.example good.example",
		"   ",
		"",
	}, "\r\n")
	rules, res := parseAll(t, src, Options{Format: FormatHosts})
	var got []string
	for _, r := range rules {
		if r.Kind != KindExact || r.Syntax != SyntaxHosts {
			t.Errorf("rule %v has kind %v syntax %v", r, r.Kind, r.Syntax)
		}
		got = append(got, r.Pattern)
	}
	want := []string{"ads.example.com", "tracker.example.net", "one.example", "two.example", "three.example", "v6sink.example", "v6long.example", "loop2.example", "good.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rules = %q\nwant    %q", got, want)
	}
	if res.Title != "Test hosts" {
		t.Errorf("Title = %q", res.Title)
	}
	if res.Ignored != 9 {
		t.Errorf("Ignored = %d, want 9", res.Ignored)
	}
	if res.Unsupported != 1 || res.UnsupportedRules[0].Reason != ReasonHostsRewrite || res.UnsupportedRules[0].Pattern != "nas.lan" || res.UnsupportedRules[0].Addr.String() != "192.168.1.10" {
		t.Errorf("unsupported = %d %+v", res.Unsupported, res.UnsupportedRules)
	}
	if res.Invalid != 1 || res.InvalidSamples[0].Detail != "bad!name.example" {
		t.Errorf("invalid = %d %+v", res.Invalid, res.InvalidSamples)
	}
	if res.Lines != 16 || res.Blank != 1 || res.Comments != 1 {
		t.Errorf("Lines/Blank/Comments = %d/%d/%d, want 16/1/1", res.Lines, res.Blank, res.Comments)
	}
	if res.Rules != len(want) {
		t.Errorf("Rules = %d, want %d", res.Rules, len(want))
	}
}

func TestHostsSubdomains(t *testing.T) {
	rules, _ := parseAll(t, "0.0.0.0 ads.example\nplain.example\n||adblock.example^\n", Options{HostsSubdomains: true})
	for _, r := range rules {
		if r.Kind != KindSuffix {
			t.Errorf("%v: kind %v, want suffix", r, r.Kind)
		}
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules", len(rules))
	}
}

func TestRegexFormat(t *testing.T) {
	src := "# Pi-hole style regex list\n" +
		"^ad[sx]?[0-9]*\\.\n" +
		"(\\.|^)doubleclick\\.net$\n" +
		"/^slashed\\./\n" +
		"^tracker\\.;querytype=AAAA\n" +
		"([\n"
	rules, res := parseAll(t, src, Options{Format: FormatRegex})
	var got []string
	for _, r := range rules {
		got = append(got, r.Pattern)
	}
	want := []string{`^ad[sx]?[0-9]*\.`, `(\.|^)doubleclick\.net$`, `^slashed\.`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rules = %q, want %q", got, want)
	}
	if res.Unsupported != 1 || res.UnsupportedReasons[ReasonRegexExtension] != 1 {
		t.Errorf("unsupported = %+v", res.UnsupportedReasons)
	}
	if res.Invalid != 1 || res.InvalidReasons[ReasonInvalidRegex] != 1 {
		t.Errorf("invalid = %+v", res.InvalidReasons)
	}
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want Format
	}{
		{"hosts", "# c\n0.0.0.0 a.example\n0.0.0.0 b.example\nc.example\n", FormatHosts},
		{"domains", "a.example\nb.example\n*.c.example\n||d.example^\n", FormatDomains},
		{"adblock", "! Title: x\n||a.example^\n||b.example^\nc.example\n@@||d.example^\n", FormatAdblock},
		{"regex", "^ad[0-9]+\\.\n(\\.|^)tracker\\.net$\n^banner\\.\n", FormatRegex},
		{"empty", "", FormatDomains},
		{"comments only", "# a\n! b\n", FormatDomains},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, res := parseAll(t, tc.src, Options{})
			if res.Format != tc.want {
				t.Errorf("Format = %q, want %q", res.Format, tc.want)
			}
		})
	}
}

// TestDetectSampleBoundary covers lists longer than the detection sample so
// that buffered and streamed lines are both processed, with line numbers
// preserved.
func TestDetectSampleBoundary(t *testing.T) {
	var b strings.Builder
	for i := range detectSampleLines + 50 {
		b.WriteString("0.0.0.0 host")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(".example")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("\n")
	}
	rules, res := parseAll(t, b.String(), Options{})
	if res.Format != FormatHosts || len(rules) != detectSampleLines+50 {
		t.Fatalf("format %q, %d rules", res.Format, len(rules))
	}
	for i, r := range rules {
		if r.Line != i+1 {
			t.Fatalf("rule %d has line %d", i, r.Line)
		}
	}
}

func TestChecksumMatchesParse(t *testing.T) {
	srcs := []string{
		"",
		"\xEF\xBB\xBF||a.example^\n",
		"! Title: A\n! Last modified: today\n||a.example^\n0.0.0.0 b.example\n",
		"||a.example^\r\n  0.0.0.0 b.example  \r\n# c\n\n[Adblock Plus 2.0]\nno-newline.example",
	}
	for _, src := range srcs {
		_, res := parseAll(t, src, Options{})
		crc, err := Checksum(strings.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		if crc != res.CRC32 {
			t.Errorf("Checksum(%q) = %08x, Parse CRC32 = %08x", src, crc, res.CRC32)
		}
	}
	a, _ := Checksum(strings.NewReader("! Last modified: Mon\n||a.example^\n"))
	b, _ := Checksum(strings.NewReader("! Last modified: Tue\n\n||a.example^   \r\n"))
	c, _ := Checksum(strings.NewReader("||a.example^\n||b.example^\n"))
	if a != b {
		t.Error("comment-only and white-space changes altered the checksum")
	}
	if a == c {
		t.Error("a rule change did not alter the checksum")
	}
}

func TestSniff(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want error
	}{
		{"html", "\n  <!DOCTYPE html><html><body>captive portal</body></html>", ErrHTML},
		{"html tag", "<html>\n", ErrHTML},
		{"xml", "<?xml version=\"1.0\"?><Error/>", ErrHTML},
		{"gzip", "\x1f\x8b\x08\x00rest", ErrBinary},
		{"zip", "PK\x03\x04rest", ErrBinary},
		{"nul", "||a.example^\n\x00\x00", ErrBinary},
		{"control", strings.Repeat("\x01\x02\x03a", 100), ErrBinary},
		{"latin1 comment", "# caf\xe9\n||a.example^\n" + strings.Repeat("0.0.0.0 b.example\n", 10), nil},
		{"bom", "\xEF\xBB\xBF<html>", ErrHTML},
		{"text", "||a.example^\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.src), Options{}, nil)
			if !errors.Is(err, tc.want) {
				t.Errorf("Parse error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTooLongLine(t *testing.T) {
	src := "||a.example^\n" + strings.Repeat("x", maxLineLen+10) + "\n||b.example^\n"
	rules, res := parseAll(t, src, Options{Format: FormatAdblock})
	if len(rules) != 2 || res.InvalidReasons[ReasonTooLong] != 1 || res.Lines != 3 {
		t.Fatalf("rules %d, invalid %+v, lines %d", len(rules), res.InvalidReasons, res.Lines)
	}
	crc, _ := Checksum(strings.NewReader(src))
	if crc != res.CRC32 {
		t.Error("checksum differs with a too-long line")
	}
}

func TestReadErrors(t *testing.T) {
	boom := errors.New("boom")
	if _, err := Parse(iotest.ErrReader(boom), Options{}, nil); !errors.Is(err, boom) {
		t.Errorf("Parse = %v, want boom", err)
	}
	if _, err := Checksum(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Errorf("Checksum = %v, want boom", err)
	}
	if _, err := Parse(strings.NewReader(""), Options{Format: "csv"}, nil); !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("Parse with bad format = %v", err)
	}
	stop := errors.New("stop")
	_, err := Parse(strings.NewReader("a.example\nb.example\n"), Options{Format: FormatDomains}, func(Rule) error { return stop })
	if !errors.Is(err, stop) {
		t.Errorf("emit error not returned: %v", err)
	}
	_, err = Parse(strings.NewReader("a.example\nb.example\n"), Options{}, func(Rule) error { return stop })
	if !errors.Is(err, stop) {
		t.Errorf("emit error not returned in auto mode: %v", err)
	}
}

func TestKeepLimits(t *testing.T) {
	src := strings.Repeat("||a.example^$important\nexample.org##.ad\n", 30)
	_, res := parseAll(t, src, Options{KeepUnsupported: 5, KeepInvalid: -1})
	if res.Unsupported != 30 || len(res.UnsupportedRules) != 5 {
		t.Errorf("unsupported %d kept %d", res.Unsupported, len(res.UnsupportedRules))
	}
	if res.Invalid != 30 || len(res.InvalidSamples) != 0 {
		t.Errorf("invalid %d kept %d", res.Invalid, len(res.InvalidSamples))
	}
}

func TestModifiers(t *testing.T) {
	tests := []struct {
		in     string
		want   []Modifier
		values [][]string
	}{
		{"important", []Modifier{{Name: "important"}}, [][]string{nil}},
		{"DNSTYPE=AAAA", []Modifier{{Name: "dnstype", Value: "AAAA"}}, [][]string{{"AAAA"}}},
		{"client=127.0.0.1,dnstype=A", []Modifier{{Name: "client", Value: "127.0.0.1"}, {Name: "dnstype", Value: "A"}}, [][]string{{"127.0.0.1"}, {"A"}}},
		{`client='Frank\'s laptop'`, []Modifier{{Name: "client", Value: `'Frank\'s laptop'`}}, [][]string{{"Frank's laptop"}}},
		{`client=~'Mary\'s\, John\'s'`, []Modifier{{Name: "client", Value: `~'Mary\'s\, John\'s'`}}, [][]string{{"~Mary's, John's"}}},
		{"client=~Mom|~Dad|Kids", []Modifier{{Name: "client", Value: "~Mom|~Dad|Kids"}}, [][]string{{"~Mom", "~Dad", "Kids"}}},
		{"dnsrewrite=NOERROR;TXT;it's", []Modifier{{Name: "dnsrewrite", Value: "NOERROR;TXT;it's"}}, [][]string{{"NOERROR;TXT;it's"}}},
	}
	for _, tc := range tests {
		got, err := ParseModifiers(tc.in)
		if err != nil {
			t.Errorf("ParseModifiers(%q): %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseModifiers(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		for i, m := range got {
			if v := m.Values(); !reflect.DeepEqual(v, tc.values[i]) {
				t.Errorf("%q Values() = %q, want %q", tc.in, v, tc.values[i])
			}
		}
	}
	for _, bad := range []string{"", ",", "important,", "=x", "a b", `client='x`} {
		if _, err := ParseModifiers(bad); !errors.Is(err, ErrMalformedModifiers) {
			t.Errorf("ParseModifiers(%q) = %v, want ErrMalformedModifiers", bad, err)
		}
	}
}

func TestUnsupportedKeepsModifiers(t *testing.T) {
	_, res := parseAll(t, "@@||example.org^$dnstype=AAAA,important\n", Options{})
	if len(res.UnsupportedRules) != 1 {
		t.Fatal("rule not kept")
	}
	u := res.UnsupportedRules[0]
	want := UnsupportedRule{
		Line: 1, Text: "@@||example.org^$dnstype=AAAA,important", Reason: ReasonModifiers, Exception: true,
		Pattern: "||example.org^", Modifiers: []Modifier{{Name: "dnstype", Value: "AAAA"}, {Name: "important"}},
	}
	if !reflect.DeepEqual(u, want) {
		t.Errorf("got %+v\nwant %+v", u, want)
	}
}

// TestRetainedStringsDoNotAlias checks that everything the parser keeps
// after the callback returns survives the read buffer being overwritten.
func TestRetainedStringsDoNotAlias(t *testing.T) {
	head := "||kept.example^$dnstype=AAAA\nbad!name.example\n# Title: kept title\n"
	src := head + strings.Repeat("0.0.0.0 filler-line-overwriting-the-buffer.example\n", 20000)
	_, res, err := func() ([]Rule, *Result, error) {
		res, err := Parse(strings.NewReader(src), Options{Format: FormatAdblock}, nil)
		return nil, res, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	u := res.UnsupportedRules[0]
	if u.Text != "||kept.example^$dnstype=AAAA" || u.Pattern != "||kept.example^" || u.Modifiers[0] != (Modifier{Name: "dnstype", Value: "AAAA"}) {
		t.Errorf("unsupported rule corrupted: %+v", u)
	}
	if d := res.InvalidSamples[0]; d.Text != "bad!name.example" {
		t.Errorf("invalid sample corrupted: %+v", d)
	}
	if res.Title != "kept title" {
		t.Errorf("title corrupted: %q", res.Title)
	}
}

func TestNormalizeDomain(t *testing.T) {
	tests := []struct {
		in, want string
		err      error
	}{
		{"Example.COM.", "example.com", nil},
		{"xn--bcher-kva.example", "xn--bcher-kva.example", nil},
		{"bücher.example", "xn--bcher-kva.example", nil},
		{"", "", ErrEmptyName},
		{".", "", ErrEmptyName},
		{"a..b", "", ErrBadLabel},
		{".a", "", ErrBadLabel},
		{"a b", "", ErrBadChar},
		{"*.a", "", ErrBadChar},
		{"10.0.0.256", "", ErrNumericTLD},
		{strings.Repeat("a", 63) + ".b", strings.Repeat("a", 63) + ".b", nil},
		{strings.Repeat("a", 64) + ".b", "", ErrBadLabel},
		{strings.Repeat("a.", 126) + "b", strings.Repeat("a.", 126) + "b", nil},
		{strings.Repeat("a.", 127) + "b", "", ErrNameTooLong},
	}
	for _, tc := range tests {
		got, err := NormalizeDomain(tc.in)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("NormalizeDomain(%q) = (%q, %v), want (%q, %v)", tc.in, got, err, tc.want, tc.err)
		}
	}
	if s := "already.lower"; LowerASCII(s) != s {
		t.Error("LowerASCII changed a lower-case string")
	}
}

func TestRenderAndStrings(t *testing.T) {
	for k, want := range map[Kind]string{KindExact: "exact", KindSuffix: "suffix", KindRegex: "regex", KindIP: "ip", 0: "unknown"} {
		if k.String() != want {
			t.Errorf("Kind(%d).String() = %q", k, k.String())
		}
	}
	if got := SyntaxHosts.Render("a.example", true); got != "@@a.example" {
		t.Errorf("Render = %q", got)
	}
	if !Format("").Valid() || Format("x").Valid() {
		t.Error("Format.Valid")
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"||example.org^\n", "@@||a.b^$important\n", "0.0.0.0 a b c # x\n", "/re(/\n",
		"*.x\n", "|x|\n", "example.org##.ad\n", "$dnstype=A\n", "\xEF\xBB\xBF! Title: t\n",
		"192.0.2.0/24\n", "||1.2.3.4^\n", "bücher.example\n", "client='a\\,b'\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, format := range []Format{FormatAuto, FormatAdblock, FormatRegex} {
			res, err := Parse(bytes.NewReader(data), Options{Format: format}, func(r Rule) error {
				if r.Kind == 0 || r.Syntax == 0 || r.Pattern == "" {
					t.Fatalf("incomplete rule %+v", r)
				}
				if (r.Kind == KindExact || r.Kind == KindSuffix) && validateDomain(r.Pattern) != nil {
					t.Fatalf("rule pattern %q is not a valid name", r.Pattern)
				}
				return nil
			})
			if err != nil {
				continue
			}
			crc, err := Checksum(bytes.NewReader(data))
			if err != nil || crc != res.CRC32 {
				t.Fatalf("Checksum = %08x, %v; Parse CRC32 = %08x", crc, err, res.CRC32)
			}
		}
	})
}
