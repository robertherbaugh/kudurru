package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestServingHTTPEnrollmentFinalClockRollback(t *testing.T) {
	for _, mode := range []string{"regression", "expiry", "callback-abort"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingHTTPFixture(t, false)
			now := time.Now().UTC()
			observed := now
			f.s.opts.Now = func() time.Time { return now }
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			called := false
			var enrollment security.Enrollment
			abort := errors.New("abort after staging")
			f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				err := f.s.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
					// Registered first so the original enrollment's final clock sees the change.
					if err := tx.RequireCommitCheck(func(context.Context) error {
						if mode == "regression" {
							now = observed.Add(-time.Nanosecond)
						} else {
							now = observed.Add(1000 * time.Hour)
						}
						return nil
					}); err != nil {
						return err
					}
					var err error
					enrollment, err = security.IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "root-command", Value: "initial-password"}, observed)
					if err != nil {
						return err
					}
					called = true
					if err = f.h.enrollmentClock(ctx, tx, enrollment, observed); err != nil {
						return err
					}
					if mode == "callback-abort" {
						return abort
					}
					return nil
				})
				if mode == "callback-abort" {
					if !errors.Is(err, abort) {
						t.Error("callback abort ignored", err)
					}
				} else if !errors.Is(err, servingauth.ErrCredentials) {
					t.Error("final enrollment clock admitted", err)
				}
			})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "http://kudurru.test/api/v1/auth/login", nil))
			if !called {
				t.Fatal("test did not reach real IssueLocal")
			}
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("failed enrollment changed SQLite", err)
			}
			now = observed
			awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
				_, _, err := enrollment.Seal(v, observed)
				return errors.Is(err, servingauth.ErrCredentials)
			})
		})
	}
}
