package parser

import (
	"errors"
	"net/netip"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

const (
	maxNameLen  = 253
	maxLabelLen = 63
)

// Domain validation errors.
var (
	ErrEmptyName   = errors.New("parser: empty name")
	ErrNameTooLong = errors.New("parser: name longer than 253 bytes")
	ErrBadLabel    = errors.New("parser: empty label or label longer than 63 bytes")
	ErrBadChar     = errors.New("parser: name contains a character outside [a-z0-9_-.]")
	ErrNumericTLD  = errors.New("parser: all-numeric top-level label")
	ErrIDNA        = errors.New("parser: invalid internationalised name")
)

// idnaProfile maps Unicode names for lookup (UTS #46, non-transitional) but,
// unlike idna.Lookup, accepts '_' because real lists contain it.
var idnaProfile = idna.New(idna.MapForLookup(), idna.Transitional(false), idna.StrictDomainName(false))

// NormalizeDomain returns the canonical form of a list or query name: one
// trailing dot removed, IDNA A-labels for non-ASCII input, lower-case ASCII.
// It rejects names that cannot be DNS names.
func NormalizeDomain(s string) (string, error) {
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return "", ErrEmptyName
	}
	if !isASCII(s) {
		a, err := idnaProfile.ToASCII(s)
		if err != nil || a == "" {
			return "", ErrIDNA
		}
		s = a
	}
	s = LowerASCII(s)
	if err := validateDomain(s); err != nil {
		return "", err
	}
	return s, nil
}

func validateDomain(s string) error {
	if len(s) > maxNameLen {
		return ErrNameTooLong
	}
	label, lastStart := 0, 0
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '.':
			if label == 0 {
				return ErrBadLabel
			}
			label, lastStart = 0, i+1
			continue
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return ErrBadChar
		}
		label++
		if label > maxLabelLen {
			return ErrBadLabel
		}
	}
	if label == 0 {
		return ErrBadLabel
	}
	if isDigits(s[lastStart:]) {
		return ErrNumericTLD
	}
	return nil
}

// LowerASCII lower-cases ASCII letters, returning s itself (no allocation)
// when it has none.
func LowerASCII(s string) string {
	i := 0
	for ; i < len(s); i++ {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			break
		}
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if c := b[i]; 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// mightBeIP is a cheap filter in front of netip.ParseAddr, which allocates
// its error for the many fields that are host names.
func mightBeIP(s string) bool {
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i] // IPv6 zone
	}
	if len(s) < 2 {
		return false
	}
	sep := false
	for i := range len(s) {
		c := s[i]
		switch {
		case c == '.' || c == ':':
			sep = true
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
		default:
			return false
		}
	}
	return sep
}

// parseIP parses an address, accepting an IPv6 zone ("fe80::1%lo0") as the
// hosts files of several operating systems contain one.
func parseIP(s string) (netip.Addr, bool) {
	if !mightBeIP(s) {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a, true
}

// isSinkIP reports whether a hosts line pointing at a means "block": the
// unspecified addresses and loopback, as the specification describes.
func isSinkIP(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsUnspecified() || a.IsLoopback()
}

// hostsBoilerplate are names every OS hosts file carries; blocking them
// would break local name resolution, so they are counted as ignored.
var hostsBoilerplate = map[string]bool{
	"localhost":               true,
	"localhost.localdomain":   true,
	"localhost4":              true,
	"localhost4.localdomain4": true,
	"localhost6":              true,
	"localhost6.localdomain6": true,
	"local":                   true,
	"broadcasthost":           true,
	"ip6-localhost":           true,
	"ip6-loopback":            true,
	"ip6-localnet":            true,
	"ip6-mcastprefix":         true,
	"ip6-allnodes":            true,
	"ip6-allrouters":          true,
	"ip6-allhosts":            true,
}
