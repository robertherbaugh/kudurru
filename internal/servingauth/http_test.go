package servingauth

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestHTTPDetachedCapturedObjectsAndSessions(t *testing.T) {
	f := newFixture(t)
	v := f.view(t)
	if _, err := v.Objects("Kudurru"); !errors.Is(err, ErrUnsupported) {
		t.Fatal("settings outside IAM allowlist", err)
	}
	if _, err := v.Objects("Secret"); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unknown kind accepted", err)
	}
	objects, err := v.Objects("LocalUser")
	if err != nil || len(objects) != 1 {
		t.Fatal("missing captured account", err)
	}
	objects[0].(*internalv1.LocalUser).Spec.Groups[0] = "tampered"
	local, external, err := v.SessionRecords()
	if err != nil || len(local) != 1 || len(external) != 0 {
		t.Fatal("wrong captured inventory", err)
	}
	local[0].Grants[0] = "admin"
	local[0].CSRFHash = "tampered"
	// Destroy the live store; projections must still use the captured image.
	if err = f.st.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := v.Objects("LocalUser")
	if err != nil || again[0].(*internalv1.LocalUser).Spec.Groups[0] != "operators" {
		t.Fatal("returned mutable account", err)
	}
	local, _, err = v.SessionRecords()
	if err != nil || local[0].Grants[0] != "viewer" || local[0].CSRFHash != CSRFHash(f.csrf) {
		t.Fatal("returned mutable enrollment", err)
	}
	p, err := v.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	roles, err := v.Roles(p, f.now)
	if err != nil || !slices.Equal(roles, []string{"viewer"}) {
		t.Fatal("token scope broadened", err)
	}
	v.Release()
	if _, err = v.Objects("LocalUser"); err == nil {
		t.Fatal("released objects accepted")
	}
	if _, _, err = v.SessionRecords(); err == nil {
		t.Fatal("released enrollment accepted")
	}
	if _, err = v.Roles(p, f.now); err == nil {
		t.Fatal("released roles accepted")
	}
}
func TestHTTPRolesExternalFrozenIntersection(t *testing.T) {
	e := newExternalFixture(t)
	cookie := e.enroll(t, false)
	v := e.view(t)
	p, err := v.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, e.now)
	if err != nil {
		t.Fatal(err)
	}
	roles, err := v.Roles(p, e.now)
	if err != nil || !slices.Equal(roles, []string{"viewer"}) {
		t.Fatal("wrong external frozen roles", err)
	}
	// A held old View remains its exact historical cut; new floors revoke
	// the new View without manufacturing replacement cookie authority.
	state := e.state(t)
	state.Epoch++
	state.GrantFloors = append(state.GrantFloors, GrantFloor{FormatVersion: 1, Grant: "viewer", ValidAfter: p.GrantsIssuedAt})
	if err := e.st.Command(t.Context(), e.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return writeTxSecurity(ctx, tx, state) }); err != nil {
		t.Fatal(err)
	}
	roles, err = v.Roles(p, e.now)
	if err != nil || !slices.Equal(roles, []string{"viewer"}) {
		t.Fatal("new floor mutated held old cut", err)
	}
	roles, err = e.view(t).Roles(p, e.now)
	if err == nil && len(roles) != 0 {
		t.Fatal("revoked frozen grant survived")
	}

}
func TestHTTPAuthenticateCurrentClockRollbackExpiryAndLifetime(t *testing.T) {
	for _, kind := range []string{"local-token", "cookie-idle", "jwt", "external-cookie-idle"} {
		for _, mode := range []string{"changed", "noop", "backward"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				f, proof, deadline := clockProof(t, kind)
				before := f.image(t)
				now := f.now
				calls := 0
				err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					if err := tx.RequireCommitCheck(func(context.Context) error {
						if mode == "backward" {
							now = f.now.Add(-time.Nanosecond)
						} else {
							now = deadline
						}
						return nil
					}); err != nil {
						return err
					}
					p, err := AuthenticateCurrentWithClock(ctx, tx, f.deps, proof, func() time.Time { calls++; return now })
					if err != nil || p.ID == "" {
						t.Fatal("current authentication failed", err)
					}
					if mode == "changed" {
						u := f.user.DeepCopy()
						u.Labels = map[string]string{"http": "staged"}
						_, err = tx.Apply(ctx, u, store.ApplyOptions{Manager: store.ManagerSystem})
						return err
					}
					return nil
				})
				after := f.image(t)
				if !errors.Is(err, ErrCredentials) || calls != 2 || !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
					t.Fatal("final proof time escaped command", err, calls)
				}
			})
		}
	}
	f := newFixture(t)
	u := f.user.DeepCopy()
	u.Spec.MustChangePassword = true
	apply(t, f.st, u)
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		p, err := AuthenticateCurrentWithClock(ctx, tx, f.deps, Credential{Kind: Password, Username: u.Name, Value: "correct-password"}, func() time.Time { return f.now })
		if err != nil || !p.MustChangePassword {
			t.Fatal("self-service denied", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AuthenticateCurrentWithClock(t.Context(), nil, f.deps, Credential{}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("invalid transaction accepted", err)
	}
}
func TestHTTPAuthenticateCurrentConsumesFactorOnce(t *testing.T) {
	f := newFixture(t)
	seed := setupTOTP(t, f)
	counter := &countedPasswordVerifier{PasswordVerifier: f.deps.Hasher}
	f.deps.Hasher = counter
	proof := Credential{Kind: Password, Username: f.user.Name, Value: "correct-password", TOTPCode: factorCode(t, seed, f.now)}
	for _, mode := range []string{"rollback", "commit", "replay"} {
		before := f.image(t)
		calls := counter.calls
		n := 0
		err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			p, err := AuthenticateCurrentWithClock(ctx, tx, f.deps, proof, func() time.Time {
				n++
				if mode == "rollback" && n > 1 {
					return f.now.Add(-time.Nanosecond)
				}
				return f.now
			})
			if err == nil && p.Kind != auth.KindUser {
				t.Fatal("wrong principal")
			}
			return err
		})
		if counter.calls-calls != 1 {
			t.Fatal("factor/password verified more than once")
		}
		if mode == "commit" {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, ErrCredentials) || !before.Generation().Same(f.image(t).Generation()) {
			t.Fatal("factor failure changed state", err)
		}
	}
}
