package browseroidc

import (
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

func safeText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) || c == '\\' {
			return false
		}
	}
	return true
}

// SafeNext admits a bounded canonical same-origin absolute path. Empty means
// root. Encoded separators, nested escapes, dot segments, and fragments deny.
func SafeNext(raw string) (string, error) {
	if len(raw) > MaxNextBytes {
		return "", ErrBounds
	}
	if raw == "" {
		return "/", nil
	}
	if !safeText(raw) || strings.Contains(raw, "#") || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "", ErrInvalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || !safeText(u.Path) {
		return "", ErrInvalid
	}
	escaped := strings.ToLower(u.EscapedPath())
	for _, bad := range []string{"%2f", "%5c", "%25"} {
		if strings.Contains(escaped, bad) {
			return "", ErrInvalid
		}
	}
	clean := path.Clean(u.Path)
	if u.Path != clean && u.Path != clean+"/" {
		return "", ErrInvalid
	}
	query, err := url.QueryUnescape(u.RawQuery)
	if err != nil || !safeText(query) {
		return "", ErrInvalid
	}
	return raw, nil
}
