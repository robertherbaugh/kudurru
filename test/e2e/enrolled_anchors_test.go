package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
// its scheduled refresh and both nodes lose every trusted key; the stopped
// primary is recovered offline with exactly one root acquisition; the
// follower adopts the recovered image through its scheduled pulls; both hold
// the same tombstones and recovery record.
//
// The committed refresh is at least the one-hour RFC 5011 minimum after the
// bootstrap (D297). In the default suite the primary is restarted with
// KUDURRU_TEST_ANCHOR_UPDATER_AHEAD, a seam compiled only into the
// -tags licensetest fixture binary that moves that one process's updater
// wall clock past the committed refresh (its due and checkpoint comparisons
// only; samples and stored state keep the real clock). With
// KUDURRU_ANCHOR_SOAK=1 the test instead waits for the real refresh with
// production timers, for more than an hour.
func TestEnrolledManagedAnchorsOfflineRecovery(t *testing.T) {
	kudurrutest.RequireE2E(t)
	testEnrolledProcessesWith(t, false, newEnrolledAnchors(t, true))
}

type enrolledAnchors struct {
	t        *testing.T
	root     *anchorRoot
	recovery bool   // run the offline recovery (F28-HA-4) after the REVOKE
	digest   string // the bootstrapped owner every member must keep
	base     int32  // acquisitions the standalone bootstrap took
}

func newEnrolledAnchors(t *testing.T, recovery bool) *enrolledAnchors {
	return &enrolledAnchors{t: t, root: newAnchorRoot(t), recovery: recovery}
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
		e.t.Fatalf("%s: %d root acquisitions, want %d (at %s: %s)", why, e.root.TCP(mode), want, time.Now().Format("15:04:05.000"), e.root.TCPLog())
	}
	time.Sleep(settle)
	if got := e.root.TCP(mode); got != want {
		e.t.Fatalf("%s: %d root acquisitions after settling, want %d (at %s: %s)", why, got, want, time.Now().Format("15:04:05.000"), e.root.TCPLog())
	}
}

// settingsChange returns the hook for enrolledForwardedSettings that counts
// the deny-only check one settings change causes on the follower (nil without
// managed anchors). The deny-only cadence is bound to the cluster settings
// identity (anchorRetrySource): each installed change ends the follower's
// cooldown, so it checks the root once more. The follower answers a forwarded
// patch only once it serves it, and the next patch is sent only after this
// check has settled, so the two patches can no longer share the follower's
// one-second wake coalescing: each is exactly one acquisition.
func (e *enrolledAnchors) settingsChange(follower *staticProcessClient, why string) func(string, func()) {
	if e == nil {
		return nil
	}
	return func(value string, send func()) {
		before := e.root.TCP(anchorRootNormal)
		send()
		e.absorbSettings(follower, before, why+" "+value)
	}
}

// absorbSettings requires exactly one deny-only acquisition since before (the
// count read before the change was sent) for one settings change on the one
// running follower and adds it to the baseline. It waits
// until the follower's supervisor has settled (running, next check at the
// minimum refresh) and the count has not moved for five seconds, longer than
// the coalescing window and the follower's five-second pull, so a second check
// for the same change would be counted here, not at a later check.
func (e *enrolledAnchors) absorbSettings(follower *staticProcessClient, before int32, why string) {
	e.t.Helper()
	if !kudurrutest.Eventually(10*time.Second, 20*time.Millisecond, func() bool { return e.root.TCP(anchorRootNormal) > before }) {
		e.t.Fatalf("%s: no deny-only acquisition (at %s: %s)", why, time.Now().Format("15:04:05.000"), e.root.TCPLog())
	}
	const quiet = 5 * time.Second
	start := time.Now()
	count, since, last := e.root.TCP(anchorRootNormal), time.Now(), ""
	for {
		status := e.status(follower, func(api.RootAnchorStatus) bool { return true })
		worker := status.Supervisor
		settled := worker.State == "running" && worker.Next != nil && worker.Next.After(time.Now().Add(30*time.Minute))
		if seen := fmt.Sprintf("%s next=%v", worker.State, worker.Next); seen != last {
			e.t.Logf("%s: +%s follower supervisor %s, %d acquisitions", why, time.Since(start).Round(time.Millisecond), seen, e.root.TCP(anchorRootNormal)-before)
			last = seen
		}
		if n := e.root.TCP(anchorRootNormal); n != count || !settled {
			count, since = n, time.Now()
		} else if time.Since(since) >= quiet {
			break
		}
		if time.Since(start) > time.Minute {
			e.t.Fatalf("%s: the follower's deny-only checks did not settle: supervisor %+v, %d acquisitions", why, worker, count-before)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if extra := e.root.TCP(anchorRootNormal) - before; extra != 1 {
		e.t.Fatalf("%s: %d deny-only acquisitions, want 1 (at %s: %s)", why, extra, time.Now().Format("15:04:05.000"), e.root.TCPLog())
	}
	e.t.Logf("%s: 1 deny-only acquisition, settled after %s (at %s)", why, time.Since(start).Round(time.Millisecond), time.Now().Format("15:04:05.000"))
	e.base++
}

// awaitValidated waits until a node answers validated recursion, then
// requires every answer over UDP and TCP for the next two seconds to be
// validated as well: a member that holds valid trust never answers SERVFAIL
// once it serves.
func (e *enrolledAnchors) awaitValidated(node *kudurrutest.Node, why string) {
	e.t.Helper()
	if !kudurrutest.Eventually(20*time.Second, 50*time.Millisecond, func() bool { return anchorValidated(node.Ports.DNS) }) {
		e.t.Fatal("node does not answer validated recursion", node.Name, why)
	}
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if !anchorValidated(node.Ports.DNS) {
			e.t.Fatal("node stopped answering validated recursion", node.Name, why)
		}
	}
}

// sampleValidated queries node for validated recursion over UDP and TCP every
// 20 ms until the returned stop is called. It starts with the node's process,
// before it is ready, and judges every sample from the node's first DNS
// answer of any kind on. Before each probe it reads the node's readiness, so
// the result says whether the node had published anything yet (ready, or
// serving a DNS-only bundle while fenced or conflicted) and how many samples
// were validated while DNS-only.
//
// A member that holds valid trust answers validated recursion at every
// authority transition (D369): across its own promotion, a fence or conflict
// observation (the write is persisted while DNS keeps the served publication,
// Store.DNSServingAuthority) and the fenced-to-follower install. So once the
// node has published, every sample must be validated: a SERVFAIL, a wrong
// answer or no answer at all (a timeout, a closed TCP connection) fails. The
// only window allowed is the start of the process, before its first
// publication: its listeners answer SERVFAIL until then (startup gap). A node
// that is not validated when sampling stops fails as well.
type validatedSamples struct {
	samples, dnsOnly int  // judged samples, and those validated while DNS-only
	failed           bool // a failing sample after the first publication, or not validated at stop
	why              string
	startup          []validatedGap // non-validated runs before the first publication
}

// validatedGap is a run of judged samples that were not validated, from its
// first such sample to the next validated one.
type validatedGap struct {
	from, to time.Time
	samples  int
	first    string
}

func (g validatedGap) String() string {
	return fmt.Sprintf("%s +%s (%d samples): %s", g.from.Format("15:04:05.000"), g.to.Sub(g.from).Round(time.Millisecond), g.samples, g.first)
}

// judge fails t unless samples were judged, none failed after the node's
// first publication, the node was validated when sampling stopped and, with
// dnsOnly, some validated samples were taken while the node was DNS-only.
// Startup gaps (before the first publication) are logged.
func (r validatedSamples) judge(t *testing.T, name string, dnsOnly bool) {
	t.Helper()
	for _, g := range r.startup {
		t.Logf("%s: startup answers before its first publication: %s", name, g)
	}
	if r.failed || r.samples == 0 || (dnsOnly && r.dnsOnly == 0) {
		t.Fatalf("%s did not answer validated recursion: %d samples, %d validated while DNS-only, failed %v (%s)", name, r.samples, r.dnsOnly, r.failed, r.why)
	}
	t.Logf("%s: %d judged samples, all validated after its first publication, %d while DNS-only", name, r.samples, r.dnsOnly)
}

func (e *enrolledAnchors) sampleValidated(node *kudurrutest.Node) (stop func() validatedSamples) {
	done, result := make(chan struct{}), make(chan validatedSamples, 1)
	halt := sync.OnceFunc(func() { close(done) })
	e.t.Cleanup(halt)
	client := &http.Client{Timeout: 2 * time.Second}
	// published reports whether the node serves a publication (ready, or
	// DNS-only), and dnsOnly whether that publication is DNS-only.
	readiness := func() (published, dnsOnly bool) {
		response, err := client.Get("http://" + node.Ports.Ops + "/healthz/ready")
		if err != nil {
			return false, false
		}
		defer func() { _ = response.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		dnsOnly = strings.Contains(string(body), "dns_only")
		return response.StatusCode == http.StatusOK || dnsOnly, dnsOnly
	}
	go func() {
		var r validatedSamples
		answered, served := false, false
		var gap *validatedGap
		fail := func(at time.Time, why string) {
			if !r.failed {
				r.failed, r.why = true, fmt.Sprintf("sample %d at %s: %s", r.samples, at.Format("15:04:05.000"), why)
			}
		}
		for {
			select {
			case <-done:
				if gap != nil {
					fail(time.Now(), "not validated when sampling stopped: "+gap.first)
				}
				result <- r
				return
			default:
			}
			published, only := readiness()
			served = served || published
			kind, why := anchorValidatedProbe(node.Ports.DNS)
			now := time.Now()
			if kind != anchorProbeNoAnswer {
				answered = true
			}
			if answered {
				r.samples++
				switch {
				case kind == anchorProbeValidated:
					if only {
						r.dnsOnly++
					}
					if gap != nil {
						gap.to = now
						r.startup = append(r.startup, *gap)
						gap = nil
					}
				case kind == anchorProbeWrong:
					fail(now, why)
				case served:
					// After the first publication nothing but a validated
					// answer is acceptable, no answer included.
					fail(now, fmt.Sprintf("%s (DNS-only %v)", why, only))
				default:
					if gap == nil {
						gap = &validatedGap{from: now, first: why}
					}
					gap.samples++
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return func() validatedSamples {
		halt()
		return <-result
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
// with recovery, runs the offline recovery (F28-HA-4). The primary a and the
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
	if !e.recovery {
		enrolledAnchorsRefusal(t, e, bin, env, apiA, apiB, primary, follower, a, stop)
		stop(follower)
		again, _, _ := status(b)
		for _, field := range []string{"owner", "local", "version"} {
			if string(again[field]) != string(retired[field]) {
				t.Fatal("the repeated deny-only check or the refused pull changed the retired follower", field)
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
	followerChecks := e.root.TCP(anchorRootRevoked)
	wait := 2 * time.Minute
	if os.Getenv("KUDURRU_ANCHOR_SOAK") == "1" {
		wait = time.Until(*boot.Owner.NextRefresh) + 5*time.Minute
		t.Logf("waiting %s for the primary's scheduled RFC 5011 refresh at %s", wait.Round(time.Second), boot.Owner.NextRefresh.Format(time.RFC3339))
	} else {
		// Restart the primary with its updater's wall clock just past the
		// committed refresh (and less than the one-hour minimum ahead, so the
		// refresh it commits is not due again in this process).
		ahead := time.Until(*boot.Owner.NextRefresh) + 30*time.Second
		if ahead >= time.Hour {
			t.Fatal("committed refresh too far ahead for the wall seam", ahead)
		}
		stop(primary)
		if e.root.TCP(anchorRootRevoked) != followerChecks {
			t.Fatal("the primary acquired before its refresh was due")
		}
		primary = kudurrutest.Start(t, bin, a, kudurrutest.Options{Env: append(slices.Clone(env), "KUDURRU_TEST_ANCHOR_UPDATER_AHEAD="+ahead.String())})
	}
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
	out, err := e.offline(bin, env, recoverArgs(a)...)
	if err != nil || decodeField[string](t, out, "outcome") != "committed" || decodeField[bool](t, out, "quarantined") || decodeField[int](t, out, "recoveries") != 1 {
		t.Fatal("offline recovery on the stopped primary did not commit", err)
	}
	if e.root.TCP(anchorRootRecovered) != 1 {
		t.Fatal("recovery did not make exactly one acquisition", e.root.TCP(anchorRootRecovered))
	}
	// The follower starts again, still holding the quarantined image, and
	// keeps running unrestarted until it has installed the recovered one. Its
	// new process makes one deny-only check; with no trusted key it cannot
	// authenticate the reply and that check stops for the process.
	follower = kudurrutest.Start(t, bin, b, kudurrutest.Options{Env: env})
	e.awaitTCP(anchorRootRecovered, 2, time.Second, "quarantined follower's own check")
	primary = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	e.awaitValidated(primary, "recovered primary")
	// The soak's hour-long wait outlives the session, and the primary
	// restarted. Sign in again from a fresh cookie jar, without the old
	// session's cookie or CSRF token, and let both clients use it. The
	// request timeout is the 35 seconds the follower login of the shared
	// flow uses: the first sign-in on a just restarted process under -race
	// on a loaded machine took more than five seconds (e2e01).
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	fresh := &http.Client{Jar: jar, Transport: transport, Timeout: 35 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	apiA.client, apiB.client, apiA.csrf, apiB.csrf = fresh, fresh, "", ""
	var login api.SessionInfo
	apiA.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "audit-e2e-password-1"}, http.StatusOK, &login) // #nosec G101 -- disposable loopback fixture credential
	apiA.csrf, apiB.csrf = login.CSRF, login.CSRF
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
	// The follower's deny-only check stopped for this process (D318) and the
	// install does not restart it; its status names the stop. Nothing else
	// acquired: one recovery and the follower's one check. A follower restart
	// starts a new check, which now authenticates.
	time.Sleep(2 * time.Second)
	if e.root.TCP(anchorRootRecovered) != 2 || adopted.Supervisor.State != "stopped" || !strings.Contains(adopted.Supervisor.Error, "unauthenticated") {
		t.Fatalf("after recovery: %d acquisitions, supervisor %+v", e.root.TCP(anchorRootRecovered), adopted.Supervisor)
	}
	stop(follower)
	follower = kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	e.awaitTCP(anchorRootRecovered, 3, time.Second, "follower restart after recovery")
	e.status(apiB, func(s api.RootAnchorStatus) bool {
		return s.Supervisor.State == "running" && s.Owner.Digest == recovered.Owner.Digest
	})
	e.awaitValidated(follower, "restarted follower after recovery")
}

// enrolledAnchorsRefusal drives one real store refusal through the running
// follower's production pull path (follower.Puller, Coordinator.Pull and the
// status board's ObservePull) into GET /cluster/anchors and the
// kudurru_root_anchor_adoption_refused gauge of its metrics listener. The
// follower has denied the sole key on a REVOKE that the primary has not seen:
// the root answers normally again, and the primary is restarted with its
// updater due (the licensetest wall seam), so its scheduled refresh commits a
// new owner that still trusts the key. The follower's scheduled pull of that
// image is refused (carried_revocation) and installs nothing.
func enrolledAnchorsRefusal(t *testing.T, e *enrolledAnchors, bin string, env []string, apiA, apiB *staticProcessClient, primary, follower *kudurrutest.Node,
	a kudurrutest.NodeSpec, stop func(*kudurrutest.Node)) {
	t.Helper()
	refused := func() float64 {
		v, _ := scrapeGauge(t, follower.Ports.Metrics, "kudurru_root_anchor_adoption_refused", `reason="carried_revocation"`)
		return v
	}
	// The gauge must be exported (as 0) before the owner moves: a metrics
	// listener that does not answer would make the check below vacuous.
	var before float64
	if !kudurrutest.Eventually(10*time.Second, 100*time.Millisecond, func() bool {
		v, ok := scrapeGauge(t, follower.Ports.Metrics, "kudurru_root_anchor_adoption_refused", `reason="carried_revocation"`)
		before = v
		return ok
	}) {
		t.Fatal("the retired follower's metrics listener does not export the refusal gauge")
	}
	if before != 0 {
		t.Fatal("follower refuses before the primary's owner moved", before)
	}
	boot := e.owner(apiA, "primary")
	if boot.Owner.NextRefresh == nil {
		t.Fatal("primary has no committed refresh")
	}
	ahead := time.Until(*boot.Owner.NextRefresh) + 30*time.Second
	if ahead >= time.Hour {
		t.Fatal("committed refresh too far ahead for the wall seam", ahead)
	}
	e.root.Set(anchorRootNormal)
	stop(primary)
	polls := e.root.TCP(anchorRootNormal)
	// Its cleanup stops the restarted primary; the caller ends here.
	kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: append(slices.Clone(env), "KUDURRU_TEST_ANCHOR_UPDATER_AHEAD="+ahead.String())})
	if !kudurrutest.Eventually(30*time.Second, 20*time.Millisecond, func() bool { return e.root.TCP(anchorRootNormal) > polls }) {
		t.Fatal("restarted primary did not make its due refresh")
	}
	t.Log("refusal step: the restarted primary made its due refresh")
	moved := e.status(apiA, func(s api.RootAnchorStatus) bool {
		return s.Authority.Mode == "primary" && s.Owner.Digest != boot.Owner.Digest && s.Owner.NextRefresh != nil && s.Owner.NextRefresh.After(*boot.Owner.NextRefresh)
	})
	if len(moved.Owner.Keys) != 1 || moved.Owner.Keys[0].Status != "Valid" || moved.Updater.State != "running" {
		t.Fatalf("primary refresh did not commit an owner that still trusts the key: %+v %+v", moved.Owner, moved.Updater)
	}
	t.Log("refusal step: the primary committed a new owner that still trusts the key")
	// The follower's scheduled pull of that image is refused by its store.
	if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool { return refused() == 1 }) {
		t.Fatal("follower metrics never reported the refused adoption", refused())
	}
	if v, ok := scrapeGauge(t, follower.Ports.Metrics, "kudurru_root_anchor_adoption_refused", `reason="policy"`); ok && v != 0 {
		t.Fatal("refusal exported under another reason")
	}
	t.Log("refusal step: the follower's metrics report the refusal")
	// The route reports the same refusal when the follower's protected HTTP
	// is open. A retired follower may keep it closed (503), which is why the
	// gauge exists; then the route is checked only for that closure.
	var routeStatus int
	var status api.RootAnchorStatus
	kudurrutest.Eventually(5*time.Second, 100*time.Millisecond, func() bool {
		response, err := apiB.client.Get(apiB.base + "/cluster/anchors")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		routeStatus = response.StatusCode
		body, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
		status = api.RootAnchorStatus{}
		return err == nil && routeStatus == http.StatusOK && json.Unmarshal(body, &status) == nil && status.Pull != nil && status.Pull.Result == "refused"
	})
	switch routeStatus {
	case http.StatusOK:
		if status.Pull == nil || status.Pull.Result != "refused" || status.Pull.Refusal != "carried_revocation" || status.Owner.Digest != e.digest || status.Local == nil || !status.Local.Retired {
			t.Fatalf("follower status does not name the refusal or changed its owner: %+v %+v %+v", status.Pull, status.Owner, status.Local)
		}
		t.Log("refusal step: the follower's route reports the refusal")
	case http.StatusServiceUnavailable:
		// The protected API serves only under a lease of a published
		// complete generation (getClusterAnchors: CheckReadLease), and a
		// retired follower publishes none. Require that this is why it is
		// closed: the node's own operations listener answers that it is not
		// ready (503). A listener that does not answer (0) proves nothing.
		if ready := enrolledOpsStatus(t, follower.Ports.Ops, "/healthz/ready"); ready != http.StatusServiceUnavailable {
			t.Fatal("follower anchor status route answered 503, but readiness answered", ready, "not 503")
		}
		t.Log("refusal step: the retired follower publishes no generation, so its protected HTTP is closed (503); the gauge is its online status surface")
	default:
		t.Fatal("follower anchor status route answered", routeStatus)
	}
	// The primary's refresh is not due again in this process.
	time.Sleep(2 * time.Second)
	if got := e.root.TCP(anchorRootNormal); got != polls+1 {
		t.Fatal("primary refreshed more than once", got-polls)
	}
	if !anchorServfail(follower.Ports.DNS, "later.e2e.test.") {
		t.Fatal("refusing retired follower served")
	}
}

// enrolledOpsStatus returns the HTTP status of one GET on a node's
// operations listener, or 0 when it does not answer. Callers compare it with
// the exact status they require, so a silent listener fails the check.
func enrolledOpsStatus(t *testing.T, address, path string) int {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + address + path)
	if err != nil {
		return 0
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	return response.StatusCode
}

// scrapeGauge reads one sample of name whose label set contains match from a
// node's metrics listener, and reports whether it was present.
func scrapeGauge(t *testing.T, address, name, match string) (float64, bool) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + address + "/metrics")
	if err != nil {
		return 0, false
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil || response.StatusCode != http.StatusOK {
		return 0, false
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, name+"{") || !strings.Contains(line, match) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err == nil {
			return v, true
		}
	}
	return 0, false
}
