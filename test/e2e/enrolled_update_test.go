package e2e

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// enrolledUpdateKey is the TSIG key that both enrolled processes load from
// their own configuration file. The same name, algorithm and secret give the
// same key identity on both, so the primary admits what the follower verified.
const enrolledUpdateKey = "e2e-update."

// enrolledUpdateSecret writes one fresh base64 secret file and returns the
// secret and the tsigKeys configuration entry for it.
func enrolledUpdateSecret(t *testing.T, dir string) (string, []any) {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	secret := base64.StdEncoding.EncodeToString(raw)
	path := filepath.Join(dir, "update.key")
	if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return secret, []any{map[string]any{"name": enrolledUpdateKey, "algorithm": "hmac-sha256", "secretFile": path}}
}

// enrolledUpdateZone is a primary zone dyn.test. that grants the key A
// records under it, and nothing else.
func enrolledUpdateZone() []any {
	return []any{
		map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "dyn-test"},
			"spec": map[string]any{"zoneName": "dyn.test.", "type": "primary",
				"soa":         map[string]any{"mname": "ns1.dyn.test.", "rname": "hostmaster.dyn.test."},
				"nameservers": []any{"ns1.dyn.test."},
				"policy": map[string]any{"update": "allow", "updatePolicies": []any{
					map[string]any{"tsigKeyRef": enrolledUpdateKey, "name": "*.dyn.test.", "types": []any{"A"}}}}}},
		map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet", "metadata": map[string]any{"name": "dyn-seed", "namespace": "default"},
			"spec": map[string]any{"zoneRef": map[string]any{"name": "dyn-test"}, "endpoints": []any{
				map[string]any{"dnsName": "seed.dyn.test.", "recordType": "A", "targets": []any{"192.0.2.100"}}}}},
	}
}

func enrolledUpdateA(name, ip string) dns.RR {
	return &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP(ip)}
}

// enrolledSendUpdate sends one signed UPDATE and returns the answer. A nil
// error means the answer carried a TSIG record that verified with the key.
func enrolledSendUpdate(t *testing.T, addr, secret string, tcp bool, build func(*dns.Msg)) (*dns.Msg, error) {
	t.Helper()
	m := new(dns.Msg)
	m.SetUpdate("dyn.test.")
	build(m)
	m.SetTsig(enrolledUpdateKey, dns.HmacSHA256, 300, time.Now().Unix())
	c := &dns.Client{Timeout: 6 * time.Second, TsigSecret: map[string]string{enrolledUpdateKey: secret}}
	if tcp {
		c.Net = "tcp"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	answer, _, err := c.ExchangeContext(ctx, m, addr)
	if err == nil && (answer == nil || answer.IsTsig() == nil) {
		t.Fatal("UPDATE answer was not signed")
	}
	return answer, err
}

// enrolledExpectUpdate sends one signed UPDATE and requires its exact rcode.
func enrolledExpectUpdate(t *testing.T, node *kudurrutest.Node, secret string, tcp bool, rcode int, build func(*dns.Msg)) {
	t.Helper()
	var owner string
	sent := time.Now()
	answer, err := enrolledSendUpdate(t, node.Ports.DNS, secret, tcp, func(m *dns.Msg) {
		build(m)
		if len(m.Ns) != 0 {
			owner = m.Ns[len(m.Ns)-1].Header().Name
		}
	})
	if err != nil {
		t.Fatal("signed UPDATE exchange failed", node.Name, err, time.Since(sent))
	}
	if answer.Rcode != rcode || answer.Opcode != dns.OpcodeUpdate {
		t.Fatalf("UPDATE on %s sent at %s answered %s after %s, want %s (update of %s, tcp=%t)", node.Name, sent.UTC().Format(time.RFC3339Nano),
			dns.RcodeToString[answer.Rcode], time.Since(sent), dns.RcodeToString[rcode], owner, tcp)
	}
}

// enrolledEventuallyUpdate sends an idempotent signed UPDATE until it is
// answered NOERROR, as an RFC 2136 client retries SERVFAIL. Any other rcode
// fails at once; every SERVFAIL is logged.
func enrolledEventuallyUpdate(t *testing.T, node *kudurrutest.Node, secret string, build func(*dns.Msg)) {
	t.Helper()
	enrolledEventuallyRcode(t, node, secret, false, dns.RcodeSuccess, build)
}

// enrolledEventuallyRcode sends an idempotent signed UPDATE until it is
// answered rcode. SERVFAIL is retried and logged, as an RFC 2136 client sends
// the update again: a native UPDATE to the primary still gets it right after
// a commit, before anything is applied (the D399 recapture does not cover a
// signed UPDATE; docs/architecture/access.md). An UPDATE through a follower
// is sent once instead (enrolledExpectUpdate). Any other rcode than the
// wanted one fails at once.
func enrolledEventuallyRcode(t *testing.T, node *kudurrutest.Node, secret string, tcp bool, rcode int, build func(*dns.Msg)) {
	t.Helper()
	var owner string
	if !kudurrutest.Eventually(20*time.Second, 200*time.Millisecond, func() bool {
		answer, err := enrolledSendUpdate(t, node.Ports.DNS, secret, tcp, func(m *dns.Msg) {
			build(m)
			if len(m.Ns) != 0 {
				owner = m.Ns[len(m.Ns)-1].Header().Name
			}
		})
		switch {
		case err != nil:
			t.Logf("UPDATE on %s at %s: %v (update of %s, tcp=%t)", node.Name, time.Now().Format(time.RFC3339Nano), err, owner, tcp)
			return false
		case answer.Opcode != dns.OpcodeUpdate:
			t.Fatalf("UPDATE on %s answered opcode %d (update of %s, tcp=%t)", node.Name, answer.Opcode, owner, tcp)
		case answer.Rcode == rcode:
			return true
		case answer.Rcode == dns.RcodeServerFailure:
			t.Logf("UPDATE on %s at %s: SERVFAIL (update of %s, tcp=%t)", node.Name, time.Now().Format(time.RFC3339Nano), owner, tcp)
			return false
		default:
			t.Fatalf("UPDATE on %s answered %s, want %s (update of %s, tcp=%t)", node.Name, dns.RcodeToString[answer.Rcode], dns.RcodeToString[rcode], owner, tcp)
		}
		return false
	}) {
		t.Fatalf("UPDATE on %s never answered %s (update of %s, tcp=%t)", node.Name, dns.RcodeToString[rcode], owner, tcp)
	}
}

// enrolledDynAnswer asks one node for name over UDP and TCP and reports
// whether both answers are authoritative with this rcode and first A record.
func enrolledDynAnswer(node *kudurrutest.Node, name string, qtype uint16, ip string, rcode int) bool {
	for _, tcp := range []bool{false, true} {
		answer, err := exchange(node.Ports.DNS, name, qtype, tcp)
		if err != nil || answer.Rcode != rcode || firstA(answer) != ip || !answer.Authoritative {
			return false
		}
	}
	return true
}

func enrolledAwaitDyn(t *testing.T, node *kudurrutest.Node, name, ip string, rcode int) {
	t.Helper()
	if !kudurrutest.Eventually(20*time.Second, 50*time.Millisecond, func() bool { return enrolledDynAnswer(node, name, dns.TypeA, ip, rcode) }) {
		t.Fatal("dyn.test answer never converged", node.Name, name, ip, dns.RcodeToString[rcode])
	}
}

// enrolledForwardBound is the follower's four-second forward bound
// (memberForwardUpdateTimeout in cmd/kudurru), less the client side's
// scheduling slack. A forwarded UPDATE answered SERVFAIL this late was not
// sent, or not answered, within the bound: its outcome is unknown.
const enrolledForwardBound = 3900 * time.Millisecond

// enrolledFollowerUpdate sends one signed UPDATE through a follower and
// requires rcode. With atBound set, a SERVFAIL answered at the forward bound
// (enrolledForwardBound or later) is logged and the idempotent update is
// sent again, at most four times; a SERVFAIL before the bound, and any other
// rcode, fails at once. Without atBound the update is sent exactly once.
func enrolledFollowerUpdate(t *testing.T, follower *kudurrutest.Node, secret string, tcp bool, rcode int, atBound bool, build func(*dns.Msg)) {
	t.Helper()
	var owner string
	for attempt := 1; ; attempt++ {
		sent := time.Now()
		answer, err := enrolledSendUpdate(t, follower.Ports.DNS, secret, tcp, func(m *dns.Msg) {
			build(m)
			if len(m.Ns) != 0 {
				owner = m.Ns[len(m.Ns)-1].Header().Name
			}
		})
		elapsed := time.Since(sent)
		switch {
		case err != nil:
			t.Fatal("signed UPDATE exchange failed", follower.Name, err, elapsed)
		case answer.Opcode == dns.OpcodeUpdate && answer.Rcode == rcode:
			return
		case atBound && attempt < 5 && answer.Opcode == dns.OpcodeUpdate && answer.Rcode == dns.RcodeServerFailure && elapsed >= enrolledForwardBound:
			t.Logf("UPDATE on %s sent at %s answered SERVFAIL at the forward bound after %s (update of %s, tcp=%t); sending it again",
				follower.Name, sent.UTC().Format(time.RFC3339Nano), elapsed, owner, tcp)
		default:
			t.Fatalf("UPDATE on %s sent at %s answered %s after %s, want %s (update of %s, tcp=%t, attempt %d)", follower.Name, sent.UTC().Format(time.RFC3339Nano),
				dns.RcodeToString[answer.Rcode], elapsed, dns.RcodeToString[rcode], owner, tcp, attempt)
		}
	}
}

// enrolledFollowerUpdates sends signed UPDATEs to the follower (D381). The
// primary commits a granted one, and both nodes serve it; a failed
// prerequisite and an ungranted type are the primary's own answers and write
// nothing. Every answer is signed by the follower. A follower that installed
// a newer commit before it sent the update waits for its current publication
// and sends it then, so no SERVFAIL before the forward bound remains: any
// other rcode, REFUSED and SERVFAIL included, fails at once (HA-OPEN6).
//
// atBound accepts the documented SERVFAIL at the four-second bound and
// sends the update again (enrolledFollowerUpdate). The rejoined member needs
// it: on the newly promoted primary a forwarded commit took 1.1 to 3.5
// seconds to be published in the HA-OPEN6 diagnostic run (fix-diag01), and
// one that also lost a compare-and-swap round against the primary's own
// status write committed after the bound and answered SERVFAIL. In one
// later run (fix-e2e01) five sends of a burst through the rejoined member
// met the bound, one update twice. That is the publication latency
// PUBLISH-STARVATION (D405) works on, not a transient refusal. The first
// follower sends every update once.
func enrolledFollowerUpdates(t *testing.T, primary, follower *kudurrutest.Node, secret, name, ip string, atBound bool) {
	t.Helper()
	for i, tcp := range []bool{false, true} {
		owner := []string{name + "-udp.dyn.test.", name + "-tcp.dyn.test."}[i]
		enrolledFollowerUpdate(t, follower, secret, tcp, dns.RcodeSuccess, atBound, func(m *dns.Msg) { m.Insert([]dns.RR{enrolledUpdateA(owner, ip)}) })
		enrolledAwaitDyn(t, primary, owner, ip, dns.RcodeSuccess)
		enrolledAwaitDyn(t, follower, owner, ip, dns.RcodeSuccess)
	}
	// The prerequisite "this RRset exists" fails on the primary's data.
	prereq := name + "-prereq.dyn.test."
	enrolledFollowerUpdate(t, follower, secret, false, dns.RcodeNXRrset, atBound, func(m *dns.Msg) {
		m.RRsetUsed([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "absent.dyn.test.", Rrtype: dns.TypeA, Class: dns.ClassANY}}})
		m.Insert([]dns.RR{enrolledUpdateA(prereq, "192.0.2.199")})
	})
	// The key is granted A records only.
	ungranted := name + "-txt.dyn.test."
	enrolledFollowerUpdate(t, follower, secret, true, dns.RcodeRefused, atBound, func(m *dns.Msg) {
		m.Insert([]dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: ungranted, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{"refused"}}})
	})
	// A third update on the same follower proves that the refused ones changed
	// nothing the primary then serves.
	marker := name + "-after.dyn.test."
	enrolledFollowerUpdate(t, follower, secret, false, dns.RcodeSuccess, atBound, func(m *dns.Msg) { m.Insert([]dns.RR{enrolledUpdateA(marker, ip)}) })
	for _, node := range []*kudurrutest.Node{primary, follower} {
		enrolledAwaitDyn(t, node, marker, ip, dns.RcodeSuccess)
		enrolledAbsent(t, node, prereq, dns.TypeA)
		enrolledAbsent(t, node, ungranted, dns.TypeTXT)
	}
	enrolledFollowerBurst(t, primary, follower, secret, name, ip, atBound)
}

// enrolledFollowerBurst sends committing UPDATEs to the follower back to
// back, as a DHCP server does, without waiting for the follower to install
// each commit. Every one must answer NOERROR at once and be served by both
// nodes (D381). Each commit is followed by the primary's publication and its
// own status write, so the next forwarded command often reached the primary
// while its served publication was being replaced; the primary then refused
// it with SERVFAIL in its writer check before any admission, and most of a
// burst failed. The primary now waits for its current publication within the
// command's four-second bound and captures again only when the writer check
// was all that refused the update. atBound is enrolledFollowerUpdates'.
func enrolledFollowerBurst(t *testing.T, primary, follower *kudurrutest.Node, secret, name, ip string, atBound bool) {
	t.Helper()
	const burst = 8
	var owners []string
	started := time.Now()
	for i := range burst {
		owner := fmt.Sprintf("%s-burst%d.dyn.test.", name, i)
		enrolledFollowerUpdate(t, follower, secret, false, dns.RcodeSuccess, atBound, func(m *dns.Msg) { m.Insert([]dns.RR{enrolledUpdateA(owner, ip)}) })
		owners = append(owners, owner)
	}
	t.Logf("burst of %d UPDATEs on %s: all NOERROR in %s", burst, follower.Name, time.Since(started))
	for _, owner := range owners {
		enrolledAwaitDyn(t, primary, owner, ip, dns.RcodeSuccess)
		enrolledAwaitDyn(t, follower, owner, ip, dns.RcodeSuccess)
	}
}

// enrolledAbsent requires that node answers owner with an authoritative
// NXDOMAIN over UDP and over TCP. Every query must be answered: an ordinary
// answer is bound to the publication it was resolved from and is still sent
// across a publication swap (D378, D395), so a commit or republication right
// before cannot withhold it, and an unanswered query fails at once. An answer
// that differs is logged and asked again for at most two seconds, the bound
// for a zone that is briefly not served; an applied record answers NOERROR
// and never converges.
func enrolledAbsent(t *testing.T, node *kudurrutest.Node, owner string, qtype uint16) {
	t.Helper()
	if !kudurrutest.Eventually(2*time.Second, 50*time.Millisecond, func() bool {
		for _, tcp := range []bool{false, true} {
			answer, err := exchange(node.Ports.DNS, owner, qtype, tcp)
			switch {
			case err != nil:
				t.Fatalf("%s on %s (tcp=%t) was not answered at %s: %v", owner, node.Name, tcp, time.Now().Format(time.RFC3339Nano), err)
			case answer.Rcode != dns.RcodeNameError || len(answer.Answer) != 0 || !answer.Authoritative:
				t.Logf("%s on %s (tcp=%t) at %s: rcode=%s aa=%t answers=%d", owner, node.Name, tcp, time.Now().Format(time.RFC3339Nano), dns.RcodeToString[answer.Rcode], answer.Authoritative, len(answer.Answer))
				return false
			}
		}
		return true
	}) {
		t.Fatal("a refused write was applied", node.Name, owner)
	}
}

// enrolledUnreachedUpdate sends a signed UPDATE to a node that cannot reach
// its primary. It answers SERVFAIL, signed, at once: the update was not sent,
// or its outcome is unknown, and the node never retries it (D381).
func enrolledUnreachedUpdate(t *testing.T, node *kudurrutest.Node, secret, owner, ip string) {
	t.Helper()
	started := time.Now()
	enrolledExpectUpdate(t, node, secret, true, dns.RcodeServerFailure, func(m *dns.Msg) { m.Insert([]dns.RR{enrolledUpdateA(owner, ip)}) })
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatal("unreached UPDATE answer took longer than the forward bound", elapsed)
	}
}

// enrolledNeverApplied checks that a refused UPDATE is absent on every node
// after the cluster converged on a later change.
func enrolledNeverApplied(t *testing.T, owner string, nodes ...*kudurrutest.Node) {
	t.Helper()
	for _, node := range nodes {
		if !enrolledDynAnswer(node, owner, dns.TypeA, "", dns.RcodeNameError) {
			t.Fatal("an UPDATE that was answered SERVFAIL was applied or replayed", node.Name, owner)
		}
	}
}
