package session

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// realisticSession is an OIDC user with a long issuer URL, an opaque
// subject and three grants: what a typical admin's cookie carries.
func realisticSession(st *Sealed) auth.Session {
	p := auth.Principal{
		Kind:     auth.KindUser,
		ID:       "oidc:corp-sso:https%3A%2F%2Fauth.example.com%2Fapplication%2Fo%2Fkudurru%2F:CiQwOGE4Njg0Yi1kYjg4LTRiNzMtOTBhOS0zY2QxNjYxZjU0NjYSBWxvY2Fs",
		Name:     "corp:alice.example",
		Provider: "corp-sso",
	}
	return st.NewSession(p, []string{"operator", "zone-admin@zone:home-example-com", "zone-admin@zone:lab-example-net"}, "corp-sso")
}

// groups returns n IdP group names of a realistic length (~30 bytes).
func groups(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("corp:/departments/team-%03d-eng", i)
	}
	return out
}

// TestCookieSizeWith200Groups records the number the spike gate asks for:
// a user in 200 IdP groups gets a cookie whose size depends only on the
// resolved grants (D48). The counterfactual seals the raw group list the
// way draft v1 would have, to show why D48 exists.
func TestCookieSizeWith200Groups(t *testing.T) {
	st := newTestStore(t, newFakeClock(), nil)
	s := realisticSession(st)
	s.Principal.Groups = groups(200) // present at login, dropped by the store
	v, err := st.Issue(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	size := len(DefaultCookieName) + 1 + len(v)
	if size > MaxCookieBytes {
		t.Fatalf("cookie for a 200-group user is %d bytes", size)
	}

	type withGroups struct {
		sessionV1
		Groups []string `json:"groups"`
	}
	pt, _ := json.Marshal(withGroups{Groups: groups(200), sessionV1: sessionV1{V: 1, SID: s.ID, CSRF: s.CSRF, RolesSnapshot: s.RolesSnapshot}})
	counterfactual := len(DefaultCookieName) + 1 + SealedLen(len(pt))
	t.Logf("200-group user: roles_snapshot cookie = %d bytes (limit %d); raw-groups cookie would be %d bytes", size, MaxCookieBytes, counterfactual)
	if counterfactual <= MaxCookieBytes {
		t.Errorf("counterfactual unexpectedly fits (%d bytes); the D48 rationale needs revisiting", counterfactual)
	}
}

func BenchmarkSeal(b *testing.B) {
	st, _ := NewSealed(Options{Key: testKey(1)})
	s := realisticSession(st)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := st.Issue(context.Background(), s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpen(b *testing.B) {
	st, _ := NewSealed(Options{Key: testKey(1)})
	v, _ := st.Issue(context.Background(), realisticSession(st))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := st.Load(context.Background(), v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenPreviousKey(b *testing.B) {
	old, _ := NewSealed(Options{Key: testKey(1)})
	v, _ := old.Issue(context.Background(), realisticSession(old))
	st, _ := NewSealed(Options{Key: testKey(2), PreviousKey: testKey(1)})
	b.ReportAllocs()
	for b.Loop() {
		if _, err := st.Load(context.Background(), v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTouchReseal(b *testing.B) {
	clk := newFakeClock()
	st, _ := NewSealed(Options{Key: testKey(1), Now: clk.Now})
	s := realisticSession(st)
	b.ReportAllocs()
	for b.Loop() {
		// Every iteration crosses the touch interval, so this is the
		// worst case: a full re-seal.
		clk.Advance(time.Minute)
		s.IdleExpiresAt = clk.Now().Add(time.Minute)
		if _, _, err := st.Touch(context.Background(), &s); err != nil {
			b.Fatal(err)
		}
		if s.ExpiresAt.Sub(clk.Now()) < time.Hour {
			s.ExpiresAt = clk.Now().Add(DefaultLifetime)
		}
	}
}
