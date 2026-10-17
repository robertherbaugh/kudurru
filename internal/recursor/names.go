package recursor

import (
	"bytes"
	"strings"

	"github.com/miekg/dns"
)

// canon returns the lower-case, fully qualified form of name.
func canon(name string) string { return dns.CanonicalName(name) }

// equalName compares two names case-insensitively.
func equalName(a, b string) bool { return strings.EqualFold(dns.Fqdn(a), dns.Fqdn(b)) }

// isSubOrEqual reports whether child is parent or below it, on label
// boundaries and case-insensitively.
func isSubOrEqual(parent, child string) bool { return dns.IsSubDomain(parent, child) }

// isBelow reports whether child is strictly below parent.
func isBelow(parent, child string) bool {
	return !equalName(parent, child) && dns.IsSubDomain(parent, child)
}

// parentName returns name without its first label; the parent of the root
// is the root.
func parentName(name string) string {
	if name == "." || name == "" {
		return "."
	}
	off, end := dns.NextLabel(name, 0)
	if end || off >= len(name) {
		return "."
	}
	return name[off:]
}

// labels counts the labels of name (the root has none).
func labels(name string) int { return dns.CountLabel(name) }

// suffixLabels returns the last n labels of name ("." for n == 0).
func suffixLabels(name string, n int) string {
	if n <= 0 {
		return "."
	}
	idx := dns.Split(name)
	if n >= len(idx) {
		return name
	}
	return name[idx[len(idx)-n]:]
}

// childToward returns the name one label below ancestor on the way to name;
// ancestor must be an ancestor of name.
func childToward(ancestor, name string) string {
	return suffixLabels(name, labels(ancestor)+1)
}

// commonAncestor returns the longest name both a and b are equal to or
// below.
func commonAncestor(a, b string) string {
	return suffixLabels(a, dns.CompareDomainName(a, b))
}

// wireLabels returns the labels of name in wire form, lower-cased, from
// the leftmost; nil for the root or a name that does not pack.
func wireLabels(name string) [][]byte {
	buf := make([]byte, 256)
	n, err := dns.PackDomainName(dns.Fqdn(name), buf, 0, nil, false)
	if err != nil {
		return nil
	}
	buf = buf[:n]
	var out [][]byte
	for i := 0; i < len(buf); {
		l := int(buf[i])
		if l == 0 || i+1+l > len(buf) {
			break
		}
		out = append(out, bytes.ToLower(buf[i+1:i+1+l]))
		i += 1 + l
	}
	return out
}

// canonicalCompare orders names as RFC 4034 §6.1 does: label by label from
// the right, each compared as lower-case octets, a missing label first.
func canonicalCompare(a, b string) int {
	la, lb := wireLabels(a), wireLabels(b)
	i, j := len(la)-1, len(lb)-1
	for i >= 0 && j >= 0 {
		if c := bytes.Compare(la[i], lb[j]); c != 0 {
			return c
		}
		i--
		j--
	}
	switch {
	case i < 0 && j < 0:
		return 0
	case i < 0:
		return -1
	}
	return 1
}

// randomizeCase flips the case of the letters of name at random (DNS 0x20,
// draft-vixie-dnsext-dns0x20): a spoofed answer must also guess the case.
func randomizeCase(name string, rnd func() uint64) string {
	b := []byte(name)
	var bits uint64
	n := 0
	for i, c := range b {
		lc := c | 0x20
		if lc < 'a' || lc > 'z' {
			continue
		}
		if n == 0 {
			bits, n = rnd(), 64
		}
		if bits&1 == 1 {
			b[i] = c ^ 0x20
		}
		bits >>= 1
		n--
	}
	return string(b)
}
