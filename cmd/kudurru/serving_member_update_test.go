package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/miekg/dns"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
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
