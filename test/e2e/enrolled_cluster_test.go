package e2e

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
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
	for i, cfg := range []map[string]any{acfg, bcfg} {
		cfg["querylog"] = map[string]any{"enabled": false}
		cfg["log"] = map[string]any{"level": "error", "format": "text"}
		cfg["auth"].(map[string]any)["session"] = map[string]any{"secure": "never"}
		kudurrutest.WriteConfig(t, []kudurrutest.NodeSpec{a, b}[i].Config, cfg)
	}
	relay := newEnrolledRelay(t, a.Ports.Internal)
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
	review := func() api.ClusterEnrollmentInfo {
		var result api.ClusterEnrollmentInfo
		enrolledProcessRead(t, apiA, "/cluster/enrollment", func(body []byte) bool { return json.Unmarshal(body, &result) == nil })
		return result
	}
	original := review()
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
	var invitation api.ClusterJoinTokenReceipt
	apiA.request(t, http.MethodPost, "/cluster/join-token", api.ClusterJoinTokenCommand{ClusterEnrollmentReview: original.ClusterEnrollmentReview,
		Name: b.Name, Address: b.Ports.Internal, ExpiresAt: time.Now().Add(2 * time.Minute), Reason: "join disposable receiver"}, http.StatusOK, &invitation)
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
		"--invitation-file", invitationPath, "--confirm-name", b.Name, "--confirm-address", b.Ports.Internal)
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
	follower := kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	apiB := &staticProcessClient{client: client, base: "http://" + b.Ports.API + api.BasePath, csrf: login.CSRF}
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
	relay.partition(true)
	if !checkDNS(follower, "seed", "192.0.2.42", dns.RcodeSuccess) {
		t.Fatal("partition interrupted valid local DNS")
	}
	apiB.request(t, http.MethodPost, "/dns/blocking", map[string]any{"blocking": true}, http.StatusServiceUnavailable, nil)
	newRecord := map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
		"metadata": map[string]any{"name": "successor", "namespace": "default"},
		"spec": map[string]any{"zoneRef": map[string]any{"name": "e2e-test"}, "endpoints": []any{
			map[string]any{"dnsName": "later.e2e.test.", "recordType": "A", "targets": []string{"192.0.2.43"}}}}}
	apiA.request(t, http.MethodPost, "/objects/dnsrecordsets", newRecord, http.StatusCreated, nil)
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
	stop(follower)
	follower = kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	awaitDNS(follower, "later", "192.0.2.43", dns.RcodeSuccess)
	awaitDNS(follower, "seed", "", dns.RcodeNameError)
	enrolledProcessRead(t, apiB, "/dns/blocking", blockingOff)
	stop(primary)
	if !checkDNS(follower, "later", "192.0.2.43", dns.RcodeSuccess) {
		t.Fatal("stopped primary interrupted retained follower DNS")
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
		primary = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
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
		awaitDNS(primary, "later", "192.0.2.43", dns.RcodeSuccess)
		awaitDNS(primary, "seed", "", dns.RcodeNameError)
		if identity(a) != aIdentity || identity(b) != bIdentity {
			t.Fatal("promotion/reconnection replaced private member identities")
		}
		return
	}
	primary = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	awaitDNS(primary, "later", "192.0.2.43", dns.RcodeSuccess)
	enrolledProcessRead(t, apiA, "/dns/blocking", blockingOff)
	if identity(a) != aIdentity || identity(b) != bIdentity || review().MemberID != activated.MemberID {
		t.Fatal("restart replaced member identity or primary ownership")
	}
}
