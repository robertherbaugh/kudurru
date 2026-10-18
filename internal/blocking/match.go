package blocking

import (
	"net/netip"

	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// NormalizeName returns the form of a query name that lists are matched
// against: one trailing dot removed and ASCII lower-cased. It does not
// allocate when the name is already lower-case.
func NormalizeName(qname string) string {
	if n := len(qname); n > 0 && qname[n-1] == '.' {
		qname = qname[:n-1]
	}
	return parser.LowerASCII(qname)
}

// Match is one matched entry. Pattern is the stored entry: the name itself
// for exact entries, the matched base (a suffix of the name) for suffix
// entries, the expression for regex entries and the address for IP
// entries. Rendering the rule text is deferred to Rule so lookups do not
// allocate.
type Match struct {
	Kind      parser.Kind
	Syntax    parser.Syntax
	Pattern   string
	Exception bool
}

// Rule renders the matched rule as list text, e.g. "||ads.example^" or
// "@@*.cdn.example"; it is empty for the zero Match.
func (m Match) Rule() string {
	if m.Kind == 0 {
		return ""
	}
	return m.Syntax.Render(m.Pattern, m.Exception)
}

// chain is one cheapest-first matcher chain.
type chain struct {
	exact     *matcher.Exact
	suffix    *matcher.Suffix
	regex     *matcher.Regex
	ips       *matcher.IPSet
	exception bool
}

// match checks a normalised name with h == matcher.Hash(name).
func (c *chain) match(name string, h uint64) (Match, bool) {
	if tag, ok := c.exact.MatchHash(name, h); ok {
		return Match{Kind: parser.KindExact, Syntax: parser.Syntax(tag), Pattern: name, Exception: c.exception}, true
	}
	if base, tag, ok := c.suffix.MatchHash(name, h); ok {
		return Match{Kind: parser.KindSuffix, Syntax: parser.Syntax(tag), Pattern: base, Exception: c.exception}, true
	}
	if src, tag, ok := c.regex.Match(name); ok {
		return Match{Kind: parser.KindRegex, Syntax: parser.Syntax(tag), Pattern: src, Exception: c.exception}, true
	}
	return Match{}, false
}

func (c *chain) matchIP(a netip.Addr) (Match, bool) {
	tag, ok := c.ips.Match(a)
	if !ok {
		return Match{}, false
	}
	return Match{Kind: parser.KindIP, Syntax: parser.Syntax(tag), Pattern: a.Unmap().WithZone("").String(), Exception: c.exception}, true
}

func (c *chain) entries() int {
	return c.exact.Len() + c.suffix.Len() + c.regex.Len() + c.ips.Len()
}

func (c *chain) bytes() int {
	return c.exact.Bytes() + c.suffix.Bytes() + c.regex.Bytes() + c.ips.Bytes()
}
