package zone

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// SecondaryIdentity binds runtime data to the configuration that acquired
// it. It hashes the object UID, namespace, name and entire spec, including
// primary endpoints, protocol, key references and policy. Status, generation
// and secret values are not included. A nil object has an empty identity.
func SecondaryIdentity(dz *v1alpha1.DNSZone) string {
	if dz == nil {
		return ""
	}
	// These API fields contain only JSON-compatible values. encoding/json
	// sorts map keys, including label selectors in the spec.
	b, _ := json.Marshal(struct {
		UID       string               `json:"uid"`
		Namespace string               `json:"namespace"`
		Name      string               `json:"name"`
		Spec      v1alpha1.DNSZoneSpec `json:"spec"`
	}{string(dz.UID), dz.Namespace, dz.Name, dz.Spec})
	sum := sha256.Sum256(b)
	return HashPrefix + hex.EncodeToString(sum[:])
}

// SecondaryIdentity returns the configuration identity captured at build
// time, or an empty string for other zone types.
func (z *Zone) SecondaryIdentity() string { return z.secondaryIdentity }

// AvailableAt reports whether this zone has data that may be served at now.
// Other zone types are always available. Secondary placeholders and data
// at or beyond expiry are unavailable. Query callers must check this before
// Lookup/LookupFrom and answer SERVFAIL while retaining authority coverage.
func (z *Zone) AvailableAt(now time.Time) bool {
	return z.typ != v1alpha1.ZoneTypeSecondary ||
		(z.soa != nil && !z.nextExpiry.IsZero() && now.Before(z.nextExpiry))
}

// HasSignatures reports transferred RRSIG or legacy SIG records. It does
// not assert cryptographic validity. LookupSignedWithin assembles transferred
// signatures and proofs; ordinary Lookup remains the raw-data lookup.
func (z *Zone) HasSignatures() bool { return z.hasSignatures }

// BuildSecondaryPlaceholder retains unavailable secondary authority coverage
// even when its query ACL is invalid. Such
// errors deny all queries; they never retain old data or allow forwarding.
// The original configuration identity is preserved, and dz is not modified.
// Invalid type or apex still returns an error because coverage cannot be
// established. Callers should report configuration errors separately.
func BuildSecondaryPlaceholder(dz *v1alpha1.DNSZone) (*Zone, error) {
	z, err := BuildSecondary(dz, nil, time.Time{})
	if err == nil || dz == nil {
		return z, err
	}
	// BuildSecondary already fails closed on transfer/key errors and handles
	// notify errors independently. Only query policy
	// errors need a coverage fallback; type/origin errors remain errors.
	closed := dz.DeepCopy()
	closed.Spec.Policy.QueryAccess = v1alpha1.AccessDeny
	closed.Spec.Policy.QueryACL = nil
	z, err = BuildSecondary(closed, nil, time.Time{})
	if err == nil {
		z.secondaryIdentity = SecondaryIdentity(dz)
	}
	return z, err
}

type secondaryRecord struct {
	rr          dns.RR
	key         string
	wire, rdata []byte
}

// BuildSecondary builds an immutable secondary directly from a complete RR
// set. There must be exactly one apex SOA (no closing AXFR SOA) and an apex
// NS RRset; record order is immaterial. It preserves SOA fields, serial,
// owner/RDATA spelling, TTLs, DNSSEC and unknown types instead of applying
// primary endpoint normalization. Inputs are copied; returned RRsets and
// lookup/transfer records follow Zone's shared, read-only convention.
//
// Nil records creates a non-serving placeholder with authority coverage and
// configured ACLs. A non-nil empty set is invalid. Data requires a nonzero
// expiry; already expired data can be built for recovery and diagnostics.
// Transport configuration and cryptographic validation belong to the caller.
func BuildSecondary(dz *v1alpha1.DNSZone, records []dns.RR, expiresAt time.Time) (*Zone, error) {
	z, err := buildSecondaryRaw(dz, records, expiresAt)
	if err != nil || records == nil {
		return z, err
	}
	// BuildStub uses this raw tree builder on a copied secondary config;
	// discovery records do not establish a signed authoritative serving zone.
	if dz.Spec.Stub == nil {
		z.secondaryProofs, err = BuildSecondaryProofs(z)
		if err != nil {
			return nil, fmt.Errorf("signed secondary structure: %w", err)
		}
	}
	return z, nil
}

func buildSecondaryRaw(dz *v1alpha1.DNSZone, records []dns.RR, expiresAt time.Time) (*Zone, error) {
	if dz == nil || dz.Spec.Type != v1alpha1.ZoneTypeSecondary {
		return nil, fmt.Errorf("secondary zone configuration is required")
	}
	origin := fqdn(dz.Spec.ZoneName)
	key, err := canonicalKey(origin)
	if _, ok := dns.IsDomainName(origin); dz.Spec.ZoneName == "" || err != nil || !ok {
		return nil, fmt.Errorf("spec.zoneName must be a domain name")
	}
	path := field.NewPath("spec", "policy")
	access, errs := compileAccess(dz.Spec.Policy, path)
	if len(errs) > 0 {
		return nil, errs.ToAggregate()
	}
	transfer, terrs := compileTransfer(dz.Spec.Policy, path)
	keys, kerrs := compileKeyRefs(dz.Spec.Policy.TransferTSIGKeyRefs, path.Child("transferTSIGKeyRefs"))
	if len(terrs) > 0 || len(kerrs) > 0 {
		transfer = accessPolicy{mode: v1alpha1.AccessDeny}
	}
	notify, targets, _ := compileNotify(dz.Spec.Policy, path)
	z := &Zone{
		name: nameFromKey(key), key: key, typ: v1alpha1.ZoneTypeSecondary,
		access: access, transfer: transfer, transferKeys: keys,
		notify: notify, notifyTo: targets, generation: dz.Generation,
		secondaryIdentity: SecondaryIdentity(dz),
	}
	z.apex = z.tree.upsert(key, z.name)
	z.index = map[string]*node{key: z.apex}
	if records == nil {
		return z, nil
	}
	if expiresAt.IsZero() {
		return nil, fmt.Errorf("secondary data requires a nonzero expiry")
	}
	data := make([]secondaryRecord, 0, len(records))
	for i, rr := range records {
		c, wire, err := copySecondaryRR(rr)
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		h := c.Header()
		k, err := canonicalKey(h.Name)
		if err != nil || !dns.IsFqdn(h.Name) || !isKeyPrefix(k, key) {
			return nil, fmt.Errorf("record %d: owner must be an absolute in-zone name", i)
		}
		if h.Class != dns.ClassINET {
			return nil, fmt.Errorf("record %d: class must be IN", i)
		}
		if secondaryPseudoType(h.Rrtype) {
			return nil, fmt.Errorf("record %d: %s is not zone data", i, typeString(h.Rrtype))
		}
		switch r := c.(type) {
		case *dns.SOA:
			if k != key || z.soa != nil {
				return nil, fmt.Errorf("secondary data must contain exactly one apex SOA")
			}
			z.soa, z.serial = r, r.Serial
		case *dns.RRSIG:
			if secondaryPseudoType(r.TypeCovered) {
				return nil, fmt.Errorf("record %d: RRSIG covers a non-data type", i)
			}
			z.hasSignatures = true
		case *dns.SIG:
			if secondaryPseudoType(r.TypeCovered) {
				return nil, fmt.Errorf("record %d: SIG covers a non-data type (including SIG(0))", i)
			}
			z.hasSignatures = true
		}
		data = append(data, secondaryRecord{c, k, wire, canonicalRdata(c)})
	}
	if z.soa == nil {
		return nil, fmt.Errorf("secondary data must contain exactly one apex SOA")
	}
	// Canonical ordering without rewriting any raw record. The wire tie
	// breaker makes differing TTLs and presentation casing deterministic.
	slices.SortFunc(data, func(a, b secondaryRecord) int {
		if n := strings.Compare(a.key, b.key); n != 0 {
			return n
		}
		if n := int(a.rr.Header().Rrtype) - int(b.rr.Header().Rrtype); n != 0 {
			return n
		}
		if n := bytes.Compare(a.rdata, b.rdata); n != 0 {
			return n
		}
		return bytes.Compare(a.wire, b.wire)
	})
	b := Builder{z: z} // insertName only: materialize empty non-terminals.
	hash := sha256.New()
	var length [4]byte
	for _, r := range data {
		n := z.tree.search(r.key)
		if n == nil {
			n = b.insertName(r.key, nameFromKey(r.key))
		}
		t := r.rr.Header().Rrtype
		set := n.get(t)
		if set == nil {
			n.sets = append(n.sets, RRset{Name: n.name, Type: t, TTL: r.rr.Header().Ttl})
			set = &n.sets[len(n.sets)-1]
		}
		set.RRs = append(set.RRs, r.rr)
		set.TTL = min(set.TTL, r.rr.Header().Ttl)
		binary.BigEndian.PutUint32(length[:], uint32(len(r.wire))) //nolint:gosec // G115: wire length is bounded by copySecondaryRR.
		hash.Write(length[:])
		hash.Write(r.wire)
	}
	var structural error
	z.tree.walk(func(n *node) bool {
		z.index[n.key] = n
		for _, t := range []uint16{dns.TypeCNAME, dns.TypeDNAME} {
			if s := n.get(t); s != nil && len(s.RRs) != 1 {
				structural = fmt.Errorf("%s must have exactly one %s record", n.name, typeString(t))
				return false
			}
		}
		if n.get(dns.TypeCNAME) != nil {
			for _, s := range n.sets {
				switch s.Type {
				case dns.TypeCNAME, dns.TypeRRSIG, dns.TypeNSEC, dns.TypeSIG, dns.TypeKEY, dns.TypeNXT:
				default:
					structural = fmt.Errorf("%s: CNAME cannot coexist with %s", n.name, typeString(s.Type))
					return false
				}
			}
		}
		if n != z.apex && (n.get(dns.TypeNS) != nil || n.get(dns.TypeDNAME) != nil) {
			z.cuts = true
		}
		return true
	})
	if structural != nil {
		return nil, structural
	}
	if z.apex.get(dns.TypeNS) == nil {
		return nil, fmt.Errorf("secondary data requires apex NS records")
	}
	z.negSOA = dns.Copy(z.soa).(*dns.SOA)
	z.negSOA.Hdr.Ttl = min(z.soa.Hdr.Ttl, z.soa.Minttl)
	z.negNs = []dns.RR{z.negSOA}
	z.nextExpiry, z.records = expiresAt, len(records)-1
	z.hash = HashPrefix + hex.EncodeToString(hash.Sum(nil))
	nsAddrs := z.nameserverAddrs()
	z.access.nsAddrs = append(z.access.nsAddrs, nsAddrs...)
	z.transfer.nsAddrs = append(z.transfer.nsAddrs, nsAddrs...)
	return z, nil
}

func secondaryPseudoType(t uint16) bool {
	switch t {
	case dns.TypeNone, dns.TypeReserved, dns.TypeOPT, dns.TypeTSIG, dns.TypeTKEY,
		dns.TypeAXFR, dns.TypeIXFR, dns.TypeMAILA, dns.TypeMAILB, dns.TypeANY:
		return true
	}
	return false
}

// copySecondaryRR validates wire structure and returns a private parsed
// copy. This also decodes known types supplied in RFC 3597 form so lookup's
// concrete type assertions are safe. No caller-owned header is packed:
// dns.PackRR updates Rdlength, even when used only for validation.
func copySecondaryRR(rr dns.RR) (out dns.RR, wire []byte, err error) {
	if rr == nil || reflect.ValueOf(rr).IsNil() {
		return nil, nil, fmt.Errorf("nil resource record")
	}
	// Malformed hand-built RRs can contain nil nested values; miekg's copy
	// and pack helpers assume parsed data. Reject those without panicking.
	defer func() {
		if recover() != nil {
			out, wire, err = nil, nil, fmt.Errorf("malformed resource record")
		}
	}()
	if _, raw := rr.(*dns.RFC3597); !raw {
		newRR, known := dns.TypeToRR[rr.Header().Rrtype]
		if !known || reflect.TypeOf(rr) != reflect.TypeOf(newRR()) {
			return nil, nil, fmt.Errorf("RR type does not match its header")
		}
	}
	c := dns.Copy(rr)
	size := dns.Len(c)
	if size > dns.MaxMsgSize+255+10 {
		return nil, nil, fmt.Errorf("resource record exceeds maximum wire size")
	}
	buf := make([]byte, size+16)
	n, err := dns.PackRR(c, buf, 0, nil, false)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid RR wire data: %w", err)
	}
	out, end, err := dns.UnpackRR(buf[:n], 0)
	if err != nil || end != n {
		return nil, nil, fmt.Errorf("invalid RR wire structure")
	}
	if _, known := dns.TypeToRR[out.Header().Rrtype]; known && out.Header().Rdlength == 0 &&
		out.Header().Rrtype != dns.TypeNULL && out.Header().Rrtype != dns.TypeAPL {
		return nil, nil, fmt.Errorf("zone record requires RDATA")
	}
	check := make([]byte, dns.Len(out)+16)
	m, err := dns.PackRR(out, check, 0, nil, false)
	if err != nil || !bytes.Equal(buf[:n], check[:m]) {
		return nil, nil, fmt.Errorf("RR wire data does not round-trip")
	}
	return out, buf[:n], nil
}
