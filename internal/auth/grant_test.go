package auth

import (
	"context"
	"reflect"
	"testing"
)

func TestGrantRoundTrip(t *testing.T) {
	cases := []struct {
		in   Grant
		wire string
		back Grant
	}{
		{Grant{Role: "admin"}, "admin", Grant{Role: "admin", ScopeKind: ScopeCluster}},
		{Grant{Role: "viewer", ScopeKind: ScopeCluster}, "viewer", Grant{Role: "viewer", ScopeKind: ScopeCluster}},
		{Grant{Role: "zone-admin", ScopeKind: "zone", ScopeName: "home-example-com"}, "zone-admin@zone:home-example-com", Grant{Role: "zone-admin", ScopeKind: "zone", ScopeName: "home-example-com"}},
	}
	for _, c := range cases {
		if got := c.in.String(); got != c.wire {
			t.Errorf("%+v.String() = %q, want %q", c.in, got, c.wire)
		}
		back, err := ParseGrant(c.wire)
		if err != nil {
			t.Fatalf("ParseGrant(%q): %v", c.wire, err)
		}
		if back != c.back {
			t.Errorf("ParseGrant(%q) = %+v, want %+v", c.wire, back, c.back)
		}
	}
}

func TestParseGrantRejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "@zone:x", "admin@", "admin@zone", "admin@zone:", "admin@:x", "admin@cluster:x"} {
		if _, err := ParseGrant(s); err == nil {
			t.Errorf("ParseGrant(%q) succeeded, want error", s)
		}
	}
	if _, err := ParseGrants([]string{"admin", "bad@"}); err == nil {
		t.Error("ParseGrants with one malformed entry succeeded")
	}
}

func TestGrantStringsSortsAndDedupes(t *testing.T) {
	got := GrantStrings([]Grant{
		{Role: "viewer"},
		{Role: "zone-admin", ScopeKind: "zone", ScopeName: "b"},
		{Role: "admin", ScopeKind: ScopeCluster},
		{Role: "viewer", ScopeKind: ScopeCluster},
	})
	want := []string{"admin", "viewer", "zone-admin@zone:b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GrantStrings = %v, want %v", got, want)
	}
	if GrantStrings(nil) != nil {
		t.Error("GrantStrings(nil) should be nil")
	}
}

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	if _, ok := PrincipalFrom(ctx); ok {
		t.Fatal("empty context has a principal")
	}
	if _, ok := SessionFrom(ctx); ok {
		t.Fatal("empty context has a session")
	}
	p := Principal{Kind: KindUser, ID: "u1", Name: "alice"}
	s := Session{ID: "s1", Principal: p}
	ctx = WithSession(WithPrincipal(ctx, p), s)
	if got, ok := PrincipalFrom(ctx); !ok || got.ID != "u1" {
		t.Errorf("PrincipalFrom = %+v, %v", got, ok)
	}
	if got, ok := SessionFrom(ctx); !ok || got.ID != "s1" {
		t.Errorf("SessionFrom = %+v, %v", got, ok)
	}
}
