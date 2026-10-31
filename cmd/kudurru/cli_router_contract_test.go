package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// This fixture crosses the CLI/HTTP boundary with production routing,
// serialization, validation, pagination and authorization. Only authentication
// uses apitest identities. Actual SQLite/current-credential acceptance remains
// in the runServe tests; no fake API provides this fixture's responses.
type routerCLI struct {
	*harness
	server *httptest.Server
	store  *memory.Store
	tail   *querylog.Tail
	mu     sync.Mutex
	calls  map[string]int
}

func newRouterCLI(t *testing.T) *routerCLI {
	t.Helper()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.Name, cfg.Node.DataDir = "contract-node", t.TempDir()
	f := &routerCLI{
		harness: &harness{t: t, cfgPath: filepath.Join(t.TempDir(), "cli.yaml"),
			env: map[string]string{"KUDURRU_TOKEN": apitest.BearerPrefix + "admin"}, keyring: &memKeyring{}},
		store: memory.New(), tail: querylog.NewTail(32, 32, nil), calls: map[string]int{},
	}
	t.Cleanup(func() { _ = f.store.Close() })
	srv, err := api.New(api.Options{Config: cfg, NodeName: cfg.Node.Name, Store: f.store,
		Validator: v, Authenticate: apitest.Authenticate,
		Authorizer: apitest.Authorizer(apitest.StandardBindings()...), Tail: f.tail,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.Method+" "+r.URL.Path]++
		f.mu.Unlock()
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *routerCLI) run(args ...string) (string, string, error) {
	return f.harness.run(append([]string{"--server", f.server.URL}, args...)...)
}

func (f *routerCLI) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method+" "+path]
}

func TestCLIRealRouterApplyResultsAndErrors(t *testing.T) {
	f := newRouterCLI(t)
	owned := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "default"}}
	if _, err := f.store.Apply(t.Context(), owned, store.ApplyOptions{Manager: "file:objects.d/groups.yaml"}); err != nil {
		t.Fatal(err)
	}
	f.stdin = `apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: {name: created}
spec: {}
---
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: {name: owned}
spec: {clients: [{ip: 192.0.2.1}]}
---
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: invalid}
spec: {zoneName: "invalid..example.", type: primary}
`
	out, stderr, err := f.run("apply", "-f", "-", "-o", "json")
	var results []api.ApplyResult
	if err == nil || json.Unmarshal([]byte(out), &results) != nil || len(results) != 3 {
		t.Fatal("mixed apply did not report all document outcomes with a failed exit")
	}
	if results[0].Result != "created" || results[0].Namespace != "default" || results[0].ResourceVersion == "" ||
		results[1].Result != "error" || results[1].ErrorKey != api.KeyManagedBy || results[1].Hint != "file:objects.d/groups.yaml" ||
		results[2].Result != "error" || results[2].ErrorKey != api.KeyInvalid || !strings.Contains(stderr, "file:objects.d/groups.yaml") {
		t.Fatal("CLI lost per-document versions, structured error keys or owner hint")
	}
	var group public.ClientGroup
	if err := f.store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: "created"}, &group); err != nil {
		t.Fatal("successful document was not stored", err)
	}
	if err := f.store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: "owned"}, &group); err != nil || len(group.Spec.Clients) != 0 {
		t.Fatal("refused owned document was modified")
	}
	f.env["KUDURRU_TOKEN"] = apitest.BearerPrefix + "viewer"
	f.stdin = `{"apiVersion":"kudurru.io/v1alpha1","kind":"ClientGroup","metadata":{"name":"denied"},"spec":{}}`
	out, _, err = f.run("apply", "-f", "-", "-o", "json")
	if err == nil || json.Unmarshal([]byte(out), &results) != nil || len(results) != 1 || results[0].ErrorKey != api.KeyForbidden {
		t.Fatal("HTTP 200 with a forbidden document must still fail the CLI")
	}
	if err := f.store.Get(t.Context(), store.Key{Kind: "ClientGroup", Namespace: "default", Name: "denied"}, &group); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("denied CLI apply wrote an object")
	}
	if f.count(http.MethodPost, "/api/v1/apply") != 2 {
		t.Fatal("a refused mutation was replayed")
	}
	_, _, err = f.run("get", "clientgroup", "missing")
	if !apiclient.IsKey(err, api.KeyNotFound) {
		t.Fatal("CLI did not retain the actual router's not_found error")
	}
}

func TestCLIRealRouterWrappedSettings(t *testing.T) {
	f := newRouterCLI(t)
	out, _, err := f.run("settings", "set", "control.blockedQTypes=A,ANY")
	if err != nil || !strings.Contains(out, "control.blockedQTypes") {
		t.Fatal("CLI did not accept the actual wrapped PATCH response", err)
	}
	read := func() {
		t.Helper()
		out, _, err := f.run("settings", "get", "control.blockedQTypes", "--scope", "cluster", "-o", "json")
		var rows []struct {
			Key   string   `json:"key"`
			Scope string   `json:"scope"`
			Value []string `json:"value"`
		}
		if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].Key != "control.blockedQTypes" || rows[0].Scope != "cluster" || strings.Join(rows[0].Value, ",") != "A,ANY" {
			t.Fatal("CLI did not unwrap the actual settings document")
		}
	}
	read()
	_, _, err = f.run("settings", "set", "control.blockedQTypes=NOT-A-DNS-TYPE")
	if !apiclient.IsKey(err, api.KeyInvalid) {
		t.Fatal("CLI lost the server's typed settings validation error")
	}
	read()
	if f.count(http.MethodPatch, "/api/v1/settings") != 2 {
		t.Fatal("refused settings change was replayed")
	}
}

func TestCLIRealRouterObjectPagination(t *testing.T) {
	f := newRouterCLI(t)
	for i := range 501 {
		obj := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("group-%03d", i), Namespace: "default"}}
		if _, err := f.store.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	out, _, err := f.run("get", "clientgroups", "-n", "default", "-o", "json")
	var list struct {
		Items []public.ClientGroup `json:"items"`
	}
	if err != nil || json.Unmarshal([]byte(out), &list) != nil || len(list.Items) != 501 {
		t.Fatal("CLI did not follow the real router's continuation page")
	}
	seen := map[string]bool{}
	for _, item := range list.Items {
		if seen[item.Name] || item.Namespace != "default" || item.ResourceVersion == "" {
			t.Fatal("pagination duplicated or lost an object identity")
		}
		seen[item.Name] = true
	}
	if f.count(http.MethodGet, "/api/v1/objects/clientgroups") != 2 {
		t.Fatal("fixture did not cross the actual 500-object page boundary")
	}
}

func TestCLIRealRouterQueryStream(t *testing.T) {
	f := newRouterCLI(t)
	f.env["KUDURRU_TOKEN"] = apitest.BearerPrefix + "operator"
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	var out, stderr syncBuffer
	go func() {
		done <- f.runCtx(ctx, &out, &stderr, "--server", f.server.URL, "logs", "--follow", "--limit", "0", "--status-class", "blocked", "-o", "json")
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("CLI stream did not cancel cleanly", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("CLI stream did not join")
		}
	})
	await := func(ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatal("CLI stream observation did not converge")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	await(func() bool { return f.tail.Subscribers() == 1 })
	ev := &querylog.Event{ID: 10, Time: time.Now(), Node: "contract-node", Proto: pipeline.ProtoUDP,
		Client: netip.MustParseAddr("192.0.2.4"), Name: "allowed.example.", QType: 1, QClass: 1, Status: pipeline.StatusForwarded}
	f.tail.Publish(ev)
	blocked := *ev
	blocked.ID, blocked.Name, blocked.Status, blocked.List = 11, "blocked.example.", pipeline.StatusBlockedList, "default/ads"
	f.tail.Publish(&blocked)
	await(func() bool { return strings.Contains(out.String(), "blocked.example") })
	cancel()
	await(func() bool { return f.tail.Subscribers() == 0 })
	var row api.StreamRow
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &row) != nil || row.ID != 11 || row.Domain != "blocked.example" || row.Node != "contract-node" || row.Client.IP != "192.0.2.4" || row.StatusClass != "blocked" {
		t.Fatal("CLI lost the actual SSE query payload or server-side filter")
	}
	if f.count(http.MethodGet, "/api/v1/queries/stream") != 1 {
		t.Fatal("single live stream unexpectedly reconnected")
	}
}
