package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// TestCentralQuerylogTwoNodes (#9 with a central database, D160, D161):
// two nodes answer queries; "All nodes" on either shows both nodes' rows
// from the central database, and "this node" only its own.
func TestCentralQuerylogTwoNodes(t *testing.T) {
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
	// Each node answers a query for its own name (forwarded to the
	// discard port: SERVFAIL, logged all the same).
	c := &dns.Client{Timeout: 5 * time.Second}
	for _, n := range nodes {
		m := new(dns.Msg)
		m.SetQuestion(n.Name+".central-e2e.example.", dns.TypeA)
		if _, _, err := c.Exchange(m, n.Ports.DNS); err != nil {
			t.Fatalf("query %s: %v", n.Name, err)
		}
	}
	type page struct {
		Scope   string `json:"scope"`
		Source  string `json:"source"`
		Queries []struct {
			Node   string `json:"node"`
			Domain string `json:"domain"`
		} `json:"queries"`
	}
	get := func(n *kudurrutest.Node, path string) (page, int) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+n.Ports.API+"/api/v1"+path, nil)
		req.Header.Set("Authorization", "Bearer "+tokens[n.Name])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		var p page
		_ = json.Unmarshal(b, &p)
		return p, resp.StatusCode
	}
	has := func(p page, node string) bool {
		for _, q := range p.Queries {
			if q.Node == node && strings.HasPrefix(q.Domain, node+".central-e2e") {
				return true
			}
		}
		return false
	}
	for _, n := range nodes {
		var p page
		if !kudurrutest.Eventually(60*time.Second, 300*time.Millisecond, func() bool {
			var code int
			p, code = get(n, "/queries?scope=cluster&domain=*.central-e2e.example")
			return code == http.StatusOK && has(p, "node-a") && has(p, "node-b")
		}) {
			t.Fatalf("GET /queries?scope=cluster on %s: %+v", n.Name, p)
		}
		if p.Scope != "cluster" || p.Source != "central" {
			t.Fatalf("scope %q source %q", p.Scope, p.Source)
		}
		own, code := get(n, "/queries?domain=*.central-e2e.example")
		other := "node-b"
		if n.Name == "node-b" {
			other = "node-a"
		}
		if code != http.StatusOK || own.Scope != "node" || !has(own, n.Name) || has(own, other) {
			t.Fatalf("this node's log on %s: %d %+v", n.Name, code, own)
		}
	}
}
