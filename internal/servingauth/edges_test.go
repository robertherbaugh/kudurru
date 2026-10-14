package servingauth

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCurrentLoginMustChangePasswordAndNoTOTPBypass(t *testing.T) {
	f := newFixture(t)
	proof := Credential{Kind: Password, Username: "alice", Value: "correct-password"}
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		user := f.user.DeepCopy()
		user.Spec.MustChangePassword = true
		if _, err := tx.Apply(ctx, user, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		p, err := AuthenticateCurrent(ctx, tx, f.deps, proof, f.now)
		if err != nil || !p.MustChangePassword || p.AccountUID != string(f.user.UID) {
			t.Fatalf("current enrollment identity: %v", err)
		}
		if _, _, err = ReauthorizeCurrent(ctx, tx, f.deps, proof, readAttrs(), f.now); !errors.Is(err, ErrDenied) {
			t.Fatalf("must-change proof granted general rights: %v", err)
		}
		user.Spec.TOTPSecret = "captured-encrypted-secret"
		if _, err = tx.Apply(ctx, user, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		if _, err = AuthenticateCurrent(ctx, tx, f.deps, proof, f.now); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("password bypassed TOTP: %v", err)
		}
		user.Spec.Disabled = true
		if _, err = tx.Apply(ctx, user, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		if _, err = AuthenticateCurrent(ctx, tx, f.deps, proof, f.now); !errors.Is(err, ErrCredentials) {
			t.Fatalf("staged disabled identity accepted: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AuthenticateCurrent(t.Context(), nil, f.deps, proof, f.now); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil transaction accepted", err)
	}
}

func stageSecurity(ctx context.Context, tx *sqlite.CommandTx, s Security) error {
	rows, err := EncodeSecurity(s)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err = tx.PutSection(ctx, "security", row); err != nil {
			return err
		}
	}
	return nil
}

func TestAtomicRemoveRegrantAndRollbackNeverReviveCookie(t *testing.T) {
	f := newFixture(t)
	old := f.view(t)
	cookie, err := old.SealSession("abcdefghijklmnop", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	proof := Credential{Kind: Cookie, Value: cookie}
	before := old.Base()
	rollback := errors.New("abort staged security change")
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		changed := f.security
		changed.Sessions = slices.Clone(changed.Sessions)
		changed.Sessions[0].Revoked = true
		if err := stageSecurity(ctx, tx, changed); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	unchanged := f.view(t)
	if !before.Same(unchanged.Base()) {
		t.Fatal("rolled-back security was published")
	}
	if _, err = unchanged.Authenticate(t.Context(), proof, f.now); err != nil {
		t.Fatal("rollback revoked durable session", err)
	}
	var binding publicv1.RoleBinding
	key := store.Key{Kind: "RoleBinding", Namespace: "apps", Name: "read"}
	if err = f.st.Get(t.Context(), key, &binding); err != nil {
		t.Fatal(err)
	}
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
			return err
		}
		changed := f.security
		changed.Epoch++
		changed.GrantFloors = []GrantFloor{{FormatVersion: 1, Grant: "viewer", ValidAfter: changed.Sessions[0].IssuedAt}}
		return stageSecurity(ctx, tx, changed)
	})
	if err != nil {
		t.Fatal(err)
	}
	removed := f.view(t)
	binding.UID, binding.ResourceVersion, binding.Annotations = "", "", nil
	apply(t, f.st, &binding)
	regranted := f.view(t)
	for _, v := range []*View{removed, regranted} {
		if _, err = v.Authenticate(t.Context(), proof, f.now); !errors.Is(err, ErrCredentials) {
			t.Fatal("old cookie revived", err)
		}
	}
	if _, err = old.Authenticate(t.Context(), proof, f.now); err != nil {
		t.Fatal("held cut changed", err)
	}
}

func TestCookieSessionDetachedExactEnrollmentAndTampering(t *testing.T) {
	f := newFixture(t)
	v := f.view(t)
	cookie, err := v.SealSession("abcdefghijklmnop", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.CookieSession(cookie, f.now)
	if err != nil || got.ID != f.security.Sessions[0].ID || got.CSRF != f.csrf || !got.ExpiresAt.Equal(f.security.Sessions[0].ExpiresAt) {
		t.Fatal("verified session fields missing", err)
	}
	got.Principal.Groups[0] = "injected"
	got.RolesSnapshot[0] = "admin"
	again, err := v.CookieSession(cookie, f.now)
	if err != nil || slices.Contains(again.Principal.Groups, "injected") || slices.Contains(again.RolesSnapshot, "admin") || slices.Contains(again.Principal.Grants, "admin") {
		t.Fatal("session output aliases view", err)
	}
	raw, _, err := v.sealer.Open(cookie)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"uid", "epoch", "issued", "expiry", "idle", "csrf", "session", "version"} {
		t.Run(change, func(t *testing.T) {
			var p CookiePayload
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "uid":
				p.Account.UID = "recreated-account"
			case "epoch":
				p.Epoch++
			case "issued":
				p.IssuedAt = p.IssuedAt.Add(time.Second)
			case "expiry":
				p.ExpiresAt = p.ExpiresAt.Add(time.Second)
			case "idle":
				p.IdleExpiresAt = p.IdleExpiresAt.Add(time.Second)
			case "csrf":
				p.CSRF = strings.Repeat("x", 32)
			case "session":
				p.SessionID = "unknownsession123"
			case "version":
				p.FormatVersion = 1
			}
			modified, _ := json.Marshal(p)
			if _, err := v.CookieSession(v.sealer.Seal(modified), f.now); !errors.Is(err, ErrCredentials) {
				t.Fatal("nonmatching enrollment accepted", err)
			}
		})
	}
	if _, err = v.CookieSession(cookie[:len(cookie)-5]+"AAAAA", f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("tampered seal accepted", err)
	}
	for _, now := range []time.Time{f.security.Sessions[0].IssuedAt.Add(-time.Second), f.security.Sessions[0].IdleExpiresAt, f.security.Sessions[0].ExpiresAt} {
		if _, err = v.CookieSession(cookie, now); !errors.Is(err, ErrCredentials) {
			t.Fatal("future or expired enrollment accepted", err)
		}
	}
}

func TestLocalGroupRemovalCannotInheritFrozenSessionGrant(t *testing.T) {
	f := newFixture(t)
	old := f.view(t)
	cookie, err := old.SealSession("abcdefghijklmnop", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	user := f.user.DeepCopy()
	user.Spec.Groups = nil
	apply(t, f.st, user)
	current := f.view(t)
	for _, proof := range []Credential{{Kind: Cookie, Value: cookie}, {Kind: APIToken, Value: f.localWire}} {
		p, err := current.Authenticate(t.Context(), proof, f.now)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := current.Authorize(p, readAttrs(), f.now)
		if err != nil || decision.Allowed {
			t.Fatal("removed local groups inherited another subject's binding", err)
		}
		p, err = old.Authenticate(t.Context(), proof, f.now)
		if err != nil {
			t.Fatal(err)
		}
		decision, err = old.Authorize(p, readAttrs(), f.now)
		if err != nil || !decision.Allowed {
			t.Fatal("held cut lost captured groups", err)
		}
	}
}

func TestTokenExpiryAndCookieKeyRotation(t *testing.T) {
	f := newFixture(t)
	old := f.view(t)
	cookie, err := old.SealSession("abcdefghijklmnop", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	expired := f.token.DeepCopy()
	expired.Spec.ExpiresAt = &metav1.Time{Time: f.now}
	apply(t, f.st, expired)
	f.deps.PreviousKey = slices.Clone(f.deps.CurrentKey)
	f.deps.CurrentKey = []byte("11234567890123456789012345678901")
	f.deps.Identity, f.deps.ExpectedIdentity = "rotated-v2", "rotated-v2"
	rotated := f.view(t)
	if old.DependencyIdentity() == rotated.DependencyIdentity() {
		t.Fatal("key dependency identity unchanged")
	}
	if _, err = rotated.CookieSession(cookie, f.now); err != nil {
		t.Fatal("previous key window rejected", err)
	}
	if _, err = rotated.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("equal-time token expiry allowed", err)
	}
	if _, err = old.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now); err != nil {
		t.Fatal("old cut lost held token", err)
	}
	f.deps.PreviousKey = nil
	retired := f.view(t)
	if _, err = retired.CookieSession(cookie, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("retired key accepted", err)
	}
}

func TestCapturedPublicIAMAndConcurrentHeldCuts(t *testing.T) {
	f := newFixture(t)
	old := f.view(t)
	if old.cut.objects[store.Key{Kind: "IdentityProvider", Name: "test-idp"}] == nil {
		t.Fatal("actual public IAM was not captured")
	}
	settings, err := old.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Name = "mutated"
	again, err := old.Settings()
	if err != nil || again.Name != "default" {
		t.Fatal("settings output aliases cut", err)
	}
	disabled := f.sa.DeepCopy()
	disabled.Spec.Disabled = true
	apply(t, f.st, disabled)
	newer := f.view(t)
	if err = f.st.Close(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				proof := Credential{Kind: APIToken, Value: f.wire}
				p, err := old.Authenticate(t.Context(), proof, f.now)
				if err != nil {
					t.Error("old cut failed", err)
					return
				}
				p.Scopes[0] = "admin"
				if _, err = newer.Authenticate(t.Context(), proof, f.now); !errors.Is(err, ErrCredentials) {
					t.Error("new cut used old account", err)
					return
				}
			}
		})
	}
	wg.Wait()
	old.Release()
	if _, err = old.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("released owner accepted proof", err)
	}
}

func TestDependencyCapsBeforeCloneAndScopedBindings(t *testing.T) {
	f := newFixture(t)
	cut, err := CaptureImage(t.Context(), f.image(t), f.registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, what := range []string{"large-map", "large-string", "aggregate", "subjects", "scoped-cluster", "unknown-scope"} {
		t.Run(what, func(t *testing.T) {
			p, _ := rbac.LoadPermissions()
			d := f.deps
			d.Permissions = p
			switch what {
			case "large-map":
				p.Roles = make(map[string][]string, MaxRows+1)
				for i := range MaxRows + 1 {
					p.Roles[time.Unix(int64(i), 0).String()] = nil
				}
			case "large-string":
				p.Roles["viewer"] = []string{strings.Repeat("x", 513)}
			case "aggregate":
				p.Roles["viewer"] = make([]string, MaxRows)
				for i := range p.Roles["viewer"] {
					p.Roles["viewer"][i] = strings.Repeat("x", 512)
				}
			case "subjects":
				d.StaticBindings = []rbac.Binding{{Role: "viewer", Subjects: make([]rbac.Subject, MaxRows+1)}}
			case "scoped-cluster":
				d.StaticBindings = []rbac.Binding{{Role: "zone-admin", Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "alice"}}}}
			case "unknown-scope":
				d.StaticBindings = []rbac.Binding{{Role: "admin", Scope: rbac.Scope{Kind: "unknown", Name: "example"}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "alice"}}}}
			}
			if _, err := Prepare(t.Context(), cut, d); err == nil {
				t.Fatal("malformed dependency graph accepted")
			}
		})
	}
	f.deps.StaticBindings = []rbac.Binding{{Role: "admin", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "example"}, Subjects: []rbac.Subject{{Kind: rbac.SubjectUser, Name: "alice"}}}}
	v, err := Prepare(t.Context(), cut, f.deps)
	if err != nil {
		t.Fatal("legitimate narrowed admin role rejected", err)
	}
	v.Release()
}

func TestSecurityAggregateCapsAndDetachedDecode(t *testing.T) {
	rows := make([]replication.Entry, 70)
	for i := range rows {
		rows[i] = replication.Entry{Key: "other-owner/row", Data: json.RawMessage(strings.Repeat(" ", MaxRowBytes))}
	}
	if _, err := DecodeSecurity(rows); !errors.Is(err, ErrBounds) {
		t.Fatal("aggregate cap bypassed", err)
	}
	f := newFixture(t)
	rows, err := EncodeSecurity(f.security)
	if err != nil {
		t.Fatal(err)
	}
	s, err := DecodeSecurity(rows)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		clear(rows[i].Data)
	}
	if s.Sessions[0].Account.UID != string(f.user.UID) || s.Sessions[0].Grants[0] != "viewer" {
		t.Fatal("decoded security aliases caller rows")
	}
	if err = ValidateEntry("security", replication.Entry{Key: SessionKey("abcdefghijklmnop"), Data: json.RawMessage(`{"formatVersion":99}`)}); !errors.Is(err, ErrVersion) {
		t.Fatal("unsupported row version accepted", err)
	}
	duplicate := f.security
	duplicate.Sessions = slices.Clone(duplicate.Sessions)
	duplicate.Sessions[0].Grants = []string{"viewer", "viewer"}
	if _, err = EncodeSecurity(duplicate); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate enrolled grants accepted", err)
	}
}
