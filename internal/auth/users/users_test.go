package users

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestHashVerify(t *testing.T) {
	h := NewHasher(1)
	ctx := context.Background()
	phc, err := h.Hash(ctx, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("PHC string %q does not carry the §8.7 parameters", phc)
	}
	for _, tc := range []struct {
		pw   string
		want bool
	}{{"correct horse battery", true}, {"correct horse batterz", false}, {"", false}, {strings.Repeat("x", MaxPasswordLen+1), false}} {
		got, err := h.Verify(ctx, phc, tc.pw)
		if err != nil || got != tc.want {
			t.Errorf("Verify(%.20q) = %v, %v; want %v", tc.pw, got, err, tc.want)
		}
	}
	if _, err := h.Hash(ctx, strings.Repeat("x", MaxPasswordLen+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Errorf("hashing an oversized password: %v", err)
	}
}

func TestParsePHCRejectsHostileParameters(t *testing.T) {
	for _, s := range []string{
		"", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=4194304,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", // 4 GiB
		"$argon2id$v=19$m=19456,t=99,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=18$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=19456,t=2,x=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		if _, err := parsePHC(s); !errors.Is(err, ErrBadHash) {
			t.Errorf("parsePHC(%q) = %v, want ErrBadHash", s, err)
		}
	}
}

func TestCheckNewPassword(t *testing.T) {
	for _, tc := range []struct {
		pw   string
		want error
	}{{"short", ErrPasswordTooShort}, {"ünïcödé!", nil}, {strings.Repeat("a", MaxPasswordLen), nil}, {strings.Repeat("a", MaxPasswordLen+1), ErrPasswordTooLong}} {
		if err := CheckNewPassword(tc.pw); !errors.Is(err, tc.want) {
			t.Errorf("CheckNewPassword(%d bytes) = %v, want %v", len(tc.pw), err, tc.want)
		}
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestLimiter(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	l := NewLimiter(LimitPolicy{Failures: 3, Window: time.Minute, Lockout: 5 * time.Minute}, c.now)
	for i := range 2 {
		if d := l.Fail("k"); d != 0 {
			t.Fatalf("failure %d locked early", i+1)
		}
	}
	if d := l.Fail("k"); d != 5*time.Minute {
		t.Fatalf("third failure: lockout %s", d)
	}
	c.add(4 * time.Minute)
	if d := l.Locked("k"); d != time.Minute {
		t.Fatalf("remaining lockout %s, want 1m", d)
	}
	c.add(time.Minute)
	if d := l.Locked("k"); d != 0 {
		t.Fatalf("lockout outlived its duration: %s", d)
	}
	// Failures further apart than the window do not accumulate.
	l.Fail("w")
	c.add(2 * time.Minute)
	l.Fail("w")
	c.add(2 * time.Minute)
	if d := l.Fail("w"); d != 0 {
		t.Fatal("spread-out failures locked the key")
	}
	l.Reset("w")
	if l.Len() != 1 {
		t.Fatalf("Reset kept the key: %d keys", l.Len())
	}
}

func newUsers(t *testing.T, st store.Store, c *clock) *Users {
	t.Helper()
	ip, usr := DefaultIPLimit, DefaultUserLimit
	u, err := New(Options{Store: st, Hasher: NewHasher(2), IPLimit: &ip, UserLimit: &usr, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLoginAndLockouts(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		c := &clock{t: time.Now()}
		u := newUsers(t, st, c)
		if _, err := u.Create(ctx, CreateOptions{Name: "Alice", Password: "alice-password", Groups: []string{"ops"}}); err != nil {
			t.Fatal(err)
		}
		usr, err := u.Login(ctx, " ALICE ", "alice-password", "192.0.2.1")
		if err != nil || usr.Name != "alice" {
			t.Fatalf("login: %v, %v", usr, err)
		}
		if _, err := u.Login(ctx, "alice", "wrong", "192.0.2.1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("wrong password: %v", err)
		}
		if _, err := u.Login(ctx, "nobody", "wrong", "192.0.2.9"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("unknown user: %v", err)
		}

		// Per IP: 5 failures → 5 minutes, for any username.
		for i := range 4 {
			if _, err := u.Login(ctx, "mallory"+string(rune('a'+i)), "x", "198.51.100.7"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("failure %d: %v", i, err)
			}
		}
		var locked *LockedError
		_, err = u.Login(ctx, "alice", "alice-password", "198.51.100.7") // correct, but the IP has 4 failures: still allowed
		if err != nil {
			t.Fatalf("4 failures should not lock the IP: %v", err)
		}
		_, _ = u.Login(ctx, "malloryz", "x", "198.51.100.7") // 5th failure
		if _, err := u.Login(ctx, "alice", "alice-password", "198.51.100.7"); !errors.As(err, &locked) || locked.By != "ip" || locked.RetryAfter != 5*time.Minute {
			t.Fatalf("locked IP: %v", err)
		}
		c.add(5*time.Minute + time.Second)
		if _, err := u.Login(ctx, "alice", "alice-password", "198.51.100.7"); err != nil {
			t.Fatalf("after the IP lockout: %v", err)
		}

		// Per user: 10 failures from 10 addresses → 15 minutes.
		for i := range 10 {
			_, _ = u.Login(ctx, "alice", "bad", "203.0.113."+string(rune('0'+i)))
		}
		if _, err := u.Login(ctx, "alice", "alice-password", "203.0.113.200"); !errors.As(err, &locked) || locked.By != "user" || locked.RetryAfter != 15*time.Minute {
			t.Fatalf("locked user: %v", err)
		}
		c.add(15*time.Minute + time.Second)
		if _, err := u.Login(ctx, "alice", "alice-password", "203.0.113.200"); err != nil {
			t.Fatalf("after the user lockout: %v", err)
		}

		// Disabled accounts: correct password → ErrDisabled.
		if err := u.SetDisabled(ctx, "alice", true); err != nil {
			t.Fatal(err)
		}
		if _, err := u.Login(ctx, "alice", "alice-password", "192.0.2.50"); !errors.Is(err, ErrDisabled) {
			t.Fatalf("disabled: %v", err)
		}
		if va := u.ValidAfter(PrincipalID("alice")); va.Year() != 9999 {
			t.Fatalf("a disabled user's sessions must all be revoked, ValidAfter %v", va)
		}
		if _, err := u.LoadPrincipal(ctx, Principal(usr)); err == nil {
			t.Fatal("LoadPrincipal accepted a disabled user")
		}
	})
}

func TestPasswordChangeRevokesSessions(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		c := &clock{t: time.Unix(1_800_000_000, 0)}
		u := newUsers(t, st, c)
		if _, err := u.Create(ctx, CreateOptions{Name: "bob", Password: "first-password", MustChangePassword: true}); err != nil {
			t.Fatal(err)
		}
		if va := u.ValidAfter(PrincipalID("bob")); !va.IsZero() {
			t.Fatalf("fresh user has sessionsValidAfter %v", va)
		}
		p, err := u.LoadPrincipal(ctx, auth.Principal{Kind: auth.KindUser, ID: PrincipalID("bob"), Name: "bob", Provider: Provider, AuthMethod: "cookie", Grants: []string{"admin"}})
		if err != nil || !p.MustChangePassword || p.Grants != nil || p.AuthMethod != "cookie" {
			t.Fatalf("LoadPrincipal = %+v, %v", p, err)
		}
		c.add(time.Hour)
		if err := u.ChangePassword(ctx, "bob", "wrong-password", "second-password"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("change with a wrong current password: %v", err)
		}
		if err := u.ChangePassword(ctx, "bob", "first-password", "second-password"); err != nil {
			t.Fatal(err)
		}
		usr, _ := u.Get("bob")
		if usr.Spec.MustChangePassword || usr.Status.SessionsValidAfter == nil || !usr.Status.SessionsValidAfter.Time.Equal(c.now()) {
			t.Fatalf("after change: %+v", usr)
		}
		if va := u.ValidAfter(PrincipalID("bob")); !va.Equal(c.now()) {
			t.Fatalf("ValidAfter = %v", va)
		}
		if _, err := u.Login(ctx, "bob", "second-password", "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
		if u.ValidAfter("oidc:dex:x:y") != (time.Time{}) {
			t.Fatal("non-local principals have no sessionsValidAfter")
		}
	})
}

func TestBootstrap(t *testing.T) {
	ctx := context.Background()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	t.Run("generated", func(t *testing.T) {
		authtest.Drivers(t, func(t *testing.T, st store.Store) {
			u := newUsers(t, st, &clock{t: time.Now()})
			var out bytes.Buffer
			res, err := u.Bootstrap(ctx, BootstrapOptions{Getenv: env(nil), Out: &out, Identity: "node-a"})
			if err != nil || !res.Created || !res.Generated || res.Password == "" {
				t.Fatalf("bootstrap = %+v, %v", res, err)
			}
			if !strings.Contains(out.String(), res.Password) {
				t.Fatalf("the generated password was not printed: %q", out.String())
			}
			if !u.HasDefaultCredentials() {
				t.Fatal("hasDefaultCredentials should be true")
			}
			usr, _ := u.Get("admin")
			if !usr.Spec.BuiltinAdmin || !usr.Spec.MustChangePassword || store.ManagedBy(usr) != "bootstrap:node-a" {
				t.Fatalf("admin = %+v", usr)
			}
			// Second start: nothing happens, nothing printed.
			out.Reset()
			res, err = u.Bootstrap(ctx, BootstrapOptions{Getenv: env(nil), Out: &out})
			if err != nil || res.Created || out.Len() != 0 {
				t.Fatalf("second bootstrap = %+v, %v, %q", res, err, out.String())
			}
			if _, err := u.Login(ctx, "admin", res.Password+"x", "192.0.2.1"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatal(err)
			}
		})
	})

	t.Run("from environment and files", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "pw")
		if err := os.WriteFile(file, []byte("password-from-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, want string
			env        map[string]string
			cfgFile    string
		}{
			{"env", "password-from-env", map[string]string{EnvAdminPassword: "password-from-env", EnvAdminPasswordFile: file}, ""},
			{"env file", "password-from-file", map[string]string{EnvAdminPasswordFile: file}, ""},
			{"config file", "password-from-file", nil, file},
		} {
			t.Run(tc.name, func(t *testing.T) {
				u := newUsers(t, authtest.Memory(t), &clock{t: time.Now()})
				var out bytes.Buffer
				res, err := u.Bootstrap(ctx, BootstrapOptions{Getenv: env(tc.env), PasswordFile: tc.cfgFile, Out: &out})
				if err != nil || !res.Created || res.Generated || out.Len() != 0 {
					t.Fatalf("bootstrap = %+v, %v, %q", res, err, out.String())
				}
				if u.HasDefaultCredentials() {
					t.Fatal("a configured password is not a default credential")
				}
				if _, err := u.Login(ctx, "admin", tc.want, "192.0.2.1"); err != nil {
					t.Fatal(err)
				}
			})
		}
		u := newUsers(t, authtest.Memory(t), &clock{t: time.Now()})
		if _, err := u.Bootstrap(ctx, BootstrapOptions{Getenv: env(map[string]string{EnvAdminPassword: "short"})}); !errors.Is(err, ErrPasswordTooShort) {
			t.Fatalf("short bootstrap password: %v", err)
		}
	})

	t.Run("two replicas race", func(t *testing.T) {
		authtest.Drivers(t, func(t *testing.T, st store.Store) {
			a := newUsers(t, st, &clock{t: time.Now()})
			b := newUsers(t, st, &clock{t: time.Now()})
			ra, err := a.Bootstrap(ctx, BootstrapOptions{Getenv: env(nil), Out: &bytes.Buffer{}, Identity: "pod-a"})
			if err != nil {
				t.Fatal(err)
			}
			// b listed before a wrote (simulated by calling Create directly
			// with b's manager): it must lose, not overwrite.
			_, err = b.Create(ctx, CreateOptions{Name: "admin", Password: "other-password", BuiltinAdmin: true, Manager: store.ManagerBootstrap + ":pod-b"})
			if !store.IsManagedBy(err) {
				t.Fatalf("second replica's create = %v, want ErrManagedBy", err)
			}
			if _, err := a.Login(ctx, "admin", ra.Password, "192.0.2.1"); err != nil {
				t.Fatalf("the winner's password must stand: %v", err)
			}
		})
	})
}

func TestCacheFollowsStore(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := &clock{t: time.Now()}
		u := newUsers(t, st, c)
		go func() { _ = u.Run(ctx) }()
		other := newUsers(t, st, c) // another replica writing
		if _, err := other.Create(ctx, CreateOptions{Name: "carol", Password: "carol-password"}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, ok := u.Get("carol"); ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the watch never delivered carol")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := st.Delete(ctx, store.Key{Kind: internalv1.KindLocalUser, Name: "carol"}, store.DeleteOptions{Force: true}); err != nil {
			t.Fatal(err)
		}
		for {
			if _, ok := u.Get("carol"); !ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the watch never deleted carol")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestSanitizeForLog(t *testing.T) {
	for in, want := range map[string]string{
		"alice":                 "alice",
		"evil\nINFO forged":     "evil?INFO forged",
		"\x1b[31mred":           "?[31mred",
		strings.Repeat("a", 80): strings.Repeat("a", 64) + "…",
	} {
		if got := SanitizeForLog(in); got != want {
			t.Errorf("SanitizeForLog(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIPv6LockoutCoversThePrefix is the regression test of the M5 hardening
// "per-IP login lockouts key IPv6 clients by /128": a single host owns a
// whole /64 and could rotate addresses to try five passwords per address.
func TestIPv6LockoutCoversThePrefix(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Now()}
	u := newUsers(t, authtest.Memory(t), c)
	if _, err := u.Create(ctx, CreateOptions{Name: "alice", Password: "alice-password"}); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		ip := "2001:db8:1:2::" + string(rune('a'+i))
		if _, err := u.Login(ctx, "mallory", "x", ip); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d: %v", i+1, err)
		}
	}
	var locked *LockedError
	if _, err := u.Login(ctx, "alice", "alice-password", "2001:db8:1:2::ffff"); !errors.As(err, &locked) || locked.By != "ip" {
		t.Fatalf("another address of the same /64: %v, want an ip lockout", err)
	}
	if _, err := u.Login(ctx, "alice", "alice-password", "2001:db8:1:3::1"); err != nil {
		t.Fatalf("another /64: %v", err)
	}
	if _, err := u.Login(ctx, "alice", "alice-password", "::ffff:192.0.2.1"); err != nil {
		t.Fatalf("an IPv4-mapped address: %v", err)
	}
}
