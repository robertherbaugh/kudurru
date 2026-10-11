package parser

import (
	"net/netip"
	"strings"
)

// Format is the syntax of a list source. The values are those of
// api/v1alpha1.ListFormat, so a ListSource.Format converts directly.
type Format string

// Formats.
const (
	FormatAuto    Format = "auto"
	FormatHosts   Format = "hosts"
	FormatDomains Format = "domains"
	FormatAdblock Format = "adblock"
	FormatRegex   Format = "regex"
)

// Valid reports whether f is a known format; the empty format counts as
// auto.
func (f Format) Valid() bool {
	switch f {
	case "", FormatAuto, FormatHosts, FormatDomains, FormatAdblock, FormatRegex:
		return true
	}
	return false
}

// Kind says which matcher a rule compiles into.
type Kind uint8

// Kinds.
const (
	// KindExact matches the name exactly.
	KindExact Kind = iota + 1
	// KindSuffix matches the name and every subdomain of it.
	KindSuffix
	// KindRegex matches names against an RE2 expression.
	KindRegex
	// KindIP matches addresses in answers (response-side checks).
	KindIP
)

func (k Kind) String() string {
	switch k {
	case KindExact:
		return "exact"
	case KindSuffix:
		return "suffix"
	case KindRegex:
		return "regex"
	case KindIP:
		return "ip"
	}
	return "unknown"
}

// Syntax records how a rule was written so the matched rule can be shown
// back in the query log ("blocked by oisd: ||ads.example^") without keeping
// the original line. It fits the one-byte tag the matchers store.
type Syntax uint8

// Syntaxes.
const (
	SyntaxDomain          Syntax = iota + 1 // example.org
	SyntaxHosts                             // 0.0.0.0 example.org
	SyntaxAdblock                           // ||example.org^
	SyntaxAdblockExact                      // |example.org^
	SyntaxAdblockWildcard                   // ||*.example.org^
	SyntaxWildcard                          // *.example.org
	SyntaxRegex                             // /regex/
	SyntaxIP                                // 192.0.2.1
)

// Render returns the rule text for a pattern written in this syntax, with
// the @@ prefix for exceptions. For hosts entries only the host is shown.
func (s Syntax) Render(pattern string, exception bool) string {
	var b strings.Builder
	b.Grow(len(pattern) + 6)
	if exception {
		b.WriteString("@@")
	}
	switch s {
	case SyntaxAdblock:
		b.WriteString("||")
		b.WriteString(pattern)
		b.WriteByte('^')
	case SyntaxAdblockExact:
		b.WriteByte('|')
		b.WriteString(pattern)
		b.WriteByte('^')
	case SyntaxAdblockWildcard:
		b.WriteString("||*.")
		b.WriteString(pattern)
		b.WriteByte('^')
	case SyntaxWildcard:
		b.WriteString("*.")
		b.WriteString(pattern)
	case SyntaxRegex:
		b.WriteByte('/')
		b.WriteString(pattern)
		b.WriteByte('/')
	default:
		b.WriteString(pattern)
	}
	return b.String()
}

// Rule is one compiled-able rule. Pattern is the normalised name (or base
// name for suffix rules), the RE2 source for regex rules, or the address
// text for IP rules.
//
// Pattern may share memory with the parser's read buffer: it is valid only
// until the callback that received the Rule returns. Use strings.Clone to
// keep it (the matcher builders copy what they store).
type Rule struct {
	Kind      Kind
	Syntax    Syntax
	Pattern   string
	Addr      netip.Addr // KindIP only
	Exception bool
	Line      int
}

// String renders the rule as list text.
func (r Rule) String() string {
	return r.Syntax.Render(r.Pattern, r.Exception)
}

// Modifier is one "$name=value" token of an adblock rule. Value keeps its
// quotes and escapes exactly as written; Values splits it.
type Modifier struct {
	Name  string // lower-case
	Value string // raw; empty when the modifier has no value
}

// Values splits Value on unescaped '|' and removes quotes and backslash
// escapes from each element, keeping a leading '~' (negation) outside the
// quotes as the specification requires: ~'Mom\'s laptop' -> ~Mom's laptop.
func (m Modifier) Values() []string {
	if m.Value == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	var quote byte
	elementStart := true // a quote opens only at the start of an element
	for i := 0; i < len(m.Value); i++ {
		c := m.Value[i]
		switch {
		case c == '\\' && i+1 < len(m.Value):
			i++
			cur.WriteByte(m.Value[i])
			elementStart = false
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && elementStart && (c == '\'' || c == '"'):
			quote = c
			elementStart = false
		case quote == 0 && c == '|':
			out = append(out, cur.String())
			cur.Reset()
			elementStart = true
		default:
			cur.WriteByte(c)
			elementStart = quote == 0 && c == '~' && cur.Len() == 1
		}
	}
	return append(out, cur.String())
}

// Unsupported reasons: valid rules that v0.1 parses but does not compile.
const (
	ReasonModifiers      = "modifiers"
	ReasonWildcard       = "wildcard-pattern"
	ReasonUnanchored     = "unanchored-pattern"
	ReasonPrefix         = "prefix-pattern"
	ReasonHostsRewrite   = "hosts-rewrite"
	ReasonCIDR           = "cidr"
	ReasonRegexExtension = "regex-extension"
)

// Invalid reasons: lines that are not DNS rules.
const (
	ReasonCosmetic           = "cosmetic"
	ReasonURL                = "url"
	ReasonUnknownModifier    = "unknown-modifier"
	ReasonMalformedModifiers = "malformed-modifiers"
	ReasonInvalidDomain      = "invalid-domain"
	ReasonInvalidRegex       = "invalid-regex"
	ReasonWhitespace         = "whitespace"
	ReasonEmptyPattern       = "empty-pattern"
	ReasonTooLong            = "too-long"
)

// UnsupportedRule is a valid rule that this version does not compile, kept
// with its tokenised modifiers so that the v0.2 modifier slow path can use
// it (docs/architecture.md §7.3 item 4).
type UnsupportedRule struct {
	Line      int
	Text      string // the trimmed line
	Reason    string
	Exception bool
	Pattern   string     // the pattern without "@@" and "$modifiers"
	Modifiers []Modifier // nil unless Reason == ReasonModifiers
	Addr      netip.Addr // ReasonHostsRewrite: the address the host maps to
}

// Diagnostic describes one invalid line.
type Diagnostic struct {
	Line   int
	Text   string
	Reason string
	Detail string
}

// Result summarises one parsed source.
type Result struct {
	// Format is the effective format: the detected one when the source was
	// parsed with FormatAuto.
	Format Format
	// Title is the value of the first "! Title:" or "# Title:" header.
	Title string

	Lines    int // physical lines
	Blank    int
	Comments int
	// Rules counts the rules handed to the callback; a hosts line with
	// three names counts three.
	Rules int
	// Exceptions counts the @@ rules among Rules.
	Exceptions int
	// Unsupported counts valid rules this version does not compile.
	Unsupported int
	// Invalid counts lines (or hosts names) that are not DNS rules.
	Invalid int
	// Ignored counts hosts-file boilerplate such as "127.0.0.1 localhost".
	Ignored int

	// CRC32 is the IEEE CRC-32 of the normalised rule lines; it equals
	// Checksum of the same input.
	CRC32 uint32

	UnsupportedReasons map[string]int
	InvalidReasons     map[string]int
	// UnsupportedRules holds the first Options.KeepUnsupported unsupported
	// rules.
	UnsupportedRules []UnsupportedRule
	// InvalidSamples holds the first Options.KeepInvalid invalid lines.
	InvalidSamples []Diagnostic
}
