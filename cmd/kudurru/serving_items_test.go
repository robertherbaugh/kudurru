package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	itemsStaleSource = "current writable serving generation unavailable"
	itemsForeign     = "another change was committed on this node while the request ran; this item was not applied; review current state before a new decision"
	itemsLifetime    = "the request's lifetime ended before this item was reached; it was not applied"
)

// itemsFixture mounts the real router on the real root object command of a
// complete SQLite runtime. between runs in the request's own goroutine before
// the command of item n, which is where another writer can be placed exactly
// between two items of one request.
type itemsFixture struct {
	*servingCommandFixture
	server   *api.Server
	plain    *api.Server // the same runtime without an item sequence
	other    string      // the API token of a second administrator
	commands int
	between  func(n int, ctx context.Context)
	inside   func(n int) // runs in the transaction of item n, before its body
}

func newItemsFixture(t *testing.T, adapter func(*servingCommandFixture) *servinghttp.Adapter) *itemsFixture {
	t.Helper()
	f := &itemsFixture{}
	var otherID string
	f.other, otherID = tokens.Generate()
	f.servingCommandFixture = newServingCommandFixture(t, func(s *servingRuntime) {
		err := s.opts.Store.Command(t.Context(), s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			created := metav1.NewTime(time.Now().UTC().Add(-time.Minute))
			user, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "second-admin", CreationTimestamp: created}, Spec: internalv1.LocalUserSpec{BuiltinAdmin: true}}, store.ApplyOptions{Manager: store.ManagerSystem})
			if err != nil {
				return err
			}
			_, err = tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: otherID, CreationTimestamp: created}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(f.other), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:second-admin", Provider: "local"}, Scopes: []string{"admin"}}}, store.ApplyOptions{Manager: store.ManagerSystem})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	authenticate := f.http
	if adapter != nil {
		authenticate = adapter(f.servingCommandFixture)
	}
	f.mount(t, authenticate)
	return f
}

// mount builds the two routers over the fixture's runtime.
func (f *itemsFixture) mount(t *testing.T, authenticate *servinghttp.Adapter) {
	t.Helper()
	command := func(ctx context.Context, key store.Key, fn func(context.Context, *sqlite.CommandTx, api.ObjectAuthorize) error) error {
		n := f.commands
		f.commands++
		if f.between != nil {
			f.between(n, ctx)
		}
		return f.s.ObjectCommand(ctx, key, func(ctx context.Context, tx *sqlite.CommandTx, authorize api.ObjectAuthorize) error {
			if f.inside != nil {
				f.inside(n)
			}
			return fn(ctx, tx, authorize)
		})
	}
	var err error
	f.server, err = api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config,
		ObjectCommand: command, ObjectItems: f.s.ObjectItems, Authenticate: authenticate.Middleware, Authorizer: authenticate, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f.plain, err = api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config,
		ObjectCommand: f.s.ObjectCommand, Authenticate: f.http.Middleware, Authorizer: f.http, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *itemsFixture) settled(t *testing.T) {
	t.Helper()
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
}

func (f *itemsFixture) send(t *testing.T, srv *api.Server, token, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, "http://kudurru.test/api/v1"+path, bytes.NewReader(raw))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func (f *itemsFixture) apply(t *testing.T, srv *api.Server, names ...string) []api.ApplyResult {
	t.Helper()
	docs := make([]any, 0, len(names))
	for _, name := range names {
		group := servingObjectGroup(name)
		group.TypeMeta = metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "ClientGroup"}
		docs = append(docs, group)
	}
	w := f.send(t, srv, f.token, http.MethodPost, "/apply", docs)
	var out api.List[api.ApplyResult]
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Items) != len(names) {
		t.Fatalf("apply did not answer one result for each document: %d %s", w.Code, w.Body)
	}
	return out.Items
}

func (f *itemsFixture) exists(t *testing.T, name string) bool {
	t.Helper()
	var group public.ClientGroup
	err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: name}, &group)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// followUp commits as the node's own reconcilers do: under the Store's writer
// lock alone, without the publication gate and outside the command coordinator.
func (f *itemsFixture) followUp(t *testing.T, key string) {
	t.Helper()
	token, err := f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx := sqlite.WithWriterToken(t.Context(), token)
	if err := f.s.opts.Store.Command(ctx, token.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: key, Data: []byte(`{}`)})
	}); err != nil {
		t.Fatal(err)
	}
}

func itemsResults(items []api.ApplyResult) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, item.Name+"="+item.Result+"/"+item.ErrorKey+"/"+item.Error)
	}
	return strings.Join(parts, "; ")
}

// One POST /apply with three new documents creates all three on the complete
// SQLite runtime, although every commit retires the request's original lease
// and a follow-up writer commits between every two items. The same documents
// again are three unchanged results without a commit. The same request through
// a server without the item sequence shows the defect that D378 removes.
func TestServingItemsApplyFollowsOwnCommitsAcrossFollowUpWrites(t *testing.T) {
	f := newItemsFixture(t, nil)
	f.between = func(n int, _ context.Context) {
		if n > 0 {
			f.followUp(t, fmt.Sprintf("between-%d", n))
		}
	}
	before := f.s.opts.Store.Authority().Installed.Revision
	results := f.apply(t, f.server, "multi-a", "multi-b", "multi-c")
	for _, result := range results {
		if result.Result != api.ResultCreated || result.ResourceVersion == "" || result.Error != "" {
			t.Fatalf("one apply of three new documents did not create all of them: %s", itemsResults(results))
		}
	}
	if !f.exists(t, "multi-a") || !f.exists(t, "multi-b") || !f.exists(t, "multi-c") || f.commands != 3 {
		t.Fatal("created documents are not stored, or an item ran more than once", f.commands)
	}
	if after := f.s.opts.Store.Authority().Installed.Revision; after != before+5 {
		t.Fatalf("three items and two follow-up writes made %d commits", after-before)
	}
	f.settled(t)
	f.between = nil
	unchanged := f.s.opts.Store.Authority().Installed.Revision
	results = f.apply(t, f.server, "multi-a", "multi-b", "multi-c")
	for _, result := range results {
		if result.Result != api.ResultUnchanged {
			t.Fatalf("second identical apply did not report three unchanged documents: %s", itemsResults(results))
		}
	}
	if f.s.opts.Store.Authority().Installed.Revision != unchanged {
		t.Fatal("unchanged documents committed")
	}
	// Without the item sequence every command is a single command: the second
	// new document meets the lease that the first one retired.
	results = f.apply(t, f.plain, "single-a", "single-b", "single-c")
	if results[0].Result != api.ResultCreated || results[1].ErrorKey != api.KeyUnavailable || results[1].Error != itemsStaleSource || results[2].Error != itemsStaleSource || f.exists(t, "single-b") || f.exists(t, "single-c") {
		t.Fatalf("independent commands of one request were not held to the original lease: %s", itemsResults(results))
	}
}

// The request's first item is admitted exactly as a single write is: a commit
// by anyone between the request's capture and that item refuses it, and no
// later item is admitted on the newer state.
func TestServingItemsFirstItemKeepsTheOriginalSource(t *testing.T) {
	f := newItemsFixture(t, nil)
	f.between = func(n int, _ context.Context) {
		if n == 0 {
			f.followUp(t, "before-first")
		}
	}
	results := f.apply(t, f.server, "strict-a", "strict-b", "strict-c")
	for _, result := range results {
		if result.Result != api.ResultError || result.ErrorKey != api.KeyUnavailable || result.Error != itemsStaleSource {
			t.Fatalf("a request whose source changed before its first item was admitted: %s", itemsResults(results))
		}
	}
	if f.exists(t, "strict-a") || f.exists(t, "strict-b") || f.exists(t, "strict-c") || f.commands != 3 {
		t.Fatal("a refused item was applied or replayed", f.commands)
	}
}

// A change committed by another credential between two items is a foreign
// commit: every later item answers unavailable with the documented message,
// none of them is applied, none is tried again, and the earlier item and the
// foreign change stay committed.
func TestServingItemsForeignCommandRefusesLaterItems(t *testing.T) {
	f := newItemsFixture(t, nil)
	f.between = func(n int, _ context.Context) {
		if n != 1 {
			return
		}
		f.settled(t)
		w := f.send(t, f.plain, f.other, http.MethodPut, "/objects/clientgroups/default/foreign", servingObjectGroup("foreign"))
		if w.Code != http.StatusCreated {
			t.Errorf("the second administrator's write was refused: %d %s", w.Code, w.Body)
		}
	}
	results := f.apply(t, f.server, "mine-a", "mine-b", "mine-c")
	if results[0].Result != api.ResultCreated {
		t.Fatalf("the item before the foreign commit was not applied: %s", itemsResults(results))
	}
	for _, result := range results[1:] {
		if result.Result != api.ResultError || result.ErrorKey != api.KeyUnavailable || result.Error != itemsForeign {
			t.Fatalf("an item after a foreign commit was not refused with the documented answer: %s", itemsResults(results))
		}
	}
	if !f.exists(t, "mine-a") || !f.exists(t, "foreign") || f.exists(t, "mine-b") || f.exists(t, "mine-c") || f.commands != 3 {
		t.Fatal("a refused item was applied or replayed, or a committed one was lost", f.commands)
	}
	// A new request is a new decision on current state.
	f.settled(t)
	f.between = nil
	results = f.apply(t, f.server, "mine-a", "mine-b", "mine-c")
	if results[0].Result != api.ResultUnchanged || results[1].Result != api.ResultCreated || results[2].Result != api.ResultCreated {
		t.Fatalf("a new request did not apply the rest: %s", itemsResults(results))
	}
}

// The request's one original lifetime bounds every item. An item reached after
// it ended is not applied and says so, instead of blaming the credential.
func TestServingItemsOriginalLifetimeBoundsEveryItem(t *testing.T) {
	f := newItemsFixture(t, func(f *servingCommandFixture) *servinghttp.Adapter {
		adapter, err := servinghttp.New(servinghttp.Options{Publisher: f.s.publisher, Now: f.s.opts.Now, MaxDuration: 3 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	})
	f.between = func(n int, ctx context.Context) {
		if n == 1 {
			<-ctx.Done()
		}
	}
	started := time.Now()
	results := f.apply(t, f.server, "bounded-a", "bounded-b", "bounded-c")
	if elapsed := time.Since(started); elapsed < 2*time.Second || elapsed > 20*time.Second {
		t.Fatal("the request did not run to its own lifetime", elapsed)
	}
	if results[0].Result != api.ResultCreated {
		t.Fatalf("the item inside the lifetime was not applied: %s", itemsResults(results))
	}
	for _, result := range results[1:] {
		if result.Result != api.ResultError || result.ErrorKey != api.KeyUnavailable || result.Error != itemsLifetime {
			t.Fatalf("an item after the request's lifetime was not refused with its cause: %s", itemsResults(results))
		}
	}
	if !f.exists(t, "bounded-a") || f.exists(t, "bounded-b") || f.exists(t, "bounded-c") {
		t.Fatal("an item was applied after the request's lifetime ended")
	}
}

// Every item verifies the original raw credential in its own transaction. A
// token that is revoked or loses its grant between two items stops the later
// items with the credential's own answer, and nothing more is applied.
func TestServingItemsRevokedOrDowngradedCredentialStopsLaterItems(t *testing.T) {
	for _, mode := range []string{"revoked", "downgraded"} {
		t.Run(mode, func(t *testing.T) {
			f := newItemsFixture(t, nil)
			f.between = func(n int, _ context.Context) {
				if n != 1 {
					return
				}
				writer, err := f.s.opts.Store.WriterToken()
				if err != nil {
					t.Error(err)
					return
				}
				ctx := sqlite.WithWriterToken(t.Context(), writer)
				err = f.s.opts.Store.Command(ctx, writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
					list, err := tx.List(ctx, "APIToken", store.ListOptions{})
					if err != nil {
						return err
					}
					for _, obj := range list.Items {
						token := obj.(*internalv1.APIToken)
						if token.Spec.SecretHash != tokens.Hash(f.token) {
							continue
						}
						if mode == "revoked" {
							return tx.Delete(ctx, store.KeyOf(token), store.DeleteOptions{Manager: store.ManagerSystem, Force: true})
						}
						token.Spec.Scopes = []string{"viewer"}
						_, err := tx.Apply(ctx, token, store.ApplyOptions{Manager: store.ManagerSystem, Force: true})
						return err
					}
					return errors.New("the request's token is not stored")
				})
				if err != nil {
					t.Error(err)
				}
			}
			results := f.apply(t, f.server, "cred-a", "cred-b", "cred-c")
			if results[0].Result != api.ResultCreated {
				t.Fatalf("the item before the credential change was not applied: %s", itemsResults(results))
			}
			want := api.KeyUnauthenticated
			if mode == "downgraded" {
				want = api.KeyForbidden
			}
			for _, result := range results[1:] {
				if result.Result != api.ResultError || result.ErrorKey != want {
					t.Fatalf("an item after the credential was %s was not refused as %s: %s", mode, want, itemsResults(results))
				}
			}
			if !f.exists(t, "cred-a") || f.exists(t, "cred-b") || f.exists(t, "cred-c") || f.commands != 3 {
				t.Fatal("a refused item was applied or replayed", f.commands)
			}
		})
	}
}

// The node replaces its local dependency installations on its own schedule:
// provider resource sets, keys and catalogs. A replacement between two items is
// no foreign commit and decides nothing for a later item, whose transaction
// pins the installations that are current when it runs. The request's original
// lease, whose installations are retired, is only the comparison operand for
// the writer's authority. A mounted restore of eight documents was cut at its
// fifth by such a replacement while the lease's installations were compared.
func TestServingItemsReplacedDependencyKeepsLaterItems(t *testing.T) {
	f := newItemsFixture(t, nil)
	f.between = func(n int, _ context.Context) {
		if n != 1 {
			return
		}
		if err := f.s.opts.Keyrings.Replace(t.Context(), f.s.opts.Keyrings.Capture().Keys()); err != nil {
			t.Error(err)
		}
	}
	var original bool
	f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](r.Context(), "runtime")
		if err != nil {
			t.Error(err)
			return
		}
		if err := f.s.opts.Keyrings.Replace(t.Context(), f.s.opts.Keyrings.Capture().Keys()); err != nil {
			t.Error(err)
		}
		original = owner.DependenciesCurrent()
	})).ServeHTTP(httptest.NewRecorder(), f.request())
	if original {
		t.Fatal("the fixture replacement did not retire a held lease's installations")
	}
	f.settled(t)
	results := f.apply(t, f.server, "dep-a", "dep-b", "dep-c")
	for _, result := range results {
		if result.Result != api.ResultCreated {
			t.Fatalf("a dependency replacement between two items refused a later item: %s", itemsResults(results))
		}
	}
	if !f.exists(t, "dep-a") || !f.exists(t, "dep-b") || !f.exists(t, "dep-c") {
		t.Fatal("an applied item is not stored")
	}
}

// POST /{kind}:batchDelete deletes several names in one request, and a foreign
// commit between two names refuses the later names.
func TestServingItemsBatchDeleteFollowsOwnCommits(t *testing.T) {
	f := newItemsFixture(t, nil)
	names := []string{"gone-a", "gone-b", "gone-c", "kept-d", "kept-e"}
	for _, result := range f.apply(t, f.server, names...) {
		if result.Result != api.ResultCreated {
			t.Fatal("fixture groups were not created")
		}
	}
	f.settled(t)
	remove := func(names ...string) api.Processed {
		t.Helper()
		w := f.send(t, f.server, f.token, http.MethodPost, "/clientgroups:batchDelete", map[string]any{"names": names})
		var out api.Processed
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("batch delete did not answer per-name results: %d %s", w.Code, w.Body)
		}
		return out
	}
	out := remove("gone-a", "gone-b", "gone-c")
	if len(out.Processed.Success) != 3 || len(out.Processed.Errors) != 0 || f.exists(t, "gone-a") || f.exists(t, "gone-b") || f.exists(t, "gone-c") {
		t.Fatalf("one batch delete did not delete every name: %+v", out.Processed)
	}
	f.settled(t)
	first := f.commands
	f.between = func(n int, _ context.Context) {
		if n != first+1 {
			return
		}
		f.settled(t)
		if w := f.send(t, f.plain, f.other, http.MethodPut, "/objects/clientgroups/default/foreign", servingObjectGroup("foreign")); w.Code != http.StatusCreated {
			t.Errorf("the second administrator's write was refused: %d %s", w.Code, w.Body)
		}
	}
	out = remove("kept-d", "kept-e")
	if len(out.Processed.Success) != 1 || out.Processed.Success[0].Item != "default/kept-d" || len(out.Processed.Errors) != 1 ||
		out.Processed.Errors[0].Item != "default/kept-e" || out.Processed.Errors[0].Error != api.KeyUnavailable+": "+itemsForeign {
		t.Fatalf("a name after a foreign commit was not refused with the documented answer: %+v", out.Processed)
	}
	if f.exists(t, "kept-d") || !f.exists(t, "kept-e") || !f.exists(t, "foreign") {
		t.Fatal("a refused name was deleted, or a deleted one came back")
	}
}

// An item sequence stays in the elected writer episode of its first item.
func TestServingItemsBindOneWriterEpisode(t *testing.T) {
	first, second := &primaryWriterTerm{}, &primaryWriterTerm{}
	items := &requestItems{}
	target := serving.TermActivation{Term: replication.Term{ClusterID: "cluster", Epoch: 1, PrimaryID: "member", PromotionID: "promotion"}, Incarnation: 7}
	if err := items.bind(first, target); err != nil {
		t.Fatal(err)
	}
	if err := items.bind(first, target); err != nil {
		t.Fatal("the same episode was refused", err)
	}
	later := target
	later.Incarnation++
	if err := items.bind(second, target); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("a later writer episode was adopted", err)
	}
	if err := items.bind(first, later); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("a later incarnation was adopted", err)
	}
}

// A request whose items have run for its pace lets the node publish before
// its next item (D378): the item's transaction begins while the node has a
// current publication of everything committed before it, including the node's
// own follow-up write after the previous item, so local DNS answers and other
// callers are served between the items of a long request. Every item is still
// applied, and none is admitted again.
func TestServingItemsLetTheNodePublishBetweenItems(t *testing.T) {
	f := newItemsFixture(t, nil)
	f.s.itemPace = &itemPace{work: time.Nanosecond}
	f.between = func(n int, _ context.Context) {
		if n > 0 {
			f.followUp(t, fmt.Sprintf("paced-%d", n))
		}
	}
	var current []bool
	f.inside = func(_ int) {
		current = append(current, f.s.publisher.Status().ProtectedReady)
	}
	names := []string{"paced-a", "paced-b", "paced-c", "paced-d", "paced-e"}
	results := f.apply(t, f.server, names...)
	for _, result := range results {
		if result.Result != api.ResultCreated {
			t.Fatalf("a paced request did not create every document: %s", itemsResults(results))
		}
	}
	if f.commands != len(names) || len(current) != len(names) {
		t.Fatal("an item ran more than once", f.commands, len(current))
	}
	for n, ok := range current {
		if !ok {
			t.Fatalf("item %d began without a current publication of the request's earlier items: %v", n, current)
		}
	}

	// After the node has published, the request commits nothing for the
	// pace's rest, so that the publication is used.
	f.settled(t)
	f.between, f.inside = nil, nil
	f.s.itemPace = &itemPace{work: time.Nanosecond, rest: 250 * time.Millisecond}
	started := time.Now()
	results = f.apply(t, f.server, "rested-a", "rested-b", "rested-c")
	if elapsed := time.Since(started); elapsed < 500*time.Millisecond {
		t.Fatal("the request did not rest after the node published its earlier items", elapsed)
	}
	for _, result := range results {
		if result.Result != api.ResultCreated {
			t.Fatalf("a rested request did not create every document: %s", itemsResults(results))
		}
	}
}

// The wait for a publication runs inside the request's one lifetime. An item
// whose wait outlives it is not applied and says so.
func TestServingItemsPaceEndsWithTheRequestLifetime(t *testing.T) {
	f := newItemsFixture(t, func(f *servingCommandFixture) *servinghttp.Adapter {
		adapter, err := servinghttp.New(servinghttp.Options{Publisher: f.s.publisher, Now: f.s.opts.Now, MaxDuration: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	})
	f.s.itemPace = &itemPace{work: time.Nanosecond, rest: time.Minute}
	started := time.Now()
	results := f.apply(t, f.server, "waited-a", "waited-b", "waited-c")
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatal("the wait outlived the request's lifetime", elapsed)
	}
	if results[0].Result != api.ResultCreated {
		t.Fatalf("the item before the wait was not applied: %s", itemsResults(results))
	}
	for _, result := range results[1:] {
		if result.Result != api.ResultError || result.ErrorKey != api.KeyUnavailable || result.Error != itemsLifetime {
			t.Fatalf("an item whose wait outlived the lifetime was not refused with its cause: %s", itemsResults(results))
		}
	}
	if !f.exists(t, "waited-a") || f.exists(t, "waited-b") || f.exists(t, "waited-c") || f.commands != 3 {
		t.Fatal("an item was applied after the request's lifetime ended, or replayed", f.commands)
	}
}

// The wait for a publication is bounded (D378): when the node does not
// publish within it, the item goes on without the publication and without
// the rest, so the pace cannot by itself hold a long request until its
// lifetime ends. Every item is still applied once.
func TestServingItemsPaceWaitIsBounded(t *testing.T) {
	f := newItemsFixture(t, nil)
	// The rest would hold the request for minutes if an item waited for a
	// publication; the follow-up commit before each later item makes the
	// publication stale, so every wait ends at its bound instead.
	f.s.itemPace = &itemPace{work: time.Nanosecond, rest: time.Minute, wait: time.Nanosecond}
	f.between = func(n int, _ context.Context) {
		if n > 0 {
			f.followUp(t, fmt.Sprintf("bounded-%d", n))
		}
	}
	started := time.Now()
	results := f.apply(t, f.server, "bounded-a", "bounded-b", "bounded-c")
	if elapsed := time.Since(started); elapsed > 50*time.Second {
		t.Fatal("a wait for a publication was not bounded", elapsed)
	}
	for _, result := range results {
		if result.Result != api.ResultCreated {
			t.Fatalf("a request whose waits ended at their bound did not create every document: %s", itemsResults(results))
		}
	}
	if f.commands != 3 {
		t.Fatal("an item ran more than once", f.commands)
	}
}
