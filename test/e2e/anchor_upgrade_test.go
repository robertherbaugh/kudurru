package e2e

import (
	"crypto"
	"database/sql"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// upgradeAnchorRoot is a root name server for one signing key on loopback TCP: it
// answers ". DNSKEY" with the key and its signature and counts the
// questions. seed is the DS record of the key, the managed-mode seed.
type upgradeAnchorRoot struct {
	addr  string
	seed  string
	reads atomic.Int32
}

func startUpgradeAnchorRoot(t *testing.T) *upgradeAnchorRoot {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 172800}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ED25519}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 172800}, Algorithm: key.Algorithm, KeyTag: key.KeyTag(), SignerName: ".",
		Inception: uint32(time.Now().Add(-time.Hour).Unix()), Expiration: uint32(time.Now().Add(24 * time.Hour).Unix())} //nolint:gosec // G115: RRSIG times are serial arithmetic
	if err := sig.Sign(private.(crypto.Signer), []dns.RR{key}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &upgradeAnchorRoot{addr: ln.Addr().String(), seed: key.ToDS(dns.SHA256).String()}
	srv := &dns.Server{Listener: ln, Net: "tcp", Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		m.Authoritative = true
		if len(q.Question) == 1 && q.Question[0].Name == "." && q.Question[0].Qtype == dns.TypeDNSKEY {
			r.reads.Add(1)
			m.Answer = []dns.RR{key, sig}
		} else {
			m.Rcode = dns.RcodeRefused
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return r
}

// anchorRows reads the root anchor owner and the local carrier of a
// stopped node: the phase of the accepted root row (awaiting-owner, dormant
// or managed) and of the local retirement record.
func anchorRows(t *testing.T, dataDir string) (owner, local string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "state.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.QueryRow(`SELECT json_extract(data, '$.phase') FROM replication_sections WHERE section = ? AND key = ?`, accepted.Section, accepted.RootKey).Scan(&owner); err != nil {
		t.Fatalf("state.db: anchor owner: %v", err)
	}
	if err := db.QueryRow(`SELECT json_extract(value, '$.phase') FROM meta WHERE key = 'trustanchors-local/v1/state'`).Scan(&local); err != nil {
		t.Fatalf("state.db: anchor carrier: %v", err)
	}
	return owner, local
}

// stateSchemas are the schema versions of state.db and local.db of a
// stopped node without a query log.
func stateSchemas(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	return map[string]string{"state.db": schemaOf(t, dataDir, "state.db"), "local.db": schemaOf(t, dataDir, "local.db")}
}

// anchorStatus is the output of `kudurru anchors status --offline`.
type anchorStatus struct {
	Version struct {
		Generation string `json:"generation"`
		Digest     string `json:"digest"`
	} `json:"version"`
	Quarantined bool   `json:"quarantined"`
	Recoveries  int    `json:"recoveries"`
	Outcome     string `json:"outcome"`
}

func offlineAnchorStatus(t *testing.T, dataDir string) anchorStatus {
	t.Helper()
	out, err := exec.Command(kudurruBin, "anchors", "status", "--offline", "--database", filepath.Join(dataDir, "state.db")).CombinedOutput() //nolint:gosec // the test's binary
	if err != nil {
		t.Fatalf("anchors status --offline: %v\n%s", err, out)
	}
	var s anchorStatus
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("anchors status --offline: %v: %s", err, out)
	}
	return s
}

// TestAnchorUpgradeFromReleasedV010 (F28-MV-2): a data directory written by
// the released 0.1.0 binary, which has no managed root anchors, is upgraded
// by the binary under test with anchors left as they were. The first start
// leaves a dormant anchor owner and a dormant local carrier; DNS is served
// as before; `kudurru anchors status --offline` reads the directory. A
// start with managedTopology edges whose one declared edge is a 0.1.0
// internal listener, which has no observation route, is refused and changes
// nothing (that the refusal comes before any root question is proven
// in-process by TestAnchorEdgeActivationRefusesUnsupportedPeer). With
// managedTopology direct and the managed anchor settings written through
// the settings API, the node
// bootstraps managed history from the root once, a restart keeps it without
// asking again, and the 0.1.0 binary refuses the directory without
// changing a byte.
func TestAnchorUpgradeFromReleasedV010(t *testing.T) {
	kudurrutest.RequireE2E(t)
	previous := kudurrutest.Previous(t)
	_, env := sharedKey(t)
	root := startUpgradeAnchorRoot(t)
	dir := t.TempDir()
	spec, cfg := kudurrutest.NewNodeSpec(t, dir, "anchors")
	cfg["objects"] = zoneObjects(map[string]string{"ns1": "127.0.0.1", "host": "192.0.2.1"})
	cfg["querylog"] = map[string]any{"enabled": false}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	v01 := kudurrutest.Options{Env: env, Args: []string{"serve", "--config", spec.Config}}
	serves := func(t *testing.T, n *kudurrutest.Node) {
		t.Helper()
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			for _, tcp := range []bool{false, true} {
				r, err := exchange(n.Ports.DNS, "host.e2e.test.", dns.TypeA, tcp)
				if err != nil || firstA(r) != "192.0.2.1" || !r.Authoritative {
					return false
				}
			}
			return true
		}) {
			t.Fatalf("%s does not serve host.e2e.test.:\n%s", n.Name, n.Log())
		}
	}
	stop := func(t *testing.T, n *kudurrutest.Node) {
		t.Helper()
		n.Stop(20 * time.Second)
		if code, err := n.Wait(time.Second); err != nil || code != 0 {
			t.Fatalf("%s did not stop cleanly: %d %v\n%s", n.Name, code, err, n.Log())
		}
	}

	old := kudurrutest.StartReady(t, previous, spec, v01)
	serves(t, old)
	stop(t, old)
	if got := stateSchemas(t, spec.DataDir); !maps.Equal(got, map[string]string{"state.db": "1", "local.db": "1"}) {
		t.Fatalf("the directory does not have the schemas of %s: %v", kudurrutest.PreviousRelease, got)
	}

	var dormant anchorStatus
	t.Run("StaticServingUnchanged", func(t *testing.T) {
		n := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: env})
		serves(t, n)
		stop(t, n)
		if got, want := stateSchemas(t, spec.DataDir), map[string]string{"state.db": "9", "local.db": "9"}; !maps.Equal(got, want) {
			t.Fatalf("schema versions after the upgrade: %v, want %v", got, want)
		}
		owner, local := anchorRows(t, spec.DataDir)
		t.Logf("after the upgrade: owner %q, local carrier %q", owner, local)
		if owner != string(accepted.Dormant) || local != "dormant" {
			t.Fatalf("after the upgrade the anchor owner is %q and the local carrier %q", owner, local)
		}
		dormant = offlineAnchorStatus(t, spec.DataDir)
		t.Logf("anchors status --offline: %+v", dormant)
		if dormant.Outcome != "read-only" || dormant.Quarantined || dormant.Recoveries != 0 {
			t.Fatalf("anchors status --offline on the upgraded directory: %+v", dormant)
		}
		if root.reads.Load() != 0 {
			t.Fatalf("static serving asked the root %d times", root.reads.Load())
		}
	})

	// The topology is a host declaration in the node file. The anchor mode,
	// seeds and root hints are cluster settings: after the first start of
	// this release they live in the Kudurru/default settings object, which
	// later edits of the node file do not change
	// (docs/architecture/configuration-and-store.md), so the test enables
	// them with a settings write, as an operator does.
	topology := func(topology string, edges []any) {
		t.Helper()
		cfg["upstreams"].(map[string]any)["recursion"] = map[string]any{"managedTopology": topology, "managedEdges": edges}
		kudurrutest.WriteConfig(t, spec.Config, cfg)
	}
	original := maps.Clone(cfg)
	original["upstreams"] = maps.Clone(cfg["upstreams"].(map[string]any))
	t.Run("EdgeWithoutObservationRouteRefused", func(t *testing.T) {
		peerSpec, peerCfg := kudurrutest.NewNodeSpec(t, dir, "v01-edge")
		peerCfg["querylog"] = map[string]any{"enabled": false}
		kudurrutest.WriteConfig(t, peerSpec.Config, peerCfg)
		peer := kudurrutest.StartReady(t, previous, peerSpec, kudurrutest.Options{Env: env, Args: []string{"serve", "--config", peerSpec.Config}})
		topology("edges", []any{map[string]any{
			"node": peerSpec.Name, "address": peerSpec.Ports.Internal, "deployment": strings.Repeat("ab", 32),
			"backendAddress": spec.Ports.DNS, "backendInternalAddress": spec.Ports.Internal,
		}})
		before := internalRequests(t, peerSpec.Ports.Metrics)
		n := kudurrutest.Start(t, kudurruBin, spec, kudurrutest.Options{Env: env})
		code, err := n.Wait(60 * time.Second)
		if err != nil {
			t.Fatalf("the node started behind an edge without the observation route: %v\n%s", err, n.Log())
		}
		if code == 0 || !strings.Contains(n.Log(), "managed edge observation is unsupported") {
			t.Errorf("start behind a %s edge: exit %d\n%s", kudurrutest.PreviousRelease, code, n.Log())
		}
		if got := internalRequests(t, peerSpec.Ports.Metrics); got["other"]-before["other"] != 1 {
			t.Errorf("the %s edge got %d requests for routes it does not serve, want the one observation: %v then %v", kudurrutest.PreviousRelease, got["other"]-before["other"], before, got)
		}
		// The stored settings still have anchorMode static and no hint to
		// this root here, so this check cannot tell a refusal before the
		// root question from an admitted start. That proof is in-process:
		// cmd/kudurru TestAnchorEdgeActivationRefusesUnsupportedPeer runs in
		// managed mode with seeds and hints and asserts no root read.
		if root.reads.Load() != 0 {
			t.Errorf("the refused start asked the root %d times", root.reads.Load())
		}
		if got := offlineAnchorStatus(t, spec.DataDir); got != dormant {
			t.Errorf("the refused start changed the anchor owner: %+v, was %+v", got, dormant)
		}
		stop(t, peer)
	})
	var bootstrapped anchorStatus
	t.Run("DirectBootstrapsOnce", func(t *testing.T) {
		topology("direct", []any{})
		for run := range 2 {
			n := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: env})
			serves(t, n)
			settle(t, spec)
			if run == 0 {
				if root.reads.Load() != 0 {
					t.Fatalf("the direct topology alone asked the root %d times", root.reads.Load())
				}
				c := sessionClient(t, spec.Ports.API)
				c.request(t, http.MethodPatch, "/settings", map[string]any{
					"upstreams.recursion.anchorMode": "managed", "upstreams.recursion.dnssec": true,
					"upstreams.recursion.trustAnchors": []any{root.seed}, "upstreams.recursion.rootHints": []any{root.addr},
					"upstreams.recursion.ipv6": "off",
				}, http.StatusOK, nil)
				// The elected updater wakes on the settings commit.
				kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool { return root.reads.Load() >= 1 })
				settle(t, spec)
			}
			serves(t, n)
			stop(t, n)
			if got := root.reads.Load(); got != 1 {
				t.Fatalf("run %d: the root was asked %d times, want once", run, got)
			}
			owner, local := anchorRows(t, spec.DataDir)
			status := offlineAnchorStatus(t, spec.DataDir)
			t.Logf("run %d: owner %q, local carrier %q, status %+v", run, owner, local, status)
			if owner != string(accepted.Managed) || status.Outcome != "read-only" || status.Quarantined || status.Version.Digest == dormant.Version.Digest {
				t.Fatalf("run %d: owner %q, status %+v", run, owner, status)
			}
			if run == 0 {
				bootstrapped = status
			} else if status != bootstrapped {
				t.Fatalf("the restart changed the managed owner: %+v, was %+v", status, bootstrapped)
			}
		}
	})
	t.Run("PreviousReleaseRefuses", func(t *testing.T) {
		// The node file 0.1.0 ran with: it refuses keys it does not know.
		kudurrutest.WriteConfig(t, spec.Config, original)
		var databases []string
		for _, name := range []string{"state.db", "local.db"} {
			databases = append(databases, filepath.Join(spec.DataDir, name))
		}
		before := databaseSums(t, databases...)
		n := kudurrutest.Start(t, previous, spec, v01)
		code, err := n.Wait(30 * time.Second)
		if err != nil {
			t.Fatalf("%s kept running on the managed directory: %v\n%s", kudurrutest.PreviousRelease, err, n.Log())
		}
		if code == 0 || !strings.Contains(n.Log(), "was written by a newer kudurru") {
			t.Errorf("%s on the managed directory: exit %d\n%s", kudurrutest.PreviousRelease, code, n.Log())
		}
		if got := databaseSums(t, databases...); !maps.Equal(got, before) {
			t.Errorf("the refused start changed the databases: %v, were %v", got, before)
		}
		if got := offlineAnchorStatus(t, spec.DataDir); got != bootstrapped {
			t.Errorf("after the refused downgrade: %+v, was %+v", got, bootstrapped)
		}
	})
}

// TestAnchorUpgradeStaticTwinV010 (F28-MV-2, static twins): a static twin
// (`cluster.role: static`, docs/deploy/compose-ha.md) refuses settings
// writes, so the settings-write route of TestAnchorUpgradeFromReleasedV010
// does not exist for it. Its first start of this release copies the cluster
// keys of kudurru.yaml into the Kudurru/default settings object, so the
// managed anchor keys work when they are in the node file before that first
// start: the twin bootstraps from the root once and keeps the owner across
// a restart. A later edit of a cluster key in the file is not applied: the
// stored settings keep the value of the first start. This pins the release
// gap that docs/deploy/runbooks.md "Upgrading from 0.1.x" describes; when
// static twins learn to take cluster keys from the file again, the last
// subtest and the runbook change together.
func TestAnchorUpgradeStaticTwinV010(t *testing.T) {
	kudurrutest.RequireE2E(t)
	previous := kudurrutest.Previous(t)
	_, env := sharedKey(t)
	root := startUpgradeAnchorRoot(t)
	dir := t.TempDir()
	spec, cfg := kudurrutest.NewNodeSpec(t, dir, "static-anchors")
	cfg["objects"] = zoneObjects(map[string]string{"ns1": "127.0.0.1", "host": "192.0.2.1"})
	cfg["querylog"] = map[string]any{"enabled": false}
	cfg["auth"].(map[string]any)["session"] = map[string]any{"secure": "never"}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	stop := func(t *testing.T, n *kudurrutest.Node) {
		t.Helper()
		n.Stop(20 * time.Second)
		if code, err := n.Wait(time.Second); err != nil || code != 0 {
			t.Fatalf("%s did not stop cleanly: %d %v\n%s", n.Name, code, err, n.Log())
		}
	}
	serves := func(t *testing.T, n *kudurrutest.Node) {
		t.Helper()
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			r, err := exchange(n.Ports.DNS, "host.e2e.test.", dns.TypeA, false)
			return err == nil && firstA(r) == "192.0.2.1" && r.Authoritative
		}) {
			t.Fatalf("%s does not serve host.e2e.test.:\n%s", n.Name, n.Log())
		}
	}
	// 0.1.0 has no cluster.role and no managed anchor keys: the host runs it
	// with the file it had before the upgrade.
	old := kudurrutest.StartReady(t, previous, spec, kudurrutest.Options{Env: env, Args: []string{"serve", "--config", spec.Config}})
	serves(t, old)
	stop(t, old)

	// The file of the upgrade: static role, the managed topology and the
	// managed anchor settings, all present before the first start of this
	// release.
	cfg["cluster"] = map[string]any{"role": "static", "peers": []any{map[string]any{"name": spec.Name, "address": spec.Ports.Internal}}}
	cfg["upstreams"].(map[string]any)["recursion"] = map[string]any{
		"managedTopology": "direct", "managedEdges": []any{},
		"anchorMode": "managed", "dnssec": true, "trustAnchors": []any{root.seed}, "rootHints": []any{root.addr}, "ipv6": "off",
	}
	cfg["blocking"] = map[string]any{"refreshInterval": "12h"}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	refreshInterval := func(t *testing.T, c *staticProcessClient) any {
		t.Helper()
		var body api.SettingsBody
		c.request(t, http.MethodGet, "/settings", nil, http.StatusOK, &body)
		blocking, _ := body.Settings["blocking"].(map[string]any)
		return blocking["refreshInterval"]
	}

	var bootstrapped anchorStatus
	var seeded any
	t.Run("FileBeforeFirstStartBootstrapsOnce", func(t *testing.T) {
		for run := range 2 {
			n := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: env})
			serves(t, n)
			if run == 0 {
				if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool { return root.reads.Load() >= 1 }) {
					t.Fatalf("the static twin never asked the root:\n%s", n.Log())
				}
				c := sessionClient(t, spec.Ports.API)
				var problem struct {
					Error api.ErrorBody `json:"error"`
				}
				c.request(t, http.MethodPatch, "/settings", map[string]any{"blocking.refreshInterval": "6h"}, http.StatusServiceUnavailable, &problem)
				if problem.Error.Key != api.KeyReadOnly {
					t.Errorf("settings write on a static twin: %+v, want %s", problem.Error, api.KeyReadOnly)
				}
				seeded = refreshInterval(t, c)
				t.Logf("blocking.refreshInterval after the first start: %v", seeded)
				// The file's 12h seeded the settings: the later check below
				// compares against it, so it must not pass on two absent keys.
				if seeded != "12h0m0s" {
					t.Fatalf("blocking.refreshInterval after the first start is %v, want the file's 12h0m0s", seeded)
				}
			}
			settle(t, spec)
			stop(t, n)
			if got := root.reads.Load(); got != 1 {
				t.Fatalf("run %d: the root was asked %d times, want once", run, got)
			}
			owner, _ := anchorRows(t, spec.DataDir)
			status := offlineAnchorStatus(t, spec.DataDir)
			t.Logf("run %d: owner %q, status %+v", run, owner, status)
			if owner != string(accepted.Managed) || status.Outcome != "read-only" || status.Quarantined {
				t.Fatalf("run %d: owner %q, status %+v", run, owner, status)
			}
			if run == 0 {
				bootstrapped = status
			} else if status != bootstrapped {
				t.Fatalf("the restart changed the managed owner: %+v, was %+v", status, bootstrapped)
			}
		}
	})
	t.Run("LaterFileEditNotApplied", func(t *testing.T) {
		cfg["blocking"] = map[string]any{"refreshInterval": "3h"}
		kudurrutest.WriteConfig(t, spec.Config, cfg)
		n := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{Env: env})
		serves(t, n)
		settle(t, spec)
		if seeded == nil {
			t.Fatal("the first start recorded no blocking.refreshInterval to compare with")
		}
		if got := refreshInterval(t, sessionClient(t, spec.Ports.API)); got != seeded {
			t.Errorf("a file edit after the first start changed blocking.refreshInterval to %v (was %v): update the runbook's static twin paragraph", got, seeded)
		}
		// The start says the file's value is not in effect, not that the
		// edit was applied (D373).
		if log := n.Log(); !strings.Contains(log, "cluster settings in kudurru.yaml are not in effect") || !strings.Contains(log, "blocking.refreshInterval") {
			t.Errorf("the start did not report that the file's blocking.refreshInterval is not in effect:\n%s", log)
		}
		stop(t, n)
	})
}
