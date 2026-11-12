package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// F28-HA-3 (D369): the separate-process enrolled cluster with managed root
// anchors and direct topology. The same public activation, invitation,
// offline join, partition, promotion, rejoin and restart flow as the static
// tests, with a signed stand-in root, and these anchor assertions on top:
// one bootstrap and no further writer acquisition on restart, join or
// promotion; one deny-only acquisition per supervised start; the accepted
// owner digest identical on every member and never re-bootstrapped; the
// operator status (GET /cluster/anchors) of each role; validated recursion
// on every serving role. The partition variant ends with a signed REVOKE
// that only the follower's deny-only check observes.
func TestEnrolledManagedAnchorsPartitionRestartAndRevoke(t *testing.T) {
	kudurrutest.RequireE2E(t)
	testEnrolledProcessesWith(t, false, newEnrolledAnchors(t, false))
}

func TestEnrolledManagedAnchorsPromoteAndRejoin(t *testing.T) {
	kudurrutest.RequireE2E(t)
	testEnrolledProcessesWith(t, true, newEnrolledAnchors(t, false))
}

// F28-HA-4 (D369): offline recovery inside an enrolled cluster. It extends
// the partition variant: the follower has retired its output on a signed
// REVOKE of the sole key; the primary's updater observes the same REVOKE at
// its scheduled refresh, at least the one-hour RFC 5011 minimum after the
// bootstrap (production timers, D297), and both nodes lose every trusted key.
// The test therefore runs for more than an hour and only with
// KUDURRU_ANCHOR_SOAK=1.
func TestEnrolledManagedAnchorsOfflineRecoverySoak(t *testing.T) {
	kudurrutest.RequireE2E(t)
	if os.Getenv("KUDURRU_ANCHOR_SOAK") != "1" {
		t.Skip("set KUDURRU_ANCHOR_SOAK=1: waits for the primary's one-hour RFC 5011 refresh")
	}
	testEnrolledProcessesWith(t, false, newEnrolledAnchors(t, true))
}

type enrolledAnchors struct {
	t      *testing.T
	root   *anchorRoot
	soak   bool
	digest string // the bootstrapped owner every member must keep
	base   int32  // acquisitions the standalone bootstrap took
}

func newEnrolledAnchors(t *testing.T, soak bool) *enrolledAnchors {
	return &enrolledAnchors{t: t, root: newAnchorRoot(t), soak: soak}
}

// configure adds managed anchors to a node's configuration.
func (e *enrolledAnchors) configure(cfg map[string]any) {
	if e != nil {
		anchorNodeConfig(cfg, e.root)
	}
}

// status reads GET /cluster/anchors until ready accepts it.
func (e *enrolledAnchors) status(c *staticProcessClient, ready func(api.RootAnchorStatus) bool) api.RootAnchorStatus {
	e.t.Helper()
	var status api.RootAnchorStatus
	enrolledProcessRead(e.t, c, "/cluster/anchors", func(body []byte) bool {
		status = api.RootAnchorStatus{}
		return json.Unmarshal(body, &status) == nil && ready(status)
	})
	return status
}

// awaitTCP waits until exactly want acquisitions answered in mode (in normal
// mode counted after the bootstrap) and requires the count to stay there for
// settle.
func (e *enrolledAnchors) awaitTCP(mode int32, want int32, settle time.Duration, why string) {
	e.t.Helper()
	if mode == anchorRootNormal {
		want += e.base
	}
	if !kudurrutest.Eventually(20*time.Second, 20*time.Millisecond, func() bool { return e.root.TCP(mode) >= want }) || e.root.TCP(mode) != want {
		e.t.Fatalf("%s: %d root acquisitions, want %d", why, e.root.TCP(mode), want)
	}
	time.Sleep(settle)
	if got := e.root.TCP(mode); got != want {
		e.t.Fatalf("%s: %d root acquisitions after settling, want %d", why, got, want)
	}
}

// absorbSettings requires the one or two deny-only acquisitions that a
// settings change (two patches) causes on the one running follower and adds
// them to the baseline.
func (e *enrolledAnchors) absorbSettings(before int32, why string) {
	e.t.Helper()
	kudurrutest.Eventually(10*time.Second, 20*time.Millisecond, func() bool { return e.root.TCP(anchorRootNormal) > before })
	time.Sleep(3 * time.Second)
	extra := e.root.TCP(anchorRootNormal) - before
	if extra < 1 || extra > 2 {
		e.t.Fatalf("%s: %d deny-only acquisitions", why, extra)
	}
	e.t.Logf("%s: %d deny-only acquisitions", why, extra)
	e.base += extra
}

// awaitValidated waits until a node answers validated recursion.
func (e *enrolledAnchors) awaitValidated(node *kudurrutest.Node, why string) {
	e.t.Helper()
	if !kudurrutest.Eventually(20*time.Second, 50*time.Millisecond, func() bool { return anchorValidated(node.Ports.DNS) }) {
		e.t.Fatal("node does not answer validated recursion", node.Name, why)
	}
}

// owner requires a managed owner with the bootstrapped digest and one Valid
// key, and the expected authority mode; ready may wait for more fields.
func (e *enrolledAnchors) owner(c *staticProcessClient, mode string, ready ...func(api.RootAnchorStatus) bool) api.RootAnchorStatus {
	e.t.Helper()
	status := e.status(c, func(s api.RootAnchorStatus) bool {
		for _, r := range ready {
			if !r(s) {
				return false
			}
		}
		return s.Authority.Mode == mode && s.Owner.Phase == "managed"
	})
	if status.Owner.Digest != e.digest || len(status.Owner.Keys) != 1 || status.Owner.Keys[0].Status != "Valid" || status.Owner.Quarantined ||
		!status.AdmissionCurrent || status.AnchorMode != "managed" || status.Local == nil || status.Local.Retired || len(status.Local.Denied) != 0 {
		e.t.Fatalf("%s owner is not the bootstrapped owner: %+v %+v", mode, status.Owner, status.Local)
	}
	if status.Updater.State == "stopped" || status.Supervisor.State == "stopped" {
		e.t.Fatalf("%s anchor worker stopped: updater %+v supervisor %+v", mode, status.Updater, status.Supervisor)
	}
	return status
}

// offlineStatus runs `kudurru anchors status --offline` on a stopped node.
func (e *enrolledAnchors) offline(bin string, env []string, args ...string) (map[string]json.RawMessage, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 35*time.Second)
	defer cancel()
	// #nosec G204 -- exact fixture-built binary and private temporary paths; no shell
	cmd := exec.CommandContext(ctx, bin, append([]string{"anchors"}, args...)...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "KUDURRU_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	raw, err := cmd.Output()
	var out map[string]json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		out = nil
	}
	return out, err
}

func decodeField[T any](t *testing.T, out map[string]json.RawMessage, field string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(out[field], &v); err != nil {
		t.Fatalf("offline anchors output lacks %s: %v", field, err)
	}
	return v
}

// enrolledAnchorsRevoke ends the partition variant (F28-HA-3 step 5) and,
// with soak, runs the offline recovery (F28-HA-4). The primary a and the
// follower b are both serving when it starts.
func enrolledAnchorsRevoke(t *testing.T, e *enrolledAnchors, bin string, env []string, apiA, apiB *staticProcessClient, primary, follower *kudurrutest.Node,
	a, b kudurrutest.NodeSpec, stop func(*kudurrutest.Node), checkDNS func(*kudurrutest.Node, string, string, int) bool) {
	t.Helper()
	database := func(spec kudurrutest.NodeSpec) string { return filepath.Join(spec.DataDir, "state.db") }
	status := func(spec kudurrutest.NodeSpec) (map[string]json.RawMessage, api.RootAnchorOwner, *api.RootAnchorLocal) {
		t.Helper()
		out, err := e.offline(bin, env, "status", "--offline", "--database", database(spec))
		if err != nil {
			t.Fatal("offline anchor status failed", spec.Name, err)
		}
		return out, decodeField[api.RootAnchorOwner](t, out, "owner"), decodeField[*api.RootAnchorLocal](t, out, "local")
	}
	// The root now publishes only the sole key, revoked and self-signed. The
	// primary's updater is not due for an hour; the follower's next process
	// makes its deny-only check at once, verifies the REVOKE and retires its
	// output. It answers SERVFAIL for everything, authoritative data included,
	// and emits no data; the primary keeps serving its zone.
	e.root.Set(anchorRootRevoked)
	stop(follower)
	follower = kudurrutest.Start(t, bin, b, kudurrutest.Options{Env: env})
	if !kudurrutest.Eventually(30*time.Second, 50*time.Millisecond, func() bool { return anchorServfail(follower.Ports.DNS, "later.e2e.test.") }) {
		t.Fatal("follower did not retire its output on a signed REVOKE")
	}
	e.awaitTCP(anchorRootRevoked, 1, time.Second, "follower revocation check")
	if !checkDNS(primary, "later", "192.0.2.43", dns.RcodeSuccess) {
		t.Fatal("a follower's local denial changed the primary's DNS")
	}
	e.owner(apiA, "primary")
	stop(follower)
	retired, owner, local := status(b)
	if owner.Digest != e.digest || len(owner.Keys) != 1 || owner.Keys[0].Status != "Valid" || local == nil || !local.Retired || len(local.Denied) != 1 ||
		local.Denied[0].RevokedByOwner || local.Denied[0].KeyTag != owner.Keys[0].KeyTag {
		t.Fatalf("follower denial changed its canonical owner or lost the denial: %+v %+v", owner, local)
	}
	// A restart keeps the retirement: the member answers SERVFAIL from its
	// first publication. Its new process makes one new deny-only check,
	// which verifies the same REVOKE and changes nothing.
	follower = kudurrutest.Start(t, bin, b, kudurrutest.Options{Env: env})
	if !kudurrutest.Eventually(30*time.Second, 50*time.Millisecond, func() bool { return anchorServfail(follower.Ports.DNS, "later.e2e.test.") }) {
		t.Fatal("restarted follower served after its denial")
	}
	e.awaitTCP(anchorRootRevoked, 2, 2*time.Second, "restarted retired follower")
	if !anchorServfail(follower.Ports.DNS, "later.e2e.test.") {
		t.Fatal("restarted retired follower served")
	}
	if !e.soak {
		stop(follower)
		again, _, _ := status(b)
		for _, field := range []string{"owner", "local", "version"} {
			if string(again[field]) != string(retired[field]) {
				t.Fatal("the repeated deny-only check changed the retired follower", field)
			}
		}
		return
	}

	// F28-HA-4. The primary's updater observes the REVOKE at its committed
	// refresh. Every trusted key is lost: it answers SERVFAIL and its API
	// closes, so its state is read offline below.
	boot := e.owner(apiA, "primary")
	if boot.Owner.NextRefresh == nil {
		t.Fatal("primary has no committed refresh")
	}
	wait := time.Until(*boot.Owner.NextRefresh) + 5*time.Minute
	t.Logf("waiting %s for the primary's scheduled RFC 5011 refresh at %s", wait.Round(time.Second), boot.Owner.NextRefresh.Format(time.RFC3339))
	followerChecks := e.root.TCP(anchorRootRevoked)
	if !kudurrutest.Eventually(wait, time.Second, func() bool { return e.root.TCP(anchorRootRevoked) > followerChecks }) {
		t.Fatal("primary never made its scheduled refresh")
	}
	if !kudurrutest.Eventually(time.Minute, 100*time.Millisecond, func() bool { return anchorServfail(primary.Ports.DNS, "later.e2e.test.") }) {
		t.Fatal("primary kept serving after it lost every trusted key")
	}
	// The follower installs the quarantined image through its scheduled pull
	// (it revokes the denied key) and stays without trust as well.
	time.Sleep(15 * time.Second)
	if !anchorServfail(follower.Ports.DNS, "later.e2e.test.") {
		t.Fatal("follower served while the cluster has no trusted key")
	}
	stop(follower)
	stop(primary)
	_, lost, _ := status(a)
	if !lost.Quarantined || lost.Digest == e.digest || len(lost.Keys) != 1 || lost.Keys[0].Status != "Revoked" || len(lost.RecoveryGenerations) != 0 {
		t.Fatalf("primary did not quarantine on the REVOKE: %+v", lost)
	}
	followerBefore, followerLost, followerLocal := status(b)
	if followerLost.Digest != lost.Digest || followerLocal == nil || len(followerLocal.Denied) != 1 || !followerLocal.Denied[0].RevokedByOwner {
		t.Fatalf("follower did not install the quarantined image: %+v %+v", followerLost, followerLocal)
	}
	// recover needs this database's own writer term: a follower's database
	// refuses it before any acquisition and changes nothing.
	e.root.Set(anchorRootRecovered)
	recoverArgs := func(spec kudurrutest.NodeSpec) []string {
		return []string{"recover", "--offline", "--database", database(spec), "--expected-digest", lost.Digest,
			"--reason", "replacement root key verified out of band", "--ds", e.root.DSB, "--root-server", e.root.Addr}
	}
	if out, err := e.offline(bin, env, recoverArgs(b)...); err == nil || decodeField[string](t, out, "outcome") != "not-attempted" {
		t.Fatal("recover on a follower database was not refused before acquisition", err)
	}
	followerAfter, _, _ := status(b)
	if string(followerAfter["owner"]) != string(followerBefore["owner"]) || string(followerAfter["local"]) != string(followerBefore["local"]) ||
		string(followerAfter["version"]) != string(followerBefore["version"]) || e.root.TCP(anchorRootRecovered) != 0 {
		t.Fatal("refused follower recovery changed the database or acquired")
	}
	// The follower keeps running, unrestarted, through the primary's recovery.
	follower = kudurrutest.Start(t, bin, b, kudurrutest.Options{Env: env})
	out, err := e.offline(bin, env, recoverArgs(a)...)
	if err != nil || decodeField[string](t, out, "outcome") != "committed" || decodeField[bool](t, out, "quarantined") || decodeField[int](t, out, "recoveries") != 1 {
		t.Fatal("offline recovery on the stopped primary did not commit", err)
	}
	if e.root.TCP(anchorRootRecovered) != 1 {
		t.Fatal("recovery did not make exactly one acquisition", e.root.TCP(anchorRootRecovered))
	}
	primary = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	e.awaitValidated(primary, "recovered primary")
	recovered := e.status(apiA, func(s api.RootAnchorStatus) bool { return s.Authority.Mode == "primary" && !s.Owner.Quarantined })
	statuses := map[string]int{}
	for _, key := range recovered.Owner.Keys {
		statuses[key.Status]++
	}
	if len(recovered.Owner.RecoveryGenerations) != 1 || statuses["Revoked"] != 1 || statuses["Valid"] != 1 || len(recovered.Owner.Keys) != 2 || recovered.Updater.State == "stopped" {
		t.Fatalf("recovered primary status: %+v %+v", recovered.Owner, recovered.Updater)
	}
	// The follower installs the recovered image through scheduled member TLS
	// and publishes again without a restart: the recovered owner revokes the
	// key it denied, and its generation is above the retired floor.
	e.awaitValidated(follower, "follower after recovery")
	if !kudurrutest.Eventually(30*time.Second, 50*time.Millisecond, func() bool { return checkDNS(follower, "later", "192.0.2.43", dns.RcodeSuccess) }) {
		t.Fatal("follower did not serve its zone after installing the recovered image")
	}
	adopted := e.status(apiB, func(s api.RootAnchorStatus) bool { return s.Owner.Digest == recovered.Owner.Digest })
	keys := func(s api.RootAnchorStatus) []string {
		var out []string
		for _, key := range s.Owner.Keys {
			out = append(out, key.ID+"/"+key.Status)
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(keys(adopted), keys(recovered)) || !slices.Equal(adopted.Owner.RecoveryGenerations, recovered.Owner.RecoveryGenerations) ||
		adopted.Local == nil || adopted.Local.Retired || adopted.Authority.Mode != "follower" {
		t.Fatalf("follower does not hold the recovered tombstones and record: %+v %+v", adopted.Owner, adopted.Local)
	}
	// The follower's deny-only check stopped with its denial for this
	// process (D318): an install does not restart it. Exactly one recovery
	// acquisition has happened; a follower restart starts a new check.
	time.Sleep(2 * time.Second)
	if e.root.TCP(anchorRootRecovered) != 1 || (adopted.Supervisor.State != "retired" && adopted.Supervisor.State != "stopped") {
		t.Fatalf("after recovery: %d acquisitions, supervisor %+v", e.root.TCP(anchorRootRecovered), adopted.Supervisor)
	}
	stop(follower)
	follower = kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	e.awaitTCP(anchorRootRecovered, 2, time.Second, "follower restart after recovery")
	e.status(apiB, func(s api.RootAnchorStatus) bool {
		return s.Supervisor.State == "running" && s.Owner.Digest == recovered.Owner.Digest
	})
	e.awaitValidated(follower, "restarted follower after recovery")
}
