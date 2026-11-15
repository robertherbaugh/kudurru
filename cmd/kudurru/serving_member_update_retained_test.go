package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/serving"
)

// memberUpdateRerun decides which of a forwarded UPDATE's SERVFAILs the
// primary runs again with the same command ID after its next publication
// (D381). A replayed command ID whose admission refused is final: it is
// Deduplicated and not Published, which the HA-OPEN6 rule ran again, but
// its cause does not clear by waiting (HA-OPEN6 verification, new problem 1).
func TestMemberUpdateRerunOnlyWhenNothingNewWasDecided(t *testing.T) {
	admission := errors.New("admission refused")
	for _, c := range []struct {
		name    string
		stale   bool
		applied *pr.UpdateOutcome
		rcode   int
		want    bool
	}{
		{name: "stale publication before admission", stale: true, rcode: dns.RcodeServerFailure, want: true},
		{name: "every compare-and-swap lost, nothing committed", applied: &pr.UpdateOutcome{Rcode: dns.RcodeServerFailure, Err: pr.ErrConflict}, rcode: dns.RcodeServerFailure, want: true},
		{name: "committed, publication failed", applied: &pr.UpdateOutcome{Rcode: dns.RcodeServerFailure, Committed: true, PublishFailed: true, Err: serving.ErrSuperseded}, rcode: dns.RcodeServerFailure, want: true},
		{name: "retained outcome, its publication superseded", applied: &pr.UpdateOutcome{Rcode: dns.RcodeServerFailure, Committed: true, Deduplicated: true, PublishFailed: true, Err: serving.ErrSuperseded}, rcode: dns.RcodeServerFailure, want: true},
		{name: "recovery publication of an earlier commit failed", applied: &pr.UpdateOutcome{Rcode: dns.RcodeServerFailure, PublishFailed: true, Err: pr.ErrConflict}, rcode: dns.RcodeServerFailure, want: true},
		{name: "replayed command ID, admission refused", applied: &pr.UpdateOutcome{Rcode: dns.RcodeServerFailure, Committed: true, Deduplicated: true, Err: admission}, rcode: dns.RcodeServerFailure, want: false},
		{name: "a conflict after a commit", applied: &pr.UpdateOutcome{Rcode: dns.RcodeServerFailure, Committed: true, Err: pr.ErrConflict}, rcode: dns.RcodeServerFailure, want: false},
		{name: "zone not active", applied: &pr.UpdateOutcome{Rcode: dns.RcodeServerFailure, Err: pr.ErrUnavailable}, rcode: dns.RcodeServerFailure, want: false},
		{name: "refused before Apply", rcode: dns.RcodeServerFailure, want: false},
		{name: "a final answer", stale: true, applied: &pr.UpdateOutcome{Rcode: dns.RcodeSuccess, Committed: true, Published: true}, rcode: dns.RcodeSuccess, want: false},
		{name: "a prerequisite failed", applied: &pr.UpdateOutcome{Rcode: dns.RcodeNXRrset, PublishFailed: true}, rcode: dns.RcodeNXRrset, want: false},
	} {
		if got := memberUpdateRerun(c.stale, c.applied, c.rcode); got != c.want {
			t.Errorf("%s: rerun=%t, want %t", c.name, got, c.want)
		}
	}
	if !memberUpdateRetained(&pr.UpdateOutcome{Rcode: dns.RcodeSuccess, Committed: true, Published: true, Deduplicated: true}, dns.RcodeSuccess) {
		t.Error("a published retained outcome is not the retained answer")
	}
	for _, out := range []*pr.UpdateOutcome{nil, {Rcode: dns.RcodeSuccess, Committed: true, Published: true}, {Rcode: dns.RcodeRefused, Committed: true, Deduplicated: true, Err: pr.ErrInvalid}} {
		if memberUpdateRetained(out, dns.RcodeSuccess) {
			t.Errorf("%+v was taken for the retained answer", out)
		}
	}
}

// The command ID of a forwarded UPDATE is bound to the exact signed message:
// the same bytes give the same ID on any follower, a message signed again
// (another time signed or message ID) or sent over another transport gets
// another one (D381).
func TestMemberUpdateCommandIDIsTheSignedMessage(t *testing.T) {
	identity := dnsserver.TSIGIdentity{Key: "updates.", Algorithm: dns.HmacSHA256, CredentialVersion: "v1"}
	signed := func(id uint16, at int64) *dns.Msg {
		m := primaryUpdateMessage("example.", "same.example.", false)
		m.Id = id
		m.SetTsig("updates.", dns.HmacSHA256, 300, at)
		m.Extra[len(m.Extra)-1].(*dns.TSIG).MAC = "00ff"
		return m
	}
	now := time.Now().Unix()
	first, err := memberUpdateCommandID(identity, "udp", signed(7, now))
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := memberUpdateCommandID(identity, "udp", signed(7, now)); again != first {
		t.Fatal("the same signed message got another command ID")
	}
	for name, other := range map[string]func() (string, error){
		"time signed": func() (string, error) { return memberUpdateCommandID(identity, "udp", signed(7, now+1)) },
		"message ID":  func() (string, error) { return memberUpdateCommandID(identity, "udp", signed(8, now)) },
		"transport":   func() (string, error) { return memberUpdateCommandID(identity, "tcp", signed(7, now)) },
		"credential": func() (string, error) {
			changed := identity
			changed.CredentialVersion = "v2"
			return memberUpdateCommandID(changed, "udp", signed(7, now))
		},
	} {
		if id, err := other(); err != nil || id == first {
			t.Errorf("another %s gave the same command ID (err=%v)", name, err)
		}
	}
	if _, err := memberUpdateCommandID(identity, "udp", primaryUpdateMessage("example.", "same.example.", false)); err == nil {
		t.Error("an unsigned message got a command ID")
	}
}

// memberUpdateSendWire sends one already signed UPDATE, byte for byte, to
// the follower's UDP listener and returns its answer.
func memberUpdateSendWire(t *testing.T, dnsf *primaryNetworkFixture, wire []byte, timeout time.Duration) *dns.Msg {
	t.Helper()
	conn, err := net.Dial("udp", dnsf.listeners.Addr(dnsserver.ProtoUDP).String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, dns.MaxMsgSize)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal("UPDATE answer", err)
	}
	answer := new(dns.Msg)
	if err := answer.Unpack(buf[:n]); err != nil {
		t.Fatal(err)
	}
	if answer.IsTsig() == nil {
		t.Fatal("UPDATE answer was not signed")
	}
	return answer
}

// A signed UPDATE through a follower whose answer the primary committed but
// could not deliver within the follower's bound is answered SERVFAIL: its
// outcome is unknown (D381). The client then sends the same signed message
// again, as an RFC 2136 client does after SERVFAIL or a timeout. The repeat
// carries the same command ID and is answered from the retained outcome
// with the original NOERROR: the update requires its name to be unused, so
// running it again would answer YXDOMAIN. A message signed again is a new
// request and is decided again.
func TestRootMemberFollowerUpdateRepeatIsAnsweredFromTheRetainedOutcome(t *testing.T) {
	const bound = 3 * time.Second
	f, dnsf := memberUpdateFollowerZone(t, bound)
	var held atomic.Bool
	held.Store(true)
	f.primary.s.memberUpdateAnswered = func(ctx context.Context) {
		if held.CompareAndSwap(true, false) {
			<-ctx.Done() // the answer is not delivered within the bound
		}
	}
	const name = "retained.example."
	build := func() *dns.Msg {
		m := primaryUpdateMessage("example.", name, false)
		m.NameNotUsed([]dns.RR{&dns.ANY{Hdr: dns.RR_Header{Name: name}}})
		m.SetTsig("updates.", dns.HmacSHA256, 300, time.Now().Unix())
		return m
	}
	wire, _, err := dns.TsigGenerateWithProvider(build(), dnsserver.TSIGProvider(dnsf.keys), "", false)
	if err != nil {
		t.Fatal(err)
	}
	sent := time.Now()
	if rcode := memberUpdateRcode(memberUpdateSendWire(t, dnsf, wire, 10*time.Second)); rcode != dns.RcodeServerFailure {
		t.Fatalf("UPDATE whose answer was not delivered answered %s, want SERVFAIL", dns.RcodeToString[rcode])
	}
	if elapsed := time.Since(sent); elapsed < bound-100*time.Millisecond {
		t.Fatal("SERVFAIL before the forward bound", elapsed)
	}
	if held.Load() {
		t.Fatal("the primary never decided the held UPDATE")
	}
	// The primary committed it.
	query := new(dns.Msg)
	query.SetQuestion(name, dns.TypeA)
	memberUpdatePull(t, f)
	answer, err := dnsf.exchange(query, false)
	if err != nil || answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
		t.Fatalf("the UPDATE answered SERVFAIL was not committed: rcode=%d err=%v", memberUpdateRcode(answer), err)
	}

	// The same signed message again: the retained NOERROR, nothing applied.
	if rcode := memberUpdateRcode(memberUpdateSendWire(t, dnsf, wire, 10*time.Second)); rcode != dns.RcodeSuccess {
		t.Fatalf("the same signed UPDATE sent again answered %s, want the retained NOERROR", dns.RcodeToString[rcode])
	}
	// Signed again, it is a new request and fails its prerequisite.
	again, err := dnsf.exchange(build(), false)
	if err != nil || memberUpdateRcode(again) != dns.RcodeYXDomain {
		t.Fatalf("the UPDATE signed again answered rcode=%d err=%v, want YXDOMAIN", memberUpdateRcode(again), err)
	}
}

// A forwarded UPDATE whose command ID the zone's receipt ledger already
// holds, and whose admission then refuses, is answered SERVFAIL at once
// (D381). Its cause does not clear by waiting for another publication, so
// the primary does not run it again. The HA-OPEN6 rule that 9397a519 added
// (Deduplicated && !Published) ran every such answer again and held it until
// the command's bound ended without an answer; it was written only for the
// retained outcome whose publication was superseded (memberUpdateRerun).
// Here the zone stops allowing updates after the first one committed, so the
// receipt is found and the admission of its retained answer refuses.
func TestRootMemberUpdateReplayRefusedByAdmissionIsAnsweredAtOnce(t *testing.T) {
	h, identity := newMemberUpdateHarness(t)
	f := h.f
	h.client.Timeout = 25 * time.Second
	m := new(dns.Msg)
	m.SetUpdate("example.")
	const name = "replay-refused.example."
	m.NameNotUsed([]dns.RR{&dns.ANY{Hdr: dns.RR_Header{Name: name}}})
	m.Insert([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 9}}})
	wire, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	command := func() clusterreplica.Command {
		payload, err := json.Marshal(memberUpdate{Message: wire, Algorithm: identity.Algorithm, Transport: "udp", SignatureDeadline: time.Now().Add(time.Minute).UTC()})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		return clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: "replay-refused", ExpectedTerm: f.s.opts.Store.Authority().Term,
			Hops: 1, IssuedAt: now, NotAfter: now.Add(20 * time.Second), Operation: memberDNSUpdate,
			Subject: clusterreplica.SubjectRef{Kind: clusterreplica.SubjectTSIGKey, ID: identity.Key, Provider: "tsig", CredentialKind: clusterreplica.CredentialTSIG, CredentialID: identity.CredentialVersion, IssuedAt: now},
			Payload: payload}
	}
	answer := func(c clusterreplica.Command) (int, time.Duration) {
		t.Helper()
		sent := time.Now()
		status, out := h.send(c)
		elapsed := time.Since(sent)
		if status != http.StatusOK || out.Status != http.StatusOK {
			t.Fatalf("forwarded UPDATE was not answered after %s: status=%d outcome=%d err=%v", elapsed, status, out.Status, *h.sendErr)
		}
		var result memberUpdateResult
		if err := json.Unmarshal(out.Body, &result); err != nil {
			t.Fatal(err)
		}
		return result.Rcode, elapsed
	}
	if rcode, _ := answer(command()); rcode != dns.RcodeSuccess {
		t.Fatalf("forwarded UPDATE answered %s, want NOERROR", dns.RcodeToString[rcode])
	}
	committed := memberUpdateHead(t, f.s)

	zone, err := json.Marshal(map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "home"},
		"spec": map[string]any{"zoneName": "example.", "type": "primary", "nameservers": []any{"ns.example."},
			"policy": map[string]any{"notify": "none", "update": "deny"}}})
	if err != nil {
		t.Fatal(err)
	}
	if status, out := h.send(h.command("deny-home", memberObjectWrite, api.ForwardedObject{Operation: api.ForwardedWrite, Kind: "DNSZone", Name: "home", Document: zone})); status != http.StatusOK || out.Status != http.StatusOK {
		t.Fatal("zone policy change refused", status, out.Status, string(out.Body))
	}
	awaitServingRuntime(t, f.s, func() bool {
		head := memberUpdateHead(t, f.s)
		return head.CommitSequence > committed.CommitSequence && f.s.primary.driver.Status().Ready
	})
	settled := memberUpdateHead(t, f.s)

	rcode, elapsed := answer(command())
	if rcode != dns.RcodeServerFailure {
		t.Fatalf("a replayed command ID whose admission refused answered %s, want SERVFAIL", dns.RcodeToString[rcode])
	}
	// The command's bound is 20 seconds; the answer must not wait for it.
	if elapsed > 5*time.Second {
		t.Fatalf("a replayed command ID whose admission refused was answered after %s: it was run again until its bound", elapsed)
	}
	if after := memberUpdateHead(t, f.s); !memberUpdateSameData(after, settled) {
		t.Fatal("a refused replay changed the zone", settled, after)
	}
}
