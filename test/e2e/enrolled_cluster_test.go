package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// The relay controls connectivity only. TLS terminates in the real server
// processes, with their independently generated member CAs and original trust.
type enrolledRelay struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	blocked  bool
	live     map[net.Conn]bool
	wg       sync.WaitGroup
}

func newEnrolledRelay(t *testing.T, target string) *enrolledRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &enrolledRelay{listener: listener, target: target, live: make(map[net.Conn]bool)}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			r.wg.Add(1)
			go func() { defer r.wg.Done(); r.exchange(conn) }()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		r.partition(true)
		r.wg.Wait()
	})
	return r
}

func (r *enrolledRelay) partition(blocked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blocked = blocked
	if blocked {
		for conn := range r.live {
			_ = conn.Close()
		}
	}
}

func (r *enrolledRelay) exchange(client net.Conn) {
	defer func() { _ = client.Close() }()
	r.mu.Lock()
	blocked := r.blocked
	r.mu.Unlock()
	if blocked {
		return
	}
	upstream, err := net.DialTimeout("tcp", r.target, time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	r.mu.Lock()
	if r.blocked {
		r.mu.Unlock()
		return
	}
	r.live[client], r.live[upstream] = true, true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.live, client)
		delete(r.live, upstream)
		r.mu.Unlock()
	}()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, client)
		_ = upstream.Close()
		close(done)
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	<-done
}

func enrolledProcessRead(t *testing.T, c *staticProcessClient, path string, ready func([]byte) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for {
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := c.client.Do(r)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 128<<10))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK && readErr == nil && ready(body) {
				return
			}
			if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusServiceUnavailable {
				t.Fatal("enrolled read refused", path, response.StatusCode)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("enrolled observation never converged", path)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// enrolledZoneSettled waits until the primary zone's status reconciliation has
// published Ready for its current generation, its data answers authoritatively
// over UDP and the zone RV stays unchanged across that answer. A status-only
// source gap makes the zone unavailable until the driver realigns, and requests
// holding the retired publication are refused. Reviews and one-time decisions
// must therefore start after this cascade instead of being replayed after it.
func enrolledZoneSettled(t *testing.T, c *staticProcessClient, dnsAddr, name, ip string) {
	t.Helper()
	enrolledZoneObjectSettled(t, c, dnsAddr, "e2e-test", name+".e2e.test.", ip)
}

// enrolledZoneObjectSettled is enrolledZoneSettled for the zone object
// zoneObject, whose data answers fqdn with ip. Every zone created before a
// review needs its own wait: each zone's first Ready status write is its own
// publication, and a review taken before it names a source that the decision
// then finds changed (409 conflict). A record change is followed by a status
// write of its zone as well (serial, content hash, record count), and a
// command whose request began before that write publishes is refused 503
// unavailable. So the zone's status serial must also equal the SOA serial the
// primary serves: Ready alone is already true from the zone's first status
// write.
func enrolledZoneObjectSettled(t *testing.T, c *staticProcessClient, dnsAddr, zoneObject, fqdn, ip string) {
	t.Helper()
	apex := strings.ReplaceAll(zoneObject, "-", ".") + "."
	if !dns.IsSubDomain(apex, fqdn) {
		t.Fatal("zone settle name is outside the zone", zoneObject, fqdn)
	}
	served := func() uint32 {
		answer, err := exchange(dnsAddr, apex, dns.TypeSOA, true)
		if err != nil || answer.Rcode != dns.RcodeSuccess || !answer.Authoritative {
			return 0
		}
		for _, rr := range answer.Answer {
			if soa, ok := rr.(*dns.SOA); ok {
				return soa.Serial
			}
		}
		return 0
	}
	var lastStatus, lastServed uint32
	read := func() (string, bool) {
		var zone struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
				Generation      int64  `json:"generation"`
			} `json:"metadata"`
			Status struct {
				Serial     uint32 `json:"serial"`
				Conditions []struct {
					Type               string `json:"type"`
					Status             string `json:"status"`
					ObservedGeneration int64  `json:"observedGeneration"`
				} `json:"conditions"`
			} `json:"status"`
		}
		ready := false
		enrolledProcessRead(t, c, "/objects/dnszones/"+zoneObject, func(body []byte) bool {
			if json.Unmarshal(body, &zone) != nil {
				return false
			}
			for _, condition := range zone.Status.Conditions {
				ready = ready || condition.Type == "Ready" && condition.Status == "True" && condition.ObservedGeneration == zone.Metadata.Generation
			}
			return true
		})
		lastStatus, lastServed = zone.Status.Serial, served()
		return zone.Metadata.ResourceVersion, ready && lastStatus != 0 && lastStatus == lastServed
	}
	answers := func() bool {
		answer, err := exchange(dnsAddr, fqdn, dns.TypeA, false)
		return err == nil && answer.Rcode == dns.RcodeSuccess && answer.Authoritative && firstA(answer) == ip
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if before, ready := read(); ready && kudurrutest.Eventually(10*time.Second, 50*time.Millisecond, answers) {
			if after, still := read(); still && after == before {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("primary zone status never settled", zoneObject, "status serial", lastStatus, "served SOA serial", lastServed)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// This uses public activation/invitation and the offline CLI, never fixture SQL,
// direct image installation, injected transport, or a manually invoked pull.
// The temporary partition retains the original primary term. Promotion and
// conflicting-primary recovery have separate acceptance requirements.
func TestEnrolledClusterProcessesPartitionCatchupAndRestart(t *testing.T) {
	testEnrolledProcesses(t, false)
}

func TestEnrolledClusterProcessesPromoteAndRejoin(t *testing.T) {
	testEnrolledProcesses(t, true)
}

func testEnrolledProcesses(t *testing.T, promote bool) {
	t.Helper()
	testEnrolledProcessesWith(t, promote, nil)
}

// testEnrolledProcessesWith runs the flow with managed root anchors when
// anchors is set (F28-HA-3, HA-4; see enrolled_anchors_test.go).
func testEnrolledProcessesWith(t *testing.T, promote bool, anchors *enrolledAnchors) {
	t.Helper()
	kudurrutest.RequireE2E(t)
	bin, env := licensedNodeFixture(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	env = append(env, "KUDURRU_ENCRYPTION_KEY="+hex.EncodeToString(key))
	dir := t.TempDir()
	a, acfg := kudurrutest.NewNodeSpec(t, dir, "enrolled-a")
	b, bcfg := kudurrutest.NewNodeSpec(t, dir, "enrolled-b")
	updateSecret, updateKeys := enrolledUpdateSecret(t, dir)
	for i, cfg := range []map[string]any{acfg, bcfg} {
		cfg["tsigKeys"] = updateKeys
		cfg["querylog"] = map[string]any{"enabled": false}
		cfg["log"] = map[string]any{"level": "error", "format": "text"}
		cfg["auth"].(map[string]any)["session"] = map[string]any{"secure": "never"}
		anchors.configure(cfg)
		kudurrutest.WriteConfig(t, []kudurrutest.NodeSpec{a, b}[i].Config, cfg)
	}
	relay := newEnrolledRelay(t, a.Ports.Internal)
	// The follower is reached through its own relay as well, so that the
	// former primary can be kept from pulling after a promotion while the new
	// primary still reaches it.
	relayB := newEnrolledRelay(t, b.Ports.Internal)
	addrB := relayB.listener.Addr().String()
	primary := kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Jar: jar, Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	apiA := &staticProcessClient{client: client, base: "http://" + a.Ports.API + api.BasePath}
	var login api.SessionInfo
	apiA.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "audit-e2e-password-1"}, http.StatusOK, &login) // #nosec G101 -- disposable loopback fixture credential
	apiA.csrf = login.CSRF
	objects := zoneObjects(map[string]string{"seed": "192.0.2.42"})
	apiA.request(t, http.MethodPost, "/objects/dnszones", objects[0], http.StatusCreated, nil)
	apiA.request(t, http.MethodPost, "/objects/dnsrecordsets", objects[1], http.StatusCreated, nil)
	for _, object := range enrolledUpdateZone() {
		apiA.request(t, http.MethodPost, "/objects/"+map[string]string{"DNSZone": "dnszones", "DNSRecordSet": "dnsrecordsets"}[object.(map[string]any)["kind"].(string)], object, http.StatusCreated, nil)
	}
	// The review below names the exact served source, so both zones'
	// startup status writes must have landed first: activation refuses a
	// review that an intervening publication made stale.
	enrolledZoneObjectSettled(t, apiA, a.Ports.DNS, "dyn-test", "seed.dyn.test.", "192.0.2.100")
	enrolledZoneSettled(t, apiA, a.Ports.DNS, "seed", "192.0.2.42")
	if anchors != nil {
		// The standalone primary bootstraps managed trust once. An ordinary
		// commit (the login, the zone status) that lands while the reply is
		// in flight makes it discard that reply and acquire again (F28-EF-1),
		// so the bootstrap may take more than one acquisition; every later
		// count is relative to it.
		boot := anchors.status(apiA, func(s api.RootAnchorStatus) bool {
			return s.Owner.Phase == "managed" && s.Updater.State == "running" && s.Updater.Next != nil && s.Updater.Next.After(time.Now().Add(50*time.Minute))
		})
		anchors.base = anchors.root.TCP(anchorRootNormal)
		anchors.digest = boot.Owner.Digest
		if anchors.base < 1 || anchors.base > 4 {
			t.Fatal("standalone bootstrap acquisitions", anchors.base)
		}
		anchors.awaitTCP(anchorRootNormal, 0, time.Second, "standalone bootstrap")
		anchors.owner(apiA, "standalone")
		if boot.Pull != nil || boot.Supervisor.State != "not_running" || boot.Owner.NextRefresh == nil || boot.Owner.NextRefresh.Before(time.Now().Add(50*time.Minute)) {
			t.Fatalf("bootstrapped standalone status: %+v", boot)
		}
		anchors.awaitValidated(primary, "standalone")
	}
	review := func() api.ClusterEnrollmentInfo {
		var result api.ClusterEnrollmentInfo
		enrolledProcessRead(t, apiA, "/cluster/enrollment", func(body []byte) bool { return json.Unmarshal(body, &result) == nil })
		return result
	}
	// Review only once the source has been quiet: an activation that the
	// node's own post-zone-write commits supersede is refused 503 with no
	// effect (enrolledQuietReview, CI-RED-3).
	original := enrolledQuietReview(t, apiA, 3*time.Second)
	if !original.Dormant || original.Mode != "standalone" {
		t.Fatal("new process did not expose dormant enrollment")
	}
	var activated api.ClusterActivationReceipt
	apiA.request(t, http.MethodPost, "/cluster/activate", api.ActivateClusterCommand{ClusterEnrollmentReview: original.ClusterEnrollmentReview,
		Name: a.Name, Address: relay.listener.Addr().String(), Reason: "disposable process acceptance"}, http.StatusOK, &activated)
	if !activated.Committed || !activated.RestartRequired || activated.MemberID != original.MemberID {
		t.Fatal("activation did not confirm the reviewed member")
	}
	stop := func(node *kudurrutest.Node) {
		node.Stop(15 * time.Second)
		if code, err := node.Wait(time.Second); err != nil || code != 0 {
			t.Fatal("enrolled process did not stop cleanly", node.Name, code, err)
		}
	}
	stop(primary)
	primary = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	original = review()
	if original.Dormant || original.Mode != "primary" {
		t.Fatal("restart did not mount enrolled primary")
	}
	if anchors != nil {
		// Activation over existing managed history and a restart acquire
		// nothing and keep the bootstrapped owner.
		anchors.owner(apiA, "primary")
		anchors.awaitValidated(primary, "activated primary")
		anchors.awaitTCP(anchorRootNormal, 0, time.Second, "activated primary restart")
	}
	var invitation api.ClusterJoinTokenReceipt
	apiA.request(t, http.MethodPost, "/cluster/join-token", api.ClusterJoinTokenCommand{ClusterEnrollmentReview: original.ClusterEnrollmentReview,
		Name: b.Name, Address: addrB, ExpiresAt: time.Now().Add(2 * time.Minute), Reason: "join disposable receiver"}, http.StatusOK, &invitation)
	if !invitation.Committed || invitation.Invitation == "" {
		t.Fatal("invitation not committed")
	}
	invitationPath := filepath.Join(dir, "invitation")
	if err := os.WriteFile(invitationPath, []byte(invitation.Invitation), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	// #nosec G204 -- exact fixture-built binary and private temporary paths; no shell
	join := exec.CommandContext(ctx, bin, "cluster", "join", "--offline", "--data-dir", b.DataDir,
		"--invitation-file", invitationPath, "--confirm-name", b.Name, "--confirm-address", addrB)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "KUDURRU_") {
			join.Env = append(join.Env, entry)
		}
	}
	join.Env = append(join.Env, env...)
	output, err := join.Output()
	if err != nil {
		t.Fatal("one actual offline join failed", err) // never log private output
	}
	var joined replica.JoinResult
	if json.Unmarshal(output, &joined) != nil || !joined.Primary.Committed || joined.Installed.Validate() != nil {
		t.Fatal("join did not confirm membership and installed data")
	}
	identity := func(spec kudurrutest.NodeSpec) [32]byte {
		// #nosec G304 -- exact retained fixture identity; only hash is compared
		data, err := os.ReadFile(filepath.Join(spec.DataDir, "member-identity.json"))
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(data)
	}
	aIdentity, bIdentity := identity(a), identity(b)
	if anchors != nil {
		anchors.awaitTCP(anchorRootNormal, 0, 0, "offline join")
	}
	follower := kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	apiB := &staticProcessClient{client: client, base: "http://" + b.Ports.API + api.BasePath, csrf: login.CSRF}
	if anchors != nil {
		// The joined follower's first image carries the managed owner; its
		// deny-only supervisor makes its own acquisition (D318).
		anchors.awaitTCP(anchorRootNormal, 1, time.Second, "follower deny-only check")
		joinedStatus := anchors.owner(apiB, "follower", func(s api.RootAnchorStatus) bool { return s.Pull != nil && s.Supervisor.State == "running" })
		if joinedStatus.Supervisor.State != "running" || joinedStatus.Updater.State != "not_running" || joinedStatus.Pull == nil || joinedStatus.Pull.Result != "installed" || joinedStatus.Pull.Refusal != "" {
			t.Fatalf("joined follower status: %+v", joinedStatus)
		}
		anchors.awaitValidated(follower, "joined follower")
	}
	checkDNS := func(node *kudurrutest.Node, name, ip string, rcode int) bool {
		for _, tcp := range []bool{false, true} {
			answer, err := exchange(node.Ports.DNS, name+".e2e.test.", dns.TypeA, tcp)
			if err != nil || answer.Rcode != rcode || firstA(answer) != ip || !answer.Authoritative {
				return false
			}
		}
		return true
	}
	awaitDNS := func(node *kudurrutest.Node, name, ip string, rcode int) {
		t.Helper()
		if !kudurrutest.Eventually(20*time.Second, 50*time.Millisecond, func() bool { return checkDNS(node, name, ip, rcode) }) {
			t.Fatal("enrolled native DNS never converged", node.Name, name, ip, rcode)
		}
	}
	awaitDNS(follower, "seed", "192.0.2.42", dns.RcodeSuccess)
	// A signed RFC 2136 UPDATE sent to the follower commits once on the
	// primary, which also answers its refusals (D381).
	enrolledAwaitDyn(t, follower, "seed.dyn.test.", "192.0.2.100", dns.RcodeSuccess)
	enrolledFollowerUpdates(t, primary, follower, updateSecret, "via-follower", "192.0.2.101")
	var forwarded api.BlockingCommandReceipt
	apiB.request(t, http.MethodPost, "/dns/blocking", map[string]any{"blocking": false}, http.StatusAccepted, &forwarded)
	if !forwarded.Committed || forwarded.Publication != "pending" || !forwarded.Disabled {
		t.Fatal("actual follower did not preserve primary commit/publication distinction")
	}
	blockingOff := func(body []byte) bool {
		var status api.BlockingState
		return json.Unmarshal(body, &status) == nil && !status.Blocking && status.Scope == "cluster"
	}
	enrolledProcessRead(t, apiA, "/dns/blocking", blockingOff)
	enrolledProcessRead(t, apiB, "/dns/blocking", blockingOff)
	if anchors != nil {
		// The blocking switch writes the cluster settings (Kudurru/default
		// status), so the follower that now serves it checks the root once
		// for the changed settings identity, as for any settings change.
		anchors.awaitTCP(anchorRootNormal, 2, time.Second, "forwarded blocking switch")
		anchors.base++
	}
	// A fresh client logs in through the follower: the primary admits it under
	// the shared counters and the follower seals the session after installing it.
	freshJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	freshTransport := &http.Transport{}
	t.Cleanup(freshTransport.CloseIdleConnections)
	fresh := &staticProcessClient{client: &http.Client{Jar: freshJar, Transport: freshTransport, Timeout: 35 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: apiB.base}
	if status := enrolledProcessStatus(t, fresh, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "wrong-e2e-password-9"}); status != http.StatusUnauthorized { // #nosec G101 -- deliberately wrong fixture credential
		t.Fatal("follower did not relay the primary's credential refusal", status)
	}
	var followerLogin api.SessionInfo
	fresh.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "audit-e2e-password-1"}, http.StatusOK, &followerLogin) // #nosec G101 -- disposable loopback fixture credential
	if followerLogin.CSRF == "" || followerLogin.Principal.Name != "admin" {
		t.Fatal("follower login did not return a usable session")
	}
	fresh.csrf = followerLogin.CSRF
	enrolledProcessRead(t, fresh, "/auth/me", func([]byte) bool { return true })
	enrolledProcessRead(t, &staticProcessClient{client: fresh.client, base: apiA.base, csrf: fresh.csrf}, "/auth/me", func([]byte) bool { return true })
	// Cluster settings and sign-out are forwarded too (D368): the settings
	// change converges on both nodes, and the session that the follower
	// created ends on both.
	var beforeSettings int32
	if anchors != nil {
		beforeSettings = anchors.root.TCP(anchorRootNormal)
	}
	enrolledForwardedSettings(t, apiA, apiB, b.Name, "33s", "34s")
	if anchors != nil {
		// The deny-only cadence is bound to the cluster settings identity
		// (anchorRetrySource): a settings change makes the follower check the
		// root again at once, one acquisition per coalesced change.
		anchors.absorbSettings(apiB, beforeSettings, "forwarded settings change")
	}
	enrolledForwardedSignOut(t, fresh, freshJar, apiB.base, apiA.base)
	// The own password and personal tokens are forwarded too: a must-change
	// user changes the password through the follower, and `kudurru login
	// --username` against the follower stores a working token.
	enrolledForwardedSelfService(t, bin, apiA, apiB)
	// The follower names its role and primary, answers every mutation that it
	// does not forward with one typed refusal and runs node-local commands
	// itself (D368).
	enrolledFollowerContract(t, apiA, apiB, a.Name, b.Name)
	// Canonical object writes through the follower commit once on the primary
	// with its exact public result; the follower serves them after its pull.
	viaFollower := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
		"metadata": map[string]any{"name": "via-follower", "namespace": "default"},
		"spec": map[string]any{"zoneRef": map[string]any{"name": "e2e-test"}, "endpoints": []any{
			map[string]any{"dnsName": "forwarded.e2e.test.", "recordType": "A", "targets": []string{"192.0.2.44"}}}}}
	var created map[string]any
	apiB.request(t, http.MethodPost, "/objects/dnsrecordsets", viaFollower, http.StatusCreated, &created)
	awaitDNS(primary, "forwarded", "192.0.2.44", dns.RcodeSuccess)
	awaitDNS(follower, "forwarded", "192.0.2.44", dns.RcodeSuccess)
	// A stale precondition is the primary's exact public refusal, not applied.
	stale := enrolledProcessStatus(t, &staticProcessClient{client: apiB.client, base: apiB.base, csrf: apiB.csrf, ifMatch: "1"}, http.MethodPut,
		"/objects/dnsrecordsets/default/via-follower", viaFollower)
	if stale != http.StatusPreconditionFailed {
		t.Fatal("follower did not relay the primary's precondition refusal", stale)
	}
	viaFollower["spec"].(map[string]any)["endpoints"] = []any{map[string]any{"dnsName": "forwarded.e2e.test.", "recordType": "A", "targets": []string{"192.0.2.45"}}}
	apiB.request(t, http.MethodPut, "/objects/dnsrecordsets/default/via-follower", viaFollower, http.StatusOK, nil)
	awaitDNS(follower, "forwarded", "192.0.2.45", dns.RcodeSuccess)
	apiB.request(t, http.MethodDelete, "/objects/dnsrecordsets/default/via-follower", nil, http.StatusNoContent, nil)
	awaitDNS(primary, "forwarded", "", dns.RcodeNameError)
	awaitDNS(follower, "forwarded", "", dns.RcodeNameError)
	// Zone convenience commands (the dashboard's record editor) forward too.
	apiB.request(t, http.MethodPost, "/zones/e2e-test/records", map[string]any{"dnsName": "editor", "recordType": "A", "targets": []string{"192.0.2.47"}}, http.StatusCreated, nil)
	awaitDNS(follower, "editor", "192.0.2.47", dns.RcodeSuccess)
	if status := enrolledProcessStatus(t, apiB, http.MethodPost, "/zones/missing-zone/records", map[string]any{"dnsName": "x", "recordType": "A", "targets": []string{"192.0.2.48"}}); status != http.StatusNotFound {
		t.Fatal("follower did not relay the primary's missing-zone refusal", status)
	}
	apiB.request(t, http.MethodDelete, "/zones/e2e-test/records/editor/A", nil, http.StatusNoContent, nil)
	awaitDNS(follower, "editor", "", dns.RcodeNameError)
	awaitDNS(primary, "editor", "", dns.RcodeNameError)
	// Every other route that writes one canonical object uses the same forward:
	// `kudurru apply`, the dashboard's list editors and batch deletion.
	listDoc := func(name string) map[string]any {
		return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "BlockList",
			"metadata": map[string]any{"name": name, "namespace": "default"},
			"spec":     map[string]any{"mode": "block", "sources": []any{map[string]any{"name": "inline", "inline": []string{name + ".blocked.test"}}}}}
	}
	recordDoc := func(name, ip string) map[string]any {
		return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
			"metadata": map[string]any{"name": name, "namespace": "default"},
			"spec": map[string]any{"zoneRef": map[string]any{"name": "e2e-test"}, "endpoints": []any{
				map[string]any{"dnsName": name + ".e2e.test.", "recordType": "A", "targets": []string{ip}}}}}
	}
	hasList := func(name string, want bool) func([]byte) bool {
		return func(body []byte) bool {
			var page struct {
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				} `json:"items"`
			}
			if json.Unmarshal(body, &page) != nil {
				return false
			}
			found := false
			for _, item := range page.Items {
				found = found || item.Metadata.Name == name
			}
			return found == want
		}
	}
	// `kudurru apply` of two new documents is two exchanges in one request,
	// each answered with the primary's committed result. The second document
	// follows the first at once, while the primary reconciles the object that
	// the first one created. The primary answers under the writer exclusion of
	// a command's own commit, so that reconciliation cannot make the follower
	// report completion_unknown for a document that committed. Each request
	// starts right after the follower converged: its next pull, which would
	// refuse the request's remaining documents, is seconds away.
	const applyRounds = 4
	var appliedRecords []string
	for round := range applyRounds {
		list, record, address := fmt.Sprintf("via-apply-%d", round), fmt.Sprintf("applied-%d", round), fmt.Sprintf("192.0.2.%d", 60+round)
		var applied api.List[api.ApplyResult]
		apiB.request(t, http.MethodPost, "/apply", []any{listDoc(list), recordDoc(record, address)}, http.StatusOK, &applied)
		if len(applied.Items) != 2 {
			t.Fatalf("two-document apply through the follower lost a per-document result: %+v", applied.Items)
		}
		for _, item := range applied.Items {
			if item.Result != api.ResultCreated || item.ErrorKey != "" || item.Error != "" || item.ResourceVersion == "" {
				t.Fatalf("round %d: apply of two new documents through the follower did not answer both as committed: %+v", round, applied.Items)
			}
		}
		enrolledProcessRead(t, apiA, "/blocklists", hasList(list, true))
		enrolledProcessRead(t, apiB, "/blocklists", hasList(list, true))
		awaitDNS(primary, record, address, dns.RcodeSuccess)
		awaitDNS(follower, record, address, dns.RcodeSuccess)
		appliedRecords = append(appliedRecords, record)
	}
	apiB.request(t, http.MethodPut, "/blocklists/default/via-editor", listDoc("via-editor"), http.StatusCreated, nil)
	enrolledProcessRead(t, apiA, "/blocklists", hasList("via-editor", true))
	enrolledProcessRead(t, apiB, "/blocklists", hasList("via-editor", true))
	apiB.request(t, http.MethodDelete, "/blocklists/default/via-editor", nil, http.StatusNoContent, nil)
	enrolledProcessRead(t, apiA, "/blocklists", hasList("via-editor", false))
	enrolledProcessRead(t, apiB, "/blocklists", hasList("via-editor", false))
	// Batch deletion sends one command for each name and answers each.
	var removed api.Processed
	apiB.request(t, http.MethodPost, "/dnsrecordsets:batchDelete", map[string]any{"names": appliedRecords}, http.StatusOK, &removed)
	if len(removed.Processed.Success) != len(appliedRecords) || len(removed.Processed.Errors) != 0 {
		t.Fatalf("batch deletion through the follower did not answer every name as committed: %+v", removed.Processed)
	}
	for _, record := range appliedRecords {
		awaitDNS(primary, record, "", dns.RcodeNameError)
		awaitDNS(follower, record, "", dns.RcodeNameError)
	}
	// On the primary one request applies every document itself (D378). Every
	// item retires the publication the request began on, and the primary
	// reconciles the object of each item while the next one runs: neither
	// refuses a later item. The follower's pulls are reads and are no
	// foreign commit either. The request starts after the primary's own
	// follow-up work on the deletions above has ended, because its first item
	// is held to the publication it began on, as a single write is.
	enrolledZoneSettled(t, apiA, a.Ports.DNS, "seed", "192.0.2.42")
	const directDocuments = 5
	var direct []any
	var directNames []string
	for i := range directDocuments {
		name := fmt.Sprintf("direct-%d", i)
		direct = append(direct, recordDoc(name, fmt.Sprintf("192.0.2.%d", 80+i)))
		directNames = append(directNames, name)
	}
	var directApplied api.List[api.ApplyResult]
	apiA.request(t, http.MethodPost, "/apply", direct, http.StatusOK, &directApplied)
	if len(directApplied.Items) != directDocuments {
		t.Fatalf("apply on the primary lost a per-document result: %+v", directApplied.Items)
	}
	for _, item := range directApplied.Items {
		if item.Result != api.ResultCreated || item.ErrorKey != "" || item.Error != "" || item.ResourceVersion == "" {
			t.Fatalf("one apply of %d new documents on the primary did not create all of them: %+v", directDocuments, directApplied.Items)
		}
	}
	for i, name := range directNames {
		awaitDNS(primary, name, fmt.Sprintf("192.0.2.%d", 80+i), dns.RcodeSuccess)
		awaitDNS(follower, name, fmt.Sprintf("192.0.2.%d", 80+i), dns.RcodeSuccess)
	}
	enrolledZoneSettled(t, apiA, a.Ports.DNS, directNames[directDocuments-1], fmt.Sprintf("192.0.2.%d", 80+directDocuments-1))
	directApplied = api.List[api.ApplyResult]{}
	apiA.request(t, http.MethodPost, "/apply", direct, http.StatusOK, &directApplied)
	for _, item := range directApplied.Items {
		if len(directApplied.Items) != directDocuments || item.Result != api.ResultUnchanged {
			t.Fatalf("second identical apply on the primary did not report every document unchanged: %+v", directApplied.Items)
		}
	}
	var directRemoved api.Processed
	apiA.request(t, http.MethodPost, "/dnsrecordsets:batchDelete", map[string]any{"names": directNames}, http.StatusOK, &directRemoved)
	if len(directRemoved.Processed.Success) != directDocuments || len(directRemoved.Processed.Errors) != 0 {
		t.Fatalf("one batch deletion on the primary did not delete every name: %+v", directRemoved.Processed)
	}
	for _, name := range directNames {
		awaitDNS(primary, name, "", dns.RcodeNameError)
		awaitDNS(follower, name, "", dns.RcodeNameError)
	}
	// A file that does not fit between two of the follower's pulls is cut
	// through the follower (D375, docs/deploy/sqlite-cluster.md): the pull that
	// installs the request's first documents changes the follower's source, and
	// every later document is refused with unavailable ("the follower's source
	// changed"), is not applied and is not sent again. The documents before
	// stay committed. The file grows until one is cut: a short one may fit
	// between two pulls (five seconds by default).
	followerApply := func(documents []any) (api.List[api.ApplyResult], time.Duration) {
		t.Helper()
		data, err := json.Marshal(documents)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, apiB.base+"/apply", strings.NewReader(string(data)))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Kudurru-CSRF", apiB.csrf)
		long := &http.Client{Jar: apiB.client.Jar, Transport: apiB.client.Transport, CheckRedirect: apiB.client.CheckRedirect}
		began := time.Now()
		response, err := long.Do(r)
		if err != nil {
			t.Fatalf("apply of %d documents through the follower: %v", len(documents), err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		var applied api.List[api.ApplyResult]
		if err != nil || response.StatusCode != http.StatusOK || json.Unmarshal(body, &applied) != nil {
			t.Fatalf("apply of %d documents through the follower answered %d: %v %s", len(documents), response.StatusCode, err, body)
		}
		return applied, time.Since(began)
	}
	cut := false
	for round, size := range []int{40, 160, 640} {
		enrolledZoneSettled(t, apiA, a.Ports.DNS, "seed", "192.0.2.42")
		awaitDNS(follower, "seed", "192.0.2.42", dns.RcodeSuccess)
		var file []any
		var names []string
		for i := range size {
			name := fmt.Sprintf("cut-%d-%03d", round, i)
			file = append(file, recordDoc(name, fmt.Sprintf("198.51.100.%d", i%250+1)))
			names = append(names, name)
		}
		applied, took := followerApply(file)
		if len(applied.Items) != size {
			t.Fatalf("apply of %d documents through the follower answered %d results", size, len(applied.Items))
		}
		committed := 0
		for committed < size && applied.Items[committed].Result == api.ResultCreated {
			committed++
		}
		t.Logf("apply of %d documents through the follower took %s: %d created before the first refusal", size, took, committed)
		for i, item := range applied.Items[:committed] {
			if item.ErrorKey != "" || item.Error != "" || item.ResourceVersion == "" {
				t.Fatalf("document %d before the cut was not answered as committed: %+v", i, item)
			}
		}
		if committed == size {
			continue
		}
		cut = true
		// The document at the cut may have been in flight when the pull
		// landed, so its outcome may be unknown. Every later one was refused
		// before it was sent.
		if boundary := applied.Items[committed]; boundary.Result != api.ResultError || boundary.ResourceVersion != "" ||
			boundary.ErrorKey != api.KeyUnavailable && boundary.ErrorKey != "completion_unknown" {
			t.Fatalf("the first document after the committed ones is not an explicit failure: %+v", boundary)
		}
		for i := committed + 1; i < size; i++ {
			item := applied.Items[i]
			if item.Result != api.ResultError || item.ErrorKey != api.KeyUnavailable || item.ResourceVersion != "" || !strings.Contains(item.Error, "the follower's source changed") {
				t.Fatalf("document %d of %d after the cut at %d was not refused as documented: %+v", i, size, committed, item)
			}
		}
		if committed > 0 {
			last := committed - 1
			awaitDNS(primary, names[last], fmt.Sprintf("198.51.100.%d", last%250+1), dns.RcodeSuccess)
			awaitDNS(follower, names[last], fmt.Sprintf("198.51.100.%d", last%250+1), dns.RcodeSuccess)
		}
		// Every refused document was neither applied nor replayed. Both nodes
		// list exactly the committed documents of this file (the one at the
		// cut only if its outcome is unknown and it did land), and answer
		// NXDOMAIN for every refused one. The check runs again after more
		// than one further pull of the follower, so a document that was
		// sent again later would show up there.
		enrolledZoneSettled(t, apiA, a.Ports.DNS, "seed", "192.0.2.42")
		awaitDNS(follower, "seed", "192.0.2.42", dns.RcodeSuccess)
		boundaryUnknown := applied.Items[committed].ErrorKey == "completion_unknown"
		refusedNotApplied := func(pass string) {
			t.Helper()
			for _, c := range []struct {
				name   string
				client *staticProcessClient
			}{{"primary", apiA}, {"follower", apiB}} {
				// The request helper reads at most 128 KiB of a body, and the
				// 640-document round alone lists far more than that, so the
				// record sets are read in small pages.
				present := map[string]bool{}
				for page, cont := 0, ""; ; page++ {
					if page > 100 {
						t.Fatalf("%s: listing the record sets did not end after %d pages", c.name, page)
					}
					var listed api.List[struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
					}]
					path := "/objects/dnsrecordsets?limit=50"
					if cont != "" {
						path += "&continue=" + url.QueryEscape(cont)
					}
					c.client.request(t, http.MethodGet, path, nil, http.StatusOK, &listed)
					for _, item := range listed.Items {
						present[item.Metadata.Name] = true
					}
					if listed.Continue == "" {
						break
					}
					cont = listed.Continue
				}
				for i, name := range names {
					switch {
					case i < committed && !present[name]:
						t.Fatalf("%s %s: committed document %d of %d before the cut is missing", pass, c.name, i, size)
					case i == committed && present[name] && !boundaryUnknown:
						t.Fatalf("%s %s: document %d at the cut was refused as unavailable but applied", pass, c.name, i)
					case i > committed && present[name]:
						t.Fatalf("%s %s: document %d of %d, refused after the cut at %d, was applied", pass, c.name, i, size, committed)
					}
				}
			}
			for i := committed; i < size; i++ {
				if i == committed && boundaryUnknown {
					continue
				}
				for _, node := range []*kudurrutest.Node{primary, follower} {
					if !checkDNS(node, names[i], "", dns.RcodeNameError) {
						t.Fatalf("%s: document %d, refused after the cut at %d, is answered on %s", pass, i, committed, node.Name)
					}
				}
			}
		}
		refusedNotApplied("after the request")
		time.Sleep(6 * time.Second) // more than one follower pull (five seconds by default)
		enrolledZoneSettled(t, apiA, a.Ports.DNS, "seed", "192.0.2.42")
		refusedNotApplied("after another pull")
		t.Logf("all %d refused documents of %d were checked on both nodes, twice", size-committed, size)
		break
	}
	if !cut {
		t.Fatal("no apply through the follower outlasted one of its pulls, so the documented cut was never observed")
	}
	// A second session through the follower, signed out while partitioned.
	partitionedJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	partitioned := &staticProcessClient{client: &http.Client{Jar: partitionedJar, Transport: freshTransport, Timeout: 35 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: apiB.base}
	var partitionedLogin api.SessionInfo
	partitioned.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "audit-e2e-password-1"}, http.StatusOK, &partitionedLogin) // #nosec G101 -- disposable loopback fixture credential
	partitioned.csrf = partitionedLogin.CSRF
	relay.partition(true)
	if !checkDNS(follower, "seed", "192.0.2.42", dns.RcodeSuccess) {
		t.Fatal("partition interrupted valid local DNS")
	}
	if anchors != nil && !anchorValidated(follower.Ports.DNS) {
		t.Fatal("partition interrupted validated recursion on the follower")
	}
	apiB.request(t, http.MethodPost, "/dns/blocking", map[string]any{"blocking": true}, http.StatusServiceUnavailable, nil)
	// A signed UPDATE that cannot reach the primary answers SERVFAIL and is
	// never applied or replayed after reconnection.
	enrolledUnreachedUpdate(t, follower, updateSecret, "partitioned-update.dyn.test.", "192.0.2.102")
	// A mutation that the follower does not forward is refused for its role,
	// which needs no exchange with the primary: the answer is the same.
	enrolledPrimaryRequired(t, apiB, a.Name, http.MethodPost, "/allowlist", map[string]any{"domain": "partitioned-allow.e2e.test"})
	// A forwarded settings patch that cannot reach the primary is an explicit
	// 503 and is never applied or replayed after reconnection.
	enrolledUnreachedSettings(t, apiB, "35s")
	// A sign-out that cannot reach the primary clears the browser's cookies
	// and answers 503; the session stays valid and is never revoked by a
	// replay after reconnection.
	unrevoked := enrolledUnreachedSignOut(t, partitioned, partitionedJar)
	// A forwarded object write that cannot reach the primary is refused and
	// is never applied or replayed after reconnection.
	refused := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
		"metadata": map[string]any{"name": "partitioned", "namespace": "default"},
		"spec": map[string]any{"zoneRef": map[string]any{"name": "e2e-test"}, "endpoints": []any{
			map[string]any{"dnsName": "partitioned.e2e.test.", "recordType": "A", "targets": []string{"192.0.2.46"}}}}}
	apiB.request(t, http.MethodPost, "/objects/dnsrecordsets", refused, http.StatusServiceUnavailable, nil)
	// A kind route answers 503. Apply keeps its per-item answer: the document
	// is an error whose key says either that nothing was sent or that the
	// outcome of the lost exchange is unknown. It never reports a commit.
	apiB.request(t, http.MethodPut, "/blocklists/default/partitioned-list", listDoc("partitioned-list"), http.StatusServiceUnavailable, nil)
	var unreached api.List[api.ApplyResult]
	apiB.request(t, http.MethodPost, "/apply", []any{recordDoc("partitioned-apply", "192.0.2.51")}, http.StatusOK, &unreached)
	if len(unreached.Items) != 1 || unreached.Items[0].Result != api.ResultError || unreached.Items[0].ResourceVersion != "" ||
		unreached.Items[0].ErrorKey != api.KeyUnavailable && unreached.Items[0].ErrorKey != "completion_unknown" {
		t.Fatalf("partitioned apply did not report its document as an explicit failure: %+v", unreached.Items)
	}
	newRecord := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
		"metadata": map[string]any{"name": "successor", "namespace": "default"},
		"spec": map[string]any{"zoneRef": map[string]any{"name": "e2e-test"}, "endpoints": []any{
			map[string]any{"dnsName": "later.e2e.test.", "recordType": "A", "targets": []string{"192.0.2.43"}}}}}
	apiA.request(t, http.MethodPost, "/objects/dnsrecordsets", newRecord, http.StatusCreated, nil)
	// The deletion of hosts below is a single command on the primary: it
	// starts after the zone's status write for this record has published.
	enrolledZoneSettled(t, apiA, a.Ports.DNS, "later", "192.0.2.43")
	// A batch deletion through the partitioned follower reports its item as
	// an error. The later assertions on this record prove that it was neither
	// applied nor replayed.
	var retained api.Processed
	apiB.request(t, http.MethodPost, "/dnsrecordsets:batchDelete", map[string]any{"names": []string{"successor"}}, http.StatusOK, &retained)
	if len(retained.Processed.Success) != 0 || len(retained.Processed.Errors) != 1 || retained.Processed.Errors[0].Error == "" {
		t.Fatalf("partitioned batch deletion did not report its item as an explicit failure: %+v", retained.Processed)
	}
	apiA.request(t, http.MethodDelete, "/objects/dnsrecordsets/default/hosts", nil, http.StatusNoContent, nil)
	awaitDNS(primary, "seed", "", dns.RcodeNameError)
	awaitDNS(primary, "later", "192.0.2.43", dns.RcodeSuccess)
	if !checkDNS(follower, "seed", "192.0.2.42", dns.RcodeSuccess) || !checkDNS(follower, "later", "", dns.RcodeNameError) {
		t.Fatal("partitioned follower adopted data without a member exchange")
	}
	relay.partition(false)
	awaitDNS(follower, "seed", "", dns.RcodeNameError)
	awaitDNS(follower, "later", "192.0.2.43", dns.RcodeSuccess)
	enrolledProcessRead(t, apiB, "/dns/blocking", blockingOff)
	enrolledNeverApplied(t, "partitioned-update.dyn.test.", primary, follower)
	if !checkDNS(follower, "partitioned", "", dns.RcodeNameError) || !checkDNS(primary, "partitioned", "", dns.RcodeNameError) {
		t.Fatal("refused partitioned write was applied or replayed")
	}
	for _, node := range []*kudurrutest.Node{primary, follower} {
		if !checkDNS(node, "partitioned-apply", "", dns.RcodeNameError) || !checkDNS(node, "later", "192.0.2.43", dns.RcodeSuccess) {
			t.Fatal("refused partitioned apply or batch deletion was applied or replayed", node.Name)
		}
	}
	enrolledProcessRead(t, apiA, "/blocklists", hasList("partitioned-list", false))
	enrolledProcessRead(t, apiB, "/blocklists", hasList("partitioned-list", false))
	for _, base := range []string{apiA.base, apiB.base} {
		if status := enrolledCookieStatus(t, base, unrevoked); status != http.StatusOK {
			t.Fatal("a sign-out that did not reach the primary was applied or replayed after reconnection", base, status)
		}
	}
	for _, node := range []*staticProcessClient{apiA, apiB} {
		enrolledProcessRead(t, node, "/settings", func(raw []byte) bool {
			return strings.Contains(string(raw), `"34s"`) && !strings.Contains(string(raw), `"35s"`)
		})
	}
	if anchors != nil {
		// What the workers last did, for a failing count below.
		for name, c := range map[string]*staticProcessClient{"primary": apiA, "follower": apiB} {
			s := anchors.status(c, func(api.RootAnchorStatus) bool { return true })
			t.Logf("partition check at %s: %s updater %+v supervisor %+v", time.Now().Format("15:04:05.000"), name, s.Updater, s.Supervisor)
		}
		anchors.awaitTCP(anchorRootNormal, 1, 0, "partition")
		anchors.owner(apiB, "follower")
	}
	stop(follower)
	follower = kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	awaitDNS(follower, "later", "192.0.2.43", dns.RcodeSuccess)
	awaitDNS(follower, "seed", "", dns.RcodeNameError)
	enrolledProcessRead(t, apiB, "/dns/blocking", blockingOff)
	if anchors != nil {
		// A new process is a new deny-only session: one acquisition.
		anchors.awaitTCP(anchorRootNormal, 2, time.Second, "follower restart")
		anchors.owner(apiB, "follower")
		anchors.awaitValidated(follower, "restarted follower")
	}
	stop(primary)
	if !checkDNS(follower, "later", "192.0.2.43", dns.RcodeSuccess) {
		t.Fatal("stopped primary interrupted retained follower DNS")
	}
	if anchors != nil && !anchorValidated(follower.Ports.DNS) {
		t.Fatal("stopped primary interrupted validated recursion on the follower")
	}
	if promote {
		var review api.ClusterPromotionInfo
		enrolledProcessRead(t, apiB, "/cluster/promotion", func(body []byte) bool {
			return json.Unmarshal(body, &review) == nil && review.Eligible
		})
		var promotion api.ClusterPromotionReceipt
		apiB.request(t, http.MethodPost, "/cluster/promote", api.ClusterPromoteCommand{ClusterPromotionReview: review.ClusterPromotionReview,
			ConfirmNode: b.Name, RequestID: "process-promotion", Reason: "independently observed primary loss"}, http.StatusOK, &promotion)
		if !promotion.Committed || promotion.Term.Epoch != joined.Installed.Term.Epoch+1 || promotion.Term.PrimaryID != review.MemberID {
			t.Fatal("actual operator promotion lost the original term transition")
		}
		enrolledProcessRead(t, apiB, "/cluster/enrollment", func(body []byte) bool {
			var info api.ClusterEnrollmentInfo
			return json.Unmarshal(body, &info) == nil && info.Mode == "primary"
		})
		apiB.request(t, http.MethodPost, "/dns/blocking", map[string]any{"blocking": true}, http.StatusOK, nil)
		// The promoted node answers for its new role: it reports itself as the
		// primary and runs the route that it refused as a follower.
		enrolledProcessRead(t, apiB, "/cluster", func(body []byte) bool {
			var info api.ClusterInfo
			return json.Unmarshal(body, &info) == nil && info.Role == "primary" && info.Primary == b.Name && info.Leader == b.Name && !info.PrimaryRequired && len(info.ForwardedOperations) == 0
		})
		if answer := enrolledSend(t, apiB, http.MethodPatch, "/settings", map[string]any{}); answer.status != http.StatusBadRequest || answer.Error.Key != api.KeyInvalid {
			t.Fatalf("promoted node still refuses a settings write for its former role: status=%d key=%q", answer.status, answer.Error.Key)
		}
		if anchors != nil {
			// The promoted writer mounts the updater over the follower's
			// owner: it waits for the committed refresh, acquires nothing and
			// never re-bootstraps; the deny-only child was joined.
			promoted := anchors.owner(apiB, "primary")
			if promoted.Updater.State != "running" || promoted.Updater.Next == nil || promoted.Supervisor.State != "not_running" || promoted.Pull != nil {
				t.Fatalf("promoted primary status: updater %+v supervisor %+v pull %+v", promoted.Updater, promoted.Supervisor, promoted.Pull)
			}
			anchors.awaitTCP(anchorRootNormal, 2, 2*time.Second, "promotion")
			anchors.awaitValidated(follower, "promoted primary")
		}
		// The former primary restarts while it cannot reach the new primary's
		// internal listener. The new primary's scheduled announcement still
		// reaches it and fences it, and without a pull it stays fenced: its API
		// names that state and the newer primary, with no Retry-After, and it
		// keeps answering DNS from its last valid data.
		relayB.partition(true)
		primary = kudurrutest.Start(t, bin, a, kudurrutest.Options{Env: env})
		var sampled func() validatedSamples
		if anchors != nil {
			// The old primary holds valid trust from its first answer: it is
			// fenced until it installs the successor, then follows. Sample
			// it across that transition, from the start of its process.
			sampled = anchors.sampleValidated(primary)
		}
		enrolledFencedClosed(t, apiA, b.Name)
		awaitDNS(primary, "later", "192.0.2.43", dns.RcodeSuccess)
		// The fenced former primary cannot apply a signed UPDATE and cannot
		// reach the new primary: SERVFAIL, never applied (D381).
		enrolledUnreachedUpdate(t, primary, updateSecret, "fenced-update.dyn.test.", "192.0.2.103")
		if anchors != nil {
			// Fenced is a supervised role: its child checks the root once
			// (D369), before the partition heals.
			anchors.awaitTCP(anchorRootNormal, 3, 0, "fenced restart")
		}
		relayB.partition(false)
		ready, readyCancel := context.WithTimeout(t.Context(), 60*time.Second)
		if err := primary.WaitReady(ready); err != nil {
			readyCancel()
			t.Fatal("restarted old primary never became ready", err)
		}
		readyCancel()
		// Only production scheduled peer observations/pulls may fence and install.
		// No test calls ObserveAuthority, installs an image, or retries a mutation.
		enrolledProcessRead(t, apiA, "/cluster/enrollment", func(body []byte) bool {
			var info api.ClusterEnrollmentInfo
			return json.Unmarshal(body, &info) == nil && info.Mode == "follower" && info.MemberID == activated.MemberID
		})
		enrolledProcessRead(t, apiA, "/dns/blocking", func(body []byte) bool {
			var status api.BlockingState
			return json.Unmarshal(body, &status) == nil && status.Blocking
		})
		// The former primary now follows, and says so with the new primary's name.
		enrolledProcessRead(t, apiA, "/cluster", func(body []byte) bool {
			var info api.ClusterInfo
			return json.Unmarshal(body, &info) == nil && info.Role == "follower" && info.Primary == b.Name && info.PrimaryRequired
		})
		enrolledPrimaryRequired(t, apiA, b.Name, http.MethodPost, "/allowlist", map[string]any{"domain": "rejoined-allow.e2e.test"})
		// The rejoined former primary forwards a settings patch to the new
		// primary, which commits it; both nodes then serve the change.
		var beforeSettings int32
		if anchors != nil {
			// Installing the successor is a role change (fenced to
			// follower): the fenced child is joined and the follower child
			// checks the root once at its start.
			anchors.awaitTCP(anchorRootNormal, 4, time.Second, "rejoin as follower")
			beforeSettings = anchors.root.TCP(anchorRootNormal)
		}
		enrolledForwardedSettings(t, apiB, apiA, a.Name, "46s", "47s")
		if anchors != nil {
			// A changed settings identity ends the follower's cooldown, so
			// the rejoined member checks the root again (D369).
			anchors.absorbSettings(beforeSettings, "settings change through the rejoined member")
		}
		// The new primary applies a signed UPDATE itself, and the rejoined
		// former primary now forwards one to it.
		// Right after the forwarded settings patch the new primary republishes,
		// and a native UPDATE whose lease predates that republication fails its
		// writer check with SERVFAIL (the same window HA-OPEN recorded for
		// queries). RFC 2136 clients retry SERVFAIL; this insert is idempotent,
		// so the test does too and logs every SERVFAIL it receives.
		enrolledEventuallyUpdate(t, follower, updateSecret, func(m *dns.Msg) {
			m.Insert([]dns.RR{enrolledUpdateA("promoted.dyn.test.", "192.0.2.104")})
		})
		enrolledFollowerUpdates(t, follower, primary, updateSecret, "via-rejoined", "192.0.2.105")
		enrolledAwaitDyn(t, primary, "promoted.dyn.test.", "192.0.2.104", dns.RcodeSuccess)
		enrolledNeverApplied(t, "fenced-update.dyn.test.", primary, follower)
		awaitDNS(primary, "later", "192.0.2.43", dns.RcodeSuccess)
		awaitDNS(primary, "seed", "", dns.RcodeNameError)
		// The write that the fenced former primary refused exists on neither
		// node. The answer is waited for, and every answer that differs is
		// logged as it was received. Right after the forwarded settings patch
		// above, the new primary republishes, and a UDP query whose answer was
		// resolved from the generation that the republication replaced gets
		// no answer: the guarded emission refuses the write ("write failed",
		// "serving: dependency changed", proto=udp, at debug level) and UDP
		// sends nothing, so the client times out after 3 seconds (HA-OPEN
		// e2e03, e2e06, fix-03, fix-05). That is a DNS-path follow-up, not
		// what this check asserts. An applied record would answer NOERROR and
		// never converge.
		for _, node := range []*kudurrutest.Node{primary, follower} {
			if !kudurrutest.Eventually(20*time.Second, 50*time.Millisecond, func() bool {
				for _, tcp := range []bool{false, true} {
					answer, err := exchange(node.Ports.DNS, "refused-fenced.e2e.test.", dns.TypeA, tcp)
					switch {
					case err != nil:
						t.Logf("refused-fenced on %s (tcp=%t) at %s: %v", node.Name, tcp, time.Now().Format(time.RFC3339Nano), err)
						return false
					case answer.Rcode != dns.RcodeNameError || firstA(answer) != "" || !answer.Authoritative:
						t.Logf("refused-fenced on %s (tcp=%t): rcode=%s aa=%t a=%q", node.Name, tcp, dns.RcodeToString[answer.Rcode], answer.Authoritative, firstA(answer))
						return false
					}
				}
				return true
			}) {
				t.Fatal("a write refused by the fenced former primary was applied", node.Name)
			}
		}
		if identity(a) != aIdentity || identity(b) != bIdentity {
			t.Fatal("promotion/reconnection replaced private member identities")
		}
		if anchors != nil {
			// The old primary was fenced and installed the successor: the
			// same owner and no refusal. Since the promotion it checked the
			// root once per supervised role it passed through (fenced, then
			// follower: one child each) and once per coalesced settings
			// change (absorbed into the baseline above). Nothing else
			// acquires: the new primary's updater waits for the committed
			// refresh and each child's normal check holds the one-hour
			// cooldown for an unchanged owner and settings identity.
			rejoined := anchors.owner(apiA, "follower", func(s api.RootAnchorStatus) bool {
				return s.Pull != nil && s.Pull.Result == "installed" && s.Supervisor.State == "running"
			})
			if rejoined.Supervisor.State != "running" || rejoined.Updater.State != "not_running" || rejoined.Pull == nil || rejoined.Pull.Result != "installed" || rejoined.Pull.Refusal != "" {
				t.Fatalf("rejoined member status: %+v", rejoined)
			}
			// From its first answer on, no answer was wrong, any SERVFAIL gap
			// was bounded, and some validated answers were taken while it was
			// DNS-only (fenced).
			sampled().judge(t, "restarted old primary", true)
			time.Sleep(2 * time.Second)
			if n := anchors.root.TCP(anchorRootNormal) - anchors.base; n != 4 || anchors.root.TotalTCP() != n+anchors.base {
				t.Fatal("rejoin acquisitions", n)
			}
			anchors.awaitValidated(primary, "rejoined member")
			anchors.owner(apiB, "primary")
		}
		return
	}
	primary = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	awaitDNS(primary, "later", "192.0.2.43", dns.RcodeSuccess)
	enrolledProcessRead(t, apiA, "/dns/blocking", blockingOff)
	if identity(a) != aIdentity || identity(b) != bIdentity || review().MemberID != activated.MemberID {
		t.Fatal("restart replaced member identity or primary ownership")
	}
	if anchors != nil {
		anchors.awaitTCP(anchorRootNormal, 2, time.Second, "primary restart")
		anchors.owner(apiA, "primary")
		anchors.owner(apiB, "follower")
		anchors.awaitValidated(primary, "restarted primary")
		enrolledAnchorsRevoke(t, anchors, bin, env, apiA, apiB, primary, follower, a, b, stop, checkDNS)
	}
}
