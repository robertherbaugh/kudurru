package inbound

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ObjectEvidence binds an admitted object incarnation and exact desired intent.
// It is evidence only: the production transaction must reread the current object.
type ObjectEvidence struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	IntentDigest    string `json:"intentDigest"`
}

// TargetEvidence includes the canonical authority apex and original object token.
type TargetEvidence struct {
	Object ObjectEvidence `json:"object"`
	Apex   string         `json:"apex"`
}

// AllocationEvidence records the exact durable reservation used by translation.
type AllocationEvidence struct {
	Object ObjectEvidence                      `json:"object"`
	Spec   kinds.IntegrationNameAllocationSpec `json:"spec"`
}

// GroupContribution is a private MAC-only overlay, not a public spec mutation.
type GroupContribution struct {
	Group ObjectEvidence `json:"group"`
	MACs  []string       `json:"macs"`
}

// ReverseObligation preserves pending lifecycle/parent witnesses verbatim.
// Root interprets the witness through the existing primary runtime contract.
type ReverseObligation struct {
	Target        TargetEvidence `json:"target"`
	Phase         string         `json:"phase"`
	WitnessDigest string         `json:"witnessDigest"`
}

// Document is one complete original source observation and durable evidence.
// No credentials, store handles, or query callbacks are part of this codec.
type Document struct {
	FormatVersion int                  `json:"formatVersion"`
	Owner         kinds.Owner          `json:"owner"`
	Snapshot      host.SourceSnapshot  `json:"snapshot"`
	Targets       []TargetEvidence     `json:"targets,omitempty"`
	Objects       []ObjectEvidence     `json:"objects,omitempty"`
	Allocations   []AllocationEvidence `json:"allocations,omitempty"`
	Groups        []GroupContribution  `json:"groups,omitempty"`
	Reverse       []ReverseObligation  `json:"reverse,omitempty"`
}

// SourceImage is an isolated head and immutable chunk set ready for persistence.
type SourceImage struct {
	Head   *kinds.IntegrationSourceHead
	Chunks []*kinds.IntegrationSourceChunk
}

func boundedText(s string, n int) bool {
	return s != "" && len(s) <= n && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
}
func safeName(name string) bool {
	_, ok := dns.IsDomainName(name)
	return ok && len(name) <= 254 && name == dns.CanonicalName(name) && !strings.ContainsAny(name, " \\*\t\r\n\x00")
}
func evidenceValid(o ObjectEvidence, kind string) bool {
	return o.Kind == kind && len(validation.IsDNS1123Subdomain(o.Name)) == 0 && (o.Namespace == "" || len(validation.IsDNS1123Label(o.Namespace)) == 0) && boundedText(o.UID, 256) && boundedText(o.ResourceVersion, 256) && kinds.ValidDigest(o.IntentDigest)
}
func evidenceKey(o ObjectEvidence) string { return o.Kind + "\x00" + o.Namespace + "\x00" + o.Name }
func targetValid(t TargetEvidence) bool {
	return evidenceValid(t.Object, "DNSZone") && t.Object.Namespace == "" && safeName(t.Apex) && t.Apex != "."
}

// IntentDigest hashes an original desired spec without UID/RV/status noise.
func IntentDigest(spec any) (string, error) {
	if err := checkValueBounds(spec); err != nil {
		return "", err
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return kinds.Digest(b), nil
}

// ValidateDocument validates at ORIGINAL admission time, never at restore now.
// Current object authority and lifecycle receipts remain the adapter's duty.
func ValidateDocument(d Document, at time.Time) error {
	if err := checkDocumentBounds(d); err != nil {
		return err
	}
	if _, err := at.MarshalJSON(); err != nil {
		return errDocumentBound
	}
	invalid := func() error { return errors.New("inbound: invalid source document") }
	if d.FormatVersion != kinds.FormatVersion || d.Owner.Validate() != nil || at.IsZero() || !d.Snapshot.Complete || len(d.Snapshot.Clients) > kinds.MaxSourceClients || len(d.Targets) > kinds.MaxSourceZones+1 || len(d.Objects) > kinds.MaxSourceRecordSets+kinds.MaxSourceZones || len(d.Allocations) > kinds.MaxSourceAllocations || len(d.Groups) > kinds.MaxSourceGroups || len(d.Reverse) > kinds.MaxSourceZones {
		return invalid()
	}
	source := host.SourceID(d.Owner)
	verifier := inventory.New(func() time.Time { return at })
	for _, c := range d.Snapshot.Clients {
		if c.Source != source {
			return invalid()
		}
	}
	if err := verifier.Replace(source, 1, d.Snapshot.Clients); err != nil {
		return invalid()
	}
	// Validation cannot silently normalize the original persisted authority.
	normalized := map[string]inventory.Client{}
	for _, c := range verifier.Snapshot().Clients() {
		normalized[c.ID] = c
	}
	for _, c := range d.Snapshot.Clients {
		other := normalized[c.ID]
		if len(c.IPs) == 0 {
			c.IPs = nil
		}
		if len(other.IPs) == 0 {
			other.IPs = nil
		}
		a, err := json.Marshal(c)
		if err != nil {
			return invalid()
		}
		b, err := json.Marshal(other)
		if err != nil || !bytes.Equal(a, b) {
			return invalid()
		}
	}
	targets := map[string]TargetEvidence{}
	objects := map[string]ObjectEvidence{}
	for _, t := range d.Targets {
		if !targetValid(t) || targets[t.Object.Name].Object.UID != "" {
			return invalid()
		}
		targets[t.Object.Name] = t
	}
	for _, o := range d.Objects {
		if o.Kind != "DNSZone" && o.Kind != "DNSRecordSet" || !evidenceValid(o, o.Kind) || objects[evidenceKey(o)].UID != "" || o.Kind == "DNSZone" && o.Namespace != "" || o.Kind == "DNSRecordSet" && o.Namespace != d.Owner.Namespace {
			return invalid()
		}
		objects[evidenceKey(o)] = o
	}
	allocationNames := map[string]bool{}
	allocatedOwners := map[string]AllocationEvidence{}
	clientsByID := map[string]inventory.Client{}
	for _, c := range d.Snapshot.Clients {
		clientsByID[c.ID] = c
	}
	for _, a := range d.Allocations {
		if !evidenceValid(a.Object, kinds.KindIntegrationNameAllocation) || a.Object.Namespace != "" || a.Spec.Owner != d.Owner || a.Spec.CreatedAt.After(at) {
			return invalid()
		}
		carrier := &kinds.IntegrationNameAllocation{ObjectMeta: metav1.ObjectMeta{Name: a.Object.Name}, Spec: a.Spec}
		digest, _ := IntentDigest(a.Spec)
		if carrier.Validate() != nil || digest != a.Object.IntentDigest || !AllocatedLabel(a.Spec.Base, a.Spec.Label) {
			return invalid()
		}
		if allocationNames[a.Object.Name] || clientsByID[a.Spec.ClientID].ID == "" {
			return invalid()
		}
		allocationNames[a.Object.Name] = true
		name := a.Spec.Label + "." + a.Spec.Scope.Apex
		if _, exists := allocatedOwners[name]; exists {
			return invalid()
		}
		allocatedOwners[name] = a
		found := false
		for _, t := range d.Targets {
			if t.Object.UID == a.Spec.Scope.ZoneUID && t.Apex == a.Spec.Scope.Apex {
				found = true
			}
		}
		if !found {
			return invalid()
		}
	}
	seen := map[string]bool{}
	for _, r := range d.Reverse {
		t, ok := targets[r.Target.Object.Name]
		if !ok || t != r.Target || seen[r.Target.Object.Name] || !kinds.ValidDigest(r.WitnessDigest) || r.Phase != "preparing" && r.Phase != "fencing" && r.Phase != "active" && r.Phase != "retiring" && r.Phase != "retired" {
			return invalid()
		}
		seen[r.Target.Object.Name] = true
	}
	batch := d.Snapshot.Desired
	if batch == nil {
		if len(d.Targets)+len(d.Objects)+len(d.Allocations)+len(d.Groups)+len(d.Reverse) > 0 {
			return invalid()
		}
		return nil
	}
	if batch.SourceID != source || len(batch.Zones) > kinds.MaxSourceZones || len(batch.RecordSets) > kinds.MaxSourceRecordSets || len(batch.ClientGroups) > kinds.MaxSourceGroups {
		return invalid()
	}
	used := map[string]bool{}
	for _, z := range batch.Zones {
		if z == nil || z.Namespace != "" || z.Spec.Type != public.ZoneTypePrimary {
			return invalid()
		}
		t, ok := targets[z.Name]
		o := objects["DNSZone\x00\x00"+z.Name]
		digest, _ := IntentDigest(z.Spec)
		if !ok || t.Object != o || t.Apex != dns.CanonicalName(z.Spec.ZoneName) || digest != o.IntentDigest || used[evidenceKey(o)] {
			return invalid()
		}
		used[evidenceKey(o)] = true
	}
	endpoints := 0
	ownerTypes := map[string]map[string]string{}
	for _, r := range batch.RecordSets {
		if r == nil || r.Namespace != d.Owner.Namespace || r.Spec.ZoneRef.Namespace != "" || len(r.Spec.Endpoints) == 0 || len(r.Spec.Endpoints) > 1000 {
			return invalid()
		}
		o := objects["DNSRecordSet\x00"+r.Namespace+"\x00"+r.Name]
		digest, _ := IntentDigest(r.Spec)
		t, ok := targets[r.Spec.ZoneRef.Name]
		if !ok || digest != o.IntentDigest || used[evidenceKey(o)] {
			return invalid()
		}
		used[evidenceKey(o)] = true
		for _, e := range r.Spec.Endpoints {
			endpoints++
			if endpoints > kinds.MaxSourceRecordSets*16 || !safeName(e.DNSName) || !dns.IsSubDomain(t.Apex, e.DNSName) || e.SetIdentifier != "" || len(e.Labels)+len(e.ProviderSpecific) > 0 || e.Kudurru == nil || e.Kudurru.ExpiresAt == nil || !e.Kudurru.ExpiresAt.After(at.Add(-24*time.Hour)) || e.Kudurru.ExpiresAt.After(at.Add(24*time.Hour)) || e.RecordTTL <= 0 || e.RecordTTL > 86400 || len(e.Targets) == 0 || len(e.Targets) > 16 {
				return invalid()
			}
			key := t.Object.UID + "\x00" + e.DNSName
			if ownerTypes[key] == nil {
				ownerTypes[key] = map[string]string{}
			}
			types := ownerTypes[key]
			if (e.RecordType == "CNAME" && (len(e.Targets) != 1 || len(types) > 0 && types["CNAME"] == "")) || (e.RecordType != "CNAME" && types["CNAME"] != "") || types[e.RecordType] != "" && types[e.RecordType] != o.UID {
				return invalid()
			}
			types[e.RecordType] = o.UID
			if !endpointAllocationValid(e, t, allocatedOwners, clientsByID) {
				return invalid()
			}
			switch e.RecordType {
			case "A", "AAAA", "PTR", "CNAME", "TXT", "MX", "SRV", "CAA":
			default:
				return invalid()
			}
			for _, target := range e.Targets {
				if !boundedText(target, 8192) {
					return invalid()
				}
				rr, err := dns.NewRR(e.DNSName + " " + intTTL(e.RecordTTL) + " IN " + e.RecordType + " " + target)
				if err != nil || rr.Header().Name != e.DNSName || rr.Header().Class != dns.ClassINET {
					return invalid()
				}
				if _, err := dns.PackRR(rr, make([]byte, dns.Len(rr)), 0, nil, false); err != nil {
					return invalid()
				}
			}
		}
	}
	if len(used) != len(objects) {
		return invalid()
	}
	macs := map[string]bool{}
	for _, c := range verifier.Snapshot().Clients() {
		for _, m := range c.MACs {
			macs[m] = true
		}
	}
	contributions := map[string]GroupContribution{}
	for _, g := range d.Groups {
		if !evidenceValid(g.Group, "ClientGroup") || g.Group.Namespace != d.Owner.Namespace || len(g.MACs) > kinds.MaxEffectiveMembers || contributions[evidenceKey(g.Group)].Group.UID != "" {
			return invalid()
		}
		seenMAC := map[string]bool{}
		for _, mac := range g.MACs {
			m, err := net.ParseMAC(mac)
			if err != nil || len(m) != 6 || m.String() != mac || !macs[mac] || seenMAC[mac] {
				return invalid()
			}
			seenMAC[mac] = true
		}
		contributions[evidenceKey(g.Group)] = g
	}
	seenGroups := map[string]bool{}
	for _, g := range batch.ClientGroups {
		if g == nil || g.Namespace != d.Owner.Namespace || g.Spec.Sources == nil || g.Spec.Sources.IntegrationRef == nil || g.Spec.Sources.IntegrationRef.Name != d.Owner.Name || g.Spec.Sources.IntegrationRef.Namespace != "" || len(g.Spec.Clients) > kinds.MaxEffectiveMembers {
			return invalid()
		}
		key := "ClientGroup\x00" + g.Namespace + "\x00" + g.Name
		contribution, ok := contributions[key]
		if !ok || seenGroups[key] {
			return invalid()
		}
		seenGroups[key] = true
		expected := slices.Clone(contribution.MACs)
		slices.Sort(expected)
		var got []string
		for _, m := range g.Spec.Clients {
			if m.MAC == "" || m.IP != "" || m.CIDR != "" || m.ClientID != "" || m.Hostname != "" || m.NameGlob != "" || m.Name != "" || m.Comment != "" {
				return invalid()
			}
			got = append(got, m.MAC)
		}
		slices.Sort(got)
		if !slices.Equal(got, expected) {
			return invalid()
		}
	}
	if len(seenGroups) != len(contributions) {
		return invalid()
	}
	return nil
}

// EncodeSource produces immutable digest-bound chunks; inputs are never mutated.
func EncodeSource(d Document, sequence uint64, configDigest, credentialDigest string, at time.Time) (SourceImage, error) {
	if sequence == 0 || !kinds.ValidDigest(configDigest) || !kinds.ValidDigest(credentialDigest) {
		return SourceImage{}, errors.New("inbound: invalid source version")
	}
	if err := ValidateDocument(d, at); err != nil {
		return SourceImage{}, err
	}
	data, err := json.Marshal(d)
	if err != nil || len(data) > kinds.MaxSourceBytes || len(data) > kinds.MaxSourceChunks*kinds.MaxSourceChunkBytes {
		return SourceImage{}, errors.New("inbound: source encoding limit")
	}
	head := &kinds.IntegrationSourceHead{ObjectMeta: metav1.ObjectMeta{Name: kinds.SourceHeadName(d.Owner)}, Spec: kinds.IntegrationSourceHeadSpec{FormatVersion: kinds.FormatVersion, Owner: d.Owner, Sequence: sequence, ConfigDigest: configDigest, CredentialDigest: credentialDigest, RootDigest: kinds.Digest(data), AcceptedAt: at, State: "active"}}
	out := SourceImage{Head: head}
	for offset := 0; offset < len(data); offset += kinds.MaxSourceChunkBytes {
		part := slices.Clone(data[offset:min(len(data), offset+kinds.MaxSourceChunkBytes)])
		i := len(out.Chunks)
		digest := kinds.Digest(part)
		chunk := &kinds.IntegrationSourceChunk{ObjectMeta: metav1.ObjectMeta{Name: kinds.SourceChunkName(d.Owner, sequence, i, digest)}, Spec: kinds.IntegrationSourceChunkSpec{FormatVersion: kinds.FormatVersion, Owner: d.Owner, Sequence: sequence, Index: i, Digest: digest, Data: part}}
		if err := chunk.Validate(); err != nil {
			return SourceImage{}, err
		}
		out.Chunks = append(out.Chunks, chunk)
		head.Spec.Chunks = append(head.Spec.Chunks, kinds.SourceChunkRef{Name: chunk.Name, Digest: digest, Bytes: len(part)})
	}
	if err := head.Validate(); err != nil {
		return SourceImage{}, err
	}
	return out, nil
}

// DecodeSource validates the exact complete set before decoding, including age-
// independent original evidence. Extra, missing, mixed or tampered chunks reject.
func DecodeSource(image SourceImage) (Document, error) {
	if image.Head == nil || image.Head.Validate() != nil || len(image.Chunks) != len(image.Head.Spec.Chunks) {
		return Document{}, errors.New("inbound: invalid source image")
	}
	h := image.Head.Spec
	chunks := map[string]*kinds.IntegrationSourceChunk{}
	for _, c := range image.Chunks {
		if c == nil || c.Validate() != nil || c.Spec.Owner != h.Owner || c.Spec.Sequence != h.Sequence || chunks[c.Name] != nil {
			return Document{}, errors.New("inbound: invalid source chunk set")
		}
		chunks[c.Name] = c
	}
	var b bytes.Buffer
	for i, ref := range h.Chunks {
		c := chunks[ref.Name]
		if c == nil || c.Spec.Index != i || len(c.Spec.Data) != ref.Bytes || c.Spec.Digest != ref.Digest {
			return Document{}, errors.New("inbound: incomplete source image")
		}
		b.Write(c.Spec.Data)
	}
	if b.Len() > kinds.MaxSourceBytes || kinds.Digest(b.Bytes()) != h.RootDigest {
		return Document{}, errors.New("inbound: source root mismatch")
	}
	d, err := strictDocumentJSON(b.Bytes())
	if err != nil {
		return Document{}, err
	}
	if d.Owner != h.Owner {
		return Document{}, errors.New("inbound: source owner mismatch")
	}
	if err := ValidateDocument(d, h.AcceptedAt); err != nil {
		return Document{}, err
	}
	return d, nil
}

func intTTL(n int64) string { return strconv.FormatInt(n, 10) }

func endpointAllocationValid(e public.Endpoint, t TargetEvidence, allocations map[string]AllocationEvidence, clients map[string]inventory.Client) bool {
	if e.RecordType == "PTR" {
		for _, target := range e.Targets {
			allocation, ok := allocations[target]
			if !ok {
				return false
			}
			c := clients[allocation.Spec.ClientID]
			if !e.Kudurru.ExpiresAt.Time.Equal(c.ExpiresAt) {
				return false
			}
			matches := false
			for _, ip := range c.IPs {
				reverse, err := dns.ReverseAddr(ip.String())
				if err == nil && reverse == e.DNSName {
					matches = true
					break
				}
			}
			if !matches {
				return false
			}
		}
		return true
	}
	allocation, ok := allocations[e.DNSName]
	if !ok || allocation.Spec.Scope.ZoneUID != t.Object.UID || allocation.Spec.Scope.Apex != t.Apex {
		return false
	}
	c := clients[allocation.Spec.ClientID]
	if !e.Kudurru.ExpiresAt.Time.Equal(c.ExpiresAt) {
		return false
	}
	if e.RecordType == "A" || e.RecordType == "AAAA" {
		for _, target := range e.Targets {
			matched := false
			for _, ip := range c.IPs {
				if ip.String() == target && (ip.Is4() == (e.RecordType == "A")) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}
