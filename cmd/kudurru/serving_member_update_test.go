package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The mounted member listener runs a forwarded UPDATE through the primary's
// own protocol adapter and acknowledges its rcode inside the command output
// scope. Malformed or partial forwards are aborted before any execution.
func TestRootMemberUpdateCommandActualHTTP(t *testing.T) {
	h := newRootMemberHarness(t)
	f := h.f
	message := func(apex string, signed bool) []byte {
		m := new(dns.Msg)
		m.SetUpdate(apex)
		m.Insert([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "host." + apex, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 7}}})
		if signed {
			m.SetTsig("key.", dns.HmacSHA256, 300, time.Now().Unix())
		}
		wire, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	command := func(id string, in any, mutate ...func(*clusterreplica.Command)) clusterreplica.Command {
		payload, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		c := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: id, ExpectedTerm: f.s.opts.Store.Authority().Term,
			Hops: 1, IssuedAt: now, NotAfter: now.Add(20 * time.Second), Operation: memberDNSUpdate,
			Subject: clusterreplica.SubjectRef{Kind: clusterreplica.SubjectTSIGKey, ID: "key.", Provider: "tsig", CredentialKind: clusterreplica.CredentialTSIG, CredentialID: "version", IssuedAt: now},
			Payload: payload}
		for _, m := range mutate {
			m(&c)
		}
		return c
	}
	valid := memberUpdate{Message: message("missing.test.", false), Algorithm: dns.HmacSHA256, Transport: "udp", SignatureDeadline: time.Now().Add(time.Minute).UTC()}
	status, out := h.send(command("update-missing-zone", valid))
	if status != http.StatusOK || out.Status != http.StatusOK {
		t.Fatalf("forwarded UPDATE was not answered: status=%d outcome=%d err=%v", status, out.Status, *h.sendErr)
	}
	var result memberUpdateResult
	if err := json.Unmarshal(out.Body, &result); err != nil || result.Rcode != dns.RcodeNotAuth {
		t.Fatalf("forwarded UPDATE for a zone the primary does not serve answered %s, want NOTAUTH", dns.RcodeToString[result.Rcode])
	}
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	signed := valid
	signed.Message = message("missing.test.", true)
	unknownTransport := valid
	unknownTransport.Transport = "doh"
	for name, c := range map[string]clusterreplica.Command{
		"signed message":     command("update-signed", signed),
		"unknown transport":  command("update-transport", unknownTransport),
		"unknown field":      command("update-field", map[string]any{"message": valid.Message, "algorithm": valid.Algorithm, "transport": "udp", "signatureDeadline": valid.SignatureDeadline, "grants": true}),
		"name not canonical": command("update-name", valid, func(c *clusterreplica.Command) { c.Subject.ID = "Key." }),
		"other provider":     command("update-provider", valid, func(c *clusterreplica.Command) { c.Subject.Provider = "local" }),
	} {
		status, _ := h.send(c)
		if status == http.StatusOK {
			t.Fatal("malformed forwarded UPDATE was acknowledged", name)
		}
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !after.Generation().Same(before.Generation()) {
		t.Fatal("a refused forwarded UPDATE changed the store")
	}
}

// memberUpdateHead returns the head of the example. zone of the current
// DNSZone home in the store's current image. The head's CommitSequence also
// advances on a head commit that changes no data (its root digest binds
// expiry, configuration and receipts too, and a NOTIFY rearm is one), so a
// test that asks whether an UPDATE committed compares the content hash and
// the serial, which only a data commit changes (internal/zonestate/codec.go
// Prepare). The previous CommitSequence comparison failed now and then with
// "a repeated command ID committed a second time 2 3" while the repeat
// answered the retained NOERROR, which a second run cannot answer.
func memberUpdateHead(t *testing.T, s *servingRuntime) zk.ZoneStateSpec {
	t.Helper()
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var heads []zk.ZoneStateSpec
	uid := ""
	for _, raw := range image.Payload.Objects {
		var header struct {
			metav1.TypeMeta   `json:",inline"`
			metav1.ObjectMeta `json:"metadata"`
		}
		if json.Unmarshal(raw, &header) != nil {
			t.Fatal("undecodable object kind")
		}
		switch header.Kind {
		case "DNSZone":
			if header.Name == "home" {
				uid = string(header.UID)
			}
		case zk.KindZoneState:
			var head zk.ZoneState
			if json.Unmarshal(raw, &head) != nil {
				t.Fatal("undecodable zone head")
			}
			if head.Spec.Zone.Apex == "example." {
				heads = append(heads, head.Spec)
			}
		}
	}
	for _, head := range heads {
		if uid != "" && head.Zone.UID == uid {
			return head
		}
	}
	t.Fatal("the DNSZone home has no zone head", uid, len(heads))
	return zk.ZoneStateSpec{}
}

// memberUpdateSameData reports whether two heads hold the same zone data.
func memberUpdateSameData(a, b zk.ZoneStateSpec) bool {
	return a.ContentHash == b.ContentHash && a.Serial == b.Serial
}

// newMemberUpdateHarness creates the served primary zone example. through
// the forwarded object path on the mounted member listener, so that a
// forwarded UPDATE reaches the primary's key admission, and returns the
// fixture keyring's identity of the granted key updates.
func newMemberUpdateHarness(t *testing.T) (*rootMemberHarness, dnsserver.TSIGIdentity) {
	t.Helper()
	h := newRootMemberHarness(t)
	// The command fixture removes the runtime fixture's primary zone. Create
	// it again through the forwarded object path so that a forwarded UPDATE
	// reaches the primary's key admission.
	zone, err := json.Marshal(map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "home"},
		"spec": map[string]any{"zoneName": "example.", "type": "primary", "nameservers": []any{"ns.example."},
			"policy": map[string]any{"notify": "none", "update": "allow", "updatePolicies": []any{map[string]any{"tsigKeyRef": "updates", "name": "*.example.", "types": []any{"A"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var created api.ForwardedObjectResult
	if status, out := h.send(h.command("create-home", memberObjectWrite, api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "DNSZone", Name: "home", Document: zone})); status != http.StatusOK || out.Status != http.StatusOK ||
		json.Unmarshal(out.Body, &created) != nil || created.Result != api.ResultCreated {
		t.Fatal("primary zone create refused", status, out.Status, string(out.Body))
	}
	f := h.f
	awaitServingRuntime(t, f.s, func() bool {
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		owner, ok := value.(*servingbuild.RuntimeOwner)
		return ok && f.s.primary.driver.Status().Ready && owner.Primary().Admitted("example.", time.Now())
	})
	identity, ok := f.s.opts.Keyrings.Capture().Keys().Identity("updates.")
	if !ok || identity.CredentialVersion == "" || identity.Algorithm != dns.HmacSHA256 {
		t.Fatal("fixture keyring has no identity for updates.")
	}
	return h, identity
}

// The primary admits a forwarded UPDATE only for the exact key identity of
// its own current keyring (D381): the follower verified the MAC, so this is
// the only authorization of the key on the forwarded branch. A repeated
// command ID returns the retained outcome without a second effect, and an
// expired signature deadline applies nothing.
func TestRootMemberUpdateKeyIdentityAndRetryActualHTTP(t *testing.T) {
	h, identity := newMemberUpdateHarness(t)
	f := h.f
	wire := func(build func(*dns.Msg)) []byte {
		m := new(dns.Msg)
		m.SetUpdate("example.")
		build(m)
		out, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	insert := func(name string) func(*dns.Msg) {
		return func(m *dns.Msg) {
			m.Insert([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 8}}})
		}
	}
	type claim struct{ key, algorithm, version string }
	exact := claim{key: identity.Key, algorithm: identity.Algorithm, version: identity.CredentialVersion}
	command := func(id string, c claim, message []byte, deadline time.Time) clusterreplica.Command {
		payload, err := json.Marshal(memberUpdate{Message: message, Algorithm: c.algorithm, Transport: "udp", SignatureDeadline: deadline.UTC()})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		return clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: id, ExpectedTerm: f.s.opts.Store.Authority().Term,
			Hops: 1, IssuedAt: now, NotAfter: now.Add(20 * time.Second), Operation: memberDNSUpdate,
			Subject: clusterreplica.SubjectRef{Kind: clusterreplica.SubjectTSIGKey, ID: c.key, Provider: "tsig", CredentialKind: clusterreplica.CredentialTSIG, CredentialID: c.version, IssuedAt: now},
			Payload: payload}
	}
	answer := func(c clusterreplica.Command) int {
		t.Helper()
		status, out := h.send(c)
		if status != http.StatusOK || out.Status != http.StatusOK {
			t.Fatalf("forwarded UPDATE %s was not answered: status=%d outcome=%d err=%v", c.ID, status, out.Status, *h.sendErr)
		}
		var result memberUpdateResult
		if err := json.Unmarshal(out.Body, &result); err != nil {
			t.Fatal(err)
		}
		return result.Rcode
	}
	later := time.Now().Add(time.Minute)

	before := memberUpdateHead(t, f.s)
	for name, c := range map[string]claim{
		"other credential version": {key: exact.key, algorithm: exact.algorithm, version: "not-" + exact.version},
		"unknown key name":         {key: "unknown.", algorithm: exact.algorithm, version: exact.version},
		"other algorithm":          {key: exact.key, algorithm: dns.HmacSHA512, version: exact.version},
	} {
		if rcode := answer(command("refused-"+strings.ReplaceAll(name, " ", "-"), c, wire(insert("refused.example.")), later)); rcode != dns.RcodeRefused {
			t.Fatalf("%s: forwarded UPDATE answered %s, want REFUSED", name, dns.RcodeToString[rcode])
		}
	}
	if after := memberUpdateHead(t, f.s); !memberUpdateSameData(after, before) {
		t.Fatal("a forwarded UPDATE with a foreign key identity changed the zone", before, after)
	}

	// The signature deadline the follower verified bounds the primary too.
	if rcode := answer(command("expired", exact, wire(insert("expired.example.")), time.Now().Add(-time.Second))); rcode != dns.RcodeServerFailure {
		t.Fatalf("an expired forwarded UPDATE answered %s, want SERVFAIL", dns.RcodeToString[rcode])
	}
	if after := memberUpdateHead(t, f.s); !memberUpdateSameData(after, before) {
		t.Fatal("an expired forwarded UPDATE changed the zone", before, after)
	}

	// "dedup.example. is not in use" holds only once, so a second run of the
	// same request would answer YXDOMAIN. The same command ID returns the
	// retained NOERROR without a second commit; a new ID runs it again.
	once := wire(func(m *dns.Msg) {
		m.NameNotUsed([]dns.RR{&dns.ANY{Hdr: dns.RR_Header{Name: "dedup.example."}}})
		insert("dedup.example.")(m)
	})
	first := command("dedup-once", exact, once, later)
	if rcode := answer(first); rcode != dns.RcodeSuccess {
		t.Fatalf("forwarded UPDATE with the exact key identity answered %s, want NOERROR", dns.RcodeToString[rcode])
	}
	committed := memberUpdateHead(t, f.s)
	if memberUpdateSameData(committed, before) || committed.CommitSequence <= before.CommitSequence {
		t.Fatal("an accepted forwarded UPDATE did not commit", before, committed)
	}
	repeat := first
	repeat.IssuedAt = time.Now().UTC()
	repeat.Subject.IssuedAt = repeat.IssuedAt
	if rcode := answer(repeat); rcode != dns.RcodeSuccess {
		t.Fatalf("a repeated command ID answered %s, want the retained NOERROR", dns.RcodeToString[rcode])
	}
	if after := memberUpdateHead(t, f.s); !memberUpdateSameData(after, committed) {
		t.Fatal("a repeated command ID committed a second time", committed, after)
	}
	if rcode := answer(command("dedup-again", exact, once, later)); rcode != dns.RcodeYXDomain {
		t.Fatalf("a new command ID for a used name answered %s, want YXDOMAIN", dns.RcodeToString[rcode])
	}
	if after := memberUpdateHead(t, f.s); !memberUpdateSameData(after, committed) {
		t.Fatal("a failed prerequisite changed the zone", committed, after)
	}
}

// A burst of forwarded UPDATEs, each sent as soon as the previous one was
// answered as a DHCP server does, commits every one (D381). A commit is
// followed by its publication and often by the node's own writes and their
// publications, so the next command often arrives while the primary's
// served publication is about to be replaced. Before the fix such a command
// answered SERVFAIL from captureUpdate's writer check, before any admission.
func TestRootMemberUpdateBurstActualHTTP(t *testing.T) {
	h, identity := newMemberUpdateHarness(t)
	f := h.f
	// A command waits for the primary's publications within its own 20-second
	// bound (below). The harness client's six seconds gave up first under the
	// race detector on a shared machine ("Client.Timeout exceeded while
	// awaiting headers", HA-OPEN5 fix-race02) and reported a command that was
	// still running as unanswered.
	h.client.Timeout = 25 * time.Second
	// command inserts an A record for owner and requires that owner is not
	// in use, so it answers YXDOMAIN once the record exists.
	command := func(id, owner string) clusterreplica.Command {
		t.Helper()
		m := new(dns.Msg)
		m.SetUpdate("example.")
		m.NameNotUsed([]dns.RR{&dns.ANY{Hdr: dns.RR_Header{Name: owner}}})
		m.Insert([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: owner, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 9}}})
		wire, err := m.Pack()
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(memberUpdate{Message: wire, Algorithm: identity.Algorithm, Transport: "udp", SignatureDeadline: time.Now().Add(time.Minute).UTC()})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		// A follower bounds its exchange at four seconds. A publication under
		// the race detector on a loaded machine can take longer, so this is
		// the bound the other member tests use; the process test keeps the
		// follower's own.
		return clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: id, ExpectedTerm: f.s.opts.Store.Authority().Term,
			Hops: 1, IssuedAt: now, NotAfter: now.Add(20 * time.Second), Operation: memberDNSUpdate,
			Subject: clusterreplica.SubjectRef{Kind: clusterreplica.SubjectTSIGKey, ID: identity.Key, Provider: "tsig", CredentialKind: clusterreplica.CredentialTSIG, CredentialID: identity.CredentialVersion, IssuedAt: now},
			Payload: payload}
	}
	answer := func(c clusterreplica.Command) (int, error) {
		status, out := h.send(c)
		if status != http.StatusOK || out.Status != http.StatusOK {
			return 0, fmt.Errorf("forwarded UPDATE %s was not answered: status=%d outcome=%d err=%v", c.ID, status, out.Status, *h.sendErr)
		}
		var result memberUpdateResult
		if err := json.Unmarshal(out.Body, &result); err != nil {
			return 0, err
		}
		return result.Rcode, nil
	}
	var owners []string
	for i := range 12 {
		owner := fmt.Sprintf("burst%d.example.", i)
		rcode, err := answer(command(fmt.Sprintf("burst-%d", i), owner))
		if err != nil || rcode != dns.RcodeSuccess {
			t.Fatalf("burst UPDATE %d answered %s, want NOERROR (%v)", i+1, dns.RcodeToString[rcode], err)
		}
		owners = append(owners, owner)
	}
	// A new command that requires the name to be unused proves each record
	// was committed.
	for _, owner := range owners {
		if rcode, err := answer(command("present-"+strings.TrimSuffix(owner, "."), owner)); err != nil || rcode != dns.RcodeYXDomain {
			t.Errorf("%s after its UPDATE: a name-not-in-use prerequisite answered %s, want YXDOMAIN (%v)", owner, dns.RcodeToString[rcode], err)
		}
	}
}

// A forwarded UPDATE for a zone that the primary is retiring is answered at
// once and applies nothing: a lifecycle never returns from retirement, so
// unlike a zone whose lifecycle is being realigned it is not waited for until
// the command's bound (captureUpdate, HA-OPEN5 c8c9e3b1).
func TestRootMemberUpdateForRetiringZoneIsAnsweredAtOnce(t *testing.T) {
	h, identity := newMemberUpdateHarness(t)
	f := h.f
	h.client.Timeout = 25 * time.Second
	status, out := h.send(h.command("delete-home", memberObjectDelete, api.ForwardedObject{Operation: api.ForwardedDelete, Kind: "DNSZone", Name: "home"}))
	if status != http.StatusOK || out.Status != http.StatusOK {
		t.Fatal("primary zone delete refused", status, out.Status, string(out.Body))
	}
	// Wait until the primary's current publication shows the zone retiring
	// or retired.
	awaitServingRuntime(t, f.s, func() bool {
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		owner, ok := value.(*servingbuild.RuntimeOwner)
		if !ok || owner.Primary() == nil {
			return false
		}
		member, found := owner.Primary().Cut(time.Now()).Member("example.")
		return found && (member.Phase == zk.PhaseRetiring || member.Phase == zk.PhaseRetired)
	})
	m := new(dns.Msg)
	m.SetUpdate("example.")
	m.Insert([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "late.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 10}}})
	wire, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(memberUpdate{Message: wire, Algorithm: identity.Algorithm, Transport: "udp", SignatureDeadline: time.Now().Add(time.Minute).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const bound = 20 * time.Second
	command := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: "retiring", ExpectedTerm: f.s.opts.Store.Authority().Term,
		Hops: 1, IssuedAt: now, NotAfter: now.Add(bound), Operation: memberDNSUpdate,
		Subject: clusterreplica.SubjectRef{Kind: clusterreplica.SubjectTSIGKey, ID: identity.Key, Provider: "tsig", CredentialKind: clusterreplica.CredentialTSIG, CredentialID: identity.CredentialVersion, IssuedAt: now},
		Payload: payload}
	started := time.Now()
	status, out = h.send(command)
	elapsed := time.Since(started)
	if status != http.StatusOK || out.Status != http.StatusOK {
		t.Fatalf("forwarded UPDATE for a retiring zone was not answered after %s: status=%d outcome=%d err=%v", elapsed, status, out.Status, *h.sendErr)
	}
	var result memberUpdateResult
	if err := json.Unmarshal(out.Body, &result); err != nil {
		t.Fatal(err)
	}
	// SERVFAIL while it is retiring or retired in the cut, NOTAUTH once it
	// left the cut; never NOERROR, and never after waiting out the bound.
	if result.Rcode != dns.RcodeServerFailure && result.Rcode != dns.RcodeNotAuth {
		t.Fatalf("forwarded UPDATE for a retiring zone answered %s, want SERVFAIL or NOTAUTH", dns.RcodeToString[result.Rcode])
	}
	if elapsed > bound/2 {
		t.Fatalf("forwarded UPDATE for a retiring zone waited %s of its %s bound", elapsed, bound)
	}
	t.Logf("forwarded UPDATE for a retiring zone answered %s after %s", dns.RcodeToString[result.Rcode], elapsed)
}
