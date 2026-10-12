package oidc

import (
	"strings"
	"testing"
)

func TestIdentityKeyRoundTrip(t *testing.T) {
	for _, k := range []IdentityKey{
		{"corp", "https://idp.example/realms/main", "248289761001"},
		{"dex", "http://127.0.0.1:5556/dex", "CiQwOGE4Njg0Yi1kYjg4LTRiNzMtOTBhOS0zY2QxNjYxZjU0NjYSBWxvY2Fs"},
		{"weird", "https://idp.example/a:b?c=d", "sub:with:colons and spaces/%"},
	} {
		s := k.String()
		back, err := ParseIdentityKey(s)
		if err != nil || back != k {
			t.Errorf("round trip %+v -> %q -> %+v, %v", k, s, back, err)
		}
	}
	for _, bad := range []string{"", "user:x", "oidc:a:b", "oidc:a:b:c:d", "oidc::b:c", "oidc:a:%zz:c"} {
		if _, err := ParseIdentityKey(bad); err == nil {
			t.Errorf("ParseIdentityKey(%q) succeeded", bad)
		}
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                              "/",
		"/":                             "/",
		"/zones":                        "/zones",
		"/zones?filter=a&b=c#frag":      "/zones?filter=a&b=c#frag",
		"/api/v1/auth/me":               "/api/v1/auth/me",
		"zones":                         "/",
		"//evil.example":                "/",
		"//evil.example/zones":          "/",
		`/\evil.example`:                "/",
		`/zones\..\x`:                   "/",
		"https://evil.example/":         "/",
		"javascript:alert(1)":           "/",
		"/\r\nSet-Cookie:x":             "/",
		"/zones\t":                      "/",
		"/%2F%2Fevil.example":           "/%2F%2Fevil.example", // stays a local path
		"/" + strings.Repeat("a", 3000): "/",
	}
	for in, want := range cases {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
