package oidc

import (
	"net/url"
	"strings"
)

const maxNextLen = 2048

// SafeNext returns next if it is a same-origin absolute path and "/"
// otherwise, so the post-login redirect can never leave kudurru. Rejected:
// anything with a scheme or host, protocol-relative "//host" and the
// "/\host" form browsers normalise to it, control characters, and values
// longer than 2 KiB.
func SafeNext(next string) string {
	if next == "" || len(next) > maxNextLen || next[0] != '/' {
		return "/"
	}
	if len(next) > 1 && (next[1] == '/' || next[1] == '\\') {
		return "/"
	}
	if strings.ContainsFunc(next, func(r rune) bool { return r < 0x20 || r == 0x7f || r == '\\' }) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	return next
}
