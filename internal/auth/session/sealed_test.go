package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
)

// fakeClock is a mutex-guarded clock for expiry tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func testPrincipal() auth.Principal {
	return auth.Principal{
		Kind:     auth.KindUser,
		ID:       "oidc:corp:https%3A%2F%2Fidp.example:248289761001",
		Name:     "corp:alice",
		Groups:   []string{"corp:/dns-admins", "corp:/staff"},
		Provider: "corp",
	}
}

func newTestStore(t *testing.T, clk *fakeClock, mutate func(*Options)) *Sealed {
	t.Helper()
	o := Options{Key: testKey(1), Now: clk.Now}
	if mutate != nil {
		mutate(&o)
	}
	st, err := NewSealed(o)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestSealedIssueLoadRoundTrip(t *testing.T) {
	clk := newFakeClock()
	st := newTestStore(t, clk, nil)
	ctx := context.Background()

	s := st.NewSession(testPrincipal(), []string{"admin", "zone-admin@zone:home-example-com"}, "corp")
	v, err := st.Issue(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	want := s
	want.Principal.Groups = nil // D48: groups never sealed
	want.IssuedAt = want.IssuedAt.Truncate(time.Second)
	want.ExpiresAt = want.ExpiresAt.Truncate(time.Second)
	want.IdleExpiresAt = want.IdleExpiresAt.Truncate(time.Second)
	if !reflect.DeepEqual(normalize(got), normalize(want)) {
		t.Fatalf("Load =\n%+v\nwant\n%+v", got, want)
	}
	if got.ExpiresAt.Sub(got.IssuedAt) != DefaultLifetime || got.IdleExpiresAt.Sub(got.IssuedAt) != DefaultIdleTimeout {
		t.Errorf("defaults not applied: iat=%v exp=%v idle=%v", got.IssuedAt, got.ExpiresAt, got.IdleExpiresAt)
	}
	if len(got.ID) < 20 || len(got.CSRF) < 40 {
		t.Errorf("sid %q / csrf %q too short", got.ID, got.CSRF)
	}
}

// normalize makes times comparable with reflect.DeepEqual.
func normalize(s auth.Session) auth.Session {
	s.IssuedAt = s.IssuedAt.UTC()
	s.ExpiresAt = s.ExpiresAt.UTC()
	s.IdleExpiresAt = s.IdleExpiresAt.UTC()
	return s
}

func TestSealedIssueFillsAndCaps(t *testing.T) {
	clk := newFakeClock()
	st := newTestStore(t, clk, nil)
	ctx := context.Background()

	// Issue on a bare session generates sid/csrf/times.
	v, err := st.Issue(ctx, auth.Session{Principal: testPrincipal()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Load(ctx, v)
	if err != nil || got.ID == "" || got.CSRF == "" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	// A caller cannot mint a session longer than Lifetime.
	v, err = st.Issue(ctx, auth.Session{Principal: testPrincipal(), ExpiresAt: clk.Now().Add(30 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = st.Load(ctx, v)
	if d := got.ExpiresAt.Sub(clk.Now()); d != DefaultLifetime {
		t.Errorf("ExpiresAt capped to %v from now, want %v", d, DefaultLifetime)
	}
	if _, err := st.Issue(ctx, auth.Session{}); err == nil {
		t.Error("Issue without a principal succeeded")
	}
}

func TestSealedExpiryTable(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		advance time.Duration
		want    error
	}{
		{"fresh", 0, nil},
		{"just inside idle", DefaultIdleTimeout - time.Second, nil},
		{"idle exceeded", DefaultIdleTimeout, ErrIdleExpired},
		{"absolute exceeded", DefaultLifetime, ErrExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := newFakeClock()
			st := newTestStore(t, clk, nil)
			v, err := st.Issue(ctx, st.NewSession(testPrincipal(), []string{"viewer"}, "corp"))
			if err != nil {
				t.Fatal(err)
			}
			clk.Advance(c.advance)
			if _, err := st.Load(ctx, v); !errors.Is(err, c.want) {
				t.Fatalf("Load after %v: err = %v, want %v", c.advance, err, c.want)
			}
		})
	}
}

func TestSealedTouchSlidesIdleButNotAbsolute(t *testing.T) {
	clk := newFakeClock()
	st := newTestStore(t, clk, nil)
	ctx := context.Background()
	start := clk.Now()
	v, err := st.Issue(ctx, st.NewSession(testPrincipal(), []string{"viewer"}, "corp"))
	if err != nil {
		t.Fatal(err)
	}

	// Within the touch interval: no re-seal.
	clk.Advance(30 * time.Second)
	s, err := st.Load(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if nv, changed, err := st.Touch(ctx, &s); err != nil || changed || nv != "" {
		t.Fatalf("Touch after 30s = %q, %v, %v; want no re-seal", nv, changed, err)
	}

	// Active every 20 minutes: idle (30m) never fires, absolute (12h) does.
	clk.Advance(-30 * time.Second)
	for i := 0; ; i++ {
		clk.Advance(20 * time.Minute)
		s, err := st.Load(ctx, v)
		if clk.Now().Sub(start) >= DefaultLifetime {
			if !errors.Is(err, ErrExpired) {
				t.Fatalf("at %v: err = %v, want ErrExpired", clk.Now().Sub(start), err)
			}
			break
		}
		if err != nil {
			t.Fatalf("iteration %d at %v: %v", i, clk.Now().Sub(start), err)
		}
		nv, changed, err := st.Touch(ctx, &s)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			v = nv
		}
		if s.IdleExpiresAt.After(s.ExpiresAt) {
			t.Fatalf("idle_exp %v slid past exp %v", s.IdleExpiresAt, s.ExpiresAt)
		}
		if !s.ExpiresAt.Equal(start.Add(DefaultLifetime)) {
			t.Fatalf("Touch moved exp to %v", s.ExpiresAt)
		}
	}
}

func TestSealedTouchRejectsExpired(t *testing.T) {
	clk := newFakeClock()
	st := newTestStore(t, clk, nil)
	s := st.NewSession(testPrincipal(), nil, "corp")
	clk.Advance(DefaultIdleTimeout)
	if _, _, err := st.Touch(context.Background(), &s); !errors.Is(err, ErrIdleExpired) {
		t.Fatalf("Touch on idle-expired session: %v", err)
	}
	clk.Advance(DefaultLifetime)
	if _, _, err := st.Touch(context.Background(), &s); !errors.Is(err, ErrExpired) {
		t.Fatalf("Touch on expired session: %v", err)
	}
}

func TestSealedValidAfter(t *testing.T) {
	clk := newFakeClock()
	var mu sync.Mutex
	validAfter := map[string]time.Time{}
	st := newTestStore(t, clk, func(o *Options) {
		o.ValidAfter = func(id string) time.Time {
			mu.Lock()
			defer mu.Unlock()
			return validAfter[id]
		}
	})
	ctx := context.Background()
	p := testPrincipal()
	v, _ := st.Issue(ctx, st.NewSession(p, []string{"viewer"}, "corp"))
	other, _ := st.Issue(ctx, st.NewSession(auth.Principal{Kind: auth.KindUser, ID: "someone-else"}, nil, "corp"))

	set := func(t time.Time) { mu.Lock(); validAfter[p.ID] = t; mu.Unlock() }
	cases := []struct {
		name string
		va   time.Time
		want error
	}{
		{"unset", time.Time{}, nil},
		{"in the past", clk.Now().Add(-time.Hour), nil},
		{"same second as iat (fail closed)", clk.Now(), ErrRevoked},
		{"in the future", clk.Now().Add(time.Hour), ErrRevoked},
	}
	for _, c := range cases {
		set(c.va)
		if _, err := st.Load(ctx, v); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
		if _, err := st.Load(ctx, other); err != nil {
			t.Errorf("%s: other principal's session affected: %v", c.name, err)
		}
	}
	// After a revocation, a fresh login is valid again.
	set(clk.Now())
	clk.Advance(time.Second)
	fresh, _ := st.Issue(ctx, st.NewSession(p, nil, "corp"))
	if _, err := st.Load(ctx, fresh); err != nil {
		t.Fatalf("session issued after sessionsValidAfter rejected: %v", err)
	}
}

func TestSealedKeyRotation(t *testing.T) {
	clk := newFakeClock()
	ctx := context.Background()
	oldStore := newTestStore(t, clk, func(o *Options) { o.Key = testKey(1) })
	oldCookie, _ := oldStore.Issue(ctx, oldStore.NewSession(testPrincipal(), []string{"admin"}, "corp"))

	rotating := newTestStore(t, clk, func(o *Options) { o.Key, o.PreviousKey = testKey(2), testKey(1) })
	s, err := rotating.Load(ctx, oldCookie)
	if err != nil {
		t.Fatalf("rotation window: old cookie rejected: %v", err)
	}
	// The next touch re-seals under the new key.
	clk.Advance(2 * time.Minute)
	newCookie, changed, err := rotating.Touch(ctx, &s)
	if err != nil || !changed {
		t.Fatalf("Touch = %v, %v", changed, err)
	}
	rotated := newTestStore(t, clk, func(o *Options) { o.Key = testKey(2) })
	if _, err := rotated.Load(ctx, newCookie); err != nil {
		t.Fatalf("re-sealed cookie rejected after rotation: %v", err)
	}
	if _, err := rotated.Load(ctx, oldCookie); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old cookie accepted after previous key removed: %v", err)
	}
}

func TestSealedSizeGuard(t *testing.T) {
	clk := newFakeClock()
	st := newTestStore(t, clk, nil)
	ctx := context.Background()

	roles := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("zone-admin@zone:zone-%04d-example-com", i)
		}
		return out
	}
	// Find the boundary: the largest snapshot that fits, then one more.
	n := 1
	for {
		v, err := st.Issue(ctx, st.NewSession(testPrincipal(), roles(n+1), "corp"))
		if err != nil {
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("unexpected error: %v", err)
			}
			break
		}
		if got := len(DefaultCookieName) + 1 + len(v); got > MaxCookieBytes {
			t.Fatalf("issued a %d-byte cookie", got)
		}
		n++
	}
	if n < 50 {
		t.Fatalf("guard tripped at %d grants; expected room for dozens", n)
	}
	t.Logf("4 KiB guard: %d zone-scoped grants fit, %d do not", n, n+1)

	// Touch hits the same guard instead of emitting an oversized cookie.
	s := st.NewSession(testPrincipal(), roles(n+1), "corp")
	clk.Advance(5 * time.Minute)
	if _, _, err := st.Touch(ctx, &s); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Touch on oversized session: %v", err)
	}
	// Load refuses absurdly long input before decoding it.
	if _, err := st.Load(ctx, strings.Repeat("A", MaxCookieBytes+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load(huge) = %v", err)
	}
}

func TestSealedRevokeAndListUnsupported(t *testing.T) {
	st := newTestStore(t, newFakeClock(), nil)
	if err := st.Revoke(context.Background(), "sid"); !errors.Is(err, auth.ErrUnsupported) {
		t.Errorf("Revoke = %v", err)
	}
	if _, err := st.List(context.Background(), "p"); !errors.Is(err, auth.ErrUnsupported) {
		t.Errorf("List = %v", err)
	}
}

func TestSealedRejectsForeignPayload(t *testing.T) {
	// A value sealed with the right key but an unknown payload version is
	// refused rather than half-parsed.
	clk := newFakeClock()
	st := newTestStore(t, clk, nil)
	for _, pt := range []string{`{"v":2,"sid":"x","principal":{"kind":"User","id":"u"}}`, `{"v":1}`, `not json`} {
		v := st.sealer.Seal([]byte(pt))
		if _, err := st.Load(context.Background(), v); !errors.Is(err, ErrInvalid) {
			t.Errorf("Load(%s) = %v, want ErrInvalid", pt, err)
		}
	}
}

func TestNewSealedValidation(t *testing.T) {
	for name, o := range map[string]Options{
		"no key":                {},
		"idle > lifetime":       {Key: testKey(1), IdleTimeout: 2 * time.Hour, Lifetime: time.Hour},
		"negative":              {Key: testKey(1), IdleTimeout: -time.Minute},
		"touch >= idle timeout": {Key: testKey(1), IdleTimeout: time.Minute, TouchInterval: time.Minute},
	} {
		if _, err := NewSealed(o); err == nil {
			t.Errorf("%s: NewSealed succeeded", name)
		}
	}
}

func TestSealedConcurrentUse(t *testing.T) {
	clk := newFakeClock()
	st := newTestStore(t, clk, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				s := st.NewSession(auth.Principal{Kind: auth.KindUser, ID: fmt.Sprintf("u%d-%d", i, j)}, []string{"viewer"}, "corp")
				v, err := st.Issue(ctx, s)
				if err != nil {
					t.Error(err)
					return
				}
				if got, err := st.Load(ctx, v); err != nil || got.ID != s.ID {
					t.Errorf("Load = %v, %v", got.ID, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
