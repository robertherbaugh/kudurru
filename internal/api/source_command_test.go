package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

const sourceCommandPath = "/api/v1/blocklists/default/captured/sources/original/entries"

var sourceCommandKey = store.Key{Kind: "BlockList", Namespace: "default", Name: "captured"}

func TestSourceCommandUsesOnlyTransactionAndChecksNoop(t *testing.T) {
	calls := 0
	deny := false
	target := "captured"
	h, st := objectCommandHarness(t, func(_ context.Context, attrs []rbac.Attributes) error {
		calls++
		want := []rbac.Attributes{{Resource: "blocklists", Verb: "write", Scope: clusterScope, Namespace: "default", Name: target}}
		if !reflect.DeepEqual(attrs, want) {
			t.Fatal("source command authority differs", attrs)
		}
		if deny {
			return errForbidden(Missing{Resource: "blocklists", Verb: "write", Scope: "cluster"}, "")
		}
		return nil
	})
	list := sourceTestList()
	if _, err := st.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	competing := list.DeepCopy()
	competing.Spec.Sources[0].Inline = []string{"unrelated.example"}
	h.seed(store.ManagerFile("competing.yaml"), competing)
	var result SourceEntriesResult
	r := h.do(http.MethodPatch, sourceCommandPath, "operator", SourceEntriesPatch{Add: []string{"new.example"}, Remove: []string{"alpha.example"}}).want(http.StatusOK)
	r.json(&result)
	var got public.BlockList
	if err := st.Get(t.Context(), sourceCommandKey, &got); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !slices.Equal(got.Spec.Sources[0].Inline, []string{"beta.example", "gamma.example", "new.example"}) || result.ResourceVersion != got.ResourceVersion || r.Header().Get("ETag") != got.ResourceVersion {
		t.Fatal("source edit did not use actual transaction/result", result)
	}
	before, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	h.do(http.MethodPatch, sourceCommandPath, "operator", SourceEntriesPatch{Add: []string{"new.example"}}).want(http.StatusOK).json(&result)
	if calls != 2 || len(result.Added) != 0 || result.ResourceVersion != got.ResourceVersion {
		t.Fatal("no-op skipped authority or changed receipt", result)
	}
	deny = true
	h.do(http.MethodPatch, sourceCommandPath, "operator", SourceEntriesPatch{Add: []string{"new.example"}}).want(http.StatusForbidden)
	target = "missing"
	h.do(http.MethodPatch, "/api/v1/blocklists/default/missing/sources/0/entries", "operator", SourceEntriesPatch{Add: []string{"new.example"}}).want(http.StatusForbidden)
	after, err := st.ReadGeneration(t.Context())
	if err != nil || calls != 4 || !before.Generation().Same(after.Generation()) {
		t.Fatal("no-op/refusal changed durable generation", calls, err)
	}
	if err := h.st.Get(t.Context(), sourceCommandKey, &got); err != nil || !slices.Equal(got.Spec.Sources[0].Inline, []string{"unrelated.example"}) {
		t.Fatal("source command touched unrelated live Store", err)
	}
}

func TestSourceCommandOwnershipCASAndValidation(t *testing.T) {
	for _, owner := range []string{store.ManagerAPI, store.ManagerBootstrap, store.ManagerFile("lists.yaml")} {
		t.Run(owner, func(t *testing.T) {
			h, st := objectCommandHarness(t, nil)
			if _, err := st.Apply(t.Context(), sourceTestList(), store.ApplyOptions{Manager: owner}); err != nil {
				t.Fatal(err)
			}
			var got public.BlockList
			if err := st.Get(t.Context(), sourceCommandKey, &got); err != nil {
				t.Fatal(err)
			}
			rv, uid := got.ResourceVersion, got.UID
			h.do(http.MethodPatch, sourceCommandPath, "operator", SourceEntriesPatch{Add: []string{"alpha.example"}}, "If-Match", "stale").want(http.StatusPreconditionFailed)
			want := http.StatusOK
			if owner == store.ManagerFile("lists.yaml") {
				want = http.StatusConflict
			}
			// Even unchanged foreign-manager operations must refuse ownership.
			h.do(http.MethodPatch, sourceCommandPath, "operator", SourceEntriesPatch{Add: []string{"alpha.example"}}, "If-Match", rv).want(want)
			if err := st.Get(t.Context(), sourceCommandKey, &got); err != nil {
				t.Fatal(err)
			}
			if got.UID != uid || (want == http.StatusOK && store.ManagedBy(&got) != store.ManagerAPI) || (owner == store.ManagerAPI && got.ResourceVersion != rv) || (want != http.StatusOK && got.ResourceVersion != rv) {
				t.Fatal("ownership/no-op changed unexpected metadata")
			}
		})
	}
	h, st := objectCommandHarness(t, nil)
	if _, err := st.Apply(t.Context(), sourceTestList(), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	before, _ := st.ReadGeneration(t.Context())
	h.do(http.MethodPatch, sourceCommandPath, "operator", SourceEntriesPatch{Disable: []string{"beta.example"}, Add: []string{"invalid name!"}}).want(http.StatusBadRequest)
	h.do(http.MethodPatch, sourceCommandPath, "operator", SourceEntriesPatch{Remove: []string{"alpha.example", "beta.example", "gamma.example"}}).want(http.StatusUnprocessableEntity)
	h.do(http.MethodPatch, "/api/v1/blocklists/default/captured/sources/missing/entries", "operator", SourceEntriesPatch{Add: []string{"new.example"}}).want(http.StatusNotFound)
	after, err := st.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("invalid source patch partially applied", err)
	}
}

func TestSourceCommandStructuredAndRemoteExclusions(t *testing.T) {
	h, st := objectCommandHarness(t, nil)
	list := sourceTestList()
	list.Spec.Sources = []public.ListSource{
		{Name: "structured", Entries: []public.ListEntry{{Type: public.EntryExact, Value: "alpha.example"}}},
		{Name: "remote", Format: "hosts", URL: "https://example.test/list"},
		{Name: "rpz", Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.example.", Primaries: []string{"192.0.2.1:53"}}},
	}
	if _, err := st.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	var result SourceEntriesResult
	h.do(http.MethodPatch, "/api/v1/blocklists/default/captured/sources/structured/entries", "operator", SourceEntriesPatch{Disable: []string{"alpha.example"}}).want(http.StatusOK).json(&result)
	if result.Total != 1 || result.Excluded != 1 || len(result.Disabled) != 1 {
		t.Fatal("structured result differs", result)
	}
	h.do(http.MethodPatch, "/api/v1/blocklists/default/captured/sources/1/entries", "operator", SourceEntriesPatch{Disable: []string{"0.0.0.0 alpha.example beta.example"}}).want(http.StatusOK).json(&result)
	if result.Excluded != 2 || len(result.Disabled) != 1 {
		t.Fatal("remote exclusions lost parser semantics", result)
	}
	var got public.BlockList
	if err := st.Get(t.Context(), sourceCommandKey, &got); err != nil || got.Spec.Sources[0].Entries[0].IsEnabled() || got.Spec.Sources[0].Entries[0].Modified == nil || len(got.Spec.Sources[1].Exclude) != 2 {
		t.Fatal("source types did not preserve each other", err)
	}
	h.do(http.MethodPatch, "/api/v1/blocklists/default/captured/sources/rpz/entries", "operator", SourceEntriesPatch{Disable: []string{"alpha.example"}}).want(http.StatusUnprocessableEntity)
}

func TestSourceCommandRollbackRefusalAndNoFallback(t *testing.T) {
	for _, mode := range []string{"refused", "after-staging", "missing-result"} {
		t.Run(mode, func(t *testing.T) {
			h, st := objectCommandHarness(t, nil)
			if _, err := st.Apply(t.Context(), sourceTestList(), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				t.Fatal(err)
			}
			h.seed(store.ManagerAPI, sourceTestList())
			calls := 0
			command := h.srv.opts.ObjectCommand
			h.srv.opts.ObjectCommand = func(ctx context.Context, key store.Key, fn func(context.Context, *sqlite.CommandTx, ObjectAuthorize) error) error {
				calls++
				switch mode {
				case "refused":
					return errUnavailable("original command unavailable")
				case "missing-result":
					return nil
				default:
					return command(ctx, key, func(ctx context.Context, tx *sqlite.CommandTx, authorize ObjectAuthorize) error {
						if err := fn(ctx, tx, authorize); err != nil {
							return err
						}
						return errors.New("injected failure after staging")
					})
				}
			}
			before, _ := st.ReadGeneration(t.Context())
			response := h.do(http.MethodPatch, sourceCommandPath, "operator", SourceEntriesPatch{Add: []string{"new.example"}})
			if response.Code < 500 || calls != 1 {
				t.Fatal("source command retried or accepted refusal", response.Code, calls)
			}
			after, err := st.ReadGeneration(t.Context())
			if err != nil || !before.Generation().Same(after.Generation()) {
				t.Fatal("failed command escaped rollback", err)
			}
			var live public.BlockList
			if err := h.st.Get(t.Context(), sourceCommandKey, &live); err != nil || slices.Contains(live.Spec.Sources[0].Inline, "new.example") {
				t.Fatal("failed command fell back to live Store", err)
			}
		})
	}
}
