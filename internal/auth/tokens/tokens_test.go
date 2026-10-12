package tokens

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestWireFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		wire, id := Generate()
		if len(wire) != WireLen || !strings.HasPrefix(wire, "kdr_") || len(id) != IDLen {
			t.Fatalf("Generate() = %q (%d bytes)", wire, len(wire))
		}
		got, ok := Parse(wire)
		if !ok || got != id {
			t.Fatalf("Parse(%q) = %q, %v; want %q", wire, got, ok, id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
		if len(Hash(wire)) != 64 || Hash(wire) == Hash(wire+"x") {
			t.Fatal("hash is not a sha256 of the full token")
		}
	}
	good, _ := Generate()
	for _, bad := range []string{
		"", "kdr_", good[:len(good)-1], good + "x", "kdx_" + good[4:],
		"kdr_ABCDEFGHIJKL_" + good[17:], // upper-case id
		"kdr_abcdefghijk__" + good[17:], // '_' inside the id
		good[:16] + "-" + good[17:],     // no separator
		good[:20] + "!" + good[21:],     // not base64url
		"Bearer " + good,
	} {
		if _, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) accepted a malformed token", bad)
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

func newTokens(t *testing.T, st store.Store, c *clock, maxLife time.Duration) *Tokens {
	t.Helper()
	tk, err := New(Options{Store: st, MaxLifetime: maxLife, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	if err := tk.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	return tk
}

func alice() internalv1.TokenOwner {
	return internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: "alice", ID: "local:alice", Provider: "local"}
}

func TestCreateVerifyRevoke(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		c := &clock{t: time.Unix(1_800_000_000, 0)}
		tk := newTokens(t, st, c, 0)
		wire, tok, err := tk.Create(ctx, CreateOptions{Owner: alice(), Description: "laptop", Scopes: []string{"viewer"}})
		if err != nil {
			t.Fatal(err)
		}
		if tok.Spec.SecretHash != Hash(wire) || strings.Contains(tok.Spec.SecretHash, wire[17:]) || tok.Spec.ExpiresAt != nil {
			t.Fatalf("stored token %+v", tok.Spec)
		}
		// Nothing secret at rest: the stored object never contains the secret.
		var stored internalv1.APIToken
		if err := st.Get(ctx, store.Key{Kind: internalv1.KindAPIToken, Name: tok.Name}, &stored); err != nil {
			t.Fatal(err)
		}
		if stored.Spec.SecretHash != Hash(wire) {
			t.Fatal("hash not persisted")
		}
		p, err := tk.Verify(ctx, wire, "192.0.2.1")
		if err != nil {
			t.Fatal(err)
		}
		if p.Kind != auth.KindUser || p.ID != "local:alice" || p.Name != "alice" || p.AuthMethod != auth.MethodToken || p.TokenID != tok.Name || len(p.Scopes) != 1 {
			t.Fatalf("principal %+v", p)
		}

		// Unknown id and wrong secret: the same error, the same message.
		other, _ := Generate()
		wrongSecret := wire[:17] + other[17:]
		_, e1 := tk.Verify(ctx, other, "")
		_, e2 := tk.Verify(ctx, wrongSecret, "")
		_, e3 := tk.Verify(ctx, "kdr_garbage", "")
		for _, e := range []error{e1, e2, e3} {
			ae := auth.AsError(e)
			if ae.Status != http.StatusUnauthorized || ae.Message != "invalid or expired credentials" {
				t.Fatalf("rejection %+v", ae)
			}
		}

		if tk.List("local:alice")[0].Name != tok.Name || len(tk.List("local:bob")) != 0 {
			t.Fatal("List by owner")
		}
		if err := tk.Revoke(ctx, tok.Name); err != nil {
			t.Fatal(err)
		}
		if _, err := tk.Verify(ctx, wire, ""); err == nil {
			t.Fatal("revoked token still verifies")
		}
		if err := tk.Revoke(ctx, tok.Name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second revoke: %v", err)
		}
	})
}

func TestExpiryAndMaxLifetime(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	tk := newTokens(t, authtest.Memory(t), c, 30*24*time.Hour)
	if _, _, err := tk.Create(ctx, CreateOptions{Owner: alice(), ExpiresIn: 31 * 24 * time.Hour}); !errors.Is(err, ErrLifetimeTooLong) {
		t.Fatalf("over maxLifetime: %v", err)
	}
	wire, tok, err := tk.Create(ctx, CreateOptions{Owner: alice()})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Spec.ExpiresAt == nil || !tok.Spec.ExpiresAt.Time.Equal(c.now().Add(30*24*time.Hour)) {
		t.Fatalf("a token without expiry gets maxLifetime: %v", tok.Spec.ExpiresAt)
	}
	short, _, err := tk.Create(ctx, CreateOptions{Owner: alice(), ExpiresIn: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour - time.Second)
	if _, err := tk.Verify(ctx, short, ""); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	c.add(time.Second)
	if _, err := tk.Verify(ctx, short, ""); err == nil {
		t.Fatal("expired token verified")
	}
	if _, err := tk.Verify(ctx, wire, ""); err != nil {
		t.Fatal(err)
	}
}

func TestServiceAccountTokens(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		c := &clock{t: time.Now()}
		tk := newTokens(t, st, c, 0)
		owner := internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: "ci", Namespace: "dns"}
		if _, _, err := tk.Create(ctx, CreateOptions{Owner: owner}); !errors.Is(err, ErrNoServiceAccount) {
			t.Fatalf("token for a missing account: %v", err)
		}
		if _, err := tk.EnsureServiceAccount(ctx, "dns", "ci", "CI", false); err != nil {
			t.Fatal(err)
		}
		wire, _, err := tk.Create(ctx, CreateOptions{Owner: owner})
		if err != nil {
			t.Fatal(err)
		}
		p, err := tk.Verify(ctx, wire, "")
		if err != nil {
			t.Fatal(err)
		}
		if p.Kind != auth.KindServiceAccount || p.ServiceAccount != "dns/ci" || p.ID != "sa:dns/ci" || p.BuiltinAdmin {
			t.Fatalf("principal %+v", p)
		}
		var sa internalv1.ServiceAccount
		if err := st.Get(ctx, store.Key{Kind: internalv1.KindServiceAccount, Namespace: "dns", Name: "ci"}, &sa); err != nil {
			t.Fatal(err)
		}
		sa.Spec.Disabled = true
		out, err := st.Apply(ctx, &sa, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			t.Fatal(err)
		}
		tk.ServiceAccounts().Upsert(out.(*internalv1.ServiceAccount))
		if _, err := tk.Verify(ctx, wire, ""); err == nil {
			t.Fatal("a disabled service account's token verified")
		}
	})
}

// TestRecreatedServiceAccountDoesNotInheritTokens is the service-account
// half of the M5 finding S2: a token of a deleted service account must not
// act for a later service account with the same namespace and name (which
// may be a builtin admin, like the break-glass account).
func TestRecreatedServiceAccountDoesNotInheritTokens(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		c := &clock{t: time.Now()}
		tk := newTokens(t, st, c, 0)
		if _, err := tk.EnsureServiceAccount(ctx, "dns", "ci", "CI", false); err != nil {
			t.Fatal(err)
		}
		wire, _, err := tk.Create(ctx, CreateOptions{Owner: internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: "ci", Namespace: "dns"}})
		if err != nil {
			t.Fatal(err)
		}
		key := store.Key{Kind: internalv1.KindServiceAccount, Namespace: "dns", Name: "ci"}
		if err := st.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			t.Fatal(err)
		}
		tk.ServiceAccounts().Remove(key)
		time.Sleep(1100 * time.Millisecond) // creation timestamps have one-second resolution
		if _, err := tk.EnsureServiceAccount(ctx, "dns", "ci", "re-created", true); err != nil {
			t.Fatal(err)
		}
		if p, err := tk.Verify(ctx, wire, ""); err == nil {
			t.Fatalf("the old token acts for the re-created account: %+v", p)
		}
		fresh, _, err := tk.Create(ctx, CreateOptions{Owner: internalv1.TokenOwner{Kind: internalv1.OwnerServiceAccount, Name: "ci", Namespace: "dns"}})
		if err != nil {
			t.Fatal(err)
		}
		if p, err := tk.Verify(ctx, fresh, ""); err != nil || !p.BuiltinAdmin {
			t.Fatalf("a token of the new account: %+v %v", p, err)
		}
	})
}

func TestBootstrapToken(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		tk := newTokens(t, st, &clock{t: time.Now()}, 0)
		if ok, err := tk.Bootstrap(ctx, func(string) string { return "" }); ok || err != nil {
			t.Fatalf("no variable: %v, %v", ok, err)
		}
		if _, err := tk.Bootstrap(ctx, func(k string) string {
			if k == EnvBootstrapToken {
				return "not-a-kudurru-token"
			}
			return ""
		}); err == nil {
			t.Fatal("a malformed bootstrap token was accepted")
		}
		wire, _ := Generate()
		file := filepath.Join(t.TempDir(), "tok")
		if err := os.WriteFile(file, []byte(wire+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		env := func(k string) string {
			if k == EnvBootstrapTokenFile {
				return file
			}
			return ""
		}
		for range 2 { // idempotent
			if ok, err := tk.Bootstrap(ctx, env); !ok || err != nil {
				t.Fatalf("bootstrap: %v, %v", ok, err)
			}
		}
		p, err := tk.Verify(ctx, wire, "")
		if err != nil {
			t.Fatal(err)
		}
		if !p.BuiltinAdmin || p.ServiceAccount != DefaultNamespace+"/"+BootstrapAccount {
			t.Fatalf("bootstrap principal %+v", p)
		}
		if n := len(tk.List(ServiceAccountID(DefaultNamespace, BootstrapAccount))); n != 1 {
			t.Fatalf("%d bootstrap tokens", n)
		}
	})
}

func TestAuthenticateStepAndLastUsed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := authtest.SQLite(t)
	tk := newTokens(t, st, &clock{t: time.Unix(1_800_000_000, 0)}, 0)
	done := make(chan struct{})
	go func() { _ = tk.Run(ctx); close(done) }()
	wire, tok, err := tk.Create(ctx, CreateOptions{Owner: alice()})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/zones", nil)
	if _, _, err := tk.Authenticate(httptest.NewRecorder(), req); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("no header: %v", err)
	}
	req.Header.Set("Authorization", "Bearer eyJhbGciOi.x.y")
	if _, _, err := tk.Authenticate(httptest.NewRecorder(), req); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("a JWT is not this step's: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+wire)
	req.RemoteAddr = "192.0.2.77:51000"
	p, _, err := tk.Authenticate(httptest.NewRecorder(), req)
	if err != nil || p.TokenID != tok.Name {
		t.Fatalf("authenticate: %+v, %v", p, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var cur internalv1.APIToken
		if err := st.Get(ctx, store.Key{Kind: internalv1.KindAPIToken, Name: tok.Name}, &cur); err != nil {
			t.Fatal(err)
		}
		if cur.Status.LastUsedAt != nil {
			if cur.Status.LastUsedIP != "192.0.2.77" {
				t.Fatalf("lastUsedIP %q", cur.Status.LastUsedIP)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lastUsedAt never recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}
