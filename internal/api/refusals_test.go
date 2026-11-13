package api

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// TestRefusalInventoryNamesEveryOperation is the static half of the refusal
// inventory (G21-11): every operation of the OpenAPI document has a line in
// operationRefusals, the table names no operation that is not registered,
// every key it declares is in the refusalKeys allowlist, and every allowlist
// row names an anchor that exists on the operator page. The mounted walks in
// cmd/kudurru and test/e2e hold each line to what each runtime shape answers.
func TestRefusalInventoryNamesEveryOperation(t *testing.T) {
	h := newHarness(t)
	registered := map[string]bool{}
	for path, item := range h.srv.OpenAPI().Paths {
		for method, op := range map[string]*huma.Operation{
			http.MethodGet: item.Get, http.MethodPost: item.Post, http.MethodPut: item.Put, http.MethodPatch: item.Patch, http.MethodDelete: item.Delete,
		} {
			if op == nil {
				continue
			}
			registered[op.OperationID] = true
			if _, named := operationRefusals[op.OperationID]; !named {
				t.Errorf("%s %s (%s) has no line in operationRefusals (internal/api/refusals.go): add one that names the 501/503 key it answers on each runtime shape, or an empty line if it answers none; run the refusal inventory walks (TestRunServeRefusalInventory* and TestRootMemberRefusalInventoryClosedMembers in cmd/kudurru, the enrolled process tests in test/e2e) with KUDURRU_REFUSAL_INVENTORY_DUMP=1 to see what each shape answers",
					method, path, op.OperationID)
			}
		}
	}
	for id, line := range operationRefusals {
		if !registered[id] {
			t.Errorf("operationRefusals names %q, which is no registered operation", id)
		}
		for shape, key := range line {
			switch shape {
			case ShapeStandalone, ShapePrimary, ShapeStatic, ShapeFollower, ShapeKubernetes:
			default:
				t.Errorf("%s declares a refusal for the shape %q, which no line may name", id, shape)
			}
			if key == noRefusal || key == "" {
				if key == "" {
					t.Errorf("%s declares an empty key on %s: leave the shape out, or use noRefusal", id, shape)
				}
				continue
			}
			if _, ok := refusalKeys["503 "+key]; !ok {
				if _, ok := refusalKeys["501 "+key]; !ok {
					t.Errorf("%s declares %q on %s, a key without a row in refusalKeys", id, key, shape)
				}
			}
		}
	}
	for id := range featureRefusals {
		if !registered[id] {
			t.Errorf("featureRefusals names %q, which is no registered operation", id)
		}
	}
	for id := range featureRefusalShapes {
		if _, ok := featureRefusals[id]; !ok {
			t.Errorf("featureRefusalShapes names %q, which has no line in featureRefusals", id)
		}
	}
	for id := range transientUnavailable {
		if !registered[id] {
			t.Errorf("transientUnavailable names %q, which is no registered operation", id)
		}
	}
	if _, declared := ExpectedRefusal(ShapeStandalone, "an operation added later", http.MethodPost); declared {
		t.Error("an operation without a line counts as declared")
	}

	// Every allowlist row names an anchor that exists.
	pages := map[string]string{}
	for status, row := range refusalKeys {
		file, anchor, ok := strings.Cut(row.doc, "#")
		if !ok || anchor == "" {
			t.Errorf("refusalKeys[%q] names %q, which is no doc anchor", status, row.doc)
			continue
		}
		page, read := pages[file]
		if !read {
			raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(file)))
			if err != nil {
				t.Errorf("refusalKeys[%q]: %v", status, err)
				continue
			}
			page = string(raw)
			pages[file] = page
		}
		if !strings.Contains(page, `id="`+anchor+`"`) {
			t.Errorf("refusalKeys[%q] names %s, but %s has no anchor %q", status, row.doc, file, anchor)
		}
	}
}

// The inheritance rules of ExpectedRefusal.
func TestExpectedRefusalInheritance(t *testing.T) {
	for _, tc := range []struct {
		shape        RuntimeShape
		id, method   string
		want         string
		wantDeclared bool
	}{
		// The follower refuses a FollowerPrimary mutation whatever the line
		// says, and inherits the standalone key otherwise.
		{ShapeFollower, "createAllowlistEntries", http.MethodPost, KeyPrimaryRequired, true},
		{ShapeFollower, "exportSettingsSectionYAML", http.MethodPost, KeyUnavailable, true},
		{ShapeFollower, "flushCache", http.MethodPost, "", true},
		// Static and primary inherit the standalone key; a line overrides it.
		{ShapeStatic, "createToken", http.MethodPost, KeyReadOnly, true},
		{ShapePrimary, "exportSettingsSectionYAML", http.MethodPost, KeyUnavailable, true},
		{ShapeStandalone, "createToken", http.MethodPost, "", true},
		// Kubernetes inherits nothing.
		{ShapeKubernetes, "exportSettingsSectionYAML", http.MethodPost, "", true},
		{ShapeKubernetes, "listSessions", http.MethodGet, "sessions_unavailable", true},
		// A fenced or conflicted member refuses everything.
		{ShapeFenced, "getCluster", http.MethodGet, "cluster_fenced", true},
		{ShapeConflicted, "createToken", http.MethodPost, "cluster_conflict", true},
		{ShapeFenced, "an operation added later", http.MethodGet, "", false},
	} {
		got, declared := ExpectedRefusal(tc.shape, tc.id, tc.method)
		if got != tc.want || declared != tc.wantDeclared {
			t.Errorf("ExpectedRefusal(%s, %s) = %q, %t; want %q, %t", tc.shape, tc.id, got, declared, tc.want, tc.wantDeclared)
		}
	}
	// A feature that the node turns off adds its refusal on every shape
	// but a closed member's.
	for _, shape := range []RuntimeShape{ShapeStandalone, ShapeStatic, ShapeFollower, ShapeKubernetes} {
		if got, _ := ExpectedRefusal(shape, "listQueries", http.MethodGet, FeatureQueryLog); got != KeyUnavailable {
			t.Errorf("listQueries on %s without the query log: %q", shape, got)
		}
		if got, _ := ExpectedRefusal(shape, "listQueries", http.MethodGet, FeatureDHCP); got != "" {
			t.Errorf("listQueries on %s without DHCP: %q", shape, got)
		}
	}
	// A line of featureRefusalShapes limits the feature's refusal on a Linux
	// host: only a Kubernetes node without DHCP refuses a DHCP setup write.
	// On another host no SQLite node opens the setup manager either.
	defer func(os string) { hostOS = os }(hostOS)
	for os, wants := range map[string]map[RuntimeShape]string{
		"linux":  {ShapeKubernetes: KeyUnavailable, ShapeStandalone: "", ShapeFollower: "", ShapeStatic: KeyReadOnly},
		"darwin": {ShapeKubernetes: KeyUnavailable, ShapeStandalone: KeyUnavailable, ShapePrimary: KeyUnavailable, ShapeFollower: KeyUnavailable, ShapeStatic: KeyReadOnly},
	} {
		hostOS = os
		for shape, want := range wants {
			if got, _ := ExpectedRefusal(shape, "putDHCPSetup", http.MethodPut, FeatureDHCP); got != want {
				t.Errorf("putDHCPSetup on %s (%s host) without DHCP: %q, want %q", shape, os, got, want)
			}
		}
	}
	if got, _ := ExpectedRefusal(ShapeKubernetes, "putDHCPSetup", http.MethodPut); got != "" {
		t.Errorf("putDHCPSetup on Kubernetes with DHCP: %q", got)
	}
	if got, _ := ExpectedRefusal(ShapeStandalone, "rollupEvidence", http.MethodGet, FeatureCentralHistory); got != KeyUnavailable {
		t.Errorf("rollupEvidence without a central database: %q", got)
	}
	if got, _ := ExpectedRefusal(ShapeFenced, "listQueries", http.MethodGet, FeatureQueryLog); got != "cluster_fenced" {
		t.Errorf("listQueries on a fenced member: %q", got)
	}
	if !slices.Equal(SessionEndingOperations(), []string{"logout", "browserLogout"}) {
		t.Error("session-ending operations changed:", SessionEndingOperations())
	}
}

// A declared unavailable names a durable limit and carries no Retry-After,
// except for the operations whose provider may become reachable.
func TestRefusalRetryAfterRules(t *testing.T) {
	for _, tc := range []struct {
		id, key           string
		status            int
		allowed, required bool
	}{
		{"getClusterEnrollment", KeyUnavailable, http.StatusServiceUnavailable, false, false},
		{"issueClusterJoinToken", KeyUnavailable, http.StatusServiceUnavailable, false, false},
		{"getClusterPromotion", KeyUnavailable, http.StatusServiceUnavailable, false, false},
		{"listIntegrations", KeyUnavailable, http.StatusServiceUnavailable, false, false},
		{"oidcStart", KeyUnavailable, http.StatusServiceUnavailable, true, false},
		{"verifyCLILogin", KeyUnavailable, http.StatusServiceUnavailable, true, false},
		{"oidcStart", "sessions_unavailable", http.StatusServiceUnavailable, false, false},
		{"listSessions", "generation_pending", http.StatusServiceUnavailable, true, true},
		{"login", "completion_unknown", http.StatusServiceUnavailable, true, false},
	} {
		allowed, required := RefusalRetryAfterAllowed(tc.id, tc.key, tc.status)
		if allowed != tc.allowed || required != tc.required {
			t.Errorf("%s %d %s: Retry-After allowed=%t required=%t, want %t %t", tc.id, tc.status, tc.key, allowed, required, tc.allowed, tc.required)
		}
	}
}
