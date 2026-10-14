package zonestate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type manifest struct {
	FormatVersion int              `json:"formatVersion"`
	Zone          zonekinds.ZoneID `json:"zone"`
	Bytes         int64            `json:"bytes"`
	Parts         []string         `json:"parts"`
}

func validateZone(id zonekinds.ZoneID) error {
	name, err := dynamicupdate.CanonicalName(id.Apex)
	if err != nil || name != id.Apex || id.ClusterID == "" || id.ObjectName == "" || id.UID == "" {
		return fmt.Errorf("%w: zone identity", ErrInvalid)
	}
	for _, s := range []string{id.ClusterID, id.ObjectName, id.UID, id.Apex} {
		if len(s) > 512 || bytes.ContainsRune([]byte(s), 0) {
			return ErrInvalid
		}
	}
	return nil
}

// normalized creates a deterministic detached ordering without altering raw
// RRs. The content hash excludes SOA serial, includes TTL, and ignores DNS name
// presentation case. Metadata/policy changes alone therefore retain the serial.
func normalized(a *Aggregate, l Limits) (*Aggregate, string, uint32, error) {
	l = l.Defaults()
	if a == nil || a.FormatVersion != zonekinds.FormatVersion {
		return nil, "", 0, ErrInvalid
	}
	if err := aggregatePreflight(a, l); err != nil {
		return nil, "", 0, err
	}
	if err := validateZone(a.Zone); err != nil {
		return nil, "", 0, err
	}
	out := *a
	config, err := canonicalJSON(a.Config)
	if err != nil || len(config) == 0 || config[0] != '{' {
		return nil, "", 0, fmt.Errorf("%w: frozen config", ErrInvalid)
	}
	out.Config = config
	out.Sources = append([]Source(nil), a.Sources...)
	sources := map[string]bool{"system": true}
	for i := range out.Sources {
		s := &out.Sources[i]
		if s.ID == "" || s.UID == "" || s.ResourceVersion == "" || s.Manager == "" || sources[s.ID] || len(s.ID) > 512 || len(s.UID) > 512 || len(s.ResourceVersion) > 512 || len(s.Manager) > 512 {
			return nil, "", 0, fmt.Errorf("%w: source identity", ErrInvalid)
		}
		sources[s.ID] = true
		b, err := canonicalJSON(s.Desired)
		if err != nil || len(b) == 0 || b[0] != '{' {
			return nil, "", 0, fmt.Errorf("%w: source desired", ErrInvalid)
		}
		s.Desired = b
	}
	sort.Slice(out.Sources, func(i, j int) bool { return out.Sources[i].ID < out.Sources[j].ID })
	out.Reservations = append([]dynamicupdate.Reservation(nil), a.Reservations...)
	reserved := map[string]bool{}
	for i := range out.Reservations {
		r := &out.Reservations[i]
		n, err := dynamicupdate.CanonicalName(r.Name)
		if err != nil || !dns.IsSubDomain(a.Zone.Apex, n) || !sources[r.Source] {
			return nil, "", 0, fmt.Errorf("%w: reservation", ErrInvalid)
		}
		r.Name = n
		key := fmt.Sprintf("%s\x00%d\x00%s", n, r.Type, r.Source)
		if reserved[key] {
			return nil, "", 0, fmt.Errorf("%w: duplicate reservation", ErrInvalid)
		}
		reserved[key] = true
	}
	sort.Slice(out.Reservations, func(i, j int) bool {
		x, y := out.Reservations[i], out.Reservations[j]
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		if x.Type != y.Type {
			return x.Type < y.Type
		}
		return x.Source < y.Source
	})
	out.Policy.Credentials = append([]dynamicupdate.Credential(nil), a.Policy.Credentials...)
	credentials := map[string]bool{}
	for i := range out.Policy.Credentials {
		c := &out.Policy.Credentials[i]
		n, err := dynamicupdate.CanonicalName(c.Key)
		if err != nil || c.Version == "" || len(c.Version) > 512 || credentials[n] {
			return nil, "", 0, fmt.Errorf("%w: credential version", ErrInvalid)
		}
		c.Key = n
		credentials[n] = true
	}
	sort.Slice(out.Policy.Credentials, func(i, j int) bool { return out.Policy.Credentials[i].Key < out.Policy.Credentials[j].Key })
	type sortableGrant struct {
		grant dynamicupdate.Grant
		types []byte
	}
	grants := make([]sortableGrant, len(a.Policy.Grants))
	out.Policy.Grants = make([]dynamicupdate.Grant, len(a.Policy.Grants))
	for i, original := range a.Policy.Grants {
		grants[i].grant = original
		g := &grants[i].grant
		if len(g.Types) > 65535 || len(g.Key) > dynamicupdate.MaxNameBytes || len(g.Owner) > dynamicupdate.MaxNameBytes+2 {
			return nil, "", 0, ErrCapacity
		}
		g.Types = append([]uint16(nil), g.Types...)
		sort.Slice(g.Types, func(i, j int) bool { return g.Types[i] < g.Types[j] })
		grants[i].types, _ = json.Marshal(g.Types)
	}
	sort.Slice(grants, func(i, j int) bool {
		x, y := grants[i].grant, grants[j].grant
		if x.Key != y.Key {
			return x.Key < y.Key
		}
		if x.Owner != y.Owner {
			return x.Owner < y.Owner
		}
		return bytes.Compare(grants[i].types, grants[j].types) < 0
	})
	for i, g := range grants {
		out.Policy.Grants[i] = g.grant
	}
	type item struct {
		record  Record
		key     string
		hashKey string
		ttl     uint32
	}
	items := make([]item, 0, len(a.Records))
	seen := map[string]bool{}
	var wireBytes int64
	soaCount, nsCount := 0, 0
	var serial uint32
	for _, r := range a.Records {
		if len(r.Wire) > 65535 || len(r.Wire) == 0 {
			return nil, "", 0, ErrInvalid
		}
		wireBytes += int64(len(r.Wire))
		if wireBytes > l.MaxWireBytes {
			return nil, "", 0, ErrCapacity
		}
		rr, n, err := dns.UnpackRR(r.Wire, 0)
		if err != nil || n != len(r.Wire) {
			return nil, "", 0, fmt.Errorf("%w: record framing", ErrInvalid)
		}
		if err := dynamicupdate.ValidateData(rr); err != nil {
			return nil, "", 0, fmt.Errorf("%w: RDATA", ErrInvalid)
		}
		raw, err := dynamicupdate.Wire(rr)
		if err != nil || !bytes.Equal(raw, r.Wire) {
			return nil, "", 0, fmt.Errorf("%w: uncompressed record required", ErrInvalid)
		}
		name, err := dynamicupdate.CanonicalName(rr.Header().Name)
		typ := rr.Header().Rrtype
		if err != nil || !dns.IsSubDomain(a.Zone.Apex, name) || rr.Header().Class != dns.ClassINET || typ == 0 || typ == 65535 || typ == dns.TypeOPT || (typ >= dns.TypeTKEY && typ <= dns.TypeANY) {
			return nil, "", 0, fmt.Errorf("%w: owner/class/type", ErrInvalid)
		}
		p := r.Provenance
		if (p.SourceID == "") == (p.Key == "") || len(p.SourceID) > 512 || len(p.CredentialVersion) > 512 {
			return nil, "", 0, fmt.Errorf("%w: record ownership", ErrInvalid)
		}
		if p.SourceID != "" {
			if !sources[p.SourceID] || p.CredentialVersion != "" {
				return nil, "", 0, ErrInvalid
			}
		} else {
			if _, err := dynamicupdate.CanonicalName(p.Key); err != nil || p.CredentialVersion == "" {
				return nil, "", 0, ErrInvalid
			}
		}
		key, err := dynamicupdate.Identity(rr)
		if err != nil || seen[key] {
			return nil, "", 0, fmt.Errorf("%w: duplicate record", ErrInvalid)
		}
		seen[key] = true
		hashKey := key
		if typ == dns.TypeSOA {
			soa, ok := rr.(*dns.SOA)
			if !ok || name != a.Zone.Apex {
				return nil, "", 0, ErrInvalid
			}
			serial = soa.Serial
			soaCount++
			soa.Serial = 0
			hashKey, err = dynamicupdate.Identity(soa)
			if err != nil {
				return nil, "", 0, err
			}
		}
		if name == a.Zone.Apex && typ == dns.TypeNS {
			nsCount++
		}
		items = append(items, item{record: r, key: key, hashKey: hashKey, ttl: rr.Header().Ttl})
	}
	if soaCount != 1 || nsCount == 0 {
		return nil, "", 0, fmt.Errorf("%w: apex SOA/NS required", ErrInvalid)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].key < items[j].key })
	out.Records = make([]Record, len(items))
	for i, r := range items {
		out.Records[i] = r.record
	}
	sort.Slice(items, func(i, j int) bool { return items[i].hashKey < items[j].hashKey })
	h := sha256.New()
	var fields [8]byte
	for _, r := range items {
		binary.BigEndian.PutUint32(fields[:4], uint32(len(r.hashKey))) //nolint:gosec // G115: canonical RR identity is bounded by 65535-byte wire and escaped names.
		binary.BigEndian.PutUint32(fields[4:], r.ttl)
		_, _ = h.Write(fields[:])
		_, _ = h.Write([]byte(r.hashKey))
	}
	out.Receipts = append([]Receipt(nil), a.Receipts...)
	receipts := map[string]bool{}
	for _, r := range out.Receipts {
		if r.ID == "" || len(r.ID) > 512 || receipts[r.ID] || !zonekinds.ValidDigest(r.SemanticDigest) || r.Principal.Key == "" || r.Principal.Version == "" || len(r.Principal.Version) > 512 || r.CommitSequence == 0 || r.Rcode < 0 || r.Rcode > 15 {
			return nil, "", 0, fmt.Errorf("%w: receipt", ErrInvalid)
		}
		receipts[r.ID] = true
	}
	sort.Slice(out.Receipts, func(i, j int) bool { return out.Receipts[i].ID < out.Receipts[j].ID })
	return &out, hex.EncodeToString(h.Sum(nil)), serial, nil
}

func chunk(data []byte) *zonekinds.ZoneChunk {
	digest := zonekinds.Digest(data)
	return &zonekinds.ZoneChunk{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: zonekinds.KindZoneChunk}, ObjectMeta: metav1.ObjectMeta{Name: zonekinds.ChunkName(digest)}, Spec: zonekinds.ZoneChunkSpec{FormatVersion: zonekinds.FormatVersion, Digest: digest, Data: append([]byte(nil), data...)}}
}

// Prepare bounds and deterministically encodes a complete aggregate. Data
// changes MUST carry a greater serial; metadata changes MUST retain it.
func Prepare(a *Aggregate, base *zonekinds.ZoneState, l Limits) (*Prepared, error) {
	l = l.Defaults()
	n, hash, serial, err := normalized(a, l)
	if err != nil {
		return nil, err
	}
	policyData, err := json.Marshal(n.Policy)
	if err != nil {
		return nil, err
	}
	authDigest := zonekinds.Digest(policyData)
	sequence := uint64(1)
	notifySequence := uint64(0)
	notifySerial := uint32(0)
	if base != nil {
		if err := base.Validate(); err != nil || base.Spec.Zone != a.Zone || base.Spec.CommitSequence == math.MaxUint64 {
			return nil, ErrInvalid
		}
		sequence = base.Spec.CommitSequence + 1
		notifySequence = base.Spec.NotifySequence
		notifySerial = base.Spec.NotifySerial
		if n.Policy.AuthEpoch < base.Spec.AuthEpoch {
			return nil, fmt.Errorf("%w: auth epoch regressed", ErrInvalid)
		}
		if authDigest != base.Spec.AuthDigest && n.Policy.AuthEpoch <= base.Spec.AuthEpoch {
			return nil, fmt.Errorf("%w: changed policy requires greater auth epoch", ErrInvalid)
		}
		if hash != base.Spec.ContentHash {
			if !dynamicupdate.RFC1982Greater(serial, base.Spec.Serial) {
				return nil, fmt.Errorf("%w: changed data requires greater serial", ErrInvalid)
			}
			notifySequence = sequence
			notifySerial = serial
		} else if serial != base.Spec.Serial {
			// An explicit greater SOA serial is itself a visible zone change.
			if !dynamicupdate.RFC1982Greater(serial, base.Spec.Serial) {
				return nil, fmt.Errorf("%w: serial regressed", ErrInvalid)
			}
			notifySequence = sequence
			notifySerial = serial
		}
	}
	data, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > l.MaxAggregateBytes {
		return nil, ErrCapacity
	}
	m := manifest{FormatVersion: zonekinds.FormatVersion, Zone: a.Zone, Bytes: int64(len(data))}
	chunks := map[string]*zonekinds.ZoneChunk{}
	for off := 0; off < len(data); off += zonekinds.MaxChunkBytes {
		end := min(off+zonekinds.MaxChunkBytes, len(data))
		c := chunk(data[off:end])
		chunks[c.Spec.Digest] = c
		m.Parts = append(m.Parts, c.Spec.Digest)
		if len(m.Parts)+1 > l.MaxChunks {
			return nil, ErrCapacity
		}
	}
	mdata, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if len(mdata) > zonekinds.MaxChunkBytes {
		return nil, ErrCapacity
	}
	root := chunk(mdata)
	chunks[root.Spec.Digest] = root
	prepared := &Prepared{Head: &zonekinds.ZoneState{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: zonekinds.KindZoneState}, ObjectMeta: metav1.ObjectMeta{Name: zonekinds.HeadName(a.Zone)}, Spec: zonekinds.ZoneStateSpec{FormatVersion: zonekinds.FormatVersion, Zone: a.Zone, CommitSequence: sequence, Serial: serial, AuthEpoch: n.Policy.AuthEpoch, AuthDigest: authDigest, RootDigest: root.Spec.Digest, ContentHash: hash, Lifecycle: zonekinds.LifecycleActive, NotifySequence: notifySequence, NotifySerial: notifySerial}}}
	for _, c := range chunks {
		prepared.Chunks = append(prepared.Chunks, c)
	}
	sort.Slice(prepared.Chunks, func(i, j int) bool { return prepared.Chunks[i].Spec.Digest < prepared.Chunks[j].Spec.Digest })
	return prepared, nil
}

// Decode verifies a flat manifest, all digests/identity/counts and the entire
// aggregate before returning usable data. It never follows arbitrary trees.
func Decode(head *zonekinds.ZoneState, get func(string) (*zonekinds.ZoneChunk, error), l Limits) (*Aggregate, error) {
	l = l.Defaults()
	if head == nil || get == nil {
		return nil, ErrInvalid
	}
	if err := head.Validate(); err != nil {
		return nil, err
	}
	root, err := get(head.Spec.RootDigest)
	if err != nil {
		return nil, err
	}
	if root == nil || root.Validate() != nil || root.Spec.Digest != head.Spec.RootDigest {
		return nil, ErrInvalid
	}
	var m manifest
	if err := strictDecode(root.Spec.Data, &m); err != nil {
		return nil, err
	}
	if m.FormatVersion != zonekinds.FormatVersion || m.Zone != head.Spec.Zone || m.Bytes <= 0 || m.Bytes > l.MaxAggregateBytes || len(m.Parts) == 0 || len(m.Parts) >= l.MaxChunks {
		return nil, ErrCapacity
	}
	var data bytes.Buffer
	for _, digest := range m.Parts {
		if !zonekinds.ValidDigest(digest) {
			return nil, ErrInvalid
		}
		c, err := get(digest)
		if err != nil {
			return nil, err
		}
		if c == nil || c.Spec.Digest != digest || c.Validate() != nil {
			return nil, ErrInvalid
		}
		if int64(len(c.Spec.Data)) > m.Bytes-int64(data.Len()) {
			return nil, ErrCapacity
		}
		_, _ = data.Write(c.Spec.Data)
	}
	if int64(data.Len()) != m.Bytes {
		return nil, ErrInvalid
	}
	var a Aggregate
	if err := strictDecodeAggregate(data.Bytes(), &a, l); err != nil {
		return nil, err
	}
	n, hash, serial, err := normalized(&a, l)
	if err != nil {
		return nil, err
	}
	policyData, err := json.Marshal(n.Policy)
	if err != nil {
		return nil, err
	}
	if n.Zone != head.Spec.Zone || hash != head.Spec.ContentHash || serial != head.Spec.Serial || n.Policy.AuthEpoch != head.Spec.AuthEpoch || zonekinds.Digest(policyData) != head.Spec.AuthDigest {
		return nil, fmt.Errorf("%w: aggregate/head mismatch", ErrInvalid)
	}
	return n, nil
}

func strictDecode(data []byte, into any) error {
	if _, err := canonicalJSON(data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}
