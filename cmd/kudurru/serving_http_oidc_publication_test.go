package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestServingHTTPBrowserStartOwnCommit(t *testing.T) {
	f := newServingBrowserFixture(t)
	b := f.h.opts.ProviderHandler.(*servingBrowserHTTP)
	r := httptest.NewRequest(http.MethodGet, "http://kudurru.test/api/v1/auth/oidc/corp/start?next=/", nil)
	r.SetPathValue("idp", "corp")
	r.RemoteAddr = "192.0.2.20:3131"
	w := httptest.NewRecorder()
	entered := false
	f.h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered = true
		owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](r.Context(), "runtime")
		if err != nil {
			t.Fatal(err)
		}
		_, _, current := owner.NotifyKeys()
		if !current() || !owner.DependenciesCurrent() {
			t.Fatal("original browser source was not current")
		}
		err = b.start(w, r)
		if current() || !owner.DependenciesCurrent() || len(f.security(t).BrowserAttempts) != 1 {
			t.Fatal("pending commit did not retire only its original source", err)
		}
		if err != nil {
			t.Fatal("own pending commit refused redirect with retained dependencies", err)
		}
	})).ServeHTTP(w, r)
	if !entered || w.Code != http.StatusFound || len(w.Result().Cookies()) != 1 {
		t.Fatal("committed pending attempt did not return its redirect")
	}
}

func TestServingHTTPAuthResponseCompletionRequiresCommit(t *testing.T) {
	for _, mode := range []string{"callback-error", "final-error", "poison"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingHTTPFixture(t, false)
			before, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			fault := errors.New("test: original completion refused")
			entered := false
			f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				calls := 0
				current, err := f.h.currentAuthResponseCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
					calls++
					e, err := security.IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "root-command", Value: "initial-password"}, f.h.now())
					if err != nil || e.ID() == "" {
						t.Fatal("did not reach staged enrollment", err)
					}
					entered = true
					switch mode {
					case "callback-error":
						return fault
					case "final-error":
						return tx.RequireCommitCheck(func(context.Context) error { return fault })
					default:
						_ = tx.PutSection(ctx, "invalid-section", replication.Entry{Key: "bad", Data: []byte(`{}`)})
						return nil
					}
				})
				want := fault
				if mode == "poison" {
					want = replication.ErrInvalid
				}
				if current != nil || !errors.Is(err, want) || calls != 1 {
					t.Fatal("refused command returned completion or replayed", err)
				}
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			after, err := f.s.opts.Store.ReadGeneration(t.Context())
			if err != nil || !entered || !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
				t.Fatal("refused completion changed durable state", err)
			}
		})
	}
}

func TestServingHTTPAuthResponseCompletionRetainsOriginalLifetime(t *testing.T) {
	for _, mode := range []string{"canceled", "writer", "same-key-replacement", "request-release"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingHTTPFixture(t, false)
			var current func() bool
			f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				var err error
				current, err = f.h.currentAuthResponseCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
					_, err := security.IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "root-command", Value: "initial-password"}, f.h.now())
					return err
				})
				if err != nil || current == nil || !current() {
					t.Fatal("actual enrollment did not retain response dependencies", err)
				}
				switch mode {
				case "canceled":
					cancel()
				case "writer":
					f.stop()
				case "same-key-replacement":
					keys := f.s.opts.Keyrings.Capture()
					if err := f.s.opts.Keyrings.Replace(t.Context(), keys.Keys()); err != nil {
						t.Fatal(err)
					}
				case "request-release":
					return
				}
				if current() {
					t.Fatal("response adopted successor authority or renewed lifetime")
				}
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			if current == nil || current() {
				t.Fatal("released request retained response currency")
			}
		})
	}
}

func TestServingHTTPBrowserAcknowledgmentRejectsLaterRevocation(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	entered := false
	f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		entered = true
		var enrolled security.Enrollment
		responseCurrent, err := f.h.currentAuthResponseCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			var err error
			enrolled, err = security.IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "root-command", Value: "initial-password"}, f.h.now())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		acknowledgments := 0
		publishedCurrent, err := f.h.browserPublication(r.Context(), func(v *servingauth.View) (bool, error) {
			if _, _, err := enrolled.Seal(v, f.h.now()); err != nil {
				return false, nil
			}
			acknowledgments++
			return true, nil
		})
		if err != nil || !responseCurrent() || publishedCurrent == nil || !publishedCurrent() || acknowledgments != 1 {
			t.Fatal("exact committed enrollment not acknowledged", err)
		}
		// A separate real revocation advances only canonical state. Original
		// dependencies remain installed, so the acknowledgment source itself
		// must refuse emission. It never triggers another seal or command.
		err = f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			state, err := security.Load(ctx, tx)
			if err != nil {
				return err
			}
			if err := state.Revoke(enrolled.ID()); err != nil {
				return err
			}
			return state.Commit(ctx)
		})
		if err != nil || !responseCurrent() || publishedCurrent() || acknowledgments != 1 {
			t.Fatal("later revocation adopted a new sealing source", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if !entered {
		t.Fatal("request did not reach original command")
	}
}
