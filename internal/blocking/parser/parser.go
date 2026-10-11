package parser

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/netip"
	"regexp/syntax"
	"strings"
	"unicode/utf8"
)

// Errors returned by Parse for bodies that are not lists at all; callers
// keep the previous version of the list.
var (
	ErrHTML          = errors.New("parser: source is an HTML or XML page, not a list")
	ErrBinary        = errors.New("parser: source is binary, not a text list")
	ErrUnknownFormat = errors.New("parser: unknown format")
)

const (
	defaultKeepUnsupported = 1000
	defaultKeepInvalid     = 20
	// detectSampleLines is how many rule lines FormatAuto inspects.
	detectSampleLines = 256
	sniffLen          = 4096
	// maxLineLen bounds a line; longer lines are invalid (ReasonTooLong).
	maxLineLen = 64 << 10
)

var (
	utf8BOM = []byte{0xEF, 0xBB, 0xBF}
	newline = []byte{'\n'}
)

// Options control parsing of one source.
type Options struct {
	// Format of the source; empty means FormatAuto.
	Format Format
	// HostsSubdomains makes hosts and plain-domain entries match their
	// subdomains too (KindSuffix instead of KindExact), for lists published
	// as "domains with subdomains".
	HostsSubdomains bool
	// KeepUnsupported bounds Result.UnsupportedRules: 0 means 1000, a
	// negative value keeps none. Counting is never bounded.
	KeepUnsupported int
	// KeepInvalid bounds Result.InvalidSamples: 0 means 20, a negative value
	// keeps none.
	KeepInvalid int
}

// Parse reads a list from r and calls emit for every rule, in order. A
// non-nil error from emit stops parsing and is returned. emit may be nil to
// only count.
func Parse(r io.Reader, opts Options, emit func(Rule) error) (*Result, error) {
	if !opts.Format.Valid() {
		return nil, fmt.Errorf("%w %q", ErrUnknownFormat, opts.Format)
	}
	if opts.Format == "" {
		opts.Format = FormatAuto
	}
	opts.KeepUnsupported = keep(opts.KeepUnsupported, defaultKeepUnsupported)
	opts.KeepInvalid = keep(opts.KeepInvalid, defaultKeepInvalid)

	br := bufio.NewReaderSize(r, maxLineLen)
	if err := sniffReader(br); err != nil {
		return nil, err
	}
	p := &state{
		opts:   opts,
		emit:   emit,
		format: opts.Format,
		res: &Result{
			UnsupportedReasons: map[string]int{},
			InvalidReasons:     map[string]int{},
		},
	}

	detecting := opts.Format == FormatAuto
	var pending []pendingLine
	sampled := 0
	flush := func() error {
		p.format = detectFormat(pending)
		for i, pl := range pending {
			if err := p.process(i+1, pl.data, pl.tooLong); err != nil {
				return err
			}
		}
		pending, detecting = nil, false
		return nil
	}

	for n := 1; ; n++ {
		raw, tooLong, err := readLine(br)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parser: read: %w", err)
		}
		atEOF := err != nil
		if atEOF && len(raw) == 0 && !tooLong {
			break
		}
		if n == 1 {
			raw = bytes.TrimPrefix(raw, utf8BOM)
		}
		if detecting {
			pending = append(pending, pendingLine{data: bytes.Clone(raw), tooLong: tooLong})
			if t := bytes.TrimSpace(raw); !tooLong && len(t) > 0 && !isCommentLine(t) {
				sampled++
			}
			if sampled >= detectSampleLines {
				if err := flush(); err != nil {
					return nil, err
				}
			}
		} else if err := p.process(n, raw, tooLong); err != nil {
			return nil, err
		}
		if atEOF {
			break
		}
	}
	if detecting {
		if err := flush(); err != nil {
			return nil, err
		}
	}
	p.res.Format = p.format
	p.res.CRC32 = p.crc
	return p.res, nil
}

// Checksum returns the CRC-32 (IEEE) of the normalised rule lines of r:
// every line trimmed of surrounding white space, with blank lines, comments
// ("!", "#") and "[Adblock ...]" headers removed, each followed by '\n'. It
// equals Result.CRC32 of Parse on the same input but does not parse rules,
// so a refresh can detect an unchanged list cheaply.
func Checksum(r io.Reader) (uint32, error) {
	br := bufio.NewReaderSize(r, maxLineLen)
	var crc uint32
	for n := 1; ; n++ {
		raw, tooLong, err := readLine(br)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("parser: read: %w", err)
		}
		if n == 1 {
			raw = bytes.TrimPrefix(raw, utf8BOM)
		}
		if t := bytes.TrimSpace(raw); !tooLong && len(t) > 0 && !isCommentLine(t) {
			crc = crc32.Update(crc, crc32.IEEETable, t)
			crc = crc32.Update(crc, crc32.IEEETable, newline)
		}
		if err != nil {
			return crc, nil
		}
	}
}

func keep(v, def int) int {
	switch {
	case v == 0:
		return def
	case v < 0:
		return 0
	}
	return v
}

type pendingLine struct {
	data    []byte
	tooLong bool
}

// readLine returns the next line including its '\n'. A line longer than the
// reader's buffer is consumed and reported as tooLong.
func readLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	line, err = br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = br.ReadSlice('\n')
		}
		return nil, true, err
	}
	return line, false, err
}

func isCommentLine(t []byte) bool {
	switch t[0] {
	case '!', '#':
		return true
	case '[':
		return t[len(t)-1] == ']' // [Adblock Plus 2.0]
	}
	return false
}

type state struct {
	opts   Options
	emit   func(Rule) error
	format Format
	res    *Result
	crc    uint32
}

func (p *state) process(n int, raw []byte, tooLong bool) error {
	p.res.Lines++
	if tooLong {
		p.invalid(n, "", ReasonTooLong, "")
		return nil
	}
	t := bytes.TrimSpace(raw)
	switch {
	case len(t) == 0:
		p.res.Blank++
		return nil
	case isCommentLine(t):
		p.res.Comments++
		p.title(t)
		return nil
	}
	p.crc = crc32.Update(p.crc, crc32.IEEETable, t)
	p.crc = crc32.Update(p.crc, crc32.IEEETable, newline)
	line := string(t)
	if p.format == FormatRegex {
		return p.regexLine(n, line)
	}
	return p.ruleLine(n, line)
}

func (p *state) title(t []byte) {
	if p.res.Title != "" {
		return
	}
	s := strings.TrimLeft(string(t[1:]), " \t!#")
	if len(s) > 6 && strings.EqualFold(s[:6], "title:") {
		p.res.Title = strings.TrimSpace(s[6:])
	}
}

func (p *state) ruleLine(n int, line string) error {
	s := line
	if s[0] != '/' && !strings.HasPrefix(s, "@@/") {
		s = stripInlineComment(s)
	}
	exception := false
	if strings.HasPrefix(s, "@@") {
		exception, s = true, s[2:]
	}
	if s == "" {
		p.invalid(n, line, ReasonEmptyPattern, "")
		return nil
	}
	if s[0] == '/' {
		return p.regexRule(n, line, s, exception)
	}
	if hasCosmeticMarker(s) {
		p.invalid(n, line, ReasonCosmetic, "")
		return nil
	}
	if !exception {
		if first, rest := nextField(s); rest != "" {
			if addr, ok := parseIP(first); ok {
				return p.hostsLine(n, line, addr, rest)
			}
		}
	}

	pat, mods, hasMods := strings.Cut(s, "$")
	if strings.ContainsAny(pat, " \t") {
		p.invalid(n, line, ReasonWhitespace, "")
		return nil
	}
	c := p.classify(pat)
	if hasMods {
		ms, err := ParseModifiers(mods)
		if err != nil {
			p.invalid(n, line, ReasonMalformedModifiers, "")
			return nil
		}
		for _, m := range ms {
			if !KnownModifier(m.Name) {
				p.invalid(n, line, ReasonUnknownModifier, m.Name)
				return nil
			}
		}
		if c.invalid != "" {
			p.invalid(n, line, c.invalid, c.detail)
			return nil
		}
		p.unsupported(UnsupportedRule{Line: n, Text: line, Reason: ReasonModifiers, Exception: exception, Pattern: pat, Modifiers: ms})
		return nil
	}
	switch {
	case c.invalid != "":
		p.invalid(n, line, c.invalid, c.detail)
	case c.ignored:
		p.res.Ignored++
	case c.unsupported != "":
		p.unsupported(UnsupportedRule{Line: n, Text: line, Reason: c.unsupported, Exception: exception, Pattern: pat})
	default:
		return p.rule(Rule{Kind: c.kind, Syntax: c.syntax, Pattern: c.pattern, Addr: c.addr, Exception: exception, Line: n})
	}
	return nil
}

// class is the classification of an adblock pattern (no "@@", no "$...").
type class struct {
	kind        Kind
	syntax      Syntax
	pattern     string
	addr        netip.Addr
	unsupported string
	invalid     string
	detail      string
	ignored     bool
}

func (p *state) classify(pat string) class {
	if pat == "" {
		// "$dnstype=AAAA,..." applies to every name: only valid with modifiers.
		return class{unsupported: ReasonWildcard}
	}
	if i := strings.IndexByte(pat, '/'); i >= 0 {
		if mightBeIP(pat[:i]) {
			if _, err := netip.ParsePrefix(pat); err == nil {
				return class{unsupported: ReasonCIDR}
			}
		}
		return class{invalid: ReasonURL}
	}
	switch {
	case strings.HasPrefix(pat, "||"):
		body, _ := trimTerminator(pat[2:])
		if rest, ok := strings.CutPrefix(body, "*."); ok && rest != "" && !hasSpecial(rest) {
			return domainClass(rest, KindSuffix, SyntaxAdblockWildcard)
		}
		if body == "" || hasSpecial(body) {
			return specialClass(body)
		}
		if a, ok := parseIP(body); ok {
			return ipClass(a)
		}
		return domainClass(body, KindSuffix, SyntaxAdblock)
	case strings.HasPrefix(pat, "|"):
		body, terminated := trimTerminator(pat[1:])
		if body == "" || hasSpecial(body) {
			return specialClass(body)
		}
		if !terminated {
			return class{unsupported: ReasonPrefix}
		}
		if a, ok := parseIP(body); ok {
			return ipClass(a)
		}
		return domainClass(body, KindExact, SyntaxAdblockExact)
	case strings.HasPrefix(pat, "*."):
		body, _ := trimTerminator(pat[2:])
		if body == "" || hasSpecial(body) {
			return class{unsupported: ReasonWildcard}
		}
		return domainClass(body, KindSuffix, SyntaxWildcard)
	case hasSpecial(pat):
		return specialClass(pat)
	}
	if a, ok := parseIP(pat); ok {
		return ipClass(a)
	}
	if strings.IndexByte(pat, ':') >= 0 {
		return class{invalid: ReasonURL} // "example.org:8080", "http:"
	}
	kind := KindExact
	if p.opts.HostsSubdomains {
		kind = KindSuffix
	}
	c := domainClass(pat, kind, SyntaxDomain)
	if c.invalid == "" && hostsBoilerplate[c.pattern] {
		c.ignored = true
	}
	return c
}

func domainClass(s string, kind Kind, syn Syntax) class {
	d, err := NormalizeDomain(s)
	if err != nil {
		return class{invalid: ReasonInvalidDomain, detail: err.Error()}
	}
	return class{kind: kind, syntax: syn, pattern: d}
}

func ipClass(a netip.Addr) class {
	a = a.Unmap().WithZone("")
	return class{kind: KindIP, syntax: SyntaxIP, pattern: a.String(), addr: a}
}

func specialClass(s string) class {
	if strings.IndexByte(s, '*') >= 0 || s == "" {
		return class{unsupported: ReasonWildcard}
	}
	return class{unsupported: ReasonUnanchored}
}

func (p *state) hostsLine(n int, line string, addr netip.Addr, rest string) error {
	sink := isSinkIP(addr)
	kind := KindExact
	if p.opts.HostsSubdomains {
		kind = KindSuffix
	}
	for {
		var f string
		f, rest = nextField(rest)
		if f == "" {
			return nil
		}
		if _, ok := parseIP(f); ok {
			p.res.Ignored++ // "0.0.0.0 0.0.0.0"
			continue
		}
		d, err := NormalizeDomain(f)
		if err != nil {
			p.invalid(n, line, ReasonInvalidDomain, f)
			continue
		}
		switch {
		case hostsBoilerplate[d]:
			p.res.Ignored++
		case !sink:
			p.unsupported(UnsupportedRule{Line: n, Text: line, Reason: ReasonHostsRewrite, Pattern: d, Addr: addr})
		default:
			if err := p.rule(Rule{Kind: kind, Syntax: SyntaxHosts, Pattern: d, Line: n}); err != nil {
				return err
			}
		}
	}
}

// regexRule handles "/re/" and "/re/$modifiers" in adblock-style lists.
func (p *state) regexRule(n int, line, s string, exception bool) error {
	last := strings.LastIndexByte(s, '/')
	if last <= 1 {
		p.invalid(n, line, ReasonInvalidRegex, "empty or unterminated /regex/")
		return nil
	}
	re, tail := s[1:last], s[last+1:]
	var mods []Modifier
	if tail != "" {
		if tail[0] != '$' {
			p.invalid(n, line, ReasonURL, "")
			return nil
		}
		ms, err := ParseModifiers(tail[1:])
		if err != nil {
			p.invalid(n, line, ReasonMalformedModifiers, "")
			return nil
		}
		for _, m := range ms {
			if !KnownModifier(m.Name) {
				p.invalid(n, line, ReasonUnknownModifier, m.Name)
				return nil
			}
		}
		mods = ms
	}
	if _, err := syntax.Parse(re, syntax.Perl); err != nil {
		p.invalid(n, line, ReasonInvalidRegex, err.Error())
		return nil
	}
	if mods != nil {
		p.unsupported(UnsupportedRule{Line: n, Text: line, Reason: ReasonModifiers, Exception: exception, Pattern: s[:last+1], Modifiers: mods})
		return nil
	}
	return p.rule(Rule{Kind: KindRegex, Syntax: SyntaxRegex, Pattern: re, Exception: exception, Line: n})
}

// regexExtensions are Pi-hole regex-list options that change the meaning
// of a rule; they are kept for a later version.
var regexExtensions = []string{";querytype=", ";invert", ";reply="}

// regexLine handles one line of a FormatRegex source: an RE2 expression
// with or without /.../ delimiters.
func (p *state) regexLine(n int, line string) error {
	s := line
	if len(s) >= 2 && s[0] == '/' && s[len(s)-1] == '/' {
		s = s[1 : len(s)-1]
	}
	if s == "" {
		// An empty expression would match every name.
		p.invalid(n, line, ReasonInvalidRegex, "empty expression")
		return nil
	}
	for _, ext := range regexExtensions {
		if strings.Contains(s, ext) {
			p.unsupported(UnsupportedRule{Line: n, Text: line, Reason: ReasonRegexExtension, Pattern: s})
			return nil
		}
	}
	if _, err := syntax.Parse(s, syntax.Perl); err != nil {
		p.invalid(n, line, ReasonInvalidRegex, err.Error())
		return nil
	}
	return p.rule(Rule{Kind: KindRegex, Syntax: SyntaxRegex, Pattern: s, Line: n})
}

func (p *state) rule(r Rule) error {
	p.res.Rules++
	if r.Exception {
		p.res.Exceptions++
	}
	if p.emit == nil {
		return nil
	}
	return p.emit(r)
}

func (p *state) unsupported(u UnsupportedRule) {
	p.res.Unsupported++
	p.res.UnsupportedReasons[u.Reason]++
	if len(p.res.UnsupportedRules) < p.opts.KeepUnsupported {
		p.res.UnsupportedRules = append(p.res.UnsupportedRules, u)
	}
}

func (p *state) invalid(n int, text, reason, detail string) {
	p.res.Invalid++
	p.res.InvalidReasons[reason]++
	if len(p.res.InvalidSamples) < p.opts.KeepInvalid {
		p.res.InvalidSamples = append(p.res.InvalidSamples, Diagnostic{Line: n, Text: text, Reason: reason, Detail: detail})
	}
}

// trimTerminator removes a closing "^|", "^" or "|" and reports whether one
// was present.
func trimTerminator(s string) (string, bool) {
	for _, suf := range [...]string{"^|", "^", "|"} {
		if t, ok := strings.CutSuffix(s, suf); ok {
			return t, true
		}
	}
	return s, false
}

func hasSpecial(s string) bool {
	return strings.ContainsAny(s, "*^|")
}

// stripInlineComment cuts a '#' comment that follows white space.
func stripInlineComment(s string) string {
	for i := strings.IndexByte(s, '#'); i > 0; {
		if s[i-1] == ' ' || s[i-1] == '\t' {
			return strings.TrimRight(s[:i], " \t")
		}
		j := strings.IndexByte(s[i+1:], '#')
		if j < 0 {
			break
		}
		i += j + 1
	}
	return s
}

// cosmeticMarkers separate a browser element-hiding, CSS, scriptlet or
// HTML-filtering rule from its domains.
var cosmeticMarkers = [...]string{"##", "#@#", "#?#", "#$#", "#%#", "#@$#", "#@%#", "#@?#"}

func hasCosmeticMarker(s string) bool {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		rest := s[i:]
		for _, m := range cosmeticMarkers {
			if strings.Contains(rest, m) {
				return true
			}
		}
	}
	return strings.Contains(s, "$$") || strings.Contains(s, "$@$")
}

// nextField returns the first run of non-blank bytes of s and what follows
// it.
func nextField(s string) (field, rest string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	j := i
	for j < len(s) && s[j] != ' ' && s[j] != '\t' {
		j++
	}
	rest = s[j:]
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	return s[i:j], rest
}

func detectFormat(lines []pendingLine) Format {
	var hosts, adblock, domains, regex int
	for _, l := range lines {
		t := bytes.TrimSpace(l.data)
		if l.tooLong || len(t) == 0 || isCommentLine(t) {
			continue
		}
		switch detectLine(string(t)) {
		case FormatHosts:
			hosts++
		case FormatAdblock:
			adblock++
		case FormatDomains:
			domains++
		case FormatRegex:
			regex++
		}
	}
	switch {
	case regex > hosts+adblock+domains:
		return FormatRegex
	case adblock > 0 && adblock >= hosts && adblock >= domains:
		return FormatAdblock
	case hosts > 0 && hosts >= domains:
		return FormatHosts
	}
	return FormatDomains
}

func detectLine(s string) Format {
	if first, rest := nextField(s); rest != "" {
		if _, ok := parseIP(first); ok {
			return FormatHosts
		}
	}
	switch {
	case strings.HasPrefix(s, "||"), strings.HasPrefix(s, "@@"), strings.HasPrefix(s, "|"), hasCosmeticMarker(s):
		return FormatAdblock
	case len(s) > 1 && s[0] == '/' && strings.LastIndexByte(s, '/') > 0:
		return FormatAdblock
	}
	if _, err := NormalizeDomain(strings.TrimPrefix(stripInlineComment(s), "*.")); err == nil {
		return FormatDomains
	}
	if strings.ContainsAny(s, `\()[]{}+?`) || s[0] == '^' || s[len(s)-1] == '$' {
		return FormatRegex
	}
	if hasSpecial(s) || strings.IndexByte(s, '$') >= 0 {
		return FormatAdblock
	}
	return ""
}

func sniffReader(br *bufio.Reader) error {
	head, err := br.Peek(sniffLen)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return fmt.Errorf("parser: read: %w", err)
	}
	return sniff(head)
}

var htmlPrefixes = [...]string{"<!doctype", "<html", "<?xml", "<head", "<body", "<!--"}

// sniff rejects HTML/XML pages (captive portals, error pages) and binary
// bodies (archives, images) by looking at the first bytes.
func sniff(head []byte) error {
	head = bytes.TrimPrefix(head, utf8BOM)
	if len(head) == 0 {
		return nil
	}
	if bytes.HasPrefix(head, []byte{0x1f, 0x8b}) || bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.IndexByte(head, 0) >= 0 {
		return ErrBinary
	}
	bad := 0
	for i := 0; i < len(head); {
		c := head[i]
		if c < utf8.RuneSelf {
			if c < 0x20 && c != '\t' && c != '\n' && c != '\r' && c != '\f' && c != '\v' {
				bad++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(head[i:])
		if r == utf8.RuneError && size == 1 {
			if !utf8.FullRune(head[i:]) {
				break // a rune cut by the end of the window
			}
			bad++
		}
		i += size
	}
	if bad*50 > len(head) {
		return ErrBinary
	}
	first := bytes.TrimLeft(head, " \t\r\n")
	if len(first) > 16 {
		first = first[:16]
	}
	first = bytes.ToLower(first)
	for _, pfx := range htmlPrefixes {
		if bytes.HasPrefix(first, []byte(pfx)) {
			return ErrHTML
		}
	}
	return nil
}
