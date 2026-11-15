package trustanchors

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// Seed is explicit out-of-band operator trust. It contains full root DS records,
// never fetched keys, default substitution, or a request to reset prior history.
type Seed struct {
	ClusterID string
	PolicyID  string
	DS        []string
}

// Observation retains the exact cryptographically authenticated original cut.
// Its fields are private: a wire AD flag or a caller's secure boolean cannot
// manufacture lifecycle authority. Durable admission remains the adapter's job.
type Observation struct {
	verified      bool
	base          Version
	bootstrap     Version
	at            Sample
	authenticated bool
	keys          map[string]string
	revoked       map[string]string
	support       []string
	signatures    []*dns.RRSIG
	holdTTL       uint32
	refreshTTL    uint32
	until         time.Time
	expiresAt     time.Time
	lifetime      time.Duration
}

// ValidUntil is the detached original observation deadline. It bounds evidence
// consumption, not future trust-anchor use, and never replaces caller deadlines.
func (o Observation) ValidUntil() time.Time { return o.expiresAt }

// CheckLifetime checks the original receipt-relative wall AND monotonic bounds.
// It never restamps TTLs or captures a new source. A future adapter must retain
// this evidence and its original caller lifetime through final commit admission.
func (o Observation) CheckLifetime(final Sample) error {
	if !o.verified {
		return ErrProof
	}
	if !final.valid() || final.session != o.at.session || final.tick < o.at.tick || final.wall.Before(o.at.wall) {
		return ErrClock
	}
	if !final.wall.Before(o.expiresAt) || final.tick-o.at.tick >= o.lifetime {
		return ErrExpired
	}
	for _, sig := range o.signatures {
		if !signatureCurrent(sig, final.wall) {
			return ErrExpired
		}
	}
	return nil
}

// CheckBootstrap binds initial admission to the exact proposed state AND its
// original detached observation lifetime. The caller must retain this evidence
// through its final create-only durable CAS; State/Encode/Decode cannot recreate
// it. This check grants neither a writer nor permission to replace prior history.
func (o Observation) CheckBootstrap(proposed State, final Sample) error {
	if !o.verified || o.bootstrap.Generation != 1 || !validDigest(o.bootstrap.Digest) {
		return ErrProof
	}
	v, err := proposed.Version()
	if err != nil {
		return err
	}
	if v != o.bootstrap {
		return ErrStale
	}
	return o.CheckLifetime(final)
}

// Revocations returns detached deny-only identities. This is NOT permission to
// stage a transition with a successor state, writer, deadline or source.
func (o Observation) Revocations() []string {
	ids := make([]string, 0, len(o.revoked))
	for id := range o.revoked {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Bootstrap returns a proposed new state AND its original opaque evidence,
// using the selected DS seed. Retain both through initial durable admission and
// invoke CheckBootstrap at the actual final sample without restamping the proof.
// A future persistence adapter must refuse this operation beside prior history.
// The result is PROPOSED: only a successful original-admission durable CAS makes
// its checkpoint accepted input. Encode/Decode are not evidence of that commit.
func Bootstrap(seed Seed, message *dns.Msg, at Sample) (State, Observation, error) {
	if !validLabel(seed.ClusterID) || !validLabel(seed.PolicyID) || !at.valid() || len(seed.DS) == 0 || len(seed.DS) > MaxLiveKeys {
		return State{}, Observation{}, ErrInvalid
	}
	set, _, err := rootSet(message)
	if err != nil {
		return State{}, Observation{}, err
	}
	seedText := make([]string, 0, len(seed.DS))
	seedKeys := make(map[string]string)
	for _, text := range seed.DS {
		if len(text) > 2048 {
			return State{}, Observation{}, ErrBounds
		}
		rr, err := dns.NewRR(text)
		if err != nil {
			return State{}, Observation{}, ErrInvalid
		}
		d, ok := rr.(*dns.DS)
		if !ok || d.Hdr.Name != "." || d.Hdr.Class != dns.ClassINET || !algorithm(d.Algorithm) || !digestAlgorithm(d.DigestType) {
			return State{}, Observation{}, ErrInvalid
		}
		d.Hdr.Ttl = 0
		d.Digest = strings.ToUpper(d.Digest)
		canonical := d.String()
		if slices.Contains(seedText, canonical) {
			return State{}, Observation{}, ErrInvalid
		}
		seedText = append(seedText, canonical)
		for _, rr := range set {
			k := rr.(*dns.DNSKEY)
			if !eligible(k) || k.Flags&dns.REVOKE != 0 || k.Algorithm != d.Algorithm || k.KeyTag() != d.KeyTag {
				continue
			}
			candidate := k.ToDS(d.DigestType)
			if candidate != nil && strings.EqualFold(candidate.Digest, d.Digest) {
				id, text, err := canonicalKey(k)
				if err != nil {
					return State{}, Observation{}, err
				}
				seedKeys[id] = text
			}
		}
	}
	if len(seedKeys) == 0 {
		return State{}, Observation{}, ErrProof
	}
	slices.Sort(seedText)
	keys := make([]Key, 0, len(seedKeys))
	for id, text := range seedKeys {
		keys = append(keys, Key{ID: id, DNSKEY: text, Status: Valid, FirstSeen: at.wall, LastSeen: at.wall, AcceptedAt: at.wall, Support: []string{}})
	}
	o, err := authenticate(message, keys, at)
	if err != nil || !o.authenticated {
		if err != nil {
			return State{}, Observation{}, err
		}
		return State{}, Observation{}, ErrProof
	}
	if err := o.CheckLifetime(at); err != nil {
		return State{}, Observation{}, err
	}
	for _, k := range o.keys {
		rr, err := dns.NewRR(k)
		if err != nil {
			return State{}, Observation{}, ErrInvalid
		}
		id, text, err := canonicalKey(rr.(*dns.DNSKEY))
		if err != nil {
			return State{}, Observation{}, err
		}
		if _, seeded := seedKeys[id]; !seeded {
			keys = append(keys, Key{ID: id, DNSKEY: text, Status: AddPending, FirstSeen: at.wall, LastSeen: at.wall, OriginalTTL: o.holdTTL, Support: slices.Clone(o.support)})
		}
	}
	for i := range keys {
		keys[i].OriginalTTL = o.holdTTL
	}
	slices.SortFunc(keys, func(a, b Key) int { return strings.Compare(a.ID, b.ID) })
	sched, err := schedule(at.wall, o.refreshTTL, o.until)
	if err != nil {
		return State{}, Observation{}, err
	}
	s := State{doc: Snapshot{Format: FormatVersion, ClusterID: seed.ClusterID, PolicyID: seed.PolicyID, SeedDigest: digest([]byte(strings.Join(seedText, "\n"))), Seeds: slices.Clone(seedText), Generation: 1, Keys: keys, Checkpoint: Checkpoint{Session: at.session, Tick: at.tick, Wall: at.wall}, Schedule: sched}}
	v, err := s.Version()
	if err != nil {
		return State{}, Observation{}, err
	}
	o.bootstrap = v
	return s, o, nil
}

// Verify authenticates bounded fresh wire evidence against only the exact
// supplied state. It neither advances timers nor accepts candidate-only trust.
func Verify(s State, message *dns.Msg, at Sample) (Observation, error) {
	v, err := s.Version()
	if err != nil {
		return Observation{}, err
	}
	if !at.valid() {
		return Observation{}, ErrClock
	}
	o, err := authenticate(message, s.doc.Keys, at)
	if err != nil {
		return Observation{}, err
	}
	o.base = v
	return o, nil
}

func rootSet(m *dns.Msg) ([]dns.RR, []*dns.RRSIG, error) {
	if m == nil || !m.Response || m.Truncated || m.Opcode != dns.OpcodeQuery || m.Rcode != dns.RcodeSuccess || len(m.Question) != 1 || m.Question[0].Name != "." || m.Question[0].Qclass != dns.ClassINET || m.Question[0].Qtype != dns.TypeDNSKEY {
		return nil, nil, ErrInvalid
	}
	if len(m.Answer) == 0 || len(m.Answer) > MaxLiveKeys+MaxSignatures || len(m.Ns) != 0 || len(m.Extra) > 1 {
		return nil, nil, ErrBounds
	}
	if len(m.Extra) == 1 {
		opt, ok := m.Extra[0].(*dns.OPT)
		if !ok || opt == nil || opt.Hdr.Name != "." || len(opt.Option) > 8 {
			return nil, nil, ErrInvalid
		}
		for _, option := range opt.Option {
			if option == nil || len(option.String()) > 1024 {
				return nil, nil, ErrBounds
			}
		}
	}
	set := make([]dns.RR, 0, min(len(m.Answer), MaxLiveKeys))
	sigs := make([]*dns.RRSIG, 0, min(len(m.Answer), MaxSignatures))
	seen := map[string]bool{}
	for _, rr := range m.Answer {
		switch r := rr.(type) {
		case *dns.DNSKEY:
			if r == nil || r.Hdr.Name != "." || r.Hdr.Class != dns.ClassINET {
				return nil, nil, ErrInvalid
			}
			if len(set) == MaxLiveKeys || len(r.PublicKey) > 4096 {
				return nil, nil, ErrBounds
			}
			if r.Hdr.Rrtype != dns.TypeDNSKEY || r.Protocol != 3 || r.Flags & ^uint16(dns.ZONE|dns.SEP|dns.REVOKE) != 0 {
				return nil, nil, ErrInvalid
			}
			id, _, err := canonicalKey(r)
			if err != nil || seen[id] {
				return nil, nil, ErrInvalid
			}
			seen[id] = true
			set = append(set, dns.Copy(r))
		case *dns.RRSIG:
			if r == nil || r.Hdr.Name != "." || r.Hdr.Class != dns.ClassINET {
				return nil, nil, ErrInvalid
			}
			if len(sigs) == MaxSignatures || len(r.Signature) > 4096 {
				return nil, nil, ErrBounds
			}
			if r.Hdr.Rrtype != dns.TypeRRSIG || r.TypeCovered != dns.TypeDNSKEY || r.SignerName != "." || r.Labels != 0 {
				return nil, nil, ErrInvalid
			}
			sigs = append(sigs, dns.Copy(r).(*dns.RRSIG))
		default:
			return nil, nil, ErrInvalid
		}
	}
	if len(set) == 0 || len(sigs) == 0 {
		return nil, nil, ErrProof
	}
	if m.Len() > MaxWireBytes {
		return nil, nil, ErrBounds
	}
	return set, sigs, nil
}

func authenticate(m *dns.Msg, known []Key, at Sample) (Observation, error) {
	set, sigs, err := rootSet(m)
	if err != nil {
		return Observation{}, err
	}
	o := Observation{verified: true, at: at, keys: map[string]string{}, revoked: map[string]string{}, support: []string{}, signatures: []*dns.RRSIG{}, refreshTTL: ^uint32(0)}
	wireTTL := ^uint32(0)
	for _, rr := range set {
		wireTTL = min(wireTTL, rr.Header().Ttl)
	}
	o.expiresAt = at.wall.Add(time.Duration(wireTTL) * time.Second)
	retainSignature := func(sig *dns.RRSIG) {
		o.signatures = append(o.signatures, sig)
		ttl := min(wireTTL, sig.Hdr.Ttl, sig.OrigTtl)
		until := at.wall.Add(time.Duration(ttl) * time.Second)
		expires := time.Unix(int64(sig.Expiration), 0).UTC()
		if expires.Before(until) {
			until = expires
		}
		if until.Before(o.expiresAt) {
			o.expiresAt = until
		}
	}
	checks := 0
	// Revocation is checked against the exact received REVOKE RDATA, including
	// its changed key tag. Neither an old-key signature nor another anchor's
	// signature over a revoke flag is a self-revocation.
	for _, old := range known {
		if old.Status == Revoked {
			continue
		}
		for _, rr := range set {
			key := rr.(*dns.DNSKEY)
			id, text, err := canonicalKey(key)
			if err != nil {
				return Observation{}, err
			}
			if id != old.ID || !eligible(key) || key.Flags&dns.REVOKE == 0 {
				continue
			}
			for _, sig := range sigs {
				if sig.Algorithm != key.Algorithm || sig.KeyTag != key.KeyTag() || !signatureCurrent(sig, at.wall) {
					continue
				}
				checks++
				if checks > MaxSignatureChecks {
					return Observation{}, ErrBounds
				}
				if sig.Verify(key, set) != nil {
					continue
				}
				o.revoked[id] = text
				retainSignature(sig)
			}
		}
	}
	for _, old := range known {
		if !trusted(old) {
			continue
		}
		if _, revoked := o.revoked[old.ID]; revoked {
			continue
		}
		rr, err := dns.NewRR(old.DNSKEY)
		if err != nil {
			return Observation{}, ErrInvalid
		}
		key := rr.(*dns.DNSKEY)
		for _, sig := range sigs {
			if sig.Algorithm != key.Algorithm || sig.KeyTag != key.KeyTag() || !signatureCurrent(sig, at.wall) {
				continue
			}
			checks++
			if checks > MaxSignatureChecks {
				return Observation{}, ErrBounds
			}
			if sig.Verify(key, set) != nil {
				continue
			}
			if !slices.Contains(o.support, old.ID) {
				o.support = append(o.support, old.ID)
			}
			retainSignature(sig)
			o.holdTTL = max(o.holdTTL, sig.OrigTtl)
			o.refreshTTL = min(o.refreshTTL, sig.OrigTtl)
			until := time.Unix(int64(sig.Expiration), 0).UTC()
			if o.until.IsZero() || until.Before(o.until) {
				o.until = until
			}
		}
	}
	// The revoked key may authenticate ONLY its own revocation, never additions,
	// omissions or a new root set. A revoked/nonrevoked duplicate was refused.
	o.authenticated = len(o.support) > 0
	if !o.authenticated && len(o.revoked) == 0 {
		return Observation{}, ErrProof
	}
	o.lifetime = o.expiresAt.Sub(at.wall)
	slices.Sort(o.support)
	if o.authenticated {
		for _, rr := range set {
			k := rr.(*dns.DNSKEY)
			if !eligible(k) || k.Flags&dns.REVOKE != 0 {
				continue
			}
			id, text, err := canonicalKey(k)
			if err != nil {
				return Observation{}, err
			}
			o.keys[id] = text
		}
	}
	return o, nil
}

func signatureCurrent(sig *dns.RRSIG, now time.Time) bool {
	return validTime(now) && int64(sig.Inception) <= now.Unix() && now.Unix() < int64(sig.Expiration) && sig.ValidityPeriod(now)
}

func canonicalKey(key *dns.DNSKEY) (string, string, error) {
	if key == nil || key.Hdr.Name != "." || key.Hdr.Class != dns.ClassINET || key.Hdr.Rrtype != dns.TypeDNSKEY || len(key.PublicKey) == 0 || len(key.PublicKey) > 4096 {
		return "", "", ErrInvalid
	}
	b, err := base64.StdEncoding.Strict().DecodeString(key.PublicKey)
	if err != nil || len(b) == 0 || base64.StdEncoding.EncodeToString(b) != key.PublicKey {
		return "", "", ErrInvalid
	}
	normal := *key
	normal.Hdr.Ttl = 0
	actual := normal.String()
	normal.Flags &^= dns.REVOKE
	return digest([]byte(normal.String())), actual, nil
}

func eligible(k *dns.DNSKEY) bool {
	return k.Protocol == 3 && k.Flags&dns.ZONE != 0 && k.Flags&dns.SEP != 0 && algorithm(k.Algorithm) && keyMaterial(k)
}

func keyMaterial(k *dns.DNSKEY) bool {
	b, err := base64.StdEncoding.Strict().DecodeString(k.PublicKey)
	if err != nil {
		return false
	}
	switch k.Algorithm {
	case dns.ED25519:
		return len(b) == 32
	case dns.ECDSAP256SHA256, dns.ECDSAP384SHA384:
		curve, size := ecdh.P256(), 32
		if k.Algorithm == dns.ECDSAP384SHA384 {
			curve, size = ecdh.P384(), 48
		}
		if len(b) != 2*size {
			return false
		}
		point := make([]byte, 1+len(b))
		point[0] = 4
		copy(point[1:], b)
		_, err := curve.NewPublicKey(point)
		return err == nil
	case dns.RSASHA1, dns.RSASHA1NSEC3SHA1, dns.RSASHA256, dns.RSASHA512:
		if len(b) < 1 {
			return false
		}
		n, offset := int(b[0]), 1
		if n == 0 {
			if len(b) < 3 {
				return false
			}
			n, offset = int(b[1])<<8|int(b[2]), 3
		}
		if n < 1 || n > 4 || offset+n >= len(b) {
			return false
		}
		e := uint64(0)
		for _, v := range b[offset : offset+n] {
			e = e<<8 | uint64(v)
		}
		modulus := new(big.Int).SetBytes(b[offset+n:])
		return e >= 3 && e&1 != 0 && modulus.BitLen() >= 512 && modulus.BitLen() <= 8192 && modulus.Bit(0) == 1
	}
	return false
}

func algorithm(a uint8) bool {
	switch a {
	case dns.RSASHA1, dns.RSASHA1NSEC3SHA1, dns.RSASHA256, dns.RSASHA512, dns.ECDSAP256SHA256, dns.ECDSAP384SHA384, dns.ED25519:
		return true
	}
	return false
}

func digestAlgorithm(a uint8) bool { return a == dns.SHA1 || a == dns.SHA256 || a == dns.SHA384 }

func parseSeed(text string) (*dns.DS, error) {
	if len(text) > 2048 {
		return nil, ErrBounds
	}
	rr, err := dns.NewRR(text)
	if err != nil {
		return nil, ErrInvalid
	}
	d, ok := rr.(*dns.DS)
	if !ok || d.Hdr.Name != "." || d.Hdr.Class != dns.ClassINET || d.Hdr.Ttl != 0 || !algorithm(d.Algorithm) || !digestAlgorithm(d.DigestType) || d.String() != text {
		return nil, ErrInvalid
	}
	b, err := hex.DecodeString(d.Digest)
	if err != nil {
		return nil, ErrInvalid
	}
	size := map[uint8]int{dns.SHA1: 20, dns.SHA256: 32, dns.SHA384: 48}[d.DigestType]
	if len(b) != size || strings.ToUpper(hex.EncodeToString(b)) != d.Digest {
		return nil, ErrInvalid
	}
	return d, nil
}

func seeded(key *dns.DNSKEY, seeds []*dns.DS) bool {
	normal := *key
	normal.Flags &^= dns.REVOKE
	for _, d := range seeds {
		if normal.Algorithm != d.Algorithm || normal.KeyTag() != d.KeyTag {
			continue
		}
		candidate := normal.ToDS(d.DigestType)
		if candidate != nil && strings.EqualFold(candidate.Digest, d.Digest) {
			return true
		}
	}
	return false
}
