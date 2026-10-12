package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// TestCentralAuditTwoNodes: two sqlite nodes (the compose shapes) with one
// central database (D158, D159). A change on either node appears in GET
// /audit on both; while the database is away the change waits in state.db
// and appears once it is back.
func TestCentralAuditTwoNodes(t *testing.T) {
	kudurrutest.RequireE2E(t)
	dsn := centraltest.DSN(t)
	dir := t.TempDir()
	env := []string{"KUDURRU_DATABASE__DSN=" + dsn}
	var nodes []*kudurrutest.Node
	tokens := map[string]string{}
	for _, name := range []string{"node-a", "node-b"} {
		spec, cfg := kudurrutest.NewNodeSpec(t, dir, name)
		kudurrutest.WriteConfig(t, spec.Config, cfg)
		n := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: env})
		nodes = append(nodes, n)
		out, err := exec.Command(kudurruBin, "admin", "create-token", "--socket", filepath.Join(spec.DataDir, "admin.sock")).Output() //nolint:gosec // the test's binary
		if err != nil {
			t.Fatalf("create-token on %s: %v", name, err)
		}
		tokens[name] = strings.TrimSpace(string(out))
	}
	call := func(n *kudurrutest.Node, method, path string, body any) (int, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, "http://"+n.Ports.API+"/api/v1"+path, rd)
		req.Header.Set("Authorization", "Bearer "+tokens[n.Name])
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	seen := func(n *kudurrutest.Node, action, node string) bool {
		code, b := call(n, http.MethodGet, "/audit?order=desc&limit=200&kind=blocking", nil)
		if code != http.StatusOK {
			return false
		}
		var page struct {
			Items []struct {
				Action string `json:"action"`
				Node   string `json:"node"`
				Reason string `json:"reason"`
			} `json:"items"`
		}
		_ = json.Unmarshal(b, &page)
		for _, it := range page.Items {
			if it.Action == action && it.Node == node {
				return true
			}
		}
		return false
	}
	if code, b := call(nodes[0], http.MethodPost, "/dns/blocking", map[string]any{"blocking": false, "timer": 30}); code != http.StatusOK {
		t.Fatalf("toggle on node-a: %d %s", code, b)
	}
	if code, b := call(nodes[1], http.MethodPost, "/dns/blocking", map[string]any{"blocking": true}); code != http.StatusOK {
		t.Fatalf("toggle on node-b: %d %s", code, b)
	}
	for _, n := range nodes {
		for _, from := range []string{"node-a", "node-b"} {
			if !kudurrutest.Eventually(30*time.Second, 200*time.Millisecond, func() bool { return seen(n, "blocking.toggle", from) }) {
				t.Fatalf("GET /audit on %s lacks node %s's toggle", n.Name, from)
			}
		}
	}

	// The audit table goes away (as for an outage, writes and reads
	// fail): a change waits in node-a's state.db, the API answers 503,
	// and the change appears when the table is back.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `ALTER TABLE audit RENAME TO audit_away`); err != nil {
		t.Fatal(err)
	}
	if code, b := call(nodes[0], http.MethodPost, "/cache/flush", map[string]any{}); code != http.StatusOK {
		t.Fatalf("flush on node-a: %d %s", code, b)
	}
	if code, _ := call(nodes[1], http.MethodGet, "/audit", nil); code != http.StatusInternalServerError && code != http.StatusServiceUnavailable {
		t.Fatalf("GET /audit without the table: %d", code)
	}
	time.Sleep(3 * time.Second) // the shipper tries and fails
	if _, err := conn.Exec(ctx, `ALTER TABLE audit_away RENAME TO audit`); err != nil {
		t.Fatal(err)
	}
	flushed := func(n *kudurrutest.Node) bool {
		code, b := call(n, http.MethodGet, "/audit?order=desc&kind=cache", nil)
		return code == http.StatusOK && strings.Contains(string(b), `"cache.flush"`)
	}
	if !kudurrutest.Eventually(90*time.Second, 500*time.Millisecond, func() bool { return flushed(nodes[1]) }) {
		t.Fatal("the flush recorded during the outage never reached the central database")
	}
}
