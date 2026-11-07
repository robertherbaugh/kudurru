package servingauth

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Every setup uses real token hashes, enrolled sealed cookies, or signed JWTs.
func clockProof(t *testing.T, kind string) (*fixture, Credential, time.Time) {
	t.Helper()
	if kind == "jwt" || kind == "external-token" || kind == "external-cookie-idle" || kind == "external-cookie-absolute" {
		e := newExternalFixture(t)
		if kind == "jwt" {
			e.claims["nbf"] = e.now.Unix()
			return e.fixture, Credential{Kind: OIDCJWT, Value: e.sign(t, e.claims)}, e.now.Add(5 * time.Minute)
		}
		cookie := e.enroll(t, false)
		state := e.state(t)
		if kind == "external-token" {
			i := state.ExternalIdentities[0]
			wire, id := tokens.Generate()
			deadline := e.now.Add(time.Minute)
			apply(t, e.st, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(e.now)}, Spec: internalv1.APITokenSpec{
				Owner:      internalv1.TokenOwner{Kind: "User", Name: i.Username, ID: (oidccore.IdentityKey{Provider: i.Provider.ID, Issuer: i.Issuer, Subject: i.Subject}).String(), Provider: i.Provider.ID, UID: i.UID},
				SecretHash: tokens.Hash(wire), Grants: []string{"viewer"}, Scopes: []string{"viewer"}, ExpiresAt: &metav1.Time{Time: deadline},
			}})
			return e.fixture, Credential{Kind: APIToken, Value: wire}, deadline
		}
		deadline := state.ExternalSessions[0].IdleExpiresAt
		if kind == "external-cookie-absolute" {
			state.ExternalSessions[0].IdleExpiresAt = state.ExternalSessions[0].ExpiresAt
			state.Epoch++
			if err := e.st.Command(t.Context(), e.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				return writeTxSecurity(ctx, tx, state)
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			cookie, err = e.view(t).SealSession("external-session-id", e.csrf)
			if err != nil {
				t.Fatal(err)
			}
			deadline = state.ExternalSessions[0].ExpiresAt
		}
		return e.fixture, Credential{Kind: Cookie, Value: cookie}, deadline
	}
	f := newFixture(t)
	if kind == "sa-token" || kind == "local-token" {
		wire := f.wire
		if kind == "local-token" {
			wire = f.localWire
		}
		id, _ := tokens.Parse(wire)
		token := &internalv1.APIToken{}
		if err := f.st.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, token); err != nil {
			t.Fatal(err)
		}
		deadline := f.now.Add(time.Minute)
		token.Spec.ExpiresAt = &metav1.Time{Time: deadline}
		apply(t, f.st, token)
		return f, Credential{Kind: APIToken, Value: wire}, deadline
	}
	deadline := f.security.Sessions[0].IdleExpiresAt
	if kind == "cookie-absolute" {
		f.security.Sessions[0].IdleExpiresAt = f.security.Sessions[0].ExpiresAt
		f.security.Epoch++
		f.writeSecurity(t)
		deadline = f.security.Sessions[0].ExpiresAt
	}
	cookie, err := f.view(t).SealSession("abcdefghijklmnop", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	return f, Credential{Kind: Cookie, Value: cookie}, deadline
}

func TestCurrentClockFinalExpiryRollsBackAndRejectsNoopDedup(t *testing.T) {
	for _, kind := range []string{"sa-token", "local-token", "cookie-idle", "cookie-absolute", "jwt", "external-token", "external-cookie-idle", "external-cookie-absolute"} {
		for _, operation := range []string{"changed", "noop", "dedup"} {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				f, proof, deadline := clockProof(t, kind)
				if operation == "dedup" {
					if err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						return tx.PutSection(ctx, "dedup", replication.Entry{Key: "command", Data: []byte(`{"result":"ready"}`)})
					}); err != nil {
						t.Fatal(err)
					}
				}
				before := f.image(t)
				now := f.now
				returned, final, calls := false, false, 0
				clock := func() time.Time { calls++; return now }
				err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					defer func() { returned = true }()
					// Runs after callback return, including no-op/dedup. This
					// advances only memory; no transaction reads in final hooks.
					if err := tx.RequireCommitCheck(func(context.Context) error {
						if !returned {
							t.Error("final check ran before callback return")
						}
						final = true
						now = deadline
						return nil
					}); err != nil {
						return err
					}
					_, d, err := ReauthorizeCurrentWithClock(ctx, tx, f.deps, proof, readAttrs(), clock)
					if err != nil || !d.Allowed {
						t.Fatalf("initial proof denied: %v", err)
					}
					if operation == "noop" {
						return nil
					}
					if operation == "dedup" {
						rows, err := tx.ListSections(ctx, "dedup")
						if err != nil || len(rows) != 1 {
							t.Fatalf("dedup fixture missing: %v", err)
						}
						return nil
					}
					user := f.user.DeepCopy()
					user.Labels = map[string]string{"clock-test": "staged"}
					if _, err := tx.Apply(ctx, user, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
						return err
					}
					rows, err := tx.ListSections(ctx, "security")
					if err != nil {
						return err
					}
					security, err := DecodeSecurity(rows)
					if err != nil {
						return err
					}
					security.Epoch++
					if err = writeTxSecurity(ctx, tx, security); err != nil {
						return err
					}
					return tx.PutSection(ctx, "dedup", replication.Entry{Key: "command", Data: []byte(`{"result":"staged"}`)})
				})
				if !errors.Is(err, ErrCredentials) || !final || calls != 2 {
					t.Fatalf("expired final proof admitted or reverified: err=%v, final=%v, clock calls=%d", err, final, calls)
				}
				after := f.image(t)
				if !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
					t.Fatal("failed final auth leaked objects, security or dedup")
				}
			})
		}
	}
}

func TestCurrentClockSuccessRegressionAndDefault(t *testing.T) {
	for _, kind := range []string{"sa-token", "cookie-idle", "jwt", "external-cookie-idle"} {
		t.Run(kind, func(t *testing.T) {
			f, proof, deadline := clockProof(t, kind)
			for _, outcome := range []string{"before-expiry", "clock-regression", "zero-clock"} {
				t.Run(outcome, func(t *testing.T) {
					calls := 0
					err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						_, _, err := ReauthorizeCurrentWithClock(ctx, tx, f.deps, proof, readAttrs(), func() time.Time {
							calls++
							if calls == 1 {
								return f.now
							}
							switch outcome {
							case "clock-regression":
								return f.now.Add(-time.Nanosecond)
							case "zero-clock":
								return time.Time{}
							default:
								return deadline.Add(-time.Nanosecond)
							}
						})
						return err
					})
					if calls != 2 || outcome == "before-expiry" && err != nil || outcome != "before-expiry" && !errors.Is(err, ErrCredentials) {
						t.Fatalf("unexpected final time outcome: %v, calls=%d", err, calls)
					}
				})
			}
		})
	}
	f := newFixture(t)
	for _, clock := range []func() time.Time{nil, func() time.Time { return f.now.Add(24 * time.Hour) }} {
		err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, _, err := ReauthorizeCurrentWithClock(ctx, tx, f.deps, Credential{Kind: APIToken, Value: f.wire}, readAttrs(), clock)
			return err
		})
		if err != nil {
			t.Fatal("default clock or nonexpiring token denied", err)
		}
	}
}

func TestCurrentClockStagedExtensionCannotReplaceOriginalTokenDeadline(t *testing.T) {
	f, proof, deadline := clockProof(t, "sa-token")
	before := f.image(t)
	now := f.now
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, _, err := ReauthorizeCurrentWithClock(ctx, tx, f.deps, proof, readAttrs(), func() time.Time { return now }); err != nil {
			return err
		}
		token := &internalv1.APIToken{}
		if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: f.token.Name}, token); err != nil {
			return err
		}
		token.Spec.ExpiresAt = &metav1.Time{Time: deadline.Add(time.Hour)}
		if _, err := tx.Apply(ctx, token, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		now = deadline
		return nil
	})
	after := f.image(t)
	if !errors.Is(err, ErrCredentials) || !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
		t.Fatal("staged extension replaced verified token cut", err)
	}
}

func TestCurrentClockEvidenceSurvivesViewReleaseWithoutLiveDependencies(t *testing.T) {
	for _, kind := range []string{"sa-token", "cookie-idle", "jwt", "external-cookie-idle"} {
		t.Run(kind, func(t *testing.T) {
			f, proof, deadline := clockProof(t, kind)
			view := f.view(t)
			var validity credentialValidity
			if err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, evidence, err := authenticateCurrentWithValidity(ctx, tx, view, proof, f.now)
				validity = evidence
				return err
			}); err != nil {
				t.Fatal(err)
			}
			view.Release()
			// Destroy the former dependency graph and close actual SQLite;
			// retained validity must do no crypto, policy, View or Store access.
			view.cut, view.providerViews, view.sealer, view.evidenceKeys = nil, nil, nil, nil
			if err := f.st.Close(); err != nil {
				t.Fatal(err)
			}
			if err := validity.validAt(deadline.Add(-time.Nanosecond)); err != nil {
				t.Fatal("released View ended private evidence", err)
			}
			if err := validity.validAt(deadline); !errors.Is(err, ErrCredentials) {
				t.Fatal("released evidence lost expiry", err)
			}
		})
	}
	for _, evidence := range []credentialValidity{{}, {kind: OIDCJWT, authenticatedAt: time.Now()}, {kind: Cookie, authenticatedAt: time.Now()}} {
		if err := evidence.validAt(time.Now()); !errors.Is(err, ErrCredentials) {
			t.Fatal("zero or incomplete private evidence accepted", err)
		}
	}
}

type countedPasswordVerifier struct {
	PasswordVerifier
	calls int
}

func (v *countedPasswordVerifier) Verify(ctx context.Context, phc, password string) (bool, error) {
	v.calls++
	return v.PasswordVerifier.Verify(ctx, phc, password)
}

func TestCurrentClockPasswordFactorConsumedOnceAndFinalRollback(t *testing.T) {
	f := newFixture(t)
	seed := setupTOTP(t, f)
	counter := &countedPasswordVerifier{PasswordVerifier: f.deps.Hasher}
	f.deps.Hasher = counter
	proof := Credential{Kind: Password, Username: "alice", Value: "correct-password", TOTPCode: factorCode(t, seed, f.now)}
	before := f.image(t).Generation()
	for _, attempt := range []string{"missing-factor", "rollback", "commit", "replay"} {
		startCalls := counter.calls
		calls := 0
		p := proof
		if attempt == "missing-factor" {
			p.TOTPCode = ""
		}
		err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, _, err := ReauthorizeCurrentWithClock(ctx, tx, f.deps, p, readAttrs(), func() time.Time {
				calls++
				if calls == 1 {
					return f.now
				}
				if attempt == "rollback" {
					return f.now.Add(-time.Nanosecond)
				}
				return f.now.Add(2 * time.Minute)
			})
			return err
		})
		if counter.calls-startCalls != 1 {
			t.Fatal("final hook repeated password verifier")
		}
		if attempt != "commit" {
			if !errors.Is(err, ErrCredentials) {
				t.Fatalf("invalid factor/clock accepted: %s, %v", attempt, err)
			}
			if attempt != "replay" && !before.Same(f.image(t).Generation()) {
				t.Fatal("denial consumed factor floor")
			}
			continue
		}
		if err != nil || calls != 2 {
			t.Fatal("commit rechecked TOTP window/consumed twice", err)
		}
		state, err := DecodeSecurity(f.image(t).Payload.Security)
		if err != nil {
			t.Fatal(err)
		}
		step, err := totp.Match(seed, proof.TOTPCode, f.now)
		if err != nil || state.Epoch != f.security.Epoch+1 || !state.TOTP[0].HasConsumed || state.TOTP[0].LastConsumedStep != step {
			t.Fatal("factor not consumed exactly once", err)
		}
	}
}

func TestCurrentClockCheckRegistrationPoisonAndCallbackAbort(t *testing.T) {
	f := newFixture(t)
	proof := Credential{Kind: APIToken, Value: f.wire}
	before := f.image(t).Generation()
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for range sqlite.MaxCommitChecks {
			if err := tx.RequireCommitCheck(func(context.Context) error { return nil }); err != nil {
				return err
			}
		}
		p, _, err := ReauthorizeCurrentWithClock(ctx, tx, f.deps, proof, readAttrs(), func() time.Time { return f.now })
		if !errors.Is(err, replication.ErrInvalid) || p.ID != "" {
			t.Fatal("overflow registration returned successful principal", err)
		}
		return nil // ignored failure must still poison the actual command
	})
	if !errors.Is(err, replication.ErrInvalid) || !before.Same(f.image(t).Generation()) {
		t.Fatal("ignored final registration failure admitted command", err)
	}
	abort := errors.New("callback aborted")
	calls := 0
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, _, err := ReauthorizeCurrentWithClock(ctx, tx, f.deps, proof, readAttrs(), func() time.Time { calls++; return f.now }); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) || calls != 1 {
		t.Fatal("callback abort ran retained proof elsewhere", err)
	}
	if _, _, err = ReauthorizeCurrentWithClock(t.Context(), nil, f.deps, proof, readAttrs(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil transaction accepted", err)
	}
}
