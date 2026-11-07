package blocking

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

// Context is immutable query/identity input. Names/tags must come from the
// operator's published inventory, never inferred from a group or EDNS option.
type Context struct {
	Name              string
	Type, Class       uint16
	ClientIP          netip.Addr
	ClientID          string
	ClientNames, Tags []string
}

type selector struct {
	value    string
	prefix   netip.Prefix
	negative bool
}
type compiledRule struct {
	text, key                       string
	pattern                         *matcher.Pattern
	important, exception, badfilter bool
	types, notTypes                 map[uint16]bool
	clients, tags                   []selector
	denyallow                       []string
	addr                            netip.Addr
	rewrite                         *Rewrite
	specificity, modifiers, order   int
}

// Rewrite is a parsed synthetic response plan. Its records are private;
// materialization always returns copies so plans can be shared by snapshots.
type Rewrite struct {
	rcode   int
	records []dns.RR
	all     bool
}

// Rcode returns the configured DNS response code.
func (r *Rewrite) Rcode() int {
	if r == nil {
		return dns.RcodeSuccess
	}
	return r.rcode
}

// Target returns the CNAME target, or an empty string for other rewrites.
func (r *Rewrite) Target() string {
	if r != nil && len(r.records) > 0 {
		if c, ok := r.records[0].(*dns.CNAME); ok {
			return c.Target
		}
	}
	return ""
}

// Records materializes private records of the question's type plus CNAMEs.
func (r *Rewrite) Records(name string, typ, class uint16, ttl uint32) []dns.RR {
	if r == nil {
		return nil
	}
	var out []dns.RR
	for _, rr := range r.records {
		if rr.Header().Rrtype != typ && rr.Header().Rrtype != dns.TypeCNAME {
			continue
		}
		c := dns.Copy(rr)
		c.Header().Name = name
		c.Header().Class = class
		c.Header().Ttl = ttl
		out = append(out, c)
	}
	return out
}
func (r *Rewrite) identity() string {
	if r == nil {
		return ""
	}
	var texts []string
	for _, rr := range r.records {
		texts = append(texts, rr.String())
	}
	b, _ := json.Marshal(struct {
		Rcode   int
		All     bool
		Records []string
	}{r.rcode, r.all, texts})
	return string(b)
}

func invalidModifier(detail string) error { return fmt.Errorf("%w: %s", parser.ErrInvalidSlow, detail) }
func compileRule(u parser.UnsupportedRule) (*compiledRule, error) {
	switch u.Reason {
	case parser.ReasonModifiers, parser.ReasonWildcard, parser.ReasonUnanchored, parser.ReasonPrefix:
	default:
		return nil, parser.ErrUnsupportedSlow
	}
	pattern, err := normalizeSlowPattern(u.Pattern)
	if err != nil {
		return nil, invalidModifier("pattern: " + err.Error())
	}
	p, err := matcher.CompilePattern(pattern)
	if err != nil {
		return nil, invalidModifier("pattern: " + err.Error())
	}
	r := &compiledRule{text: strings.Clone(u.Text), pattern: p, exception: u.Exception, modifiers: len(u.Modifiers), specificity: len(strings.NewReplacer("*", "", "^", "", "|", "").Replace(u.Pattern))}
	body := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(pattern, "||"), "|"), "|"), "^")
	r.addr, _ = netip.ParseAddr(body)
	seen := map[string]bool{}
	var tokens []string
	for _, m := range u.Modifiers {
		if seen[m.Name] {
			return nil, invalidModifier("duplicate " + m.Name)
		}
		seen[m.Name] = true
		switch m.Name {
		case "important", "badfilter":
			if m.Value != "" {
				return nil, invalidModifier(m.Name + " takes no value")
			}
			if m.Name == "important" {
				r.important = true
			} else {
				r.badfilter = true
			}
		case "dnstype":
			values := m.Values()
			if len(values) == 0 {
				return nil, invalidModifier("dnstype requires RR types")
			}
			r.types = map[uint16]bool{}
			r.notTypes = map[uint16]bool{}
			for _, v := range values {
				negative := strings.HasPrefix(v, "~")
				v = strings.ToUpper(strings.TrimPrefix(v, "~"))
				typ, ok := dns.StringToType[v]
				if !ok && strings.HasPrefix(v, "TYPE") {
					n, e := strconv.ParseUint(v[4:], 10, 16)
					typ, ok = uint16(n), e == nil && n > 0
				}
				if !ok {
					return nil, invalidModifier("unknown dnstype " + v)
				}
				if negative {
					r.notTypes[typ] = true
				} else {
					r.types[typ] = true
				}
			}
			// Public grammar: positive types dominate a mixed include/exclude set.
			if len(r.types) > 0 {
				r.notTypes = nil
			}
		case "client", "ctag":
			values := m.Values()
			if len(values) == 0 {
				return nil, invalidModifier(m.Name + " requires selectors")
			}
			for _, v := range values {
				s := selector{negative: strings.HasPrefix(v, "~"), value: strings.Clone(strings.TrimPrefix(v, "~"))}
				if s.value == "" {
					return nil, invalidModifier("empty " + m.Name + " selector")
				}
				if m.Name == "client" {
					if a, e := netip.ParseAddr(s.value); e == nil {
						a = a.Unmap()
						s.prefix = netip.PrefixFrom(a, a.BitLen())
					} else if p, e := netip.ParsePrefix(s.value); e == nil {
						s.prefix = p.Masked()
					} else if strings.Contains(s.value, "/") {
						return nil, invalidModifier("invalid client prefix")
					}
					r.clients = append(r.clients, s)
				} else {
					if !knownTag(s.value) {
						return nil, invalidModifier("unknown ctag " + s.value)
					}
					r.tags = append(r.tags, s)
				}
			}
		case "denyallow":
			values := m.Values()
			if len(values) == 0 {
				return nil, invalidModifier("denyallow requires domains")
			}
			for _, v := range values {
				d, e := parser.NormalizeDomain(v)
				if e != nil {
					return nil, invalidModifier("invalid denyallow domain")
				}
				r.denyallow = append(r.denyallow, strings.Clone(d))
			}
		case "dnsrewrite":
			r.rewrite, err = parseRewrite(m.Value, u.Exception)
			if err != nil {
				return nil, invalidModifier(err.Error())
			}
		case "respgeo":
			return nil, parser.ErrUnsupportedSlow
		default:
			return nil, invalidModifier("unknown " + m.Name)
		}
		if m.Name != "badfilter" {
			tokens = append(tokens, m.Name+"="+strings.Clone(m.Value))
		}
	}
	slices.Sort(tokens)
	pattern = strings.Clone(pattern)
	// Match the normalized rendering of fast rules, including the documented
	// implicit terminator on ||domain rules. Regex/pattern syntax stays literal.
	if strings.HasPrefix(pattern, "||") && !strings.ContainsAny(pattern[2:], "*^|") {
		pattern += "^"
	}
	if r.addr.IsValid() {
		pattern = r.addr.Unmap().String()
	}
	r.key = pattern + "$" + strings.Join(tokens, ",")
	if r.exception {
		r.key = "@@" + r.key
	}
	return r, nil
}

// Simple domain patterns use the same IDNA/case/trailing-dot normalization as
// fast entries. Keep regexes and general wildcard syntax intact.
func normalizeSlowPattern(pattern string) (string, error) {
	if pattern == "" || strings.HasPrefix(pattern, "/") {
		return pattern, nil
	}
	body, prefix, suffix := pattern, "", ""
	for _, p := range []string{"||", "|", "*."} {
		if strings.HasPrefix(body, p) {
			prefix, body = p, body[len(p):]
			break
		}
	}
	for _, p := range []string{"^|", "^", "|"} {
		if strings.HasSuffix(body, p) {
			body, suffix = body[:len(body)-len(p)], p
			break
		}
	}
	if prefix == "||" && strings.HasPrefix(body, "*.") {
		prefix, body = "||*.", body[2:]
	}
	if strings.ContainsAny(body, "*^|") || body == "" {
		return pattern, nil
	}
	if a, err := netip.ParseAddr(body); err == nil {
		return prefix + a.Unmap().String() + suffix, nil
	}
	name, err := parser.NormalizeDomain(body)
	// Fast parser renderings canonicalize equivalent closing terminators.
	// Use that same identity for cancellation of a fast rule in another list.
	switch prefix {
	case "||", "||*.":
		suffix = "^"
	case "|":
		if suffix != "" {
			suffix = "^"
		}
	case "*.":
		suffix = ""
	}
	return prefix + name + suffix, err
}
func knownTag(tag string) bool {
	switch tag {
	case "device_audio", "device_camera", "device_gameconsole", "device_laptop", "device_nas", "device_pc", "device_phone", "device_printer", "device_securityalarm", "device_tablet", "device_tv", "device_other", "os_android", "os_ios", "os_linux", "os_macos", "os_windows", "os_other", "user_admin", "user_regular", "user_child":
		return true
	}
	return false
}
func (r *compiledRule) matches(ctx Context) bool {
	if r.badfilter {
		return false
	}
	if len(r.types) > 0 && !r.types[ctx.Type] || r.notTypes[ctx.Type] {
		return false
	}
	for _, d := range r.denyallow {
		if ctx.Name == d || strings.HasSuffix(ctx.Name, "."+d) {
			return false
		}
	}
	if !matchesSelectors(r.clients, func(s selector) bool {
		if s.prefix.IsValid() {
			return s.prefix.Contains(ctx.ClientIP.Unmap())
		}
		return s.value == ctx.ClientID || slices.Contains(ctx.ClientNames, s.value)
	}) {
		return false
	}
	return matchesSelectors(r.tags, func(s selector) bool { return slices.Contains(ctx.Tags, s.value) }) &&
		(r.pattern.Literal() == "" || strings.Contains(ctx.Name, r.pattern.Literal())) && r.pattern.Match(ctx.Name)
}
func matchesSelectors(ss []selector, match func(selector) bool) bool {
	positives, matched := false, false
	for _, s := range ss {
		yes := match(s)
		if s.negative && yes {
			return false
		}
		if !s.negative {
			positives = true
			matched = matched || yes
		}
	}
	return !positives || matched
}
func parseRewrite(value string, exception bool) (*Rewrite, error) {
	// The modifier grammar escapes its comma separator independently of DNS
	// RDATA escapes, which must otherwise reach the RR parser unchanged.
	value = strings.ReplaceAll(value, `\,`, ",")
	if value == "" {
		if exception {
			return &Rewrite{all: true}, nil
		}
		return nil, fmt.Errorf("dnsrewrite needs a response")
	}
	r := &Rewrite{}
	if a, e := netip.ParseAddr(value); e == nil {
		typ := "A"
		if a.Is6() {
			typ = "AAAA"
		}
		rr, e := dns.NewRR(". 0 IN " + typ + " " + a.String())
		if e != nil {
			return nil, e
		}
		r.records = []dns.RR{rr}
		return r, nil
	}
	fields := strings.SplitN(value, ";", 3)
	if len(fields) == 1 {
		if code, ok := dns.StringToRcode[value]; ok {
			if code != dns.RcodeSuccess && code != dns.RcodeNameError && code != dns.RcodeRefused && code != dns.RcodeServerFailure {
				return nil, fmt.Errorf("unsupported rewrite rcode")
			}
			r.rcode = code
			return r, nil
		}
		name, e := parser.NormalizeDomain(value)
		if e != nil {
			return nil, fmt.Errorf("invalid rewrite target")
		}
		rr, e := dns.NewRR(". 0 IN CNAME " + dns.Fqdn(name))
		if e != nil {
			return nil, e
		}
		r.records = []dns.RR{rr}
		return r, nil
	}
	if len(fields) != 3 {
		return nil, fmt.Errorf("dnsrewrite requires RCODE;TYPE;VALUE")
	}
	code, ok := dns.StringToRcode[fields[0]]
	if !ok || code != dns.RcodeSuccess && code != dns.RcodeNameError && code != dns.RcodeRefused && code != dns.RcodeServerFailure {
		return nil, fmt.Errorf("invalid rewrite rcode")
	}
	r.rcode = code
	if fields[1] == "" && fields[2] == "" {
		return r, nil
	}
	if code != dns.RcodeSuccess {
		return nil, fmt.Errorf("error rewrite cannot contain records")
	}
	switch fields[1] {
	case "A", "AAAA", "CNAME", "PTR", "TXT", "MX", "SRV", "HTTPS", "SVCB":
	default:
		return nil, fmt.Errorf("unsupported rewrite RR type")
	}
	if fields[2] == "" || strings.ContainsAny(fields[2], "\r\n") {
		return nil, fmt.Errorf("invalid rewrite record")
	}
	recordValue := fields[2]
	if fields[1] == "TXT" {
		if !strings.HasPrefix(recordValue, "\"") || !strings.HasSuffix(recordValue, "\"") {
			recordValue = strconv.Quote(recordValue)
		}
	}
	rr, e := dns.NewRR(". 0 IN " + fields[1] + " " + recordValue)
	if e != nil {
		return nil, fmt.Errorf("invalid rewrite record: %w", e)
	}
	switch v := rr.(type) {
	case *dns.CNAME:
		v.Target, e = rewriteName(v.Target, false)
	case *dns.PTR:
		v.Ptr, e = rewriteName(v.Ptr, true)
	case *dns.MX:
		v.Mx, e = rewriteName(v.Mx, true)
	case *dns.SRV:
		v.Target, e = rewriteName(v.Target, true)
	case *dns.SVCB:
		v.Target, e = rewriteName(v.Target, true)
	case *dns.HTTPS:
		v.Target, e = rewriteName(v.Target, true)
	}
	if e != nil {
		return nil, e
	}
	r.records = []dns.RR{rr}
	return r, nil
}
func rewriteName(name string, root bool) (string, error) {
	if name == "." && root {
		return ".", nil
	}
	n, err := parser.NormalizeDomain(name)
	if err != nil {
		return "", fmt.Errorf("invalid rewrite target: %w", err)
	}
	return dns.Fqdn(n), nil
}

// rank encodes the exception/important precedence; specificity breaks ties
// only within a rank. The outer allow-mode list remains highest policy wins.
func (r *compiledRule) rank() int {
	n := 0
	if r.important {
		n += 2
	}
	if r.exception {
		n++
	}
	return n
}
