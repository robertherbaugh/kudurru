package trustanchors

import (
	"crypto"
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type testKey struct {
	key    *dns.DNSKEY
	signer crypto.Signer
}

type fixture struct {
	t       *testing.T
	a, b, c testKey
	base    time.Time
	ttl     uint32
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	makeKey := func() testKey {
		key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
		private, err := key.Generate(256)
		if err != nil {
			t.Fatal("generate fixture signing key")
		}
		return testKey{key: key, signer: private.(crypto.Signer)}
	}
	return &fixture{t: t, a: makeKey(), b: makeKey(), c: makeKey(), base: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), ttl: 3600}
}

func (f *fixture) point(elapsed time.Duration) Sample {
	return Sample{session: "11111111111111111111111111111111", tick: elapsed, wall: f.base.Add(elapsed)}
}

func (f *fixture) wire(keys []testKey, signers ...testKey) *dns.Msg {
	f.t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	var set []dns.RR
	for _, k := range keys {
		key := dns.Copy(k.key).(*dns.DNSKEY)
		key.Hdr.Ttl = f.ttl
		set = append(set, key)
	}
	m.Answer = append(m.Answer, set...)
	for _, signer := range signers {
		sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: f.ttl}, Algorithm: signer.key.Algorithm, KeyTag: signer.key.KeyTag(), SignerName: ".", Inception: uint32(f.base.Add(-time.Hour).Unix() & 0xffffffff), Expiration: uint32(f.base.Add(365*24*time.Hour).Unix() & 0xffffffff)}
		if err := sig.Sign(signer.signer, set); err != nil {
			f.t.Fatal("sign actual fixture DNSKEY RRset")
		}
		m.Answer = append(m.Answer, sig)
	}
	return m
}

func (f *fixture) initial(keys []testKey, seeds ...testKey) State {
	f.t.Helper()
	seed := Seed{ClusterID: "cluster", PolicyID: "selected-root-seed", DS: []string{}}
	for _, k := range seeds {
		seed.DS = append(seed.DS, k.key.ToDS(dns.SHA256).String())
	}
	s, proof, err := Bootstrap(seed, f.wire(keys, seeds...), f.point(0))
	if err != nil {
		f.t.Fatal("bootstrap original signed seed", err)
	}
	if err := proof.CheckBootstrap(s, f.point(0)); err != nil {
		f.t.Fatal("original bootstrap evidence", err)
	}
	return s
}

func (f *fixture) step(s State, m *dns.Msg, at Sample) State {
	// Pure transition sequence only; this helper does not prove durable CAS.
	f.t.Helper()
	o, err := Verify(s, m, at)
	if err != nil {
		f.t.Fatal("verify original observation", err)
	}
	next, err := Advance(s, o, at)
	if err != nil {
		f.t.Fatal("advance original observation", err)
	}
	return next
}

func keyID(t *testing.T, k testKey) string {
	t.Helper()
	id, _, err := canonicalKey(k.key)
	if err != nil {
		t.Fatal("fixture key identity")
	}
	return id
}

func findKey(t *testing.T, s State, k testKey) Key {
	t.Helper()
	id := keyID(t, k)
	for _, known := range s.Inspect().Keys {
		if known.ID == id {
			return known
		}
	}
	t.Fatal("expected key missing")
	return Key{}
}

func containsKey(t *testing.T, s State, k testKey) bool {
	t.Helper()
	id := keyID(t, k)
	for _, known := range s.Inspect().Keys {
		if known.ID == id {
			return true
		}
	}
	return false
}

func revoke(k testKey) testKey {
	key := dns.Copy(k.key).(*dns.DNSKEY)
	key.Flags |= dns.REVOKE
	return testKey{key: key, signer: k.signer}
}

func TestAddRequiresOriginalHoldAndFreshPostThresholdProof(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	if findKey(t, s, f.b).Status != AddPending {
		t.Fatal("candidate trusted at first observation")
	}
	o, err := Verify(s, f.wire([]testKey{f.a, f.b}, f.a), f.point(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(s, o, f.point(AddHoldDown+time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatal("old observation survived its original receipt TTL", err)
	}
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), f.point(AddHoldDown-time.Second))
	if findKey(t, s, f.b).Status != AddPending {
		t.Fatal("early candidate trusted")
	}
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), f.point(AddHoldDown))
	if findKey(t, s, f.b).Status != AddPending {
		t.Fatal("threshold itself substituted for post-threshold observation")
	}
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), f.point(AddHoldDown+time.Second))
	if findKey(t, s, f.b).Status != Valid {
		t.Fatal("fresh authenticated post-threshold observation refused")
	}
	s = f.step(s, f.wire([]testKey{f.b}, f.b), f.point(AddHoldDown+2*time.Second))
	if findKey(t, s, f.a).Status != Missing || findKey(t, s, f.b).Status != Valid {
		t.Fatal("new trusted signer or Missing semantics lost")
	}
}

func TestFirstSignedTTLAndAuthenticatedOmissionReset(t *testing.T) {
	f := newFixture(t)
	f.ttl = uint32((40 * 24 * time.Hour) / time.Second)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	f.ttl = 3600
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), f.point(AddHoldDown+time.Second))
	if findKey(t, s, f.b).Status != AddPending {
		t.Fatal("later short TTL replaced original hold-down")
	}
	s = f.step(s, f.wire([]testKey{f.a}, f.a), f.point(31*24*time.Hour))
	if containsKey(t, s, f.b) {
		t.Fatal("authenticated omission did not reset pending candidate")
	}
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), f.point(41*24*time.Hour))
	k := findKey(t, s, f.b)
	if k.Status != AddPending || k.Elapsed != 0 || !k.FirstSeen.Equal(f.base.Add(41*24*time.Hour)) {
		t.Fatal("reappearance inherited reset progress")
	}
}

func TestRestartPreservesOnlyDurablyCheckpointedSessionProgress(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), f.point(20*24*time.Hour))
	b, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	s, err = Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	resumed := Sample{session: "22222222222222222222222222222222", wall: f.base.Add(50 * 24 * time.Hour)}
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), resumed)
	k := findKey(t, s, f.b)
	if k.Status != AddPending || k.Elapsed != 20*24*time.Hour {
		t.Fatal("downtime credited or prior durable progress reset")
	}
	resumed.tick, resumed.wall = 10*24*time.Hour+time.Second, f.base.Add(60*24*time.Hour+time.Second)
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), resumed)
	if findKey(t, s, f.b).Status != Valid {
		t.Fatal("retained proven progress did not resume")
	}
}

func TestClockJumpCannotCreditCandidateAndBackwardTimeRefuses(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	jump := f.point(24 * time.Hour)
	jump.wall = f.base.Add(60 * 24 * time.Hour)
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), jump)
	if !s.Inspect().ClockUncertain || findKey(t, s, f.b).Elapsed != 0 || findKey(t, s, f.b).Status != AddPending {
		t.Fatal("wall jump credited acceptance")
	}
	o, err := Verify(s, f.wire([]testKey{f.a, f.b}, f.a), f.point(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(s, o, f.point(time.Hour)); !errors.Is(err, ErrClock) {
		t.Fatal("backward admission accepted", err)
	}
}

func TestMissingRemainsTrustedAndReturnsWithoutNewHold(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a, f.b)
	s = f.step(s, f.wire([]testKey{f.b}, f.b), f.point(time.Hour))
	s = f.step(s, f.wire([]testKey{f.b}, f.b), f.point(90*24*time.Hour))
	if findKey(t, s, f.a).Status != Missing {
		t.Fatal("missing key discarded by time")
	}
	// A retained anchor can authenticate a set even while absent from that set.
	s = f.step(s, f.wire([]testKey{f.b}, f.a), f.point(90*24*time.Hour+time.Second))
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.a), f.point(90*24*time.Hour+2*time.Second))
	if findKey(t, s, f.a).Status != Valid {
		t.Fatal("returning trusted key restarted add hold-down")
	}
}

func TestSelfRevocationIsImmediatePermanentAndResetsOriginalSupport(t *testing.T) {
	f := newFixture(t)
	// Only A authenticates C's first observation; merely configuring B does not
	// make B an original supporter of this pending acceptance.
	s, proof, err := Bootstrap(Seed{ClusterID: "cluster", PolicyID: "policy", DS: []string{f.a.key.ToDS(dns.SHA256).String(), f.b.key.ToDS(dns.SHA256).String()}}, f.wire([]testKey{f.a, f.b, f.c}, f.a), f.point(0))
	if err != nil {
		t.Fatal(err)
	}
	if err := proof.CheckBootstrap(s, f.point(0)); err != nil {
		t.Fatal(err)
	}
	ra := revoke(f.a)
	if ra.key.KeyTag() == f.a.key.KeyTag() {
		t.Fatal("fixture revoke did not change tag")
	}
	s = f.step(s, f.wire([]testKey{ra, f.b, f.c}, ra, f.b), f.point(time.Hour))
	if findKey(t, s, f.a).Status != Revoked || containsKey(t, s, f.c) {
		t.Fatal("revoke delayed or pending acceptance adopted a new supporter")
	}
	s = f.step(s, f.wire([]testKey{f.a, f.b}, f.b), f.point(90*24*time.Hour))
	if findKey(t, s, f.a).Status != Revoked {
		t.Fatal("old public key revived after remove hold-down")
	}
	if _, err := Verify(s, f.wire([]testKey{f.a}, f.a), f.point(90*24*time.Hour+time.Second)); !errors.Is(err, ErrProof) {
		t.Fatal("revoked key authenticated ordinary data", err)
	}
}

func TestRevocationNeedsOwnSignatureAndAllAnchorLossQuarantines(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a, f.b)
	ra := revoke(f.a)
	next := f.step(s, f.wire([]testKey{ra, f.b}, f.b), f.point(time.Hour))
	if findKey(t, next, f.a).Status != Missing {
		t.Fatal("another key revoked a trusted key")
	}
	s = f.initial([]testKey{f.a, f.b}, f.a)
	o, err := Verify(s, f.wire([]testKey{ra, f.b}, ra), f.point(time.Hour))
	if err != nil || len(o.Revocations()) != 1 {
		t.Fatal("exact self-revoke proof refused", err)
	}
	next, err = Advance(s, o, f.point(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !next.Inspect().Quarantined || findKey(t, next, f.a).Status != Revoked || containsKey(t, next, f.b) {
		t.Fatal("lost root fell back, accepted candidate, or retained unsupported pending progress")
	}
	encoded, err := Encode(next)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Decode(encoded)
	if err != nil || !restored.Inspect().Quarantined {
		t.Fatal("quarantine lost during restore", err)
	}
}

func TestEveryAuthenticatingOriginalSupporterIsRetained(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b, f.c}, f.a, f.b)
	if len(findKey(t, s, f.c).Support) != 2 {
		t.Fatal("only first authenticating signer retained")
	}
	ra := revoke(f.a)
	s = f.step(s, f.wire([]testKey{ra, f.b, f.c}, ra, f.b), f.point(10*24*time.Hour))
	k := findKey(t, s, f.c)
	if k.Status != AddPending || k.Elapsed != 10*24*time.Hour || len(k.Support) != 2 {
		t.Fatal("surviving original supporter was discarded")
	}
}

func TestBootstrapRetainsOriginalEvidenceThroughInitialAdmission(t *testing.T) {
	f := newFixture(t)
	seed := Seed{ClusterID: "cluster", PolicyID: "selected-root-seed", DS: []string{f.a.key.ToDS(dns.SHA256).String()}}
	at := f.point(0)
	for _, tc := range []struct {
		name     string
		lifetime time.Duration
		adjust   func(*dns.Msg)
	}{
		{"received_set_ttl", 3 * time.Second, func(m *dns.Msg) { m.Answer[1].Header().Ttl = 3 }},
		{"received_signature_ttl", 4 * time.Second, func(m *dns.Msg) { m.Answer[2].Header().Ttl = 4 }},
		{"signed_original_ttl", 5 * time.Second, func(m *dns.Msg) {
			sig := m.Answer[2].(*dns.RRSIG)
			sig.OrigTtl = 5
			if err := sig.Sign(f.a.signer, m.Answer[:2]); err != nil {
				t.Fatal("sign bootstrap original TTL")
			}
		}},
		{"signature_expiration", 6 * time.Second, func(m *dns.Msg) {
			sig := m.Answer[2].(*dns.RRSIG)
			sig.Expiration = uint32(at.wall.Add(6*time.Second).Unix() & 0xffffffff)
			if err := sig.Sign(f.a.signer, m.Answer[:2]); err != nil {
				t.Fatal("sign bootstrap expiration")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := f.wire([]testKey{f.a, f.b}, f.a)
			tc.adjust(m)
			proposed, original, err := Bootstrap(seed, m, at)
			if err != nil {
				t.Fatal("bootstrap original signed seed", err)
			}
			if !original.ValidUntil().Equal(at.wall.Add(tc.lifetime)) {
				t.Fatal("bootstrap discarded an original proof bound")
			}
			encoded, err := Encode(proposed)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := Decode(encoded)
			if err != nil {
				t.Fatal(err)
			}
			// State serialization retains the target, never the original proof.
			if err := (Observation{}).CheckBootstrap(restored, at); !errors.Is(err, ErrProof) {
				t.Fatal("decoded state manufactured bootstrap proof", err)
			}
			for _, rr := range m.Answer {
				rr.Header().Ttl = ^uint32(0)
			}
			m.Answer[2].(*dns.RRSIG).OrigTtl = ^uint32(0)
			m.Answer[2].(*dns.RRSIG).Expiration = ^uint32(0)
			m.Answer[0].(*dns.DNSKEY).PublicKey = "AA=="
			before := f.point(tc.lifetime - time.Nanosecond)
			for _, state := range []State{proposed, restored} {
				if err := original.CheckBootstrap(state, before); err != nil {
					t.Fatal("unexpired original bootstrap refused", err)
				}
				if err := original.CheckBootstrap(state, f.point(tc.lifetime)); !errors.Is(err, ErrExpired) {
					t.Fatal("held bootstrap survived exact original expiry", err)
				}
				if err := original.CheckBootstrap(state, f.point(tc.lifetime+time.Hour)); !errors.Is(err, ErrExpired) {
					t.Fatal("held bootstrap restamped after expiry", err)
				}
			}
			lag := f.point(tc.lifetime)
			lag.wall = at.wall.Add(time.Second)
			if err := original.CheckBootstrap(restored, lag); !errors.Is(err, ErrExpired) {
				t.Fatal("wall lag extended original bootstrap monotonic lifetime", err)
			}
			lag = f.point(time.Second)
			lag.wall = at.wall.Add(tc.lifetime)
			if err := original.CheckBootstrap(restored, lag); !errors.Is(err, ErrExpired) {
				t.Fatal("monotonic lag extended original bootstrap wall lifetime", err)
			}
			wrongSession := before
			wrongSession.session = "22222222222222222222222222222222"
			if err := original.CheckBootstrap(restored, wrongSession); !errors.Is(err, ErrClock) {
				t.Fatal("bootstrap adopted another monotonic session", err)
			}
			otherSeed := seed
			otherSeed.PolicyID = "another-policy"
			other, _, err := Bootstrap(otherSeed, f.wire([]testKey{f.a, f.b}, f.a), at)
			if err != nil {
				t.Fatal(err)
			}
			if err := original.CheckBootstrap(other, before); !errors.Is(err, ErrStale) {
				t.Fatal("bootstrap adopted another policy target", err)
			}
			later, _, err := Bootstrap(seed, f.wire([]testKey{f.a, f.b}, f.a), f.point(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if err := original.CheckBootstrap(later, before); !errors.Is(err, ErrStale) {
				t.Fatal("bootstrap adopted another original checkpoint", err)
			}
			fresh, err := Verify(restored, f.wire([]testKey{f.a, f.b}, f.a), before)
			if err != nil {
				t.Fatal(err)
			}
			if err := fresh.CheckBootstrap(restored, before); !errors.Is(err, ErrProof) {
				t.Fatal("later observation reconstructed bootstrap evidence", err)
			}
		})
	}
}

func TestBootstrapRefusesZeroOriginalProofLifetime(t *testing.T) {
	f := newFixture(t)
	seed := Seed{ClusterID: "cluster", PolicyID: "selected-root-seed", DS: []string{f.a.key.ToDS(dns.SHA256).String()}}
	for _, tc := range []struct {
		name   string
		adjust func(*dns.Msg)
	}{
		{"received_set_ttl", func(m *dns.Msg) { m.Answer[1].Header().Ttl = 0 }},
		{"received_signature_ttl", func(m *dns.Msg) { m.Answer[2].Header().Ttl = 0 }},
		{"signed_original_ttl", func(m *dns.Msg) {
			m.Answer[0].Header().Ttl = 0
			m.Answer[1].Header().Ttl = 0
			sig := m.Answer[2].(*dns.RRSIG)
			sig.OrigTtl = 0
			if err := sig.Sign(f.a.signer, m.Answer[:2]); err != nil {
				t.Fatal("sign zero original TTL")
			}
			m.Answer[0].Header().Ttl = f.ttl
			m.Answer[1].Header().Ttl = f.ttl
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := f.wire([]testKey{f.a, f.b}, f.a)
			tc.adjust(m)
			proposed, original, err := Bootstrap(seed, m, f.point(0))
			if !errors.Is(err, ErrExpired) {
				t.Fatal("zero-TTL bootstrap did not refuse original expired evidence", err)
			}
			if _, err := proposed.Version(); err == nil {
				t.Fatal("failed bootstrap returned usable proposed state")
			}
			if err := original.CheckLifetime(f.point(0)); !errors.Is(err, ErrProof) {
				t.Fatal("failed bootstrap returned usable observation", err)
			}
		})
	}
}

func TestObservationRetainsExactStateAndFinalProofLifetime(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	m := f.wire([]testKey{f.a, f.b}, f.a)
	o, err := Verify(s, m, f.point(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	m.Answer[0].(*dns.DNSKEY).PublicKey = "AA=="
	next, err := Advance(s, o, f.point(time.Hour))
	if err != nil {
		t.Fatal("caller mutated original proof", err)
	}
	if _, err := Advance(next, o, f.point(2*time.Hour)); !errors.Is(err, ErrStale) {
		t.Fatal("observation adopted successor state", err)
	}
	if _, err := Advance(s, o, f.point(365*24*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatal("final expired proof admitted", err)
	}
	wrongSession := f.point(time.Hour)
	wrongSession.session = "22222222222222222222222222222222"
	if _, err := Advance(s, o, wrongSession); !errors.Is(err, ErrClock) {
		t.Fatal("original observation moved to another clock session", err)
	}
}

func TestHeldObservationExpiresByOriginalReceivedProofLifetime(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	at := f.point(time.Hour)
	for _, tc := range []struct {
		name     string
		lifetime time.Duration
		adjust   func(*dns.Msg)
	}{
		{"received_set_ttl", 3 * time.Second, func(m *dns.Msg) { m.Answer[1].Header().Ttl = 3 }},
		{"received_signature_ttl", 4 * time.Second, func(m *dns.Msg) { m.Answer[2].Header().Ttl = 4 }},
		{"signed_original_ttl", 5 * time.Second, func(m *dns.Msg) {
			sig := m.Answer[2].(*dns.RRSIG)
			sig.OrigTtl = 5
			if err := sig.Sign(f.a.signer, m.Answer[:2]); err != nil {
				t.Fatal("sign short original TTL")
			}
		}},
		{"signature_expiration", 6 * time.Second, func(m *dns.Msg) {
			sig := m.Answer[2].(*dns.RRSIG)
			sig.Expiration = uint32(at.wall.Add(6*time.Second).Unix() & 0xffffffff)
			if err := sig.Sign(f.a.signer, m.Answer[:2]); err != nil {
				t.Fatal("sign original expiration boundary")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := f.wire([]testKey{f.a, f.b}, f.a)
			tc.adjust(m)
			o, err := Verify(s, m, at)
			if err != nil {
				t.Fatal(err)
			}
			until := at.wall.Add(tc.lifetime)
			if !o.ValidUntil().Equal(until) {
				t.Fatal("original observation lifetime omitted a proof bound")
			}
			// Caller edits and delayed consumption cannot renew detached evidence.
			for _, rr := range m.Answer {
				rr.Header().Ttl = ^uint32(0)
			}
			m.Answer[2].(*dns.RRSIG).OrigTtl = ^uint32(0)
			before := f.point(time.Hour + tc.lifetime - time.Nanosecond)
			if _, err := Advance(s, o, before); err != nil {
				t.Fatal("original unexpired evidence refused", err)
			}
			original, err := s.Version()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Advance(s, o, f.point(time.Hour+tc.lifetime)); !errors.Is(err, ErrExpired) {
				t.Fatal("held original observation survived expiry", err)
			}
			if _, err := Advance(s, o, f.point(time.Hour+tc.lifetime+time.Hour)); !errors.Is(err, ErrExpired) {
				t.Fatal("expired observation restamped on later consumption", err)
			}
			if v, err := s.Version(); err != nil || v != original {
				t.Fatal("refusal changed accepted checkpoint", err)
			}
			// A lagging wall clock cannot extend the original monotonic TTL.
			lag := f.point(time.Hour + tc.lifetime)
			lag.wall = at.wall.Add(time.Second)
			if err := o.CheckLifetime(lag); !errors.Is(err, ErrExpired) {
				t.Fatal("monotonic original lifetime extended by wall lag", err)
			}
		})
	}
}

func TestOnlyValidatingSignaturesBoundLifetimeAndRevokeOnlyProofIsBounded(t *testing.T) {
	f := newFixture(t)
	s := f.initial([]testKey{f.a, f.b}, f.a)
	at := f.point(time.Hour)
	m := f.wire([]testKey{f.a, f.b}, f.a, f.b)
	// B is still a candidate. Its signature cannot authenticate or expire this
	// observation, even though its key and signature are present on the wire.
	m.Answer[3].Header().Ttl = 0
	o, err := Verify(s, m, at)
	if err != nil || !o.ValidUntil().Equal(at.wall.Add(time.Hour)) {
		t.Fatal("untested candidate signature became proof authority", err)
	}
	ra := revoke(f.a)
	m = f.wire([]testKey{ra, f.b}, ra)
	m.Answer[2].Header().Ttl = 2
	o, err = Verify(s, m, at)
	if err != nil || len(o.Revocations()) != 1 {
		t.Fatal("original self-revoke evidence unavailable", err)
	}
	if _, err := Advance(s, o, f.point(time.Hour+2*time.Second)); !errors.Is(err, ErrExpired) {
		t.Fatal("revoke-only observation lifetime not checked", err)
	}
	m.Answer[0].Header().Ttl = 0
	o, err = Verify(s, m, at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(s, o, at); !errors.Is(err, ErrExpired) {
		t.Fatal("zero-TTL held observation admitted", err)
	}
}

func TestFiveSeedAnchorsAndIndependentClockSessions(t *testing.T) {
	f := newFixture(t)
	g := newFixture(t)
	keys := []testKey{f.a, f.b, f.c, g.a, g.b}
	s := f.initial(keys, keys...)
	if len(s.Inspect().Keys) != 5 {
		t.Fatal("RFC minimum five anchors not supported")
	}
	a, err := NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	first, second := a.Sample(), b.Sample()
	if !first.valid() || !second.valid() || first.session == second.session {
		t.Fatal("clock session reused")
	}
}
