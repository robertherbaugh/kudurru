package audit

import (
	"encoding/json"
	"testing"
	"unicode/utf8"
)

func TestSanitize(t *testing.T) {
	ev := Event{
		Action: "zones.write", Actor: Actor{Name: "al\xffice", Groups: []string{"ok", "b\x00d"}},
		Resource: Resource{Kind: "DNSZone", Name: "ex\x00ample"},
		Request:  Request{Path: "/api/v1/zones/a\x00b", UA: "curl/\xff\xfe", IP: "10.0.0.1"},
		After:    json.RawMessage(`{"a":"\u0000 escaped is fine"}`),
	}
	got := Sanitize(ev)
	for _, s := range []string{got.Actor.Name, got.Actor.Groups[1], got.Resource.Name, got.Request.Path, got.Request.UA} {
		if !utf8.ValidString(s) || dirty(s) {
			t.Fatalf("%q still dirty", s)
		}
	}
	if got.Request.Path != "/api/v1/zones/a�b" || got.Request.UA != "curl/�" || got.Actor.Groups[0] != "ok" {
		t.Fatalf("sanitized %+v", got)
	}
	if ev.Actor.Groups[1] != "b\x00d" {
		t.Fatal("Sanitize changed the caller's groups")
	}
	if string(got.After) != string(ev.After) {
		t.Fatalf("a valid document changed: %s", got.After)
	}
	// Idempotent: a stored event hashes the same when sanitized again.
	again := Sanitize(got)
	if ChainHash(again, "p") != ChainHash(got, "p") {
		t.Fatal("Sanitize is not idempotent")
	}
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"aé", 2, "a"}, // é is two bytes: not split
		{"日本", 4, "日"},
		{"\xff\xfe", 1, "\xff"},
	} {
		if got := Truncate(tc.s, tc.n); got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}
