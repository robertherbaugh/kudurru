package parser

import (
	"errors"
	"strings"
)

// ErrMalformedModifiers is returned by ParseModifiers for an empty modifier,
// an unterminated quote or a name with characters outside [a-z0-9_-].
var ErrMalformedModifiers = errors.New("parser: malformed modifiers")

// dnsModifiers are the modifiers the DNS filtering specification defines.
// Rules carrying any other modifier must be ignored as a whole.
var dnsModifiers = map[string]bool{
	"important":  true,
	"badfilter":  true,
	"dnstype":    true,
	"dnsrewrite": true,
	"client":     true,
	"ctag":       true,
	"denyallow":  true,
	"respgeo":    true,
}

// KnownModifier reports whether name (lower-case) is a DNS modifier defined
// by the specification.
func KnownModifier(name string) bool {
	return dnsModifiers[name]
}

// ParseModifiers splits the text after '$' into modifiers. Commas separate
// modifiers unless escaped with '\' or inside a quoted value element; a
// quote opens only at the start of a value element (after '=', '|' or '~'),
// so an apostrophe inside a $dnsrewrite TXT value is literal.
func ParseModifiers(s string) ([]Modifier, error) {
	if s == "" {
		return nil, ErrMalformedModifiers
	}
	var out []Modifier
	start := 0
	var quote byte
	var prev byte
	for i := 0; i <= len(s); i++ {
		if i < len(s) {
			c := s[i]
			switch {
			case c == '\\':
				i++
				prev = 0
				continue
			case quote != 0:
				if c == quote {
					quote = 0
				}
				prev = c
				continue
			case (c == '\'' || c == '"') && (prev == '=' || prev == '|' || prev == '~'):
				quote = c
				prev = c
				continue
			case c != ',':
				prev = c
				continue
			}
		}
		m, err := parseModifier(s[start:i])
		if err != nil {
			return nil, err
		}
		out = append(out, m)
		start, prev = i+1, 0
	}
	if quote != 0 {
		return nil, ErrMalformedModifiers
	}
	return out, nil
}

func parseModifier(tok string) (Modifier, error) {
	name, value, _ := strings.Cut(tok, "=")
	name = LowerASCII(name)
	check := strings.TrimPrefix(name, "~") // negated browser modifiers: unknown, not malformed
	if check == "" {
		return Modifier{}, ErrMalformedModifiers
	}
	for i := range len(check) {
		c := check[i]
		if ('a' > c || c > 'z') && ('0' > c || c > '9') && c != '-' && c != '_' {
			return Modifier{}, ErrMalformedModifiers
		}
	}
	return Modifier{Name: name, Value: value}, nil
}
