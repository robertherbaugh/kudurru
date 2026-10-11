package zone

import (
	"errors"
	"strings"

	"github.com/miekg/dns"
)

// Owner names are indexed by a canonical key: the labels of the name in
// reverse order (root side first), each label decoded from presentation
// format (\. and \DDD escapes), ASCII-lowercased and terminated by a 0x00
// byte. Bytes 0x00 and 0x01 inside a label are escaped as 0x01 0x01 and
// 0x01 0x02 so the encoding is injective and plain byte comparison of two
// keys yields DNSSEC canonical order (RFC 4034 §6.1): a label that is a
// prefix of another sorts first because the terminator is the smallest
// byte.
//
// Two properties make lookups cheap:
//
//   - the key of an ancestor is a byte prefix of the key of every
//     descendant, ending at a 0x00 terminator, so walking from the zone
//     apex to the query name needs one key and no further allocations;
//   - the root name has the empty key, which is a prefix of everything.
//
// The idea of comparing names label by label from the right comes from
// CoreDNS plugin/file/tree (less.go); precomputing the key once per name
// replaces its per-comparison lowercasing.

// errBadName is returned for names that are not valid presentation-format
// domain names.
var errBadName = errors.New("not a valid domain name")

const (
	keySep = 0x00
	keyEsc = 0x01
)

// maxLabels bounds the label count of a valid name (255 octets on the wire,
// at least two octets per label except the root).
const maxLabels = 128

// canonicalKey returns the canonical key for a presentation-format name.
// The name may or may not end with a dot; it is always taken as absolute.
func canonicalKey(name string) (string, error) {
	b, err := appendKey(make([]byte, 0, len(name)+2), name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// appendKey appends the canonical key of name to dst.
func appendKey(dst []byte, name string) ([]byte, error) {
	if name == "" || name == "." {
		return dst, nil
	}
	// Decode into a wire-like buffer: label bytes only, with the start
	// offset of every label recorded.
	var (
		buf    [256]byte
		starts [maxLabels + 1]uint16 // offsets into buf, at most 256
		n      uint16                // bytes in buf
		labels int
		ll     int // length of the current label
	)
	starts[0] = 0
	wire := 1 // root octet
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch c {
		case '\\':
			if i+1 >= len(name) {
				return dst, errBadName
			}
			if i+3 < len(name) && isDigit(name[i+1]) && isDigit(name[i+2]) && isDigit(name[i+3]) {
				v := int(name[i+1]-'0')*100 + int(name[i+2]-'0')*10 + int(name[i+3]-'0')
				if v > 255 {
					return dst, errBadName
				}
				c = byte(v) //nolint:gosec // G115: v <= 255 was checked above
				i += 3
			} else {
				i++
				c = name[i]
			}
		case '.':
			if ll == 0 {
				return dst, errBadName // empty label
			}
			labels++
			if labels > maxLabels {
				return dst, errBadName
			}
			starts[labels] = n
			wire += 1 + ll
			ll = 0
			continue
		}
		if int(n) >= len(buf) || ll >= 63 {
			return dst, errBadName
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		buf[n] = c
		n++
		ll++
	}
	if ll > 0 { // no trailing dot
		labels++
		if labels > maxLabels {
			return dst, errBadName
		}
		starts[labels] = n
		wire += 1 + ll
	}
	if wire > 255 {
		return dst, errBadName
	}
	for l := labels - 1; l >= 0; l-- {
		for _, c := range buf[starts[l]:starts[l+1]] {
			switch c {
			case keySep:
				dst = append(dst, keyEsc, 0x01)
			case keyEsc:
				dst = append(dst, keyEsc, 0x02)
			default:
				dst = append(dst, c)
			}
		}
		dst = append(dst, keySep)
	}
	return dst, nil
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// keyLabels returns the number of labels encoded in key.
func keyLabels(key string) int { return strings.Count(key, "\x00") }

// isKeyPrefix reports whether ancestor is the key of name or of one of its
// ancestors (that is, whether name is at or below ancestor).
func isKeyPrefix(name, ancestor string) bool { return strings.HasPrefix(name, ancestor) }

// fqdn lowercases name and makes it absolute. Escapes are kept as written;
// use canonicalName where the spelling matters.
func fqdn(name string) string {
	return dns.Fqdn(strings.ToLower(name))
}

// canonicalName returns the one presentation spelling of name: absolute,
// ASCII-lowercased (escaped letters included) and escaped exactly as
// miekg/dns prints names (\X for the label-special characters, \DDD for
// unprintable bytes). Two spellings of the same name map to the same
// string, so owner names stored in the tree and printed by ExportBIND are
// stable. Invalid names fall back to fqdn.
func canonicalName(name string) string {
	key, err := canonicalKey(name)
	if err != nil {
		return fqdn(name)
	}
	return nameFromKey(key)
}

// nameFromKey renders a canonical key as a presentation-format name.
func nameFromKey(key string) string {
	if key == "" {
		return "."
	}
	labels := strings.Split(key[:len(key)-1], "\x00")
	var b strings.Builder
	b.Grow(len(key) + 8)
	for i := len(labels) - 1; i >= 0; i-- {
		l := labels[i]
		for j := 0; j < len(l); j++ {
			c := l[j]
			if c == keyEsc && j+1 < len(l) {
				j++
				c = l[j] - 1 // 0x01 0x01 -> 0x00, 0x01 0x02 -> 0x01
			}
			switch {
			case c == '.' || c == ' ' || c == '\'' || c == '@' || c == ';' || c == '(' || c == ')' || c == '"' || c == '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			case c < ' ' || c > '~':
				b.WriteByte('\\')
				b.WriteByte('0' + c/100)
				b.WriteByte('0' + c/10%10)
				b.WriteByte('0' + c%10)
			default:
				b.WriteByte(c)
			}
		}
		b.WriteByte('.')
	}
	return b.String()
}

// replaceSuffix rewrites qname, which must be strictly below owner, so that
// the owner suffix is replaced by target (DNAME substitution, RFC 6672
// §2.2). It reports false when the result is not a valid name, the
// YXDOMAIN case.
func replaceSuffix(qname, owner, target string) (string, bool) {
	ql := dns.Split(qname)
	ol := dns.CountLabel(owner)
	if len(ql) <= ol {
		return "", false
	}
	prefix := qname
	if ol > 0 {
		prefix = qname[:ql[len(ql)-ol]]
	}
	out := prefix + target
	if target == "." {
		out = prefix
	}
	if _, ok := dns.IsDomainName(out); !ok {
		return "", false
	}
	if _, err := canonicalKey(out); err != nil {
		return "", false
	}
	return out, true
}
