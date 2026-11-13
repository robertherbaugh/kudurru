package trustanchors_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
)

// F28-RFC-2 (D398): RFC 5011 rollover timing proven on wires that a real
// authority signed and served. A signer (BIND's dnssec-signzone or Knot's
// online signing) produces five stages of a root zone with explicit
// signature windows; the authority serves each stage and the test keeps the
// exact TCP reply to the managed acquisition request (RD clear, EDNS 1232
// with DO). The pure state machine then consumes those bytes at synthetic
// samples on a 30-day-plus timeline, which only an in-package clock can do
// (the production constants are not accelerated, D297):
//
//	s1  {K1, ZSK}             DNSKEY set signed by K1
//	s2  {K1, K2, ZSK}         K2 published, signed by K1
//	s3  {K1, K2, ZSK}         signed by K2 (the rollover)
//	s4  {K2, ZSK}             K1 removed, signed by K2
//	s5  {K1+REVOKE, K2, ZSK}  K1 revoked, signed by K1+REVOKE and K2
//
// Committed captures under testdata/rfc5011/<signer>/ replay real bytes
// where the tools are missing; provenance.json records how they were made
// and pins each file by SHA-256. With the tools present the BIND capture is
// also generated afresh (rfc5011_capture_bind_test.go), and Knot's with
// KUDURRU_KNOT=1 (rfc5011_capture_knot_test.go).

// rolloverStages are the stage file names, in order.
var rolloverStages = []string{"s1", "s2", "s3", "s4", "s5"}

// rolloverTTL is the DNSKEY TTL of every capture: the real root's two days.
// The add hold-down is max(30 days, TTL) = 30 days.
const rolloverTTL = 172800

// rolloverProvenance describes one capture.
type rolloverProvenance struct {
	Signer    string `json:"signer"`
	Authority string `json:"authority"`
	// T0 is the synthetic start of the timeline. Every signature is valid
	// from at most T0 minus one day to at least T0 plus 60 days.
	T0       time.Time         `json:"t0"`
	Captured time.Time         `json:"captured"`
	SeedDS   string            `json:"seedDS"`
	Steps    []string          `json:"steps"`
	Files    map[string]string `json:"files"`
}

type rolloverCapture struct {
	provenance rolloverProvenance
	wires      map[string][]byte
}

// rolloverRequest is the managed acquisition request
// (cmd/kudurru/serving_anchor_acquire.go, captureAnchorWirePlan).
func rolloverRequest() *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	q.RecursionDesired = false
	q.SetEdns0(1232, true)
	return q
}

// tryCaptureTCP sends the managed acquisition request over TCP and returns
// the exact reply frame.
func tryCaptureTCP(addr string) ([]byte, error) {
	packet, err := rolloverRequest().Pack()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	frame := make([]byte, 2+len(packet))
	binary.BigEndian.PutUint16(frame, uint16(len(packet))) //nolint:gosec // one question, far below 64 KiB
	copy(frame[2:], packet)
	if _, err := conn.Write(frame); err != nil {
		return nil, err
	}
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, err
	}
	raw := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeCapture stores a capture in dir (the KUDURRU_RFC5011_CAPTURE_DIR
// refresh path for the committed testdata).
func writeCapture(t *testing.T, dir string, c rolloverCapture) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // the operator's KUDURRU_RFC5011_CAPTURE_DIR
		t.Fatal(err)
	}
	c.provenance.Files = map[string]string{}
	for _, stage := range rolloverStages {
		c.provenance.Files[stage+".wire"] = sha256Hex(c.wires[stage])
		if err := os.WriteFile(filepath.Join(dir, stage+".wire"), c.wires[stage], 0o600); err != nil { //nolint:gosec // the operator's KUDURRU_RFC5011_CAPTURE_DIR
			t.Fatal(err)
		}
	}
	b, err := json.MarshalIndent(c.provenance, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), append(b, '\n'), 0o600); err != nil { //nolint:gosec // the operator's KUDURRU_RFC5011_CAPTURE_DIR
		t.Fatal(err)
	}
}

// readCapture loads a committed capture and checks every pinned file.
func readCapture(t *testing.T, dir string) rolloverCapture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "provenance.json")) //nolint:gosec // committed test data
	if err != nil {
		t.Fatal(err)
	}
	c := rolloverCapture{wires: map[string][]byte{}}
	if err := json.Unmarshal(b, &c.provenance); err != nil {
		t.Fatal(err)
	}
	for _, stage := range rolloverStages {
		raw, err := os.ReadFile(filepath.Join(dir, stage+".wire")) //nolint:gosec // committed test data
		if err != nil {
			t.Fatal(err)
		}
		if got, want := sha256Hex(raw), c.provenance.Files[stage+".wire"]; got != want {
			t.Fatalf("%s/%s.wire: SHA-256 %s, provenance pins %s", dir, stage, got, want)
		}
		c.wires[stage] = raw
	}
	return c
}

// maybeWriteCapture refreshes the committed capture of signer when
// KUDURRU_RFC5011_CAPTURE_DIR is set.
func maybeWriteCapture(t *testing.T, signer string, c rolloverCapture) {
	t.Helper()
	if dir := os.Getenv("KUDURRU_RFC5011_CAPTURE_DIR"); dir != "" {
		writeCapture(t, filepath.Join(dir, signer), c)
		t.Logf("wrote the %s capture to %s", signer, filepath.Join(dir, signer))
	}
}

// rolloverKeys are the two key-signing keys of a capture, identified from
// the wires themselves: K1 is s1's only SEP key, K2 the other one in s2.
type rolloverKeys struct {
	k1, k2 *dns.DNSKEY
}

func sepKeys(m *dns.Msg) []*dns.DNSKEY {
	var out []*dns.DNSKEY
	for _, rr := range m.Answer {
		if k, ok := rr.(*dns.DNSKEY); ok && k.Flags&dns.SEP != 0 {
			out = append(out, k)
		}
	}
	return out
}

func sameKey(a, b *dns.DNSKEY) bool {
	return a.Algorithm == b.Algorithm && a.PublicKey == b.PublicKey
}

// replayRollover runs the RFC 5011 timeline over a capture's wires.
func replayRollover(t *testing.T, c rolloverCapture) {
	t.Helper()
	t.Logf("capture: signer %s, authority %s, captured %s", c.provenance.Signer, c.provenance.Authority, c.provenance.Captured.Format(time.RFC3339))
	wire := map[string]*dns.Msg{}
	for _, stage := range rolloverStages {
		raw := c.wires[stage]
		if len(raw) < 12 || binary.BigEndian.Uint16(raw[8:10]) != 0 || binary.BigEndian.Uint16(raw[10:12]) > 1 {
			t.Fatalf("%s: the reply breaks the strict acquisition wire assumptions (NSCOUNT 0, ARCOUNT at most 1)", stage)
		}
		m := new(dns.Msg)
		if err := m.Unpack(raw); err != nil {
			t.Fatal(stage, err)
		}
		if !m.Response || !m.Authoritative || m.Rcode != dns.RcodeSuccess || m.Truncated {
			t.Fatalf("%s: not a complete authoritative answer: %s", stage, m)
		}
		wire[stage] = m
	}
	s1, s2 := sepKeys(wire["s1"]), sepKeys(wire["s2"])
	if len(s1) != 1 || len(s2) != 2 {
		t.Fatalf("stage key sets: s1 %d KSKs, s2 %d KSKs", len(s1), len(s2))
	}
	keys := rolloverKeys{k1: s1[0]}
	for _, k := range s2 {
		if !sameKey(k, keys.k1) {
			keys.k2 = k
		}
	}
	if keys.k2 == nil {
		t.Fatal("s2 has no second key-signing key")
	}
	seed := keys.k1.ToDS(dns.SHA256)
	seed.Hdr.Ttl = 0
	if want, err := dns.NewRR(c.provenance.SeedDS); err != nil || !strings.EqualFold(want.(*dns.DS).Digest, seed.Digest) || want.(*dns.DS).KeyTag != seed.KeyTag {
		t.Fatalf("the signer's DS %q is not K1's %s", c.provenance.SeedDS, seed)
	}
	t0 := c.provenance.T0
	day := 24 * time.Hour
	const sessionA, sessionB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	at := func(session string, start, offset time.Duration) trustanchors.Sample {
		return trustanchors.SyntheticSample(session, offset-start, t0.Add(offset))
	}
	a := func(offset time.Duration) trustanchors.Sample { return at(sessionA, 0, offset) }
	step := func(s trustanchors.State, stage string, sample trustanchors.Sample) trustanchors.State {
		t.Helper()
		o, err := trustanchors.Verify(s, wire[stage], sample)
		if err != nil {
			t.Fatal("verify", stage, err)
		}
		next, err := trustanchors.Advance(s, o, sample)
		if err != nil {
			t.Fatal("advance", stage, err)
		}
		return next
	}
	id := func(k *dns.DNSKEY) string {
		normal := *k
		normal.Hdr.Ttl = 0
		normal.Flags &^= dns.REVOKE
		sum := sha256.Sum256([]byte(normal.String()))
		return hex.EncodeToString(sum[:])
	}
	k1, k2 := id(keys.k1), id(keys.k2)
	key := func(s trustanchors.State, kid string) *trustanchors.Key {
		for _, k := range s.Inspect().Keys {
			if k.ID == kid {
				return &k
			}
		}
		return nil
	}
	expect := func(s trustanchors.State, why string, quarantined bool, want map[string]trustanchors.Status) {
		t.Helper()
		doc := s.Inspect()
		if doc.Quarantined != quarantined || len(doc.Keys) != len(want) {
			t.Fatalf("%s: quarantined %v with %d keys, want %v with %d", why, doc.Quarantined, len(doc.Keys), quarantined, len(want))
		}
		for kid, status := range want {
			if k := key(s, kid); k == nil || k.Status != status {
				t.Fatalf("%s: key %s is %+v, want %s", why, kid[:8], k, status)
			}
		}
	}

	// Bootstrap from the operator's DS of K1 at T0.
	s, proof, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: "rfc5011-interop", PolicyID: "rfc5011-interop-seed", DS: []string{c.provenance.SeedDS}}, wire["s1"], a(0))
	if err != nil {
		t.Fatal("bootstrap", err)
	}
	if err := proof.CheckBootstrap(s, a(0)); err != nil {
		t.Fatal(err)
	}
	expect(s, "bootstrap", false, map[string]trustanchors.Status{k1: trustanchors.Valid})
	history := []trustanchors.State{s}
	// main advances the main line and keeps every state for the owner chain.
	main := func(stage string, sample trustanchors.Sample) {
		t.Helper()
		s = step(s, stage, sample)
		history = append(history, s)
	}

	// K2 appears at T0+1d and is held down for 30 days of same-session
	// credit. The observation that brings the credit to 30 days accepts it
	// only if it is strictly after FirstSeen plus the hold-down: the one
	// exactly at the threshold does not, the next one does.
	main("s2", a(day))
	expect(s, "K2 published", false, map[string]trustanchors.Status{k1: trustanchors.Valid, k2: trustanchors.AddPending})
	beforeRestart := s
	main("s2", a(16*day))
	if k := key(s, k2); k.Status != trustanchors.AddPending || k.Elapsed != 15*day {
		t.Fatalf("mid hold-down: %+v", k)
	}
	midHold := s
	main("s2", a(31*day))
	if k := key(s, k2); k.Status != trustanchors.AddPending || k.Elapsed != 30*day {
		t.Fatalf("at the hold-down instant the key must still wait for a later observation: %+v", k)
	}
	main("s2", a(31*day+time.Hour))
	expect(s, "after the hold-down", false, map[string]trustanchors.Status{k1: trustanchors.Valid, k2: trustanchors.Valid})
	if k := key(s, k2); !k.AcceptedAt.Equal(t0.Add(31*day + time.Hour)) {
		t.Fatalf("K2 accepted at %s", k.AcceptedAt)
	}

	// The rollover: K2 signs and K1 stays published, then K1 is removed and
	// is Missing but still trusted, then returns without a new hold-down.
	main("s3", a(32*day))
	expect(s, "K2 signing", false, map[string]trustanchors.Status{k1: trustanchors.Valid, k2: trustanchors.Valid})
	main("s4", a(33*day))
	expect(s, "K1 removed", false, map[string]trustanchors.Status{k1: trustanchors.Missing, k2: trustanchors.Valid})
	main("s3", a(34*day))
	expect(s, "K1 back", false, map[string]trustanchors.Status{k1: trustanchors.Valid, k2: trustanchors.Valid})

	// K1 revokes itself: immediately and permanently, also when its
	// unrevoked form is published again.
	main("s5", a(35*day))
	expect(s, "K1 revoked", false, map[string]trustanchors.Status{k1: trustanchors.Revoked, k2: trustanchors.Valid})
	if k := key(s, k1); !k.RevokedAt.Equal(t0.Add(35 * day)) {
		t.Fatalf("revoked at %s", k.RevokedAt)
	}
	main("s3", a(36*day))
	expect(s, "K1 republished", false, map[string]trustanchors.Status{k1: trustanchors.Revoked, k2: trustanchors.Valid})

	// The accepted owner and the resolver projection of the final state:
	// only K2 is an effective anchor and K1 is a revocation.
	projection := project(t, history)
	k2ds := keys.k2.ToDS(dns.SHA256)
	k2ds.Hdr.Ttl = 0
	if projection.Disposition() != resolverstate.Usable || !slices.Equal(projection.DS(), []string{k2ds.String()}) || !slices.Equal(projection.RevokedIDs(), []string{k1}) {
		t.Fatalf("effective anchors %v revoked %v (%d), want [%s] [%s]", projection.DS(), projection.RevokedIDs(), projection.Disposition(), k2ds, k1[:8])
	}

	// A restart in the middle of the hold-down: session B starts at T0+16d
	// and the uncheckpointed credit since T0+8d is lost; nothing else is.
	r := step(beforeRestart, "s2", a(8*day))
	b := func(offset time.Duration) trustanchors.Sample { return at(sessionB, 16*day, offset) }
	r = step(r, "s2", b(16*day))
	if k := key(r, k2); k.Status != trustanchors.AddPending || k.Elapsed != 7*day {
		t.Fatalf("after the restart: %+v", k)
	}
	r = step(r, "s2", b(31*day+time.Hour))
	if k := key(r, k2); k.Status != trustanchors.AddPending || k.Elapsed != 22*day+time.Hour {
		t.Fatalf("restarted hold-down passed with lost credit: %+v", k)
	}
	r = step(r, "s2", b(38*day+23*time.Hour))
	if k := key(r, k2); k.Status != trustanchors.AddPending {
		t.Fatalf("restarted hold-down passed early: %+v", k)
	}
	r = step(r, "s2", b(39*day+time.Hour))
	if k := key(r, k2); k.Status != trustanchors.Valid || k.Elapsed != 30*day {
		t.Fatalf("restarted hold-down did not complete: %+v", k)
	}

	// Revoking the only trusted key while K2 is still pending: K2 loses its
	// only supporter and is dropped, and the state quarantines.
	q := step(midHold, "s5", a(17*day))
	expect(q, "K1 revoked during the hold-down", true, map[string]trustanchors.Status{k1: trustanchors.Revoked})
	if projection := project(t, []trustanchors.State{history[0], history[1], midHold, q}); projection.Disposition() != resolverstate.Quarantined || len(projection.DS()) != 0 {
		t.Fatalf("quarantine projection %v %d", projection.DS(), projection.Disposition())
	}
}

// project frames states (each one generation after the previous) as
// accepted owners from a dormant root and projects the last one.
func project(t *testing.T, states []trustanchors.State) resolverstate.Projection {
	t.Helper()
	first := states[0].Inspect().Checkpoint.Wall
	owner, err := accepted.NewDormant("rfc5011-interop", strings.Repeat("1", 32), first.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var chain []trustanchors.State
	for i, s := range states {
		if i > 0 && s.Inspect().Generation != chain[len(chain)-1].Inspect().Generation+1 {
			t.Fatalf("project: state %d is generation %d after %d", i, s.Inspect().Generation, chain[len(chain)-1].Inspect().Generation)
		}
		chain = append(chain, s)
	}
	for _, s := range chain {
		at := s.Inspect().Checkpoint.Wall
		if owner, err = accepted.ProposeManaged(owner, s, at, at.Add(time.Hour)); err != nil {
			t.Fatal("frame accepted owner", s.Inspect().Generation, err)
		}
	}
	p, err := resolverstate.Project(owner)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRFC5011RolloverFromBINDWires replays the committed BIND capture and,
// where named and the BIND tools are installed, a fresh one.
func TestRFC5011RolloverFromBINDWires(t *testing.T) {
	t.Run("committed", func(t *testing.T) { replayRollover(t, readCapture(t, filepath.Join("testdata", "rfc5011", "bind"))) })
	t.Run("generated", func(t *testing.T) {
		c := generateBINDRollover(t)
		maybeWriteCapture(t, "bind", c)
		replayRollover(t, c)
	})
}

// TestRFC5011RolloverFromKnotWires replays the committed Knot DNS capture
// and, with KUDURRU_KNOT=1 and a private Docker daemon, a fresh one.
func TestRFC5011RolloverFromKnotWires(t *testing.T) {
	t.Run("committed", func(t *testing.T) { replayRollover(t, readCapture(t, filepath.Join("testdata", "rfc5011", "knot"))) })
	t.Run("generated", func(t *testing.T) {
		c := generateKnotRollover(t)
		maybeWriteCapture(t, "knot", c)
		replayRollover(t, c)
	})
}
