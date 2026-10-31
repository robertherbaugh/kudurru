package servinghttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestHTTPWriterUsesOriginalLeaseThroughFinalCommand(t *testing.T) {
	f := newFixture(t)
	token, err := f.store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	target := serving.TermActivation{Term: token.Term(), Incarnation: token.Incarnation()}
	if err := CheckWriter(t.Context(), f.publisher, target); !errors.Is(err, serving.ErrReleased) {
		t.Fatal("missing request lease accepted", err)
	}
	var held context.Context
	response := httptest.NewRecorder()
	f.adapter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		held = r.Context()
		if err := CheckWriter(held, f.publisher, target); err != nil {
			t.Fatal("current original request rejected", err)
		}
		wrong := target
		wrong.Incarnation++
		if err := CheckWriter(held, f.publisher, wrong); !errors.Is(err, serving.ErrSuperseded) {
			t.Fatal("successor writer adopted", err)
		}
		canceled, cancel := context.WithCancel(held)
		cancel()
		if err := CheckWriter(canceled, f.publisher, target); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled request accepted", err)
		}
		// Use an actual complete commit under the publication gate. The final
		// check must not acquire that gate again or read SQLite while it is held.
		ctx := sqlite.WithWriterToken(held, token)
		err := f.publisher.WithWriterGate(ctx, target, func() error {
			return f.store.Command(ctx, target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var cfg public.Kudurru
				if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &cfg); err != nil {
					return err
				}
				cfg.Labels = map[string]string{"command": "changed"}
				if _, err := tx.Apply(ctx, &cfg, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(ctx context.Context) error { return CheckWriter(ctx, f.publisher, target) })
			})
		})
		if err != nil {
			t.Fatal("pinned final check failed", err)
		}
		if err := CheckWriter(held, f.publisher, target); !errors.Is(err, serving.ErrPending) {
			t.Fatal("old request survived changed durable base", err)
		}
		f.waitCurrent(t)
		if err := CheckWriter(held, f.publisher, target); !errors.Is(err, serving.ErrPending) {
			t.Fatal("old request adopted replacement publication", err)
		}
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(response, f.request(http.MethodGet))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	if err := CheckWriter(held, f.publisher, target); !errors.Is(err, serving.ErrReleased) {
		t.Fatal("released request remained usable", err)
	}
	f.adapter.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := CheckWriter(r.Context(), f.publisher, target); err != nil {
			t.Fatal("fresh original lease rejected", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.request(http.MethodGet))
}
