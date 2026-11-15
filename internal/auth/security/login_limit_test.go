package security

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type loginAdmissionHasher struct {
	base  servingauth.PasswordVerifier
	calls int
}

func (h *loginAdmissionHasher) Verify(ctx context.Context, phc, password string) (bool, error) {
	h.calls++
	return h.base.Verify(ctx, phc, password)
}

func TestLoginAdmissionDurableFailuresRestartAndNoSecondVerification(t *testing.T) {
	f := newSessionFixture(t)
	deps := loginDependencies(t, f)
	hasher := &loginAdmissionHasher{base: deps.Hasher}
	deps.Hasher = hasher
	before, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var refusal error
	for range 5 {
		var enrolled Enrollment
		err = f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var e error
			enrolled, refusal, e = IssueLocalWithAdmission(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: " ALICE ", Value: "incorrect"}, "192.0.2.1", func() time.Time { return f.now })
			if !servingauth.IsReadOnlyPasswordRefusal(refusal, tx) {
				t.Fatal("failure lost pre-mutation evidence", refusal)
			}
			return e
		})
		if err != nil || !errors.Is(refusal, servingauth.ErrCredentials) || enrolled.ID() != "" {
			t.Fatal("wrong password received success or lost counter commit", err, refusal)
		}
	}
	after, err := f.st.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision+5 || !reflect.DeepEqual(before.Payload.Security, after.Payload.Security) || !reflect.DeepEqual(before.Payload.Objects, after.Payload.Objects) {
		t.Fatal("rejected passwords changed authentication state or failed to count")
	}
	if err := sqliteadapter.ValidateImage(after); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openSecurityStore(t, f.path)
	err = f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, refusal, err = IssueLocalWithAdmission(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "alice", Value: "test-local-password-1"}, "192.0.2.1", func() time.Time { return f.now })
		return err
	})
	var locked *loginlimit.Locked
	if err != nil || !errors.As(refusal, &locked) || hasher.calls != 5 {
		t.Fatal("restart forgot lock or repeated verifier", err, refusal, hasher.calls)
	}
	last, err := f.st.ReadGeneration(ctx)
	if err != nil || !last.Generation().Same(after.Generation()) {
		t.Fatal("locked read mutated source", err)
	}
}

func TestLoginAdmissionEnrollmentAndFinalFailureAreAtomic(t *testing.T) {
	f := newSessionFixture(t)
	deps := loginDependencies(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	proof := servingauth.Credential{Kind: servingauth.Password, Username: "alice", Value: "test-local-password-1"}
	before, err := f.st.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("test: original final guard refused")
	for _, mode := range []string{"final-error", "clock-rollback", "poison", "success"} {
		t.Run(mode, func(t *testing.T) {
			var enrolled Enrollment
			var refused error
			now := f.now.Add(time.Second)
			reached := false
			err := f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var e error
				enrolled, refused, e = IssueLocalWithAdmission(ctx, tx, deps, proof, "192.0.2.2", func() time.Time { return now })
				if e != nil {
					return e
				}
				reached = true
				if mode == "final-error" {
					return tx.RequireCommitCheck(func(context.Context) error { return fault })
				}
				if mode == "clock-rollback" {
					now = now.Add(-time.Nanosecond)
				}
				if mode == "poison" {
					_ = tx.PutSection(ctx, "invalid-section", replication.Entry{Key: "bad", Data: []byte(`{}`)})
				}
				return nil
			})
			after, e := f.st.ReadGeneration(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if mode != "success" {
				want := fault
				if mode == "clock-rollback" {
					want = loginlimit.ErrState
				}
				if mode == "poison" {
					want = replication.ErrInvalid
				}
				if !reached || !errors.Is(err, want) || !after.Generation().Same(before.Generation()) || !reflect.DeepEqual(after.Payload, before.Payload) {
					t.Fatal("refused command partly committed", err)
				}
				return
			}
			if err != nil || refused != nil {
				t.Fatal(err, refused)
			}
			if _, _, err := enrolled.Seal(f.view(t), now); err != nil {
				t.Fatal("committed enrollment unavailable", err)
			}
			found := false
			for _, row := range after.Payload.Runtime {
				if row.Key == sqliteadapter.Key {
					found = true
					if bytes.Contains(row.Data, []byte("alice")) {
						t.Fatal("raw username persisted")
					}
				}
			}
			if !found || after.Revision != before.Revision+1 {
				t.Fatal("counter and enrollment did not share one commit")
			}
		})
	}
}

func TestLoginAdmissionPoisonAndMalformedHistoryCannotCommit(t *testing.T) {
	f := newSessionFixture(t)
	deps := loginDependencies(t, f)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, sqliteadapter.Section, replication.Entry{Key: sqliteadapter.Key, Data: []byte(`{"version":99}`)})
	})
	before, err := f.st.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqliteadapter.ValidateImage(before); err == nil {
		t.Fatal("malformed replica history accepted")
	}
	err = f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, _, err := IssueLocalWithAdmission(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "alice", Value: "test-local-password-1"}, "192.0.2.3", func() time.Time { return f.now })
		return err
	})
	if !errors.Is(err, loginlimit.ErrUnavailable) {
		t.Fatal("bad history admitted", err)
	}
	after, err := f.st.ReadGeneration(ctx)
	if err != nil || !after.Generation().Same(before.Generation()) {
		t.Fatal("bad history repaired", err)
	}
	if servingauth.IsReadOnlyPasswordRefusal(servingauth.ErrCredentials, nil) {
		t.Fatal("ordinary error obtained refusal evidence")
	}
}
