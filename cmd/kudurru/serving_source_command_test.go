package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const servingSourcePath = "/api/v1/blocklists/default/source-command/sources/inline/entries"

func servingSourceList() *public.BlockList {
	return &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "source-command", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "inline", Format: "domains", Inline: []string{"alpha.example", "beta.example"}}}}}
}

func servingSourceFixture(t *testing.T, f *servingCommandFixture, command api.ObjectCommand) *api.Server {
	t.Helper()
	if _, err := f.s.opts.Store.Apply(t.Context(), servingSourceList(), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
	if command == nil {
		command = f.s.ObjectCommand
	}
	srv, err := api.New(api.Options{
		Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config,
		ObjectCommand: command, Now: f.s.opts.Now, Authorizer: f.http,
		Authenticate: func(next http.Handler) http.Handler { return f.http.Middleware(servingMutationRoutes(next)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestServingSourceCommandActualSQLite(t *testing.T) {
	f := newServingCommandFixture(t)
	defer f.stop()
	srv := servingSourceFixture(t, f, nil)
	request := func(patch api.SourceEntriesPatch, match string, want int) api.SourceEntriesResult {
		t.Helper()
		w := servingObjectRequest(t, f, srv, http.MethodPatch, servingSourcePath, patch, match)
		if w.Code != want {
			t.Fatal("source patch failed", w.Code, w.Body)
		}
		var result api.SourceEntriesResult
		if want == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.ResourceVersion != w.Header().Get("ETag") {
				t.Fatal("source response lacks committed receipt", err)
			}
			awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		}
		return result
	}
	first := request(api.SourceEntriesPatch{Add: []string{"gamma.example"}, Disable: []string{"beta.example"}}, "", http.StatusOK)
	if !slices.Equal(first.Added, []string{"gamma.example"}) || first.Excluded != 1 || first.Total != 3 {
		t.Fatal("source command response differs", first)
	}
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	unchanged := request(api.SourceEntriesPatch{Add: []string{"gamma.example"}}, first.ResourceVersion, http.StatusOK)
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) || unchanged.ResourceVersion != first.ResourceVersion || len(unchanged.Added) != 0 {
		t.Fatal("source no-op changed durable state", err)
	}
	request(api.SourceEntriesPatch{Add: []string{"gamma.example"}}, "stale", http.StatusPreconditionFailed)
	request(api.SourceEntriesPatch{Remove: []string{"alpha.example"}, Enable: []string{"beta.example"}}, first.ResourceVersion, http.StatusOK)
	var got public.BlockList
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(servingSourceList()), &got); err != nil || !slices.Equal(got.Spec.Sources[0].Inline, []string{"beta.example", "gamma.example"}) || len(got.Spec.Sources[0].Exclude) != 0 {
		t.Fatal("source command did not commit exact entries", err)
	}
	// The query-log alias must not reach its legacy domain writer, even with
	// an actual authenticated administrator and an absent log row.
	w := servingObjectRequest(t, f, srv, http.MethodPost, "/api/v1/queries/42/actions", map[string]string{"action": "allow"}, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("query action escaped the legacy mutation gate", w.Code)
	}
}

func TestServingSourceCommandFinalAdmission(t *testing.T) {
	for _, mode := range []string{"expiry", "regression", "writer"} {
		for _, changed := range []bool{false, true} {
			name := mode + "/noop"
			if changed {
				name = mode + "/changed"
			}
			t.Run(name, func(t *testing.T) {
				f := newServingCommandFixture(t)
				defer f.stop()
				entered := 0
				command := func(ctx context.Context, key store.Key, fn func(context.Context, *sqlite.CommandTx, api.ObjectAuthorize) error) error {
					return f.s.ObjectCommand(ctx, key, func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
						entered++
						if err := fn(ctx, tx, authorize); err != nil {
							return err
						}
						switch mode {
						case "expiry":
							f.offset.Store(int64(f.expires.Sub(f.now)))
						case "regression":
							f.offset.Store(int64(-time.Nanosecond))
						case "writer":
							f.cancel()
						}
						return nil
					})
				}
				srv := servingSourceFixture(t, f, command)
				before, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				value := "alpha.example"
				if changed {
					value = "new.example"
				}
				w := servingObjectRequest(t, f, srv, http.MethodPatch, servingSourcePath, api.SourceEntriesPatch{Add: []string{value}}, "")
				if w.Code < 400 || entered != 1 {
					t.Fatal("expired command succeeded or retried", w.Code, entered)
				}
				f.stop()
				after, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil || !before.Generation().Same(after.Generation()) {
					t.Fatal("failed final source admission changed generation", err)
				}
			})
		}
	}
}

func TestServingSourceCommandRetainsOriginalRequest(t *testing.T) {
	f := newServingCommandFixture(t)
	defer f.stop()
	entered := 0
	command := func(ctx context.Context, key store.Key, fn func(context.Context, *sqlite.CommandTx, api.ObjectAuthorize) error) error {
		// After HTTP capture, replace the exact list incarnation and publish it.
		// Current command admission must refuse; no successor capture can repair it.
		if err := f.s.opts.Store.Delete(t.Context(), key, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.opts.Store.Apply(t.Context(), servingSourceList(), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
		awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
		return f.s.ObjectCommand(ctx, key, func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
			entered++
			return fn(ctx, tx, authorize)
		})
	}
	srv := servingSourceFixture(t, f, command)
	w := servingObjectRequest(t, f, srv, http.MethodPatch, servingSourcePath, api.SourceEntriesPatch{Add: []string{"new.example"}}, "")
	if w.Code != http.StatusServiceUnavailable || entered != 0 {
		t.Fatal("successor repaired original source command", w.Code, entered)
	}
	var got public.BlockList
	if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(servingSourceList()), &got); err != nil || slices.Contains(got.Spec.Sources[0].Inline, "new.example") {
		t.Fatal("stale command edited replacement incarnation", err)
	}
}
