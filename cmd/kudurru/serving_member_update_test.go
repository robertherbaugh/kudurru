package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
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

// memberUpdateHead returns the committed sequence of the example. zone head
// in the store's current image.
func memberUpdateHead(t *testing.T, s *servingRuntime) uint64 {
	t.Helper()
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range image.Payload.Objects {
		var header metav1.TypeMeta
		if json.Unmarshal(raw, &header) != nil {
			t.Fatal("undecodable object kind")
		}
		if header.Kind != zk.KindZoneState {
			continue
		}
		var head zk.ZoneState
		if json.Unmarshal(raw, &head) != nil {
			t.Fatal("undecodable zone head")
		}
		if head.Spec.Zone.Apex == "example." {
			return head.Spec.CommitSequence
		}
	}
	t.Fatal("example. has no zone head")
	return 0
}

// The primary admits a forwarded UPDATE only for the exact key identity of
// its own current keyring (D381): the follower verified the MAC, so this is
// the only authorization of the key on the forwarded branch. A repeated
// command ID returns the retained outcome without a second effect, and an
// expired signature deadline applies nothing.
func TestRootMemberUpdateKeyIdentityAndRetryActualHTTP(t *testing.T) {
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
	if after := memberUpdateHead(t, f.s); after != before {
		t.Fatal("a forwarded UPDATE with a foreign key identity changed the zone", before, after)
	}

	// The signature deadline the follower verified bounds the primary too.
	if rcode := answer(command("expired", exact, wire(insert("expired.example.")), time.Now().Add(-time.Second))); rcode != dns.RcodeServerFailure {
		t.Fatalf("an expired forwarded UPDATE answered %s, want SERVFAIL", dns.RcodeToString[rcode])
	}
	if after := memberUpdateHead(t, f.s); after != before {
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
	if committed != before+1 {
		t.Fatal("an accepted forwarded UPDATE did not commit exactly once", before, committed)
	}
	repeat := first
	repeat.IssuedAt = time.Now().UTC()
	repeat.Subject.IssuedAt = repeat.IssuedAt
	if rcode := answer(repeat); rcode != dns.RcodeSuccess {
		t.Fatalf("a repeated command ID answered %s, want the retained NOERROR", dns.RcodeToString[rcode])
	}
	if after := memberUpdateHead(t, f.s); after != committed {
		t.Fatal("a repeated command ID committed a second time", committed, after)
	}
	if rcode := answer(command("dedup-again", exact, once, later)); rcode != dns.RcodeYXDomain {
		t.Fatalf("a new command ID for a used name answered %s, want YXDOMAIN", dns.RcodeToString[rcode])
	}
	if after := memberUpdateHead(t, f.s); after != committed {
		t.Fatal("a failed prerequisite changed the zone", committed, after)
	}
}
