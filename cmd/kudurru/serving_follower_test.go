package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// followerAnswer is one answer of the mounted follower API.
type followerAnswer struct {
	status int
	header http.Header
	raw    []byte
	Error  struct {
		Key     string `json:"key"`
		Message string `json:"message"`
		Hint    any    `json:"hint"`
	} `json:"error"`
}

// request sends one request to the follower's mounted API over its real TLS
// listener. credential is a bearer token, or "" for an anonymous request.
func (f *memberForwardHTTPFixture) request(t *testing.T, client *http.Client, method, path, credential string, body any, header ...string) followerAnswer {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, f.live.URL+api.BasePath+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.GetBody = nil
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	for i := 0; i+1 < len(header); i += 2 {
		request.Header.Set(header[i], header[i+1])
	}
	if client == nil {
		client = f.live.Client()
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("follower request", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer := followerAnswer{status: response.StatusCode, header: response.Header}
	if answer.raw, err = io.ReadAll(io.LimitReader(response.Body, 1<<20)); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(answer.raw, &answer)
	return answer
}

// primaryRequiredAnswer checks the one typed refusal: 503, the key, the
// primary's name, a hint and no Retry-After.
func primaryRequiredAnswer(t *testing.T, what string, answer followerAnswer, primary string) {
	t.Helper()
	hint, _ := answer.Error.Hint.(string)
	if answer.status != http.StatusServiceUnavailable || answer.Error.Key != api.KeyPrimaryRequired ||
		!strings.Contains(answer.Error.Message, "follower of "+primary) || hint == "" || answer.header.Get("Retry-After") != "" {
		t.Errorf("%s: want 503 primary_required naming %s with a hint and no Retry-After, got status=%d retry-after=%q body=%s",
			what, primary, answer.status, answer.header.Get("Retry-After"), answer.raw)
	}
}

var followerPathParameter = regexp.MustCompile(`\{[^}]+\}`)

// An enrolled follower answers every mutation that it neither forwards nor
// runs itself with one typed refusal that names its primary, through the
// production middleware chain and its real lease. Nothing reaches the
// primary, nothing is stored on either node, and GET /cluster and GET
// /auth/status say the same thing the routes answer.
func TestRootMemberFollowerRefusesUnforwardedMutations(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false)
	primaryStore, followerStore := f.primary.s.opts.Store, f.follower.opts.Store
	primary := followerStore.Authority().Term.PrimaryName
	if primary == "" || followerStore.Authority().Mode != replication.Follower {
		t.Fatal("fixture follower has no named primary")
	}
	before, installed, cached := roleBindingImage(t, primaryStore), followerStore.Authority(), f.cache.Len()
	token := f.primary.token

	var info api.ClusterInfo
	if answer := f.request(t, nil, http.MethodGet, "/cluster", token, nil); answer.status != http.StatusOK || json.Unmarshal(answer.raw, &info) != nil {
		t.Fatalf("GET /cluster on the follower: %d %s", answer.status, answer.raw)
	}
	if info.Role != "follower" || info.Primary != primary || info.Leader != primary || !info.PrimaryRequired || info.Node != "receiver" || info.ReadOnly {
		t.Fatalf("follower cluster info does not name its role and primary: %+v", info)
	}
	if !slices.Contains(info.ForwardedOperations, "apply") || !slices.Contains(info.ForwardedOperations, "setBlocking") ||
		!slices.Contains(info.LocalOperations, "flushCache") || slices.Contains(info.ForwardedOperations, "patchSettings") || slices.Contains(info.LocalOperations, "patchSettings") {
		t.Fatalf("follower capability lists: forwarded=%v local=%v", info.ForwardedOperations, info.LocalOperations)
	}

	// The node list no longer marks the follower as the leader.
	var nodes api.List[api.NodeInfo]
	if answer := f.request(t, nil, http.MethodGet, "/nodes", token, nil); answer.status != http.StatusOK || json.Unmarshal(answer.raw, &nodes) != nil ||
		len(nodes.Items) != 1 || nodes.Items[0].Name != "receiver" || !nodes.Items[0].Self || nodes.Items[0].Leader {
		t.Fatalf("GET /nodes on the follower: %d %s", answer.status, answer.raw)
	}

	type operation struct{ method, path, id string }
	var refused, admitted []operation
	for path, item := range f.api.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{http.MethodPost: item.Post, http.MethodPut: item.Put, http.MethodPatch: item.Patch, http.MethodDelete: item.Delete} {
			if op == nil {
				continue
			}
			probe := strings.NewReplacer("{kind}", "dnszones", "{namespace}", "default", "{type}", "A", "{zone}", "example").Replace(path)
			entry := operation{method, followerPathParameter.ReplaceAllString(probe, "probe"), op.OperationID}
			if api.OperationFollowerClass(op.OperationID) == api.FollowerPrimary {
				refused = append(refused, entry)
			} else {
				admitted = append(admitted, entry)
			}
		}
	}
	if len(refused) != 53 || len(admitted) != 40 {
		t.Fatalf("mounted follower API has %d refused and %d admitted mutations", len(refused), len(admitted))
	}
	for _, op := range refused {
		primaryRequiredAnswer(t, op.method+" "+op.path+" ("+op.id+")", f.request(t, nil, op.method, op.path, token, nil), primary)
	}
	// The cases the package names. A list refresh no longer answers 202.
	refresh := f.request(t, nil, http.MethodPost, "/blocklists/default/ads/refresh", token, nil)
	primaryRequiredAnswer(t, "list refresh", refresh, primary)
	if refresh.status == http.StatusAccepted {
		t.Fatal("list refresh on a follower answered 202 although nothing fetches there")
	}
	node := f.request(t, nil, http.MethodPatch, "/node/config", token, map[string]any{"log": map[string]any{"level": "debug"}})
	primaryRequiredAnswer(t, "node configuration", node, primary)
	if hint, _ := node.Error.Hint.(string); !strings.Contains(hint, "configuration file or environment") {
		t.Fatalf("node configuration refusal does not say what to change on this node: %s", node.raw)
	}
	primaryRequiredAnswer(t, "settings", f.request(t, nil, http.MethodPatch, "/settings", token, map[string]any{"blocking": map[string]any{"blockTTL": "30s"}}), primary)
	primaryRequiredAnswer(t, "allow entry", f.request(t, nil, http.MethodPost, "/allowlist", token, map[string]any{"domain": "allowed.example"}), primary)
	primaryRequiredAnswer(t, "personal token", f.request(t, nil, http.MethodPost, "/auth/tokens", token, map[string]any{"description": "refused"}), primary)
	primaryRequiredAnswer(t, "sign-out", f.request(t, nil, http.MethodPost, "/auth/logout", token, nil), primary)
	// Provider login is refused by its own admission, which runs before the
	// request holds a lease: the public CLI route and the browser GET flow.
	cli := f.request(t, nil, http.MethodPost, "/auth/cli/probe/prepare", "", map[string]any{"mode": "browser"})
	primaryRequiredAnswer(t, "CLI login prepare", cli, primary)
	browser := f.request(t, nil, http.MethodGet, "/auth/oidc/probe/start", "", nil)
	primaryRequiredAnswer(t, "OIDC browser start", browser, primary)
	for name, answer := range map[string]followerAnswer{"CLI login prepare": cli, "OIDC browser start": browser} {
		if hint, _ := answer.Error.Hint.(string); !strings.Contains(hint, "OIDC login runs on the primary") {
			t.Errorf("%s: hint does not say where provider login runs: %s", name, answer.raw)
		}
	}
	// The order of admission is unchanged: no credential, then no permission.
	if answer := f.request(t, nil, http.MethodPatch, "/settings", "", map[string]any{}); answer.status != http.StatusUnauthorized {
		t.Fatalf("anonymous PATCH /settings on a follower: %d %s", answer.status, answer.raw)
	}
	var status api.AuthStatus
	if answer := f.request(t, nil, http.MethodGet, "/auth/status", "", nil); answer.status != http.StatusOK || json.Unmarshal(answer.raw, &status) != nil ||
		status.Role != "follower" || status.Primary != primary || len(status.Providers) != 0 || !status.LocalLogin {
		t.Fatalf("follower login options: %d %s", answer.status, answer.raw)
	}

	// Nothing was sent, stored or flushed by any refused request.
	after := roleBindingImage(t, primaryStore)
	if !after.Generation().Same(before.Generation()) {
		t.Fatal("a refused mutation changed the primary")
	}
	if now := followerStore.Authority(); now.InstallSequence != installed.InstallSequence || !now.Installed.Same(installed.Installed) || f.cache.Len() != cached {
		t.Fatal("a refused mutation changed the follower")
	}

	// Forwarded and local operations are never refused for the node's role.
	for _, op := range admitted {
		if answer := f.request(t, nil, op.method, op.path, token, nil); answer.Error.Key == api.KeyPrimaryRequired {
			t.Errorf("%s %s (%s, %s) answered primary_required", op.method, op.path, op.id, api.OperationFollowerClass(op.id))
		}
	}
}

// The command-admission half: a command that reaches a writer seam on a
// request that began on a follower gets the typed refusal, not the generic
// fenced-writer answer, and the private mappers relay it. A request on the
// primary is never refused for its role.
func TestRootMemberFollowerCommandSeamsNameThePrimary(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false)
	primary := f.follower.opts.Store.Authority().Term.PrimaryName
	h, err := newServingHTTP(servingHTTPOptions{Runtime: f.follower, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
	if err != nil {
		t.Fatal(err)
	}
	level := new(slog.LevelVar)
	reload := config.NewReloadable(f.follower.opts.Config)
	nodeCommand, err := newServingNodeCommand(f.follower, reload, level)
	if err != nil {
		t.Fatal(err)
	}
	before := roleBindingImage(t, f.primary.s.opts.Store)
	typed := func(what string, err error) {
		t.Helper()
		refusal, ok := asPrimaryRequired(err)
		if !ok || refusal.Status != http.StatusServiceUnavailable || !strings.Contains(refusal.Body.Message, "follower of "+primary) {
			t.Errorf("%s on a follower request: %v", what, err)
		}
	}
	ran := false
	body := func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error { ran = true; return nil }
	reached := false
	handler := h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		ctx := r.Context()
		if role := f.follower.ClusterRole(ctx); !role.Follower || role.Primary != primary {
			t.Errorf("follower request role: %+v", role)
		}
		typed("currentAuthCommand", f.follower.currentAuthCommand(ctx, body))
		_, err := f.follower.currentAuthCommandThen(ctx, body, nil)
		typed("currentAuthCommandThen", err)
		_, err = f.follower.currentAuthCommandOutput(ctx, new(sqlite.CommandOutputRequest), body, func(context.Context) error { ran = true; return nil })
		typed("currentAuthCommandOutput", err)
		typed("WriteSettings", f.follower.WriteSettings(ctx, f.follower.opts.Config, []string{"blocking.blockTTL"}))
		typed("ObjectCommand", f.follower.ObjectCommand(ctx, store.Key{Kind: "DNSZone", Name: "refused"}, func(context.Context, *sqlite.CommandTx, api.ObjectAuthorize) error {
			ran = true
			return nil
		}))
		next := *f.follower.opts.Config
		next.Log.Level = "debug"
		_, err = nodeCommand(ctx, &next, []string{"log.level"})
		typed("node configuration command", err)
		// No typed command carries a DHCPScope: the forward answers the same refusal.
		_, forwarded, err := f.follower.ObjectForward(ctx, api.ForwardedObject{Operation: api.ForwardedDelete, Kind: "DHCPScope", Namespace: "default", Name: "lan"})
		if !forwarded {
			t.Error("a follower handed a DHCPScope write to its local command")
		}
		typed("DHCPScope object forward", err)
		// The private mappers relay the refusal and the envelope drops Retry-After.
		refusal := f.follower.primaryRequired(ctx)
		typed("tsigKeyAPIError", tsigKeyAPIError(refusal))
		typed("integrationAdminError", integrationAdminError(refusal))
		typed("currentCommandAPIError", currentCommandAPIError(refusal))
		recorder := httptest.NewRecorder()
		servingHTTPError(recorder, r, refusal)
		if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "" || !strings.Contains(recorder.Body.String(), `"key":"primary_required"`) {
			t.Errorf("authentication envelope of the refusal: %d retry-after=%q %s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body)
		}
	}))
	request := httptest.NewRequest(http.MethodPost, api.BasePath+"/allowlist", nil)
	request.Header.Set("Authorization", "Bearer "+f.primary.token)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if !reached || ran {
		t.Fatal("follower request did not reach the seams, or a refused command ran", reached, ran)
	}
	if level.Level() != slog.LevelInfo || reload.Current().Log.Level == "debug" {
		t.Fatal("a refused node configuration command changed the local logger")
	}
	if after := roleBindingImage(t, f.primary.s.opts.Store); !after.Generation().Same(before.Generation()) {
		t.Fatal("a refused command changed the primary")
	}
	if refusal, follower := f.follower.storePrimaryRequired("hint"); !follower || !strings.Contains(refusal.Body.Message, "follower of "+primary) || refusal.Body.Hint != "hint" {
		t.Fatal("follower store role", refusal, follower)
	}
	// A request on the primary keeps its ordinary answers.
	reached = false
	f.primary.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		if role := f.primary.s.ClusterRole(r.Context()); role.Follower || role.Primary != "" {
			t.Errorf("primary request role: %+v", role)
		}
		if err := f.primary.s.primaryRequired(r.Context()); err != nil {
			t.Error("primary request refused for its role", err)
		}
	})).ServeHTTP(httptest.NewRecorder(), f.primary.request())
	if _, follower := f.primary.s.storePrimaryRequired(""); follower || !reached {
		t.Fatal("primary store reported as a follower, or its request was not served", follower, reached)
	}
	if message := unblockRequestsUnavailable(f.follower.opts.Store)(); message == "" {
		t.Fatal("a follower's block page takes unblock requests that its store cannot keep")
	}
	if message := unblockRequestsUnavailable(f.primary.s.opts.Store)(); message != "" {
		t.Fatal("the primary's block page refuses unblock requests:", message)
	}
}

// POST /cache/flush is node-local: an enrolled follower flushes its own
// response cache, with the route's permission and CSRF checks, without a
// store write on either node and without a command to the primary.
func TestRootMemberFollowerCacheFlushIsNodeLocal(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false, memberForwardScheduledPull)
	primaryStore, followerStore := f.primary.s.opts.Store, f.follower.opts.Store
	// A viewer token and a password for the cookie session, written on the
	// primary and installed on the follower by its own pull.
	viewer, viewerID := tokens.Generate()
	hash, err := users.NewHasher(1).Hash(t.Context(), "follower-cache-password-1")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := primaryStore.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := primaryStore.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
			return err
		}
		user.Spec.PasswordHash = hash
		if _, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		expires := metav1.NewTime(f.primary.expires)
		_, err := tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: viewerID, CreationTimestamp: metav1.NewTime(f.primary.now.Add(-time.Minute))},
			Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(viewer), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: user.GetName(), UID: string(user.GetUID()), ID: "local:root-command", Provider: "local"},
				Scopes: []string{"viewer"}, ExpiresAt: &expires}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.primary.s.publisher.Wake()
	pull, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if !kudurrutestEventually(20*time.Second, func() bool {
		_ = f.puller.Pull(pull)
		var token internalv1.APIToken
		installed := followerStore.Authority().Installed
		return followerStore.Get(pull, store.Key{Kind: "APIToken", Name: viewerID}, &token) == nil && roleBindingImage(t, primaryStore).Generation().Same(installed) &&
			f.follower.publisher.Status().ProtectedReady && f.follower.publisher.Status().Served.Base.Same(installed)
	}) {
		t.Fatal("follower did not install and publish the fixture credentials")
	}
	settled := func() (replication.Generation, replication.Authority) {
		t.Helper()
		return roleBindingImage(t, primaryStore).Generation(), followerStore.Authority()
	}

	// Bearer admin: both scoped entries of the name are removed here.
	primaryBefore, followerBefore := settled()
	var flushed api.FlushResult
	answer := f.request(t, nil, http.MethodPost, "/cache/flush", f.primary.token, map[string]any{"name": "target.example"})
	if answer.status != http.StatusOK || json.Unmarshal(answer.raw, &flushed) != nil || flushed.Flushed != 2 || flushed.Node != "receiver" || f.cache.Len() != 0 {
		t.Fatalf("follower cache flush: %d %s (cache entries left: %d)", answer.status, answer.raw, f.cache.Len())
	}
	primaryAfter, followerAfter := settled()
	if !primaryAfter.Same(primaryBefore) || followerAfter.InstallSequence != followerBefore.InstallSequence || !followerAfter.Installed.Same(followerBefore.Installed) {
		t.Fatal("a node-local cache flush wrote a store or reached the primary")
	}

	// Permission: a viewer is refused by the route and by the command itself.
	refill := func() {
		for _, scope := range []pipeline.CacheScope{{ID: [32]byte{1}, Epoch: 1}, {ID: [32]byte{2}, Epoch: 2}} {
			f.cache.Set(cache.MakeScopedKey(scope, "target.example.", 1, 1, false, false, netip.Prefix{}), []byte("wire"), cache.Positive, time.Minute)
		}
	}
	refill()
	if answer := f.request(t, nil, http.MethodPost, "/cache/flush", viewer, nil); answer.status != http.StatusForbidden || answer.Error.Key != api.KeyForbidden || f.cache.Len() != 2 {
		t.Fatalf("viewer cache flush on a follower: %d %s (cache entries left: %d)", answer.status, answer.raw, f.cache.Len())
	}
	h, err := newServingHTTP(servingHTTPOptions{Runtime: f.follower, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
	if err != nil {
		t.Fatal(err)
	}
	command, err := newServingCacheCommand(f.follower, f.cache)
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	direct := httptest.NewRequest(http.MethodPost, api.BasePath+"/cache/flush", nil)
	direct.Header.Set("Authorization", "Bearer "+viewer)
	h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		reached = true
		var refusal *api.Error
		if removed, err := command(r.Context(), ""); removed != 0 || !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
			t.Errorf("follower-local command admitted a credential without cache.flush: removed=%d err=%v", removed, err)
		}
	})).ServeHTTP(httptest.NewRecorder(), direct)
	if !reached || f.cache.Len() != 2 {
		t.Fatal("unauthorized follower-local command flushed the cache, or was not reached", reached, f.cache.Len())
	}

	// CSRF: a cookie session created through the follower's forwarded login.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := *f.live.Client()
	browser.Jar = jar
	var login api.SessionInfo
	if !kudurrutestEventually(20*time.Second, func() bool {
		answer = f.request(t, &browser, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "root-command", Password: "follower-cache-password-1"})
		return answer.status == http.StatusOK && json.Unmarshal(answer.raw, &login) == nil && login.CSRF != ""
	}) {
		t.Fatalf("login through the follower: %d %s", answer.status, answer.raw)
	}
	if answer := f.request(t, &browser, http.MethodPost, "/cache/flush", "", nil); answer.status != http.StatusForbidden || answer.Error.Key != api.KeyCSRF || f.cache.Len() != 2 {
		t.Fatalf("cookie cache flush without the CSRF header: %d %s (cache entries left: %d)", answer.status, answer.raw, f.cache.Len())
	}
	answer = f.request(t, &browser, http.MethodPost, "/cache/flush", "", nil, "X-Kudurru-CSRF", login.CSRF)
	if answer.status != http.StatusOK || json.Unmarshal(answer.raw, &flushed) != nil || flushed.Flushed != 2 || f.cache.Len() != 0 {
		t.Fatalf("cookie cache flush with the CSRF header: %d %s (cache entries left: %d)", answer.status, answer.raw, f.cache.Len())
	}
}
