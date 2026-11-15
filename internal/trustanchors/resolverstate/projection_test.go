package resolverstate

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

type signingKey struct {
	key    *dns.DNSKEY
	signer crypto.Signer
}

func deterministicKey(label string) signingKey {
	seed := sha256.Sum256([]byte(label))
	private := ed25519.NewKeyFromSeed(seed[:])
	public := private.Public().(ed25519.PublicKey)
	return signingKey{key: &dns.DNSKEY{
		Hdr:       dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600},
		Flags:     dns.ZONE | dns.SEP,
		Protocol:  3,
		Algorithm: dns.ED25519,
		PublicKey: base64.StdEncoding.EncodeToString(public),
	}, signer: private}
}

func fixtureKey(t *testing.T, label string) signingKey {
	t.Helper()
	// miekg/dns treats signature key tag zero as unset. Deterministic bounded
	// selection keeps that library constraint out of unrelated fixture cases.
	for i := range 16 {
		key := deterministicKey(label + "/" + strconv.Itoa(i))
		if key.key.KeyTag() != 0 {
			return key
		}
	}
	t.Fatal("fixture could not select a nonzero signing tag")
	return signingKey{}
}

func signedMessage(t *testing.T, keys []signingKey, signers ...signingKey) *dns.Msg {
	t.Helper()
	if len(keys) == 0 || len(signers) == 0 {
		t.Fatal("fixture needs a signed nonempty set")
	}
	query := new(dns.Msg)
	query.SetQuestion(".", dns.TypeDNSKEY)
	message := new(dns.Msg)
	message.SetReply(query)
	message.Authoritative = true
	set := make([]dns.RR, 0, len(keys))
	for _, key := range keys {
		set = append(set, dns.Copy(key.key))
	}
	message.Answer = append(message.Answer, set...)
	now := time.Now()
	for _, key := range signers {
		sig := &dns.RRSIG{
			Hdr:        dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: keys[0].key.Hdr.Ttl},
			Algorithm:  key.key.Algorithm,
			KeyTag:     key.key.KeyTag(),
			SignerName: ".",
			Inception:  uint32(now.Add(-time.Hour).Unix() & 0xffffffff),
			Expiration: uint32(now.Add(2*time.Hour).Unix() & 0xffffffff),
		}
		if err := sig.Sign(key.signer, set); err != nil {
			t.Fatal("sign original fixture DNSKEY set", err)
		}
		message.Answer = append(message.Answer, sig)
	}
	return message
}

type ownerFixture struct {
	dormant     accepted.Snapshot
	owner       accepted.Snapshot
	state       trustanchors.State
	observation trustanchors.Observation
	clock       *trustanchors.SessionClock
}

func newOwnerFixture(t *testing.T, keys, seeds []signingKey) *ownerFixture {
	t.Helper()
	dormant, err := accepted.NewDormant("projection-cluster", strings.Repeat("1", 32), time.Now().Add(-time.Second).UTC())
	if err != nil {
		t.Fatal("create structural dormant fixture", err)
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	seed := trustanchors.Seed{ClusterID: "projection-cluster", PolicyID: "projection-selected-seed"}
	for _, key := range seeds {
		seed.DS = append(seed.DS, key.key.ToDS(dns.SHA256).String())
	}
	state, observation, err := trustanchors.Bootstrap(seed, signedMessage(t, keys, seeds...), clock.Sample())
	if err != nil {
		t.Fatal("authenticate original seed fixture", err)
	}
	if err := observation.CheckBootstrap(state, clock.Sample()); err != nil {
		t.Fatal("check original bootstrap evidence", err)
	}
	owner, err := accepted.ProposeManaged(dormant, state, state.Inspect().Checkpoint.Wall, observation.ValidUntil())
	if err != nil {
		t.Fatal("frame structural fixture proposal", err)
	}
	// These are pure cryptographic/structural fixtures, not durable acceptance.
	// Project must also handle proposals; no test helper certifies their commit.
	return &ownerFixture{dormant: dormant, owner: owner, state: state, observation: observation, clock: clock}
}

func (f *ownerFixture) step(t *testing.T, keys []signingKey, signers ...signingKey) {
	t.Helper()
	observation, err := trustanchors.Verify(f.state, signedMessage(t, keys, signers...), f.clock.Sample())
	if err != nil {
		t.Fatal("verify original fixture observation", err)
	}
	state, err := trustanchors.Advance(f.state, observation, f.clock.Sample())
	if err != nil {
		t.Fatal("pure fixture transition", err)
	}
	owner, err := accepted.ProposeManaged(f.owner, state, state.Inspect().Checkpoint.Wall, observation.ValidUntil())
	if err != nil {
		t.Fatal("frame structural successor fixture", err)
	}
	f.state, f.owner, f.observation = state, owner, observation
}

func revokeKey(key signingKey) signingKey {
	key.key = dns.Copy(key.key).(*dns.DNSKEY)
	key.key.Flags |= dns.REVOKE
	return key
}

func expectedDS(keys ...signingKey) []string {
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		ds := key.key.ToDS(dns.SHA256)
		ds.Hdr.Ttl = 0
		result = append(result, ds.String())
	}
	slices.Sort(result)
	return result
}

func normalizedID(key signingKey) string {
	normal := dns.Copy(key.key).(*dns.DNSKEY)
	normal.Hdr.Ttl = 0
	normal.Flags &^= dns.REVOKE
	id := sha256.Sum256([]byte(normal.String()))
	return hex.EncodeToString(id[:])
}

func TestProjectZeroIsInvalidAndDormantHasNoFallback(t *testing.T) {
	var zero Projection
	if zero.Disposition() != Invalid || len(zero.DS()) != 0 || len(zero.RevokedIDs()) != 0 || zero.Identity() != (Identity{}) || zero.DomainID() != ([32]byte{}) || zero.OwnerID() != ([32]byte{}) {
		t.Fatal("zero projection acquired keys or identity")
	}
	refused, err := Project(accepted.Snapshot{})
	if !errors.Is(err, ErrInvalid) || refused.Disposition() != Invalid || refused.OwnerID() != ([32]byte{}) || len(refused.DS()) != 0 {
		t.Fatal("zero owner did not return an empty refusal")
	}
	dormant, err := accepted.NewDormant("projection-cluster", strings.Repeat("2", 32), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	p, err := Project(dormant)
	if err != nil || p.Disposition() != AwaitingBootstrap || len(p.DS()) != 0 || len(p.RevokedIDs()) != 0 || p.Identity().Owner != dormant.Version() {
		t.Fatal("dormant projection substituted keys or lost exact history", err)
	}
	if p.Identity().PolicyID != "" || p.Identity().SeedDigest != "" || p.OwnerID() == ([32]byte{}) || p.DomainID() == ([32]byte{}) {
		t.Fatal("dormant identity invented policy or lost history")
	}
}

func TestProjectSignedValidMissingPendingAndRevokedSelection(t *testing.T) {
	a, b := fixtureKey(t, "status-a"), fixtureKey(t, "status-b")
	c, candidate := fixtureKey(t, "status-c"), fixtureKey(t, "status-candidate")
	f := newOwnerFixture(t, []signingKey{a, b, c}, []signingKey{a, b, c})
	revoked := revokeKey(a)
	f.step(t, []signingKey{revoked, c, candidate}, revoked, c)
	counts := map[trustanchors.Status]int{}
	for _, key := range f.state.Inspect().Keys {
		counts[key.Status]++
	}
	if counts[trustanchors.Valid] != 1 || counts[trustanchors.Missing] != 1 || counts[trustanchors.AddPending] != 1 || counts[trustanchors.Revoked] != 1 {
		t.Fatal("signed fixture did not establish all four protocol states")
	}
	p, err := Project(f.owner)
	if err != nil || p.Disposition() != Usable || !slices.Equal(p.DS(), expectedDS(b, c)) || !slices.Equal(p.RevokedIDs(), []string{normalizedID(a)}) {
		t.Fatal("projection granted pending/revoked trust or lost Missing trust", err)
	}
	if p.Identity().Owner != f.owner.Version() || p.Identity().PolicyID != f.state.Inspect().PolicyID || p.Identity().SeedDigest != f.state.Inspect().SeedDigest {
		t.Fatal("projection replaced exact envelope/pure/policy identity")
	}
	if slices.Contains(p.DS(), expectedDS(a)[0]) || slices.Contains(p.DS(), expectedDS(candidate)[0]) {
		t.Fatal("seed or candidate DS leaked into effective keys")
	}
}

func TestProjectSelfRevokeQuarantinesWithoutSeedFallback(t *testing.T) {
	a := fixtureKey(t, "quarantine")
	f := newOwnerFixture(t, []signingKey{a}, []signingKey{a})
	before, err := Project(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	revoked := revokeKey(a)
	f.step(t, []signingKey{revoked}, revoked)
	p, err := Project(f.owner)
	if err != nil || p.Disposition() != Quarantined || len(p.DS()) != 0 || !slices.Equal(p.RevokedIDs(), []string{normalizedID(a)}) {
		t.Fatal("self-revoked history gained fallback trust", err)
	}
	if p.DomainID() != before.DomainID() || p.OwnerID() == before.OwnerID() || p.Identity().Owner != f.owner.Version() {
		t.Fatal("quarantine lost domain or exact successor identity")
	}
}

func TestProjectOutputsAndInputInspectionAreDetached(t *testing.T) {
	a, b := fixtureKey(t, "detach-a"), fixtureKey(t, "detach-b")
	f := newOwnerFixture(t, []signingKey{a, b}, []signingKey{a, b})
	revoked := revokeKey(a)
	f.step(t, []signingKey{revoked, b}, revoked, b)
	p, err := Project(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	ds, revokedIDs := p.DS(), p.RevokedIDs()
	id, domain, owner := p.Identity(), p.DomainID(), p.OwnerID()
	copyDS, copyRevoked := p.DS(), p.RevokedIDs()
	copyDS[0], copyRevoked[0] = "mutated", "mutated"
	copyID := p.Identity()
	copyID.ClusterID, copyID.Owner.Pure.Digest = "mutated", "mutated"
	copyDomain, copyOwner := p.DomainID(), p.OwnerID()
	copyDomain[0], copyOwner[0] = copyDomain[0]^0xff, copyOwner[0]^0xff
	root := f.owner.Inspect()
	root.Chunks[0].Digest = "mutated"
	doc := f.state.Inspect()
	doc.Keys[0].DNSKEY, doc.Seeds[0] = "mutated", "mutated"
	if !slices.Equal(p.DS(), ds) || !slices.Equal(p.RevokedIDs(), revokedIDs) || p.Identity() != id || p.DomainID() != domain || p.OwnerID() != owner {
		t.Fatal("detached inspection changed projection")
	}
	if copyID == id || copyDomain == domain || copyOwner == owner {
		t.Fatal("fixture did not mutate detached value copies")
	}
	again, err := Project(f.owner)
	if err != nil || again.OwnerID() != owner || !slices.Equal(again.DS(), ds) || !slices.Equal(again.RevokedIDs(), revokedIDs) {
		t.Fatal("input inspection escaped immutable history", err)
	}
}

func TestProjectKeepsOriginalObservationExpirySeparate(t *testing.T) {
	a := fixtureKey(t, "historical-observation")
	a.key.Hdr.Ttl = 1
	f := newOwnerFixture(t, []signingKey{a}, []signingKey{a})
	before, err := Project(f.owner)
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(max(0, time.Until(f.observation.ValidUntil())) + time.Millisecond)
	defer timer.Stop()
	select {
	case <-t.Context().Done():
		t.Fatal("original test context expired")
	case <-timer.C:
	}
	if err := f.observation.CheckBootstrap(f.state, f.clock.Sample()); !errors.Is(err, trustanchors.ErrExpired) {
		t.Fatal("held bootstrap proof did not retain original expiry", err)
	}
	// This structural projection neither admits the now-expired proposal nor
	// treats historical observation TTL as a lifetime for accepted anchor use.
	after, err := Project(f.owner)
	if err != nil || after.Disposition() != Usable || after.OwnerID() != before.OwnerID() || !slices.Equal(after.DS(), before.DS()) {
		t.Fatal("structural projection restamped or expired anchor history", err)
	}
	if err := f.observation.CheckBootstrap(f.state, f.clock.Sample()); !errors.Is(err, trustanchors.ErrExpired) {
		t.Fatal("Project renewed original evidence", err)
	}
}

func TestProjectAuthenticatedUnsupportedReceivedKeyNeverGrants(t *testing.T) {
	a, unsupported := fixtureKey(t, "supported"), fixtureKey(t, "unsupported")
	unsupported.key.Algorithm = dns.ED448
	f := newOwnerFixture(t, []signingKey{a, unsupported}, []signingKey{a})
	p, err := Project(f.owner)
	if err != nil || p.Disposition() != Usable || !slices.Equal(p.DS(), expectedDS(a)) || len(f.state.Inspect().Keys) != 1 {
		t.Fatal("signed unsupported received key became resolver trust", err)
	}
}

func TestProjectBoundsRetainPendingKeysWithoutGrantingThem(t *testing.T) {
	keys := make([]signingKey, 0, trustanchors.MaxLiveKeys+1)
	for i := range trustanchors.MaxLiveKeys + 1 {
		keys = append(keys, fixtureKey(t, "bounded/"+strconv.Itoa(i)))
	}
	f := newOwnerFixture(t, keys[:trustanchors.MaxLiveKeys], keys[:1])
	p, err := Project(f.owner)
	if err != nil || p.Disposition() != Usable || !slices.Equal(p.DS(), expectedDS(keys[0])) || len(f.state.Inspect().Keys) != trustanchors.MaxLiveKeys {
		t.Fatal("bounded pending set acquired trust or lost retained history", err)
	}
	seed := trustanchors.Seed{ClusterID: "projection-cluster", PolicyID: "projection-selected-seed", DS: []string{keys[0].key.ToDS(dns.SHA256).String()}}
	_, _, err = trustanchors.Bootstrap(seed, signedMessage(t, keys, keys[0]), f.clock.Sample())
	if !errors.Is(err, trustanchors.ErrBounds) {
		t.Fatal("oversized signed input escaped original pure bound", err)
	}
}

func collisionKeys(t *testing.T) (signingKey, signingKey) {
	t.Helper()
	seen := make(map[uint16]signingKey)
	// Deterministic fixtures and a finite search avoid a probabilistic test.
	// The test observes an actual collision before using either key; it never
	// forces a tag or replaces the library's real signing/verification logic.
	for i := range 1 << 17 {
		key := deterministicKey("collision/" + strconv.Itoa(i))
		tag := key.key.KeyTag()
		if tag == 0 {
			continue
		}
		if previous, ok := seen[tag]; ok {
			return previous, key
		}
		seen[tag] = key
	}
	t.Fatal("bounded deterministic fixture search found no nonzero tag collision")
	return signingKey{}, signingKey{}
}

func TestProjectDistinctSignedKeysWithSameTagRemainDistinct(t *testing.T) {
	a, b := collisionKeys(t)
	if a.key.KeyTag() != b.key.KeyTag() || a.key.PublicKey == b.key.PublicKey || normalizedID(a) == normalizedID(b) {
		t.Fatal("fixture did not produce distinct keys with equal tags")
	}
	f := newOwnerFixture(t, []signingKey{a, b}, []signingKey{a, b})
	p, err := Project(f.owner)
	if err != nil || p.Disposition() != Usable || len(p.DS()) != 2 || !slices.Equal(p.DS(), expectedDS(a, b)) {
		t.Fatal("key-tag collision conflated effective trust", err)
	}
}
