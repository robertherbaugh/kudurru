package servingbuild

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func competingListCache(t *testing.T, f *fixture) *zonecache.Cache {
	t.Helper()
	list, err := blocking.Compile(blocking.Spec{Name: "legacy/competing", Mode: blocking.ModeBlock, Sources: []blocking.Source{blocking.LinesSource("legacy", parser.FormatDomains, []string{"search.example"})}}, nil, blocking.CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lists, err := blocking.NewSnapshot(1, list)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := zonecache.New(f.store, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	cache.SetLists(lists)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- cache.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for cache.Snapshot().Objects.Lists() != lists {
		if time.Now().After(deadline) {
			t.Fatal("competing legacy list cache did not publish")
		}
		time.Sleep(time.Millisecond)
	}
	return cache
}

func listReadHTTP(t *testing.T, f *fixture, source apihttp.ObjectReadSource, cache *zonecache.Cache) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	auth, err := servinghttp.New(servinghttp.Options{Publisher: f.pub, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop})
	if err != nil {
		t.Fatal(err)
	}
	api, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host,
		Authenticate: auth.Middleware, Authorizer: auth, ObjectReads: source, Cache: cache, CrossOrigin: cop})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server
}

func listReadHits(t *testing.T, result objectReadResponse) []apihttp.ListSearchHit {
	t.Helper()
	body := diagnosticWant(t, result.diagnosticResponse, http.StatusOK)
	var list apihttp.List[apihttp.ListSearchHit]
	if err := json.Unmarshal([]byte(body), &list); err != nil || list.Items == nil {
		t.Fatal("search did not return a detached array", body, err)
	}
	return list.Items
}

func TestCapturedListSearchActualSQLiteHeldPublication(t *testing.T) {
	for _, change := range []string{"contents", "mode", "source"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: "default"}, Spec: public.BlockListSpec{
				Mode: "block", Sources: []public.ListSource{{Name: "original", Inline: []string{"||search.example^"}}},
			}}
			if _, err := f.store.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				t.Fatal(err)
			}
			f.waitCurrent(t)
			cache := competingListCache(t, f)
			h := &heldObjectReads{ObjectReads: NewObjectReads(), entered: make(chan context.Context, 1), resume: make(chan struct{})}
			h.remaining.Store(1)
			server := listReadHTTP(t, f, h, cache)
			cleanupHeldObjectReads(t, h)
			done := make(chan objectReadResponse, 1)
			go func() {
				done <- objectReadGET(t.Context(), server, f.token, "blocklists/_search?domain=SEARCH.Example.")
			}()
			original := objectReadWait(t, h)
			objects, base, err := h.ObjectReads.ObjectSnapshot(original)
			if err != nil || objects.Lists() == nil || objects.Lists().Contains("search.example")["default/search"] != "||search.example^" {
				t.Fatal("fixture original compiled rules not held", err)
			}
			fresh := []apihttp.ListSearchHit{{List: "default/search", Mode: "block", Rule: "||search.example^"}}
			switch change {
			case "contents":
				list.Spec.Sources[0].Inline = []string{"changed.example"}
				fresh = []apihttp.ListSearchHit{}
			case "mode":
				list.Spec.Mode = "allow"
				fresh[0].Mode = "allow"
			case "source":
				list.Spec.Sources = []public.ListSource{{Name: "replacement", Inline: []string{"search.example"}}}
				fresh[0].Rule = "search.example"
			}
			if _, err := f.store.Apply(t.Context(), list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				t.Fatal(err)
			}
			f.waitCurrent(t)
			close(h.resume)
			old := listReadHits(t, <-done)
			if !slices.Equal(old, []apihttp.ListSearchHit{{List: "default/search", Mode: "block", Rule: "||search.example^"}}) {
				t.Fatal("held search mixed live successor or legacy cache", old)
			}
			current := listReadHits(t, objectReadGET(t.Context(), server, f.token, "blocklists/_search?domain=search.example"))
			if !slices.Equal(current, fresh) {
				t.Fatal("fresh search did not observe exact successor compilation", current, fresh)
			}
			if cache.Snapshot().Objects.Lists().Contains("search.example")["legacy/competing"] != "search.example" {
				t.Fatal("fixture lost the competing live compiled cache")
			}
			lease, err := f.pub.CaptureHTTP(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if base.Same(lease.Bundle().Identity().Base) {
				t.Fatal("fixture did not publish a successor generation")
			}
			lease.Release()
			if _, _, err := h.ObjectReads.ObjectSnapshot(original); err == nil {
				t.Fatal("search retained a usable released original lease")
			}
		})
	}
}

// Explicit test-only incomplete owner fixture: keep original desired objects
// and Base but omit compiled lists. Production ObjectReads never recompiles.
type listReadWithoutCompiled struct{ *ObjectReads }

func (r listReadWithoutCompiled) ObjectSnapshot(ctx context.Context) (zonecache.ObjectSet, replication.Generation, error) {
	objects, base, err := r.ObjectReads.ObjectSnapshot(ctx)
	if err != nil {
		return zonecache.ObjectSet{}, replication.Generation{}, err
	}
	var desired []store.Object
	for _, kind := range objects.Kinds() {
		desired = append(desired, objects.List(kind)...)
	}
	snapshot, err := zonecache.Prepare(zonecache.PrepareOptions{Generation: base.Revision}, desired, nil)
	if err != nil {
		return zonecache.ObjectSet{}, replication.Generation{}, err
	}
	return snapshot.Objects, base, nil
}

func TestCapturedListSearchActualSQLiteMissingCompiledFailsClosed(t *testing.T) {
	f := newFixture(t)
	cache := competingListCache(t, f)
	legacy := listReadHTTP(t, f, nil, cache)
	hits := listReadHits(t, objectReadGET(t.Context(), legacy, f.token, "blocklists/_search?domain=search.example"))
	if !slices.Equal(hits, []apihttp.ListSearchHit{{List: "legacy/competing", Mode: "block", Rule: "search.example"}}) {
		t.Fatal("nil read capability must still use competing legacy cache", hits)
	}
	incomplete := listReadHTTP(t, f, listReadWithoutCompiled{NewObjectReads()}, cache)
	diagnosticWant(t, objectReadGET(t.Context(), incomplete, f.token, "blocklists/_search?domain=search.example").diagnosticResponse, http.StatusServiceUnavailable)
	diagnosticWant(t, objectReadGET(t.Context(), incomplete, f.token, "blocklists/_search?domain=blocked.example").diagnosticResponse, http.StatusServiceUnavailable)
	server := listReadHTTP(t, f, NewObjectReads(), cache)
	got := listReadHits(t, objectReadGET(t.Context(), server, f.token, "blocklists/_search?domain=unmatched.invalid"))
	if len(got) != 0 {
		t.Fatal("no match must remain a successful empty array", got)
	}
}
