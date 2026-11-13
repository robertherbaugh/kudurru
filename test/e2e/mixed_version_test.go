package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// The mixed-version tests run the released binary of the previous release
// (kudurrutest.Previous, v0.1.0) next to the binary under test, as during a
// host-by-host upgrade, and check what D373 (3) says about mixed versions:
// a static twin of this release reads an older twin's internal listener and
// says what it cannot read, a central database migrated by this release
// stops an older node from shipping without stopping its DNS, and an
// enrolled directory is refused by the older binary without being touched.

// v01InternalProbe is a client of the internal listener written against the
// packages of the previous release: what a 0.1.0 node or edge supervisor
// sends to a peer. It reads one peer's status, served zones, the blocked
// services catalog and the list body of one digest, and prints what it got
// as JSON. PROBE_KEY is the shared encryption key in hex.
const v01InternalProbe = `package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

type result struct {
	Status         *internalapi.Status   ` + "`json:\"status,omitempty\"`" + `
	StatusError    string                ` + "`json:\"statusError,omitempty\"`" + `
	Zones          *internalapi.ZoneList ` + "`json:\"zones,omitempty\"`" + `
	ZonesError     string                ` + "`json:\"zonesError,omitempty\"`" + `
	ListDigest     string                ` + "`json:\"listDigest,omitempty\"`" + `
	ListError      string                ` + "`json:\"listError,omitempty\"`" + `
	CatalogueBytes int                   ` + "`json:\"catalogueBytes\"`" + `
	CatalogueError string                ` + "`json:\"catalogueError,omitempty\"`" + `
}

func main() {
	key, err := hex.DecodeString(os.Getenv("PROBE_KEY"))
	if err != nil {
		panic(err)
	}
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: key, NodeName: "v01-probe"})
	if err != nil {
		panic(err)
	}
	c := internalapi.NewClient(creds, internalapi.ClientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	addr, digest := os.Args[1], os.Args[2]
	var r result
	if s, err := c.Status(ctx, addr); err != nil {
		r.StatusError = err.Error()
	} else {
		r.Status = &s
	}
	if z, err := c.Zones(ctx, addr); err != nil {
		r.ZonesError = err.Error()
	} else {
		r.Zones = &z
	}
	if body, _, err := c.OpenList(ctx, addr, digest); err != nil {
		r.ListError = err.Error()
	} else {
		h := sha256.New()
		_, err := io.Copy(h, body)
		if closeErr := body.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			r.ListError = err.Error()
		} else {
			r.ListDigest = hex.EncodeToString(h.Sum(nil))
		}
	}
	if cat, err := c.Catalogue(ctx, addr); err != nil {
		r.CatalogueError = err.Error()
	} else if b, err := json.Marshal(cat); err != nil {
		r.CatalogueError = err.Error()
	} else {
		r.CatalogueBytes = len(b)
	}
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		panic(err)
	}
}
`

// v01ProbeResult is what v01InternalProbe prints.
type v01ProbeResult struct {
	Status *struct {
		Node    string `json:"node"`
		Version string `json:"version"`
		Leader  string `json:"leader"`
		State   string `json:"state"`
		Lists   []struct {
			Name    string   `json:"name"`
			Digests []string `json:"digests"`
		} `json:"lists"`
		Ready *bool `json:"ready"`
	} `json:"status"`
	StatusError string `json:"statusError"`
	Zones       *struct {
		Node  string `json:"node"`
		Zones []struct {
			Name   string `json:"name"`
			Serial uint32 `json:"serial"`
		} `json:"zones"`
	} `json:"zones"`
	ZonesError     string `json:"zonesError"`
	ListDigest     string `json:"listDigest"`
	ListError      string `json:"listError"`
	CatalogueBytes int    `json:"catalogueBytes"`
	CatalogueError string `json:"catalogueError"`
}

// runV01Probe runs the probe of the previous release against the internal
// listener at addr and asks for the list body of digest.
func runV01Probe(t *testing.T, probe string, key []byte, addr, digest string) v01ProbeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, probe, addr, digest) //nolint:gosec // the test's program
	cmd.Env = append(os.Environ(), "PROBE_KEY="+hex.EncodeToString(key))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s probe of %s: %v\n%s", kudurrutest.PreviousRelease, addr, err, stderr.String())
	}
	var r v01ProbeResult
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%s probe of %s: %v: %s", kudurrutest.PreviousRelease, addr, err, out)
	}
	return r
}

// internalRequests reads kudurru_internal_requests_total of a node's
// metrics listener, by route.
func internalRequests(t *testing.T, metrics string) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+metrics+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics of %s: %v", metrics, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]int{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "kudurru_internal_requests_total{") {
			continue
		}
		labels, value, ok := strings.Cut(strings.TrimPrefix(line, "kudurru_internal_requests_total{"), "} ")
		if !ok {
			t.Fatalf("metrics of %s: %q", metrics, line)
		}
		route := ""
		for _, l := range strings.Split(labels, ",") {
			if v, ok := strings.CutPrefix(l, `route="`); ok {
				route = strings.TrimSuffix(v, `"`)
			}
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("metrics of %s: %q", metrics, line)
		}
		out[route] += int(n)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// sessionClient logs in as admin with the password of the upgrade
// fixtures.
func sessionClient(t *testing.T, apiAddr string) *staticProcessClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	c := &staticProcessClient{client: &http.Client{Jar: jar, Transport: transport, Timeout: 5 * time.Second}, base: "http://" + apiAddr + api.BasePath}
	var session api.SessionInfo
	c.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: upgradePassword}, http.StatusOK, &session)
	c.csrf = session.CSRF
	return c
}

// sharedKey is a new encryption key shared by the nodes of one test, and
// the environment that hands it and the fixture password to a node.
func sharedKey(t *testing.T) ([]byte, []string) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key, []string{"KUDURRU_ENCRYPTION_KEY=" + hex.EncodeToString(key), "KUDURRU_ADMIN_PASSWORD=" + upgradePassword}
}

// TestMixedVersionStaticPeerV01 is the host-by-host upgrade of static twins
// (docs/deploy/compose-ha.md): one host still runs the released 0.1.0
// binary as a single node, the other already runs the binary under test
// with `cluster.role: static` and the old host as a peer, both with the
// same encryption key and the same object files. The new twin reports the
// old one's version and readiness on /nodes and says that the old one has
// no health route instead of guessing, a 0.1.0 client reads the new twin's
// status, zones, catalog and list bodies by digest, the new twin's client
// reads the old one's list bodies, and the old twin is never asked for a
// route it does not have except the two health reads the test makes. When
// the old twin stops, the new one keeps answering and reports the peer as
// unreachable.
func TestMixedVersionStaticPeerV01(t *testing.T) {
	kudurrutest.RequireE2E(t)
	previous := kudurrutest.Previous(t)
	probe := kudurrutest.PreviousProgram(t, "internal-probe", v01InternalProbe)
	key, env := sharedKey(t)
	body := []byte("blocked.mixed-static.example\n")
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(source.Close)
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	objects := append(zoneObjects(map[string]string{"ns1": "127.0.0.1", "seed": "192.0.2.42"}),
		map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "BlockList", "metadata": map[string]any{"name": "shared", "namespace": "default"},
			"spec": map[string]any{"sources": []any{map[string]any{"name": "remote", "url": source.URL, "format": "domains"}}}})
	dir := t.TempDir()
	oldSpec, oldCfg := kudurrutest.NewNodeSpec(t, dir, "twin-old")
	newSpec, newCfg := kudurrutest.NewNodeSpec(t, dir, "twin-new")
	for _, cfg := range []map[string]any{oldCfg, newCfg} {
		cfg["objects"] = objects
		cfg["blocking"] = map[string]any{"allowLocalListURLs": true}
		cfg["querylog"] = map[string]any{"enabled": false}
		cfg["auth"].(map[string]any)["session"] = map[string]any{"secure": "never"}
	}
	newCfg["cluster"] = map[string]any{"role": "static", "peers": []any{
		map[string]any{"name": oldSpec.Name, "address": oldSpec.Ports.Internal},
		map[string]any{"name": newSpec.Name, "address": newSpec.Ports.Internal},
	}}
	kudurrutest.WriteConfig(t, oldSpec.Config, oldCfg)
	kudurrutest.WriteConfig(t, newSpec.Config, newCfg)

	// 0.1.0 has no cluster.role: the old host is a single node, as it was
	// before the upgrade began.
	oldNode := kudurrutest.StartReady(t, previous, oldSpec, kudurrutest.Options{Env: env, Args: []string{"serve", "--config", oldSpec.Config}})
	if !strings.Contains(oldNode.Log(), "version="+kudurrutest.PreviousRelease) {
		t.Fatalf("the old twin is not %s:\n%s", kudurrutest.PreviousRelease, oldNode.Log())
	}
	newNode := kudurrutest.StartReady(t, kudurruBin, newSpec, kudurrutest.Options{Env: env})
	for _, n := range []*kudurrutest.Node{oldNode, newNode} {
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			udp, err := exchange(n.Ports.DNS, "seed.e2e.test.", dns.TypeA, false)
			tcp, err2 := exchange(n.Ports.DNS, "seed.e2e.test.", dns.TypeA, true)
			return err == nil && err2 == nil && firstA(udp) == "192.0.2.42" && firstA(tcp) == "192.0.2.42"
		}) {
			t.Fatalf("%s does not serve the shared files", n.Name)
		}
	}
	settle(t, newSpec)
	c := sessionClient(t, newSpec.Ports.API)

	t.Run("NodesReportTheOldTwin", func(t *testing.T) {
		var nodes api.List[api.NodeInfo]
		if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool {
			c.request(t, http.MethodGet, "/nodes", nil, http.StatusOK, &nodes)
			return len(nodes.Items) == 2 && nodes.Items[0].Ready != nil && nodes.Items[1].Ready != nil
		}) {
			t.Fatalf("/nodes: %+v", nodes.Items)
		}
		byName := map[string]api.NodeInfo{}
		for _, n := range nodes.Items {
			byName[n.Name] = n
		}
		old, self := byName[oldSpec.Name], byName[newSpec.Name]
		if old.Version != kudurrutest.PreviousRelease || old.Ready == nil || !*old.Ready || old.Error != "" || old.Self || old.Leader || old.State != "Live" {
			t.Errorf("/nodes reports the old twin as %+v", old)
		}
		if !self.Self || !self.Leader || self.Ready == nil || !*self.Ready || self.Version == kudurrutest.PreviousRelease {
			t.Errorf("/nodes reports the new twin as %+v", self)
		}
		var cluster api.ClusterInfo
		c.request(t, http.MethodGet, "/cluster", nil, http.StatusOK, &cluster)
		if !cluster.ReadOnly || cluster.Nodes != 2 || cluster.Leader != newSpec.Name {
			t.Errorf("/cluster: %+v", cluster)
		}
	})
	t.Run("OldTwinHasNoHealthRoute", func(t *testing.T) {
		before := internalRequests(t, oldSpec.Ports.Metrics)
		var problem struct {
			Error api.ErrorBody `json:"error"`
		}
		c.request(t, http.MethodGet, "/nodes/"+oldSpec.Name+"/health", nil, http.StatusServiceUnavailable, &problem)
		if problem.Error.Key != api.KeyUnavailable || problem.Error.Message != "peer health: unsupported" {
			t.Errorf("/nodes/%s/health: %+v", oldSpec.Name, problem.Error)
		}
		after := internalRequests(t, oldSpec.Ports.Metrics)
		if after["other"]-before["other"] != 1 {
			t.Errorf("the old twin got %d requests for routes it does not serve, want the one health read: %v then %v", after["other"]-before["other"], before, after)
		}
		var health api.NodeHealth
		c.request(t, http.MethodGet, "/nodes/"+newSpec.Name+"/health", nil, http.StatusOK, &health)
		if !health.Ready || health.Node != newSpec.Name {
			t.Errorf("/nodes/%s/health: %+v", newSpec.Name, health)
		}
	})
	t.Run("PreviousClientReadsTheNewTwin", func(t *testing.T) {
		for _, target := range []kudurrutest.NodeSpec{newSpec, oldSpec} {
			r := runV01Probe(t, probe, key, target.Ports.Internal, digest)
			if r.StatusError != "" || r.Status == nil || r.Status.Node != target.Name || r.Status.Ready == nil || !*r.Status.Ready || r.Status.Leader != target.Name {
				t.Errorf("%s status of %s: %+v %s", kudurrutest.PreviousRelease, target.Name, r.Status, r.StatusError)
			}
			if r.Status != nil {
				found := false
				for _, l := range r.Status.Lists {
					for _, d := range l.Digests {
						found = found || d == digest
					}
				}
				if !found {
					t.Errorf("%s status of %s lists no digest %s: %+v", kudurrutest.PreviousRelease, target.Name, digest, r.Status.Lists)
				}
			}
			if r.ZonesError != "" || r.Zones == nil || r.Zones.Node != target.Name || len(r.Zones.Zones) != 1 || r.Zones.Zones[0].Name != "e2e.test." || r.Zones.Zones[0].Serial == 0 {
				t.Errorf("%s zones of %s: %+v %s", kudurrutest.PreviousRelease, target.Name, r.Zones, r.ZonesError)
			}
			if r.ListError != "" || r.ListDigest != digest {
				t.Errorf("%s list body of %s: digest %q, %s", kudurrutest.PreviousRelease, target.Name, r.ListDigest, r.ListError)
			}
			if r.CatalogueError != "" || r.CatalogueBytes == 0 {
				t.Errorf("%s catalog of %s: %d bytes, %s", kudurrutest.PreviousRelease, target.Name, r.CatalogueBytes, r.CatalogueError)
			}
		}
	})
	t.Run("NewClientReadsTheOldTwin", func(t *testing.T) {
		creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: key, NodeName: newSpec.Name})
		if err != nil {
			t.Fatal(err)
		}
		client := internalapi.NewClient(creds, internalapi.ClientOptions{Timeout: 5 * time.Second})
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		status, err := client.Status(ctx, oldSpec.Ports.Internal)
		if err != nil || status.Node != oldSpec.Name || status.Version != kudurrutest.PreviousRelease {
			t.Errorf("status of the old twin: %+v %v", status, err)
		}
		reader, _, err := client.OpenList(ctx, oldSpec.Ports.Internal, digest)
		if err != nil {
			t.Fatalf("list body of the old twin: %v", err)
		}
		got, readErr := io.ReadAll(reader)
		if closeErr := reader.Close(); readErr == nil {
			readErr = closeErr
		}
		if readErr != nil || !bytes.Equal(got, body) {
			t.Errorf("list body of the old twin: %q %v", got, readErr)
		}
		if _, err := client.Health(ctx, oldSpec.Ports.Internal, oldSpec.Name); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Errorf("health of the old twin: %v, want unsupported", err)
		}
	})
	// Over the whole run (startup, /nodes polling, every subtest) the old
	// twin was asked for a route it does not serve exactly twice: the two
	// health reads above. In this configuration (no secondary zones, no
	// query log) the new twin sends it nothing else it does not have.
	if got := internalRequests(t, oldSpec.Ports.Metrics)["other"]; got != 2 {
		t.Errorf("the old twin got %d requests for routes it does not serve since it started, want the two health reads", got)
	}
	t.Run("OldTwinStops", func(t *testing.T) {
		oldNode.Stop(20 * time.Second)
		if code, err := oldNode.Wait(time.Second); err != nil || code != 0 {
			t.Fatalf("the old twin did not stop cleanly: %d %v", code, err)
		}
		var nodes api.List[api.NodeInfo]
		c.request(t, http.MethodGet, "/nodes", nil, http.StatusOK, &nodes)
		for _, n := range nodes.Items {
			if n.Name == oldSpec.Name && (n.Ready != nil || n.Error != "peer status: connection refused") {
				t.Errorf("/nodes reports the stopped old twin as %+v", n)
			}
		}
		for _, tcp := range []bool{false, true} {
			if r := ask(t, newSpec.Ports.DNS, "seed.e2e.test.", dns.TypeA, tcp); firstA(r) != "192.0.2.42" {
				t.Errorf("the new twin without its peer (tcp %v): %v", tcp, r)
			}
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if code, _, err := kudurrutest.Get(ctx, "http://"+newSpec.Ports.Ops+"/healthz/ready"); err != nil || code != http.StatusOK {
			t.Errorf("the new twin without its peer: ready %d %v", code, err)
		}
	})
}

// centralCount counts the central query rows of one node for one name.
func centralCount(t *testing.T, dsn, node, name string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var n int
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('query') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("central query table: %v", err)
	}
	if !exists {
		return 0 // the node that creates it has not migrated yet
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM query q JOIN dict_domain d ON d.id = q.domain JOIN dict_node n ON n.id = q.node
		WHERE n.name = $1 AND rtrim(d.name, '.') = $2`, node, strings.TrimSuffix(name, ".")).Scan(&n); err != nil {
		t.Fatalf("central query rows of %s %s: %v", node, name, err)
	}
	return n
}

// centralMeta reads kudurru_meta of the central database.
func centralMeta(t *testing.T, dsn string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	rows, err := conn.Query(ctx, `SELECT key, value FROM kudurru_meta WHERE key IN ('schema_version', 'compat_version')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMixedVersionCentralDatabaseV01 is the rolling upgrade of nodes that
// share a central PostgreSQL database (docs/deploy/postgres.md). A 0.1.0
// node ships its query log to the database (schema 3). The binary under
// test migrates the database to schema 6 with compat 6. Started again, the
// 0.1.0 node reports CentralIncompatible, keeps answering DNS and keeps its
// queries in its own query log; once its binary is replaced, the backlog is
// shipped once, what it shipped before is not shipped again, and a restart
// ships nothing twice.
func TestMixedVersionCentralDatabaseV01(t *testing.T) {
	kudurrutest.RequireE2E(t)
	dsn := centraltest.DSN(t)
	previous := kudurrutest.Previous(t)
	_, env := sharedKey(t)
	env = append(env, "KUDURRU_DATABASE__DSN="+dsn)
	dir := t.TempDir()
	oldSpec, oldCfg := kudurrutest.NewNodeSpec(t, dir, "central-old")
	newSpec, newCfg := kudurrutest.NewNodeSpec(t, dir, "central-new")
	for i, cfg := range []map[string]any{oldCfg, newCfg} {
		cfg["querylog"] = map[string]any{"enabled": true, "sink": "sqlite"}
		kudurrutest.WriteConfig(t, []kudurrutest.NodeSpec{oldSpec, newSpec}[i].Config, cfg)
	}
	const suffix = ".mixed-central.example."
	query := func(n *kudurrutest.Node, label string) string {
		t.Helper()
		name := label + "." + n.Name + suffix
		// The default group forwards to the discard port: SERVFAIL, logged.
		if r := ask(t, n.Ports.DNS, name, dns.TypeA, false); r.Rcode != dns.RcodeServerFailure {
			t.Fatalf("%s on %s: %s, want SERVFAIL from the discard port", name, n.Name, dns.RcodeToString[r.Rcode])
		}
		return name
	}
	shipped := func(node, name string, want int) {
		t.Helper()
		var got int
		if !kudurrutest.Eventually(60*time.Second, 250*time.Millisecond, func() bool {
			got = centralCount(t, dsn, node, name)
			return got >= want
		}) || got != want {
			t.Fatalf("central rows of %s for %s: %d, want %d", node, name, got, want)
		}
	}
	startOld := func() *kudurrutest.Node {
		t.Helper()
		return kudurrutest.StartReady(t, previous, oldSpec, kudurrutest.Options{Env: env, Args: []string{"serve", "--config", oldSpec.Config}})
	}
	stop := func(n *kudurrutest.Node) {
		t.Helper()
		n.Stop(20 * time.Second)
		if code, err := n.Wait(time.Second); err != nil || code != 0 {
			t.Fatalf("%s did not stop cleanly: %d %v\n%s", n.Name, code, err, n.Log())
		}
	}

	// 0.1.0 alone: the database is created at its schema, its query ships.
	old := startOld()
	before := query(old, "before")
	shipped(oldSpec.Name, before, 1)
	// 0.1.0 writes compat_version 1: every later schema up to 3 was usable.
	if got := centralMeta(t, dsn); got["schema_version"] != "3" || got["compat_version"] != "1" {
		t.Fatalf("the database 0.1.0 created: %v", got)
	}
	stop(old)

	// This release migrates it.
	upgraded := kudurrutest.StartReady(t, kudurruBin, newSpec, kudurrutest.Options{Env: env})
	if !kudurrutest.Eventually(60*time.Second, 250*time.Millisecond, func() bool {
		got := centralMeta(t, dsn)
		return got["schema_version"] == "6" && got["compat_version"] == "6"
	}) {
		t.Fatalf("this release did not migrate the database: %v", centralMeta(t, dsn))
	}
	shipped(newSpec.Name, query(upgraded, "new"), 1)

	// 0.1.0 again: incompatible, serving, keeping its queries.
	old = startOld()
	token := adminSocketToken(t, previous, oldSpec)
	oldAPI := upgradeAPI{addr: oldSpec.Ports.API, token: token}
	var messages []byte
	if !kudurrutest.Eventually(60*time.Second, 250*time.Millisecond, func() bool {
		messages = oldAPI.must(t, http.MethodGet, "/info/messages", nil, http.StatusOK)
		return bytes.Contains(messages, []byte(`"CentralIncompatible"`))
	}) {
		t.Fatalf("%s on the migrated database did not report CentralIncompatible: %s", kudurrutest.PreviousRelease, messages)
	}
	var during []string
	for i := range 3 {
		during = append(during, query(old, fmt.Sprintf("during-%d", i)))
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	code, _, err := kudurrutest.Get(ctx, "http://"+oldSpec.Ports.Ops+"/healthz/ready")
	cancel()
	if err != nil || code != http.StatusOK {
		t.Errorf("%s on the migrated database: ready %d %v", kudurrutest.PreviousRelease, code, err)
	}
	// Give a shipper that wrongly kept going time to ship.
	time.Sleep(3 * time.Second)
	for _, name := range during {
		if n := centralCount(t, dsn, oldSpec.Name, name); n != 0 {
			t.Errorf("%s shipped %s to the incompatible database: %d rows", kudurrutest.PreviousRelease, name, n)
		}
	}
	if got := centralMeta(t, dsn); got["schema_version"] != "6" || got["compat_version"] != "6" {
		t.Errorf("%s changed the migrated database: %v", kudurrutest.PreviousRelease, got)
	}
	stop(old)
	_, local := queryLogRows(t, oldSpec.DataDir)
	for _, name := range append([]string{before}, during...) {
		if !containsName(local, name) {
			t.Errorf("the query log of %s lacks %s: %v", kudurrutest.PreviousRelease, name, local)
		}
	}

	// The binary is replaced: the backlog ships once.
	for run := range 2 {
		n := kudurrutest.StartReady(t, kudurruBin, oldSpec, kudurrutest.Options{Env: env})
		after := query(n, fmt.Sprintf("after-%d", run))
		shipped(oldSpec.Name, after, 1)
		for _, name := range during {
			shipped(oldSpec.Name, name, 1)
		}
		if got := centralCount(t, dsn, oldSpec.Name, before); got != 1 {
			t.Errorf("run %d: %s, shipped by %s, has %d central rows, want 1", run, before, kudurrutest.PreviousRelease, got)
		}
		stop(n)
	}
	if got := schemaVersions(t, oldSpec.DataDir); !maps.Equal(got, currentSchemas()) {
		t.Errorf("schema versions of the replaced node: %v, want %v", got, currentSchemas())
	}
	stop(upgraded)
}

// schemaOf reads meta.schema_version of one database of a stopped node.
func schemaOf(t *testing.T, dataDir, file string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, file)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v any
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil {
		t.Fatalf("%s: schema_version: %v", file, err)
	}
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	return fmt.Sprint(v)
}

// TestV01BinaryRefusesEnrolledDirectory: schema 9 is the first that can
// enroll (D373), so a 0.1.0 binary can never be a member. Pointed at the
// data directory of an enrolled primary or of a joined follower, the
// released 0.1.0 binary exits non-zero naming the newer database, writes
// no file, and the member then starts again under this release in its
// role.
func TestV01BinaryRefusesEnrolledDirectory(t *testing.T) {
	kudurrutest.RequireE2E(t)
	previous := kudurrutest.Previous(t)
	bin, licensed := licensedNodeFixture(t)
	_, env := sharedKey(t)
	for _, kv := range licensed {
		if !strings.HasPrefix(kv, "KUDURRU_ADMIN_PASSWORD=") {
			env = append(env, kv) // the license key file; the password stays upgradePassword
		}
	}
	dir := t.TempDir()
	a, acfg := kudurrutest.NewNodeSpec(t, dir, "enrolled-a")
	b, bcfg := kudurrutest.NewNodeSpec(t, dir, "enrolled-b")
	for i, cfg := range []map[string]any{acfg, bcfg} {
		cfg["querylog"] = map[string]any{"enabled": false}
		cfg["auth"].(map[string]any)["session"] = map[string]any{"secure": "never"}
		kudurrutest.WriteConfig(t, []kudurrutest.NodeSpec{a, b}[i].Config, cfg)
	}
	stop := func(n *kudurrutest.Node) {
		t.Helper()
		n.Stop(20 * time.Second)
		if code, err := n.Wait(time.Second); err != nil || code != 0 {
			t.Fatalf("%s did not stop cleanly: %d %v\n%s", n.Name, code, err, n.Log())
		}
	}
	primary := kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	apiA := &staticProcessClient{client: &http.Client{Timeout: 5 * time.Second}, base: "http://" + a.Ports.API + api.BasePath}
	{
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		apiA.client.Jar = jar
	}
	var login api.SessionInfo
	apiA.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: upgradePassword}, http.StatusOK, &login)
	apiA.csrf = login.CSRF
	objects := zoneObjects(map[string]string{"seed": "192.0.2.42"})
	apiA.request(t, http.MethodPost, "/objects/dnszones", objects[0], http.StatusCreated, nil)
	apiA.request(t, http.MethodPost, "/objects/dnsrecordsets", objects[1], http.StatusCreated, nil)
	enrolledZoneSettled(t, apiA, a.Ports.DNS, "seed", "192.0.2.42")
	review := func() api.ClusterEnrollmentInfo {
		var result api.ClusterEnrollmentInfo
		enrolledProcessRead(t, apiA, "/cluster/enrollment", func(body []byte) bool { return json.Unmarshal(body, &result) == nil })
		return result
	}
	// Review only once the source has been quiet: an activation that the
	// node's own post-zone-write commits supersede is refused 503 with no
	// effect (enrolledQuietReview, CI-RED-3).
	original := enrolledQuietReview(t, apiA, 3*time.Second)
	var activated api.ClusterActivationReceipt
	apiA.request(t, http.MethodPost, "/cluster/activate", api.ActivateClusterCommand{ClusterEnrollmentReview: original.ClusterEnrollmentReview,
		Name: a.Name, Address: a.Ports.Internal, Reason: "mixed-version acceptance"}, http.StatusOK, &activated)
	if !activated.Committed {
		t.Fatal("activation not committed")
	}
	stop(primary)
	primary = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	enrolled := review()
	if enrolled.Mode != "primary" {
		t.Fatalf("the activated node is %q, want primary", enrolled.Mode)
	}
	var invitation api.ClusterJoinTokenReceipt
	apiA.request(t, http.MethodPost, "/cluster/join-token", api.ClusterJoinTokenCommand{ClusterEnrollmentReview: enrolled.ClusterEnrollmentReview,
		Name: b.Name, Address: b.Ports.Internal, ExpiresAt: time.Now().Add(2 * time.Minute), Reason: "mixed-version acceptance"}, http.StatusOK, &invitation)
	invitationPath := filepath.Join(dir, "invitation")
	if err := os.WriteFile(invitationPath, []byte(invitation.Invitation), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	// #nosec G204 -- the fixture's binary and private temporary paths; no shell
	join := exec.CommandContext(ctx, bin, "cluster", "join", "--offline", "--data-dir", b.DataDir,
		"--invitation-file", invitationPath, "--confirm-name", b.Name, "--confirm-address", b.Ports.Internal)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "KUDURRU_") {
			join.Env = append(join.Env, entry)
		}
	}
	join.Env = append(join.Env, env...)
	if _, err := join.Output(); err != nil {
		t.Fatal("offline join failed", err) // never log its private output
	}
	follower := kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	awaitSeed := func(n *kudurrutest.Node) {
		t.Helper()
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			r, err := exchange(n.Ports.DNS, "seed.e2e.test.", dns.TypeA, false)
			return err == nil && firstA(r) == "192.0.2.42"
		}) {
			t.Fatalf("%s does not serve the replicated zone", n.Name)
		}
	}
	awaitSeed(follower)
	stop(follower)
	stop(primary)

	// files lists every file of a stopped node's data directory, including
	// its subdirectories (the list cache), with its SHA-256, and every
	// subdirectory itself, so a created directory is a change too. SQLite
	// shared-memory files, sockets and an empty write-ahead log are left
	// out (databaseSums).
	files := func(spec kudurrutest.NodeSpec) map[string]string {
		t.Helper()
		out := map[string]string{}
		err := filepath.WalkDir(spec.DataDir, func(path string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(spec.DataDir, path)
			if err != nil {
				return err
			}
			if e.IsDir() {
				if rel != "." {
					out[rel+"/"] = "directory"
				}
				return nil
			}
			if strings.HasSuffix(e.Name(), "-shm") || strings.HasSuffix(e.Name(), ".sock") {
				return nil
			}
			raw, err := os.ReadFile(path) //nolint:gosec // the test's data directory
			if err != nil {
				return err
			}
			if strings.HasSuffix(e.Name(), "-wal") && len(raw) == 0 {
				return nil
			}
			s := sha256.Sum256(raw)
			out[rel] = hex.EncodeToString(s[:])
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, spec := range []kudurrutest.NodeSpec{a, b} {
		t.Run(spec.Name, func(t *testing.T) {
			if got := schemaOf(t, spec.DataDir, "state.db"); got != strconv.Itoa(9) {
				t.Fatalf("state.db of %s is schema %s", spec.Name, got)
			}
			before := files(spec)
			if _, ok := before["member-identity.json"]; !ok {
				t.Fatalf("%s holds no member identity: %v", spec.Name, before)
			}
			n := kudurrutest.Start(t, previous, spec, kudurrutest.Options{Env: env, Args: []string{"serve", "--config", spec.Config}})
			code, err := n.Wait(30 * time.Second)
			if err != nil {
				t.Fatalf("%s kept running on the enrolled directory: %v\n%s", kudurrutest.PreviousRelease, err, n.Log())
			}
			log := n.Log()
			if code == 0 || !strings.Contains(log, "was written by a newer kudurru") || !strings.Contains(log, "schema version 9") {
				t.Errorf("%s on the enrolled directory: exit %d\n%s", kudurrutest.PreviousRelease, code, log)
			}
			if after := files(spec); !maps.Equal(after, before) {
				t.Errorf("the refused start changed the directory: %v, were %v", after, before)
			}
		})
	}
	// Both members start again under this release, in their roles.
	_ = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	if got := review(); got.Mode != "primary" || got.MemberID != enrolled.MemberID {
		t.Errorf("after the refused downgrade the first member is %q %s", got.Mode, got.MemberID)
	}
	follower = kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	awaitSeed(follower)
	apiB := &staticProcessClient{client: apiA.client, base: "http://" + b.Ports.API + api.BasePath, csrf: login.CSRF}
	var cluster api.ClusterInfo
	enrolledProcessRead(t, apiB, "/cluster", func(body []byte) bool { return json.Unmarshal(body, &cluster) == nil && cluster.Role != "" })
	if cluster.Role != "follower" || cluster.Primary != a.Name {
		t.Errorf("after the refused downgrade the second member is %+v", cluster)
	}
}
