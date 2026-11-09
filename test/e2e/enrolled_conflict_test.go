package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// enrolledProcessStatus sends one request and returns only its status code.
// It is used where the exact refusal class matters but not its body.
func enrolledProcessStatus(t *testing.T, c *staticProcessClient, method, path string, body any) int {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r.GetBody = nil
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		r.Header.Set("X-Kudurru-CSRF", c.csrf)
	}
	response, err := c.client.Do(r)
	if err != nil {
		t.Fatal("enrolled request failed", method, path, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 128<<10))
	_ = response.Body.Close()
	return response.StatusCode
}

func recordSetBody(name, ip string) map[string]any {
	return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
		"metadata": map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{"zoneRef": map[string]any{"name": "e2e-test"}, "endpoints": []any{
			map[string]any{"dnsName": name + ".e2e.test.", "recordType": "A", "targets": []string{ip}}}}}
}

// Three real server processes, public activation/invitation/offline join and
// reviewed promotion. B and C are each promoted while isolated from the lost
// primary AND from each other, and accept conflicting writes. After the
// partition heals only production scheduled peer exchanges record the durable
// conflict; writers stay refused until an operator keeps B's branch through
// the public resolution route. C and the restarted original primary then adopt
// the new epoch and B's data through scheduled exchanges and pulls alone. C's
// divergent write is never replayed. No fixture SQL, injected transport,
// direct image installation, manual pull or ObserveAuthority call is used.
func TestEnrolledProcessConflictResolution(t *testing.T) {
	kudurrutest.RequireE2E(t)
	bin, env := licensedNodeFixture(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	env = append(env, "KUDURRU_ENCRYPTION_KEY="+hex.EncodeToString(key))
	dir := t.TempDir()
	specs := make([]kudurrutest.NodeSpec, 3)
	configs := make([]map[string]any, 3)
	for i, name := range []string{"conflict-a", "conflict-b", "conflict-c"} {
		specs[i], configs[i] = kudurrutest.NewNodeSpec(t, dir, name)
	}
	for i, cfg := range configs {
		cfg["querylog"] = map[string]any{"enabled": false}
		cfg["log"] = map[string]any{"level": "error", "format": "text"}
		cfg["auth"].(map[string]any)["session"] = map[string]any{"secure": "never"}
		kudurrutest.WriteConfig(t, specs[i].Config, cfg)
	}
	a, b, c := specs[0], specs[1], specs[2]
	// Every member is reached only through its own relay, so B and C can be
	// isolated from each other while their HTTP APIs remain reachable.
	relayA, relayB, relayC := newEnrolledRelay(t, a.Ports.Internal), newEnrolledRelay(t, b.Ports.Internal), newEnrolledRelay(t, c.Ports.Internal)
	addrA, addrB, addrC := relayA.listener.Addr().String(), relayB.listener.Addr().String(), relayC.listener.Addr().String()
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
	apiB := &staticProcessClient{client: client, base: "http://" + b.Ports.API + api.BasePath, csrf: login.CSRF}
	apiC := &staticProcessClient{client: client, base: "http://" + c.Ports.API + api.BasePath, csrf: login.CSRF}
	objects := zoneObjects(map[string]string{"seed": "192.0.2.42"})
	apiA.request(t, http.MethodPost, "/objects/dnszones", objects[0], http.StatusCreated, nil)
	apiA.request(t, http.MethodPost, "/objects/dnsrecordsets", objects[1], http.StatusCreated, nil)
	enrollment := func(c *staticProcessClient, ready func(api.ClusterEnrollmentInfo) bool) api.ClusterEnrollmentInfo {
		t.Helper()
		var result api.ClusterEnrollmentInfo
		enrolledProcessRead(t, c, "/cluster/enrollment", func(body []byte) bool {
			result = api.ClusterEnrollmentInfo{}
			return json.Unmarshal(body, &result) == nil && ready(result)
		})
		return result
	}
	anyInfo := func(api.ClusterEnrollmentInfo) bool { return true }
	original := enrollment(apiA, anyInfo)
	var activated api.ClusterActivationReceipt
	apiA.request(t, http.MethodPost, "/cluster/activate", api.ActivateClusterCommand{ClusterEnrollmentReview: original.ClusterEnrollmentReview,
		Name: a.Name, Address: addrA, Reason: "disposable conflict acceptance"}, http.StatusOK, &activated)
	if !activated.Committed || !activated.RestartRequired {
		t.Fatal("activation not committed")
	}
	stop := func(node *kudurrutest.Node) {
		t.Helper()
		node.Stop(15 * time.Second)
		if code, err := node.Wait(time.Second); err != nil || code != 0 {
			t.Fatal("enrolled process did not stop cleanly", node.Name, code, err)
		}
	}
	stop(primary)
	primary = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	join := func(spec kudurrutest.NodeSpec, address string) replica.JoinResult {
		t.Helper()
		review := enrollment(apiA, func(info api.ClusterEnrollmentInfo) bool { return info.Mode == "primary" })
		var invitation api.ClusterJoinTokenReceipt
		apiA.request(t, http.MethodPost, "/cluster/join-token", api.ClusterJoinTokenCommand{ClusterEnrollmentReview: review.ClusterEnrollmentReview,
			Name: spec.Name, Address: address, ExpiresAt: time.Now().Add(2 * time.Minute), Reason: "join disposable receiver"}, http.StatusOK, &invitation)
		path := filepath.Join(dir, spec.Name+"-invitation")
		if err := os.WriteFile(path, []byte(invitation.Invitation), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
		defer cancel()
		// #nosec G204 -- exact fixture-built binary and private temporary paths; no shell
		cmd := exec.CommandContext(ctx, bin, "cluster", "join", "--offline", "--data-dir", spec.DataDir,
			"--invitation-file", path, "--confirm-name", spec.Name, "--confirm-address", address)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "KUDURRU_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, env...)
		output, err := cmd.Output()
		if err != nil {
			t.Fatal("actual offline join failed", spec.Name, err) // never log private output
		}
		var joined replica.JoinResult
		if json.Unmarshal(output, &joined) != nil || !joined.Primary.Committed || joined.Installed.Validate() != nil {
			t.Fatal("join did not confirm membership and installed data", spec.Name)
		}
		return joined
	}
	joinedB := join(b, addrB)
	joinedC := join(c, addrC)
	identity := func(spec kudurrutest.NodeSpec) [32]byte {
		// #nosec G304 -- exact retained fixture identity; only hash is compared
		data, err := os.ReadFile(filepath.Join(spec.DataDir, "member-identity.json"))
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(data)
	}
	identities := [3][32]byte{identity(a), identity(b), identity(c)}
	nodeB := kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	nodeC := kudurrutest.StartReady(t, bin, c, kudurrutest.Options{Env: env})
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
		if !kudurrutest.Eventually(30*time.Second, 50*time.Millisecond, func() bool { return checkDNS(node, name, ip, rcode) }) {
			t.Fatal("enrolled native DNS never converged", node.Name, name, ip, rcode)
		}
	}
	awaitDNS(nodeB, "seed", "192.0.2.42", dns.RcodeSuccess)
	awaitDNS(nodeC, "seed", "192.0.2.42", dns.RcodeSuccess)
	if joinedB.Installed.Term.Epoch != joinedC.Installed.Term.Epoch {
		t.Fatal("receivers joined different terms")
	}
	epoch := joinedB.Installed.Term.Epoch

	// Lose the primary, then isolate B and C from each other before promotion.
	stop(primary)
	relayB.partition(true)
	relayC.partition(true)
	promote := func(c *staticProcessClient, spec kudurrutest.NodeSpec) api.ClusterPromotionReceipt {
		t.Helper()
		var review api.ClusterPromotionInfo
		enrolledProcessRead(t, c, "/cluster/promotion", func(body []byte) bool {
			return json.Unmarshal(body, &review) == nil && review.Eligible
		})
		var receipt api.ClusterPromotionReceipt
		c.request(t, http.MethodPost, "/cluster/promote", api.ClusterPromoteCommand{ClusterPromotionReview: review.ClusterPromotionReview,
			ConfirmNode: spec.Name, RequestID: "promote-" + spec.Name, Reason: "independently observed primary loss"}, http.StatusOK, &receipt)
		if !receipt.Committed || receipt.Term.Epoch != epoch+1 || receipt.Term.PrimaryID != review.MemberID {
			t.Fatal("promotion lost the original term transition", spec.Name)
		}
		enrollment(c, func(info api.ClusterEnrollmentInfo) bool { return info.Mode == "primary" })
		return receipt
	}
	promotedB, promotedC := promote(apiB, b), promote(apiC, c)
	if promotedB.Term.Same(promotedC.Term) || promotedB.Term.Epoch != promotedC.Term.Epoch {
		t.Fatal("fixture did not produce two equal-epoch promotions")
	}
	apiB.request(t, http.MethodPost, "/objects/dnsrecordsets", recordSetBody("bonly", "192.0.2.50"), http.StatusCreated, nil)
	apiC.request(t, http.MethodPost, "/objects/dnsrecordsets", recordSetBody("conly", "192.0.2.51"), http.StatusCreated, nil)
	awaitDNS(nodeB, "bonly", "192.0.2.50", dns.RcodeSuccess)
	awaitDNS(nodeC, "conly", "192.0.2.51", dns.RcodeSuccess)

	// Heal. Only scheduled authenticated peer exchanges may detect the conflict.
	relayB.partition(false)
	relayC.partition(false)
	// A conflicted member refuses writers and closes its HTTP API (its serving
	// bundle is DNS-only), while valid local DNS keeps answering.
	for _, c := range []*staticProcessClient{apiB, apiC} {
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			return enrolledProcessStatus(t, c, http.MethodPost, "/objects/dnsrecordsets", recordSetBody("refused", "192.0.2.60")) == http.StatusServiceUnavailable
		}) {
			t.Fatal("healed conflicting primaries kept accepting writes")
		}
	}
	describeDNS := func(node *kudurrutest.Node, name string) string {
		var out []string
		for _, tcp := range []bool{false, true} {
			answer, err := exchange(node.Ports.DNS, name+".e2e.test.", dns.TypeA, tcp)
			if err != nil {
				out = append(out, err.Error())
				continue
			}
			out = append(out, dns.RcodeToString[answer.Rcode]+"/"+firstA(answer)+"/aa="+map[bool]string{true: "1", false: "0"}[answer.Authoritative])
		}
		return node.Name + ":" + strings.Join(out, ",")
	}
	if !checkDNS(nodeB, "bonly", "192.0.2.50", dns.RcodeSuccess) || !checkDNS(nodeC, "conly", "192.0.2.51", dns.RcodeSuccess) {
		ready := func(node *kudurrutest.Node) string {
			response, err := client.Get("http://" + node.Ports.Ops + "/healthz/ready")
			if err != nil {
				return err.Error()
			}
			defer func() { _ = response.Body.Close() }()
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
			return response.Status + " " + string(body)
		}
		t.Fatal("conflict interrupted valid local DNS", describeDNS(nodeB, "bonly"), describeDNS(nodeC, "conly"), ready(nodeB), ready(nodeC))
	}
	cli := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
		defer cancel()
		// #nosec G204 -- exact fixture-built binary and private temporary paths; no shell
		cmd := exec.CommandContext(ctx, bin, args...)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "KUDURRU_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, env...)
		return cmd.Output()
	}
	// The exclusive database lock refuses host-local review of a running member.
	if _, err := cli("cluster", "resolution", "--offline", "--data-dir", b.DataDir); err == nil {
		t.Fatal("offline review opened a running member's database")
	}
	review := func(spec kudurrutest.NodeSpec) (string, map[string]any) {
		t.Helper()
		raw, err := cli("cluster", "resolution", "--offline", "--data-dir", spec.DataDir)
		if err != nil {
			t.Fatal("offline resolution review failed", spec.Name, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, spec.Name+"-resolution.json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return path, out
	}
	var reviewTerm struct {
		Mode       string                 `json:"mode"`
		Node       string                 `json:"node"`
		Term       replication.Term       `json:"term"`
		Conflict   *replication.Term      `json:"conflict"`
		Installed  replication.Generation `json:"installed"`
		Resolvable bool                   `json:"resolvable"`
	}
	decode := func(v map[string]any) {
		t.Helper()
		data, _ := json.Marshal(v)
		reviewTerm.Conflict = nil
		if err := json.Unmarshal(data, &reviewTerm); err != nil {
			t.Fatal(err)
		}
	}
	stop(nodeC)
	reviewPathC, observedC := review(c)
	decode(observedC)
	if reviewTerm.Mode != "conflict" || !reviewTerm.Resolvable || reviewTerm.Conflict == nil || !reviewTerm.Conflict.Same(promotedB.Term) || !reviewTerm.Term.Same(promotedC.Term) {
		t.Fatal("C did not durably record B's competing term")
	}
	// A restarted conflicted member is never protected-ready; it serves only
	// its last-good DNS until resolution.
	nodeC = kudurrutest.Start(t, bin, c, kudurrutest.Options{Env: env})
	awaitDNS(nodeC, "conly", "192.0.2.51", dns.RcodeSuccess)
	stop(nodeB)
	reviewPathB, observedB := review(b)
	decode(observedB)
	if reviewTerm.Mode != "conflict" || !reviewTerm.Resolvable || reviewTerm.Node != b.Name || reviewTerm.Conflict == nil ||
		!reviewTerm.Conflict.Same(promotedC.Term) || !reviewTerm.Term.Same(promotedB.Term) {
		t.Fatal("B did not durably record C's competing term")
	}
	// Another member's review, a wrong confirmation or a missing reason refuse.
	for _, args := range [][]string{
		{"--review", reviewPathC, "--confirm-node", c.Name, "--request-id", "wrong-review", "--reason", "must refuse"},
		{"--review", reviewPathB, "--confirm-node", c.Name, "--request-id", "wrong-node", "--reason", "must refuse"},
		{"--review", reviewPathB, "--confirm-node", b.Name, "--request-id", "no-reason"},
	} {
		if _, err := cli(append([]string{"cluster", "resolve", "--offline", "--data-dir", b.DataDir}, args...)...); err == nil {
			t.Fatal("offline resolution accepted a mismatched decision", args[1])
		}
	}
	raw, err := cli("cluster", "resolve", "--offline", "--data-dir", b.DataDir, "--review", reviewPathB, "--confirm-node", b.Name,
		"--request-id", "keep-b", "--reason", "keep the branch with reviewed operator changes")
	if err != nil {
		t.Fatal("offline resolution failed", err)
	}
	var resolved struct {
		Committed bool                   `json:"committed"`
		MemberID  string                 `json:"memberID"`
		Term      replication.Term       `json:"term"`
		Kept      replication.Generation `json:"kept"`
	}
	if json.Unmarshal(raw, &resolved) != nil || !resolved.Committed || resolved.Term.Epoch != epoch+2 || resolved.Term.PrimaryID != promotedB.Term.PrimaryID ||
		resolved.Term.PrimaryName != b.Name || !resolved.Kept.Same(reviewTerm.Installed) {
		t.Fatal("resolution did not allocate a new epoch for the reviewed branch")
	}
	// The same decision is never replayed into success after commit.
	if _, err := cli("cluster", "resolve", "--offline", "--data-dir", b.DataDir, "--review", reviewPathB, "--confirm-node", b.Name,
		"--request-id", "keep-b", "--reason", "keep the branch with reviewed operator changes"); err == nil {
		t.Fatal("stale resolution review succeeded after commit")
	}
	nodeB = kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	enrollment(apiB, func(info api.ClusterEnrollmentInfo) bool { return info.Mode == "primary" })
	apiB.request(t, http.MethodPost, "/objects/dnsrecordsets", recordSetBody("afterresolve", "192.0.2.52"), http.StatusCreated, nil)

	// C adopts B's branch only through scheduled exchanges and its own pull.
	enrollment(apiC, func(info api.ClusterEnrollmentInfo) bool { return info.Mode == "follower" })
	awaitDNS(nodeC, "bonly", "192.0.2.50", dns.RcodeSuccess)
	awaitDNS(nodeC, "afterresolve", "192.0.2.52", dns.RcodeSuccess)
	awaitDNS(nodeC, "conly", "", dns.RcodeNameError)
	awaitDNS(nodeB, "conly", "", dns.RcodeNameError)
	// C's former branch is not replayed and its forwarded writes reach B.
	var forwarded api.BlockingCommandReceipt
	apiC.request(t, http.MethodPost, "/dns/blocking", map[string]any{"blocking": false}, http.StatusAccepted, &forwarded)
	if !forwarded.Committed || forwarded.Publication != "pending" {
		t.Fatal("resolved follower did not forward to the new primary")
	}
	blockingOff := func(body []byte) bool {
		var status api.BlockingState
		return json.Unmarshal(body, &status) == nil && !status.Blocking
	}
	enrolledProcessRead(t, apiB, "/dns/blocking", blockingOff)

	// The original primary returns with its older term and is fenced, then
	// installs B's branch. Neither A nor C regains writer authority.
	primary = kudurrutest.StartReady(t, bin, a, kudurrutest.Options{Env: env})
	enrollment(apiA, func(info api.ClusterEnrollmentInfo) bool { return info.Mode == "follower" })
	awaitDNS(primary, "bonly", "192.0.2.50", dns.RcodeSuccess)
	awaitDNS(primary, "afterresolve", "192.0.2.52", dns.RcodeSuccess)
	awaitDNS(primary, "conly", "", dns.RcodeNameError)
	enrolledProcessRead(t, apiA, "/dns/blocking", blockingOff)

	// Physical restarts retain the resolved authority and data.
	stop(nodeC)
	nodeC = kudurrutest.StartReady(t, bin, c, kudurrutest.Options{Env: env})
	stop(nodeB)
	nodeB = kudurrutest.StartReady(t, bin, b, kudurrutest.Options{Env: env})
	enrollment(apiB, func(info api.ClusterEnrollmentInfo) bool { return info.Mode == "primary" })
	enrollment(apiC, func(info api.ClusterEnrollmentInfo) bool { return info.Mode == "follower" })
	for _, node := range []*kudurrutest.Node{nodeB, nodeC} {
		awaitDNS(node, "afterresolve", "192.0.2.52", dns.RcodeSuccess)
		awaitDNS(node, "conly", "", dns.RcodeNameError)
	}
	if identity(a) != identities[0] || identity(b) != identities[1] || identity(c) != identities[2] {
		t.Fatal("conflict/resolution replaced private member identities")
	}
}
