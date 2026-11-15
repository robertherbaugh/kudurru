package e2e

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// kubeProcess is one `kudurru serve` with the kube driver whose API server
// traffic passes through its own relay.
type kubeProcess struct {
	spec  kudurrutest.NodeSpec
	relay *enrolledRelay
	env   []string
	node  *kudurrutest.Node
}

// kubeHealth is what GET /healthz on the ops listener reports.
type kubeHealth struct {
	code        int
	leader      string
	snapshotAge string
	unready     []string
}

func readKubeHealth(ops string) (kubeHealth, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, body, err := kudurrutest.Get(ctx, "http://"+ops+"/healthz")
	if err != nil {
		return kubeHealth{}, err
	}
	var b struct {
		Leader      string   `json:"leader"`
		SnapshotAge string   `json:"snapshotAge"`
		Unready     []string `json:"unready"`
	}
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		return kubeHealth{}, fmt.Errorf("healthz %d: %w", code, err)
	}
	return kubeHealth{code: code, leader: b.Leader, snapshotAge: b.SnapshotAge, unready: b.Unready}, nil
}

// answersBoth reports whether the node answers name with ip, authoritatively,
// over UDP and TCP.
func answersBoth(addr, name, ip string) bool {
	for _, tcp := range []bool{false, true} {
		r, err := exchange(addr, name, dns.TypeA, tcp)
		if err != nil || r.Rcode != dns.RcodeSuccess || !r.Authoritative || firstA(r) != ip {
			return false
		}
	}
	return true
}

// servfailBoth reports whether the node answers name with SERVFAIL over UDP
// and TCP.
func servfailBoth(addr, name string) bool {
	for _, tcp := range []bool{false, true} {
		r, err := exchange(addr, name, dns.TypeA, tcp)
		if err != nil || r.Rcode != dns.RcodeServerFailure {
			return false
		}
	}
	return true
}

// snapshotHolds reads local.db read-only, as the running node may write it,
// and reports whether its saved DNSRecordSets mention ip and its saved
// cluster identity is clusterID.
func snapshotHolds(path, clusterID, ip string) bool {
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return false
	}
	defer func() { _ = db.Close() }()
	var saved string
	if err := db.QueryRow(`SELECT cluster_id FROM kube_snapshot WHERE id = 1`).Scan(&saved); err != nil || saved != clusterID {
		return false
	}
	rows, err := db.Query(`SELECT body FROM kube_snapshot_objects WHERE kind = 'DNSRecordSet'`)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var body string
		if rows.Scan(&body) == nil && strings.Contains(body, `"`+ip+`"`) {
			return true
		}
	}
	return false
}

// copyLocalDB copies local.db of a stopped node, with its WAL, into a new
// data directory and returns that directory.
func copyLocalDB(t *testing.T, from string) string {
	t.Helper()
	to := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(to, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(filepath.Join(from, localdb.FileName+suffix)) //nolint:gosec // the test's data directory
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, localdb.FileName+suffix), b, 0o600); err != nil { //nolint:gosec // G703: a copy into the test's own temporary directory
			t.Fatal(err)
		}
	}
	return to
}

// TestKubeProcessesColdStartLeaseLossAndRejoin is the process-level proof
// of the Kubernetes failure behavior (G22, F01, F64b, G11): two built
// binaries with the kube driver on one envtest API server, each reaching it
// through its own TCP relay, with store.kube.expectedClusterID pinned to
// the kube-system UID.
//
//  1. Both start live and answer the zone from the API server; one holds
//     the Lease kudurru-controller and both report it.
//  2. The holder is partitioned from the API server. The other acquires the
//     Lease, a change made meanwhile reaches only the connected node, and
//     the partitioned node keeps answering its last good data. After the
//     partition heals the old holder converges and does not reclaim the
//     Lease: one leader, the same data on both.
//  3. Both restart on their data directories, then on empty ones (the
//     chart's emptyDir): they resync without losing the change.
//  4. With the API server unreachable, a node restarted on its directory
//     answers from local.db over UDP and TCP, is ready, reports the
//     snapshot age and no leader, and its management API is unavailable.
//  5. The same snapshot is refused (SERVFAIL, not ready, the reason in
//     /healthz) under a foreign pin and when it carries no cluster
//     identity, and so is a first boot without local.db. Without a pin the
//     node exits before it answers (docs/deploy/runbooks.md, "Upgrade
//     compatibility").
//
// Two replicas write (Lease, status), so the binary is the licensed
// fixture's.
func TestKubeProcessesColdStartLeaseLossAndRejoin(t *testing.T) {
	kudurrutest.RequireE2E(t)
	restCfg := startEnvtest(t, "deploy/kustomize/crd/bases/*.yaml", "deploy/kustomize/crd/audit/*.yaml")
	bin, licensedEnv := licensedNodeFixture(t)
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, coordinationv1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var system corev1.Namespace
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, &system); err != nil {
		t.Fatal(err)
	}
	clusterID := string(system.UID)
	apiHost, err := url.Parse(restCfg.Host)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	// The zone, as GitOps writes it: straight to the API server.
	if err := admin.Create(ctx, &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-e2e"},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: "kube-e2e.test.", Type: v1alpha1.ZoneTypePrimary, Nameservers: []string{"ns1.kube-e2e.test."}},
	}); err != nil {
		t.Fatal(err)
	}
	records := &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "www"},
		Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "kube-e2e"},
			Endpoints: []v1alpha1.Endpoint{{DNSName: "www.kube-e2e.test", RecordType: "A", Targets: []string{"192.0.2.10"}, RecordTTL: 30}}},
	}
	if err := admin.Create(ctx, records); err != nil {
		t.Fatal(err)
	}
	const name = "www.kube-e2e.test."

	dir := t.TempDir()
	newProcess := func(nodeName string) *kubeProcess {
		spec, _ := kudurrutest.NewNodeSpec(t, dir, nodeName)
		relay := newEnrolledRelay(t, apiHost.Host)
		kubeconfig := writeKubeconfig(t, restCfg, "https://"+relay.listener.Addr().String())
		env := append(append([]string(nil), licensedEnv...), "KUBECONFIG="+kubeconfig, "KUDURRU_ENCRYPTION_KEY="+hex.EncodeToString(key))
		return &kubeProcess{spec: spec, relay: relay, env: env}
	}
	// configure writes p's configuration with the given pin ("" leaves it
	// unset) and data directory.
	configure := func(p *kubeProcess, pin, dataDir string) {
		t.Helper()
		p.spec.DataDir = dataDir
		cfg := kudurrutest.BaseConfig(p.spec.Name, dataDir, p.spec.Ports)
		kube := map[string]any{"startupTimeout": "5s"}
		if pin != "" {
			kube["expectedClusterID"] = pin
		}
		cfg["store"] = map[string]any{"driver": "kube", "kube": kube}
		kudurrutest.WriteConfig(t, p.spec.Config, cfg)
	}
	start := func(p *kubeProcess, ready bool) {
		t.Helper()
		if ready {
			p.node = kudurrutest.StartReady(t, bin, p.spec, kudurrutest.Options{Env: p.env})
		} else {
			p.node = kudurrutest.Start(t, bin, p.spec, kudurrutest.Options{Env: p.env})
		}
	}
	stop := func(ps ...*kubeProcess) {
		for _, p := range ps {
			if p.node != nil {
				p.node.Stop(30 * time.Second)
				p.node = nil
			}
		}
	}
	await := func(d time.Duration, what string, cond func() bool, logs ...*kubeProcess) {
		t.Helper()
		if !kudurrutest.Eventually(d, 100*time.Millisecond, cond) {
			for _, p := range logs {
				if p.node != nil {
					t.Logf("%s log:\n%s", p.spec.Name, p.node.Log())
				}
			}
			t.Fatalf("timed out after %s: %s", d, what)
		}
	}
	holder := func() string {
		var lease coordinationv1.Lease
		if err := admin.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kudurru-controller"}, &lease); err != nil || lease.Spec.HolderIdentity == nil {
			return ""
		}
		return *lease.Spec.HolderIdentity
	}
	leaderSeen := func(p *kubeProcess, want string) bool {
		h, err := readKubeHealth(p.spec.Ports.Ops)
		return err == nil && h.leader == want
	}

	a, b := newProcess("kube-a"), newProcess("kube-b")
	nodes := []*kubeProcess{a, b}
	for _, p := range nodes {
		configure(p, clusterID, p.spec.DataDir)
	}

	// 1. Live.
	for _, p := range nodes {
		start(p, true)
	}
	for _, p := range nodes {
		await(60*time.Second, p.spec.Name+" answers the zone", func() bool { return answersBoth(p.spec.Ports.DNS, name, "192.0.2.10") }, p)
	}
	await(60*time.Second, "one replica holds the controller Lease", func() bool { return holder() != "" }, a, b)
	first := holder()
	lead, other := a, b
	if first == b.spec.Name {
		lead, other = b, a
	} else if first != a.spec.Name {
		t.Fatalf("Lease holder %q is neither replica", first)
	}
	for _, p := range nodes {
		await(30*time.Second, p.spec.Name+" reports leader "+first, func() bool { return leaderSeen(p, first) }, p)
		await(60*time.Second, p.spec.Name+" saved the zone in local.db", func() bool {
			return snapshotHolds(filepath.Join(p.spec.DataDir, localdb.FileName), clusterID, "192.0.2.10")
		}, p)
	}
	t.Logf("live: %s holds the Lease", first)

	// 2. Partition the holder.
	partitioned := time.Now()
	lead.relay.partition(true)
	await(90*time.Second, other.spec.Name+" acquires the Lease from the partitioned "+lead.spec.Name, func() bool { return holder() == other.spec.Name }, lead, other)
	t.Logf("%s acquired the Lease %s after the partition", other.spec.Name, time.Since(partitioned).Round(time.Millisecond))
	if err := admin.Get(ctx, client.ObjectKeyFromObject(records), records); err != nil {
		t.Fatal(err)
	}
	records.Spec.Endpoints[0].Targets = []string{"192.0.2.11"}
	if err := admin.Update(ctx, records); err != nil {
		t.Fatal(err)
	}
	await(60*time.Second, other.spec.Name+" serves the change", func() bool { return answersBoth(other.spec.Ports.DNS, name, "192.0.2.11") }, other)
	if !answersBoth(lead.spec.Ports.DNS, name, "192.0.2.10") {
		t.Fatalf("the partitioned %s stopped answering its last good data", lead.spec.Name)
	}
	await(30*time.Second, other.spec.Name+" reports itself as leader", func() bool { return leaderSeen(other, other.spec.Name) }, other)
	// Fencing: while cut off, the former holder has stopped leading (its
	// renew deadline passed), so the two never both report themselves as
	// leader. Only its API server relay is cut; its ops port answers.
	await(30*time.Second, "the partitioned "+lead.spec.Name+" stops reporting itself as leader", func() bool {
		h, err := readKubeHealth(lead.spec.Ports.Ops)
		return err == nil && h.leader != lead.spec.Name
	}, lead)
	for range 3 {
		// A failed health read is a failure, not "not leader": it must
		// not let a split brain pass.
		h, err := readKubeHealth(lead.spec.Ports.Ops)
		if err != nil {
			t.Fatalf("the partitioned %s does not answer its health check: %v", lead.spec.Name, err)
		}
		if h.leader == lead.spec.Name {
			t.Fatalf("the partitioned %s reports itself as leader while %s holds the Lease", lead.spec.Name, other.spec.Name)
		}
		if !leaderSeen(other, other.spec.Name) {
			t.Fatalf("%s stopped reporting itself as leader during the partition", other.spec.Name)
		}
		time.Sleep(time.Second)
	}

	healed := time.Now()
	lead.relay.partition(false)
	await(90*time.Second, lead.spec.Name+" converges after the partition heals", func() bool { return answersBoth(lead.spec.Ports.DNS, name, "192.0.2.11") }, lead)
	t.Logf("%s converged %s after the partition healed", lead.spec.Name, time.Since(healed).Round(time.Millisecond))
	for _, p := range nodes {
		await(60*time.Second, p.spec.Name+" reports one leader, "+other.spec.Name, func() bool { return leaderSeen(p, other.spec.Name) }, p)
	}
	// Longer than a Lease duration: the healed former holder never reclaims.
	time.Sleep(20 * time.Second)
	if got := holder(); got != other.spec.Name {
		t.Fatalf("the healed %s took the Lease back: holder %q", lead.spec.Name, got)
	}
	for _, p := range nodes {
		if !answersBoth(p.spec.Ports.DNS, name, "192.0.2.11") || !leaderSeen(p, other.spec.Name) {
			t.Fatalf("%s diverged after the heal", p.spec.Name)
		}
	}

	// 3. Restart on the data directories, then on empty ones.
	for _, empty := range []bool{false, true} {
		stop(nodes...)
		for _, p := range nodes {
			if empty {
				if err := os.RemoveAll(p.spec.DataDir); err != nil {
					t.Fatal(err)
				}
			}
			start(p, true)
		}
		for _, p := range nodes {
			await(60*time.Second, fmt.Sprintf("%s answers the change after a restart (empty directory %t)", p.spec.Name, empty), func() bool {
				return answersBoth(p.spec.Ports.DNS, name, "192.0.2.11")
			}, p)
			await(60*time.Second, p.spec.Name+" saved the change in local.db", func() bool {
				return snapshotHolds(filepath.Join(p.spec.DataDir, localdb.FileName), clusterID, "192.0.2.11")
			}, p)
		}
		await(60*time.Second, "one leader after the restart", func() bool {
			h := holder()
			return h != "" && leaderSeen(a, h) && leaderSeen(b, h)
		}, a, b)
	}
	stop(nodes...)

	// 4. The API server is unreachable: a node restarts on its directory.
	for _, p := range nodes {
		p.relay.partition(true)
	}
	saved := a.spec.DataDir
	start(a, false)
	cold := time.Now()
	await(60*time.Second, "the cut-off kube-a answers from local.db", func() bool { return answersBoth(a.spec.Ports.DNS, name, "192.0.2.11") }, a)
	var health kubeHealth
	await(30*time.Second, "kube-a is ready from local.db and reports its age", func() bool {
		health, err = readKubeHealth(a.spec.Ports.Ops)
		return err == nil && health.code == http.StatusOK && health.snapshotAge != ""
	}, a)
	if health.leader != "" {
		t.Fatalf("kube-a reports leader %q without API server contact", health.leader)
	}
	age, err := time.ParseDuration(health.snapshotAge)
	if err != nil || age <= 0 || age > time.Since(cold)+5*time.Minute {
		t.Fatalf("snapshotAge %q", health.snapshotAge)
	}
	await(30*time.Second, "kube-a's management API is unavailable", func() bool {
		c := &http.Client{Timeout: 10 * time.Second}
		resp, err := c.Get("http://" + a.spec.Ports.API + "/api/v1/zones")
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusServiceUnavailable
	}, a)
	t.Logf("cut off: kube-a answers from local.db %s after its start, snapshot age %s", time.Since(cold).Round(time.Millisecond), health.snapshotAge)
	stop(a)

	// 5. Snapshots this node must not serve, with the API server unreachable.
	identityFree := copyLocalDB(t, saved)
	{
		db, err := sql.Open("sqlite", "file:"+filepath.Join(identityFree, localdb.FileName))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE kube_snapshot SET cluster_id = '' WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Without a pin the node does not start at all: the shared settings
	// have no admitted offline cache (settingsBackend), so it exits before
	// it answers, and never serves the snapshot.
	configure(a, "", copyLocalDB(t, saved))
	start(a, false)
	exited := false
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline) && !exited; {
		if r, err := exchange(a.spec.Ports.DNS, name, dns.TypeA, false); err == nil && r.Rcode == dns.RcodeSuccess {
			t.Fatalf("an unpinned node served the saved snapshot during the outage: %v", r)
		}
		select {
		case <-a.node.Done():
			exited = true
		case <-time.After(200 * time.Millisecond):
		}
	}
	if code, err := a.node.Wait(time.Second); err != nil || code == 0 {
		t.Fatalf("an unpinned node during the outage: exit %d, %v\n%s", code, err, a.node.Log())
	}
	t.Logf("cut off, no pin: exited before serving: %s", lastLine(a.node.Log()))
	a.node = nil

	for _, tc := range []struct {
		name, pin, dataDir, reason string
	}{
		{"foreign pin", "other-cluster", copyLocalDB(t, saved), "local.db snapshot rejected:"},
		{"snapshot without identity", clusterID, identityFree, "local.db snapshot rejected:"},
		{"first boot", clusterID, filepath.Join(t.TempDir(), "data"), "local.db holds no snapshot yet"},
	} {
		configure(a, tc.pin, tc.dataDir)
		start(a, false)
		await(60*time.Second, tc.name+": SERVFAIL, not ready, and the reason", func() bool {
			if !servfailBoth(a.spec.Ports.DNS, name) {
				return false
			}
			h, err := readKubeHealth(a.spec.Ports.Ops)
			if err != nil || h.code != http.StatusServiceUnavailable || h.snapshotAge != "" {
				return false
			}
			for _, r := range h.unready {
				if strings.Contains(r, tc.reason) {
					return true
				}
			}
			return false
		}, a)
		stop(a)
		t.Logf("cut off, %s: refused", tc.name)
	}
}

// lastLine is the last non-empty line of a log.
func lastLine(log string) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	return lines[len(lines)-1]
}
