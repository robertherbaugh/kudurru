package api

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// mutation is one registered operation with an unsafe method.
type mutation struct {
	method, path, id string
	op               *huma.Operation
}

func registeredMutations(t *testing.T, s *Server) []mutation {
	t.Helper()
	var out []mutation
	for path, item := range s.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{
			http.MethodPost: item.Post, http.MethodPut: item.Put, http.MethodPatch: item.Patch, http.MethodDelete: item.Delete,
		} {
			if op != nil {
				out = append(out, mutation{method: method, path: path, id: op.OperationID, op: op})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

var pathParameter = regexp.MustCompile(`\{[^}]+\}`)

// probePath fills the path parameters of a route with values the router and
// the permission check accept.
func probePath(path string) string {
	path = strings.NewReplacer("{kind}", "dnszones", "{namespace}", "default", "{type}", "A", "{zone}", "example").Replace(path)
	return BasePath + pathParameter.ReplaceAllString(path, "probe")
}

// TestFollowerOperationsClassifyEveryMutation is the inventory lint of D368:
// every registered operation with an unsafe method has a reviewed line in
// followerOperations, the table names no operation that is not registered,
// every such route is in the OpenAPI document, and the document carries the
// class. A route added later fails here until it is classified, and until
// then a follower refuses it.
func TestFollowerOperationsClassifyEveryMutation(t *testing.T) {
	h := newHarness(t)
	registered := map[string]bool{}
	counts := map[FollowerClass]int{}
	for _, m := range registeredMutations(t, h.srv) {
		registered[m.id] = true
		class, named := followerOperations[m.id]
		if !named {
			t.Errorf("%s %s (%s) has no line in followerOperations: classify it as forwarded, local or primary", m.method, m.path, m.id)
			continue
		}
		switch class {
		case FollowerForwarded, FollowerLocal, FollowerPrimary:
		default:
			t.Errorf("%s has the unknown follower class %q", m.id, class)
		}
		counts[class]++
		if got := m.op.Extensions[followerExtension]; got != string(class) {
			t.Errorf("%s documents %s=%v, want %q", m.id, followerExtension, got, class)
		}
	}
	for id := range followerOperations {
		if !registered[id] {
			t.Errorf("followerOperations names %q, which is no registered mutation", id)
		}
	}
	if OperationFollowerClass("an operation added later") != FollowerPrimary {
		t.Error("an unnamed operation is not refused by default")
	}
	// Nothing with an unsafe method is routed outside the OpenAPI document.
	unsafe := 0
	for _, route := range h.srv.Routes() {
		if unsafeMethod(route.Method) {
			unsafe++
		}
	}
	if unsafe != len(registered) {
		t.Errorf("%d routes have an unsafe method but the OpenAPI document has %d such operations", unsafe, len(registered))
	}
	// Safe methods carry no class.
	for path, item := range h.srv.OpenAPI().Paths {
		if item.Get != nil && item.Get.Extensions[followerExtension] != nil {
			t.Errorf("GET %s carries a follower class", path)
		}
	}
	// The reviewed sizes of the contract. Update them with the table and
	// with docs/deploy/sqlite-join.md.
	if counts[FollowerForwarded] != 33 || counts[FollowerLocal] != 14 || counts[FollowerPrimary] != 49 {
		t.Errorf("follower contract changed: forwarded=%d local=%d primary=%d", counts[FollowerForwarded], counts[FollowerLocal], counts[FollowerPrimary])
	}
}

func followerRole(context.Context) ClusterRole { return ClusterRole{Follower: true, Primary: "node-b"} }

// On an enrolled follower every FollowerPrimary operation answers the one
// typed refusal before its handler: after authentication and the permission
// check, without Retry-After, naming the primary. Forwarded and local
// operations are never refused that way, and no node in another role is.
func TestFollowerRefusesEveryUnforwardedMutation(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ClusterRole = followerRole })
	plain := newHarness(t)
	for _, m := range registeredMutations(t, h.srv) {
		class := OperationFollowerClass(m.id)
		r := h.do(m.method, probePath(m.path), "admin", nil)
		refused := r.Code == http.StatusServiceUnavailable && r.errKey() == KeyPrimaryRequired
		if refused != (class == FollowerPrimary) {
			t.Errorf("%s %s (%s, %s) on a follower: status=%d key=%q", m.method, m.path, m.id, class, r.Code, r.errKey())
			continue
		}
		if elsewhere := plain.do(m.method, probePath(m.path), "admin", nil); elsewhere.errKey() == KeyPrimaryRequired {
			t.Errorf("%s answers primary_required on a node that is no follower", m.id)
		}
		if !refused {
			continue
		}
		var body struct {
			Error struct {
				Message string `json:"message"`
				Hint    string `json:"hint"`
			} `json:"error"`
		}
		r.json(&body)
		if !strings.Contains(body.Error.Message, "follower of node-b") || body.Error.Hint == "" {
			t.Errorf("%s: refusal does not name the primary or carries no hint: %s", m.id, r.Body)
		}
		if r.Header().Get("Retry-After") != "" {
			t.Errorf("%s: refusal carries Retry-After", m.id)
		}
		// The permission check still comes first.
		if perm, _ := m.op.Extensions["x-kudurru-permission"].(string); perm != "public" {
			if anonymous := h.do(m.method, probePath(m.path), "", nil); anonymous.Code != http.StatusUnauthorized {
				t.Errorf("%s without credentials on a follower: status=%d key=%q", m.id, anonymous.Code, anonymous.errKey())
			}
		}
	}
	// A caller without the permission learns that, not the node's role.
	if r := h.do(http.MethodPatch, BasePath+"/settings", "viewer", map[string]any{"blocking": map[string]any{"enabled": false}}); r.Code != http.StatusForbidden || r.errKey() != KeyForbidden {
		t.Fatalf("viewer PATCH /settings on a follower: status=%d key=%q", r.Code, r.errKey())
	}
	// Nothing ran: no handler recorded an action and nothing was stored.
	if events := h.audit.Events(); len(events) != 0 {
		for _, ev := range events {
			if ev.Result != "denied" {
				t.Errorf("a refused mutation recorded %s %s", ev.Action, ev.Result)
			}
		}
	}
	// The node-configuration refusal says what to do on this node instead.
	r := h.do(http.MethodPatch, BasePath+"/node/config", "admin", map[string]any{"log": map[string]any{"level": "debug"}})
	if r.Code != http.StatusServiceUnavailable || r.errKey() != KeyPrimaryRequired || !strings.Contains(r.Body.String(), "configuration file or environment") {
		t.Fatalf("PATCH /node/config on a follower: %d %s", r.Code, r.Body)
	}
}

func TestPrimaryRequiredWithoutAPrimaryName(t *testing.T) {
	e := PrimaryRequired("")
	if e.Status != http.StatusServiceUnavailable || e.Body.Key != KeyPrimaryRequired || strings.Contains(e.Body.Message, "follower of") || len(e.GetHeaders()) != 0 {
		t.Fatalf("refusal without a primary name: %+v", e)
	}
	if hint, _ := PrimaryRequiredWithHint("node-b", "do this instead").Body.Hint.(string); hint != "do this instead" {
		t.Fatalf("hint was not kept: %q", hint)
	}
}

// GET /cluster and the settings schema say what the mutation routes of the
// same node would answer.
func TestClusterInfoAndSchemaReportFollowerCapability(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.ClusterRole = followerRole })
	var info ClusterInfo
	h.do(http.MethodGet, BasePath+"/cluster", "admin", nil).want(http.StatusOK).json(&info)
	if info.Role != "follower" || info.Primary != "node-b" || info.Leader != "node-b" || !info.PrimaryRequired {
		t.Fatalf("follower cluster info: %+v", info)
	}
	for id, class := range followerOperations {
		if got := slices.Contains(info.ForwardedOperations, id); got != (class == FollowerForwarded) {
			t.Errorf("forwardedOperations contains %s: %t (class %s)", id, got, class)
		}
		if got := slices.Contains(info.LocalOperations, id); got != (class == FollowerLocal) {
			t.Errorf("localOperations contains %s: %t (class %s)", id, got, class)
		}
	}
	if !slices.IsSorted(info.ForwardedOperations) || !slices.IsSorted(info.LocalOperations) {
		t.Error("operation lists are not sorted")
	}
	var schema struct {
		Items []SettingKey `json:"items"`
	}
	h.do(http.MethodGet, BasePath+"/settings/_schema", "admin", nil).want(http.StatusOK).json(&schema)
	if len(schema.Items) == 0 {
		t.Fatal("empty settings schema")
	}

	plain := newHarness(t)
	var raw map[string]any
	plain.do(http.MethodGet, BasePath+"/cluster", "admin", nil).want(http.StatusOK).json(&raw)
	if raw["primaryRequired"] != false || raw["forwardedOperations"] != nil || raw["localOperations"] != nil || raw["role"] != nil || raw["primary"] != nil {
		t.Fatalf("cluster info of a node that is no follower: %v", raw)
	}
	// A fresh value: decoding into the used one would keep its omitted fields.
	var ordinary struct {
		Items []SettingKey `json:"items"`
	}
	plain.do(http.MethodGet, BasePath+"/settings/_schema", "admin", nil).want(http.StatusOK).json(&ordinary)
	writable := 0
	for _, key := range ordinary.Items {
		if !key.ReadOnly && key.Scope == "cluster" {
			writable++
		}
	}
	if writable == 0 {
		t.Fatal("no cluster setting is writable on a node that is no follower")
	}
	// A follower forwards PATCH /settings (settings.patch) and refuses PATCH
	// /node/config: its cluster keys are as writable as on any other node,
	// and its node keys are read-only.
	same := map[string]bool{}
	for _, key := range ordinary.Items {
		same[key.Path] = key.ReadOnly
	}
	for _, key := range schema.Items {
		switch {
		case key.Scope == "node" && !key.ReadOnly:
			t.Errorf("%s (node) is writable in a follower's schema although its PATCH is refused", key.Path)
		case key.Scope == "cluster" && key.ReadOnly != same[key.Path]:
			t.Errorf("%s (cluster) is readOnly=%t in a follower's schema, %t elsewhere, although its PATCH is forwarded", key.Path, key.ReadOnly, same[key.Path])
		}
	}
}
