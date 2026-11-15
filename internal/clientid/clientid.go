// Package clientid parses bounded client-selected DNS policy identifiers.
// An identifier is never authentication or authorization.
package clientid

import "strings"

// Normalize accepts one ASCII DNS label and returns its lowercase form.
func Normalize(s string) (string, bool) {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return "", false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return "", false
		}
	}
	return strings.ToLower(s), true
}

// Domain accepts the explicit SNI suffix used by one listener.
func Domain(s string) bool {
	s = strings.TrimSuffix(s, ".")
	if s == "" || len(s) > 189 {
		return false
	} // Leave room for the ID label.
	for _, part := range strings.Split(s, ".") {
		if _, ok := Normalize(part); !ok {
			return false
		}
	}
	return true
}

// FromSNI requires exactly one ID label before a configured suffix.
func FromSNI(sni, domain string) string {
	if domain == "" {
		return ""
	}
	suffix := "." + strings.ToLower(strings.TrimSuffix(domain, "."))
	name := strings.ToLower(strings.TrimSuffix(sni, "."))
	if !strings.HasSuffix(name, suffix) {
		return ""
	}
	id, _ := Normalize(strings.TrimSuffix(name, suffix))
	return id
}

// FromPath matches the base endpoint or an enabled single ID path component.
// The caller rejects escaped paths before calling it.
func FromPath(path, base string, enabled bool) (string, bool) {
	if path == base {
		return "", true
	}
	if !enabled || !strings.HasPrefix(path, base+"/") {
		return "", false
	}
	return Normalize(strings.TrimPrefix(path, base+"/"))
}
