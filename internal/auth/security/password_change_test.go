package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestPasswordChangeAdmissionLateFailureAndSingleVerification(t *testing.T) {
	f, deps, proof := factorFixture(t)
	var old Enrollment
	at := f.now.Add(time.Second)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		old, err = IssueLocal(ctx, tx, deps, proof, at)
		return err
	})
	cookie, session, err := old.Seal(factorView(t, f.st, deps), at)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	hasher := &loginAdmissionHasher{base: deps.Hasher}
	deps.Hasher = hasher
	fault := errors.New("test: password change final refusal")
	for index, mode := range []string{"callback-error", "final-error", "clock-rollback", "poison", "original-canceled", "success"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			prepared, err := PrepareLocalPasswordChange(ctx, users.NewHasher(1), "test-local-password-1", "replacement-password-1", "", false)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(prepared)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{fmt.Sprint(prepared), fmt.Sprintf("%#v", prepared), string(encoded)} {
				if strings.Contains(text, prepared.state.current) || strings.Contains(text, prepared.state.hash) {
					t.Fatal("preparation diagnostic exposed credentials")
				}
			}
			now := at.Add(time.Second)
			clock := func() time.Time { now = now.Add(time.Nanosecond); return now }
			var result PasswordChangeResult
			reached := false
			err = f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var refused, err error
				result, refused, err = ChangeLocalPasswordWithAdmission(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Cookie, Value: cookie}, session.Principal, prepared, "192.0.2.5", clock)
				if refused != nil || err != nil || result.Enrollment.ID() == "" {
					t.Fatal("did not reach staged password and enrollment", refused, err)
				}
				reached = true
				switch mode {
				case "callback-error":
					return fault
				case "final-error":
					return tx.RequireCommitCheck(func(context.Context) error { return fault })
				case "clock-rollback":
					now = now.Add(-time.Hour)
				case "poison":
					_ = tx.PutSection(ctx, "invalid-section", replication.Entry{Key: "bad", Data: []byte(`{}`)})
				case "original-canceled":
					cancel()
				}
				return nil
			})
			after, readErr := f.st.ReadGeneration(t.Context())
			if readErr != nil || !reached || hasher.calls != index+1 {
				t.Fatal("target missing or current password verified more than once", readErr, hasher.calls)
			}
			if mode != "success" {
				want := fault
				switch mode {
				case "clock-rollback":
					want = servingauth.ErrCredentials
				case "poison":
					want = replication.ErrInvalid
				case "original-canceled":
					want = context.Canceled
				}
				if !errors.Is(err, want) || !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
					t.Fatal("late failure partly committed password or counters", err)
				}
				return
			}
			if err != nil || after.Revision != before.Revision+1 {
				t.Fatal("password and admission did not commit together", err)
			}
			view := factorView(t, f.st, deps)
			if _, _, err := result.Enrollment.Seal(view, clock()); err != nil {
				t.Fatal("replacement enrollment unavailable", err)
			}
			if _, err := view.CookieSession(cookie, clock()); err == nil {
				t.Fatal("old session survived password change")
			}
			err = f.st.Command(ctx, f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, refused, err := ChangeLocalPasswordWithAdmission(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Cookie, Value: cookie}, session.Principal, prepared, "192.0.2.5", clock)
				if refused != nil {
					t.Fatal("consumed preparation became a fresh guess")
				}
				return err
			})
			if !errors.Is(err, servingauth.ErrInvalid) || hasher.calls != index+1 {
				t.Fatal("consumed preparation was reused", err)
			}
		})
	}
}
