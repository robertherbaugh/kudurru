package servingauth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestCurrentAllAuthenticatesPasswordFactorOnceForAllScopes(t *testing.T) {
	f := newFixture(t)
	seed := setupTOTP(t, f)
	counter := &countedPasswordVerifier{PasswordVerifier: f.deps.Hasher}
	f.deps.Hasher = counter
	proof := Credential{Kind: Password, Username: "alice", Value: "correct-password", TOTPCode: factorCode(t, seed, f.now)}
	attrs := []rbac.Attributes{
		{Resource: "zones", Verb: "read", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "old"}},
		{Resource: "zones", Verb: "read", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: "new"}},
	}
	for _, attempt := range []string{"denied-second-scope", "clock-rollback", "commit", "replay"} {
		before := f.image(t).Generation()
		calls, checks := counter.calls, 0
		err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			want := append([]rbac.Attributes(nil), attrs...)
			if attempt == "denied-second-scope" {
				want[1].Verb = "write"
			}
			_, err := ReauthorizeCurrentAllWithClock(ctx, tx, f.deps, proof, want, func() time.Time {
				checks++
				if attempt == "clock-rollback" && checks > 1 {
					return f.now.Add(-time.Nanosecond)
				}
				return f.now
			})
			return err
		})
		if counter.calls-calls != 1 {
			t.Fatalf("%s authenticated more than once", attempt)
		}
		if attempt == "commit" {
			if err != nil || checks != 2 {
				t.Fatalf("all-scope TOTP admission failed: %v (%d clock checks)", err, checks)
			}
			state, err := DecodeSecurity(f.image(t).Payload.Security)
			if err != nil || state.Epoch != f.security.Epoch+1 || !state.TOTP[0].HasConsumed {
				t.Fatal("factor was not consumed once", err)
			}
			continue
		}
		wantErr := ErrCredentials
		if attempt == "denied-second-scope" {
			wantErr = ErrDenied
		}
		if !errors.Is(err, wantErr) || !before.Same(f.image(t).Generation()) {
			t.Fatalf("%s changed durable authority or returned wrong denial: %v", attempt, err)
		}
	}
}

func TestCurrentAllOriginalFinalExpiryAndNoop(t *testing.T) {
	for _, kind := range []string{"sa-token", "cookie-idle", "jwt"} {
		for _, changed := range []bool{false, true} {
			t.Run(kind+"/changed="+map[bool]string{true: "true", false: "false"}[changed], func(t *testing.T) {
				f, proof, deadline := clockProof(t, kind)
				before := f.image(t).Generation()
				now := f.now
				err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					if err := tx.RequireCommitCheck(func(context.Context) error { now = deadline; return nil }); err != nil {
						return err
					}
					if _, err := ReauthorizeCurrentAllWithClock(ctx, tx, f.deps, proof, []rbac.Attributes{readAttrs(), readAttrs()}, func() time.Time { return now }); err != nil {
						return err
					}
					if changed {
						obj := f.sa.DeepCopy()
						obj.Labels = map[string]string{"source": "current-all"}
						_, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerSystem})
						return err
					}
					return nil
				})
				if !errors.Is(err, ErrCredentials) || !before.Same(f.image(t).Generation()) {
					t.Fatal("exact original expiry admitted mutation/no-op", err)
				}
			})
		}
	}
}

func TestCurrentAllBoundsBeforeAuthentication(t *testing.T) {
	for _, attrs := range [][]rbac.Attributes{nil, make([]rbac.Attributes, 1002), {{Resource: strings.Repeat("x", 65), Verb: "read"}}} {
		if _, err := ReauthorizeCurrentAllWithClock(t.Context(), nil, Dependencies{}, Credential{}, attrs, nil); !errors.Is(err, ErrBounds) {
			t.Fatal("unbounded attrs reached authentication", err)
		}
	}
}

func TestCurrentAllBoundedLargeSelectionChecksEveryScope(t *testing.T) {
	for _, deniedLast := range []bool{false, true} {
		f := newFixture(t)
		proof := Credential{Kind: Password, Username: "alice", Value: "correct-password"}
		counter := &countedPasswordVerifier{PasswordVerifier: f.deps.Hasher}
		f.deps.Hasher = counter
		attrs := make([]rbac.Attributes, 1001)
		for i := range attrs {
			attrs[i] = readAttrs()
		}
		if deniedLast {
			attrs[len(attrs)-1].Verb = "write"
		}
		err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := ReauthorizeCurrentAllWithClock(ctx, tx, f.deps, proof, attrs, func() time.Time { return f.now })
			return err
		})
		if counter.calls != 1 || (deniedLast && !errors.Is(err, ErrDenied)) || (!deniedLast && err != nil) {
			t.Fatal("large selection skipped a scope or repeated authentication", err, counter.calls)
		}
	}
}
