package v1alpha1

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Source runtime kinds are private, cluster-scoped, and license-independent.
const (
	KindIntegrationSourceHead     = "IntegrationSourceHead"
	KindIntegrationSourceChunk    = "IntegrationSourceChunk"
	KindIntegrationNameAllocation = "IntegrationNameAllocation"
	MaxSourceChunks               = 128
	MaxSourceBytes                = 32 << 20
	MaxSourceChunkBytes           = 180 << 10 // leaves room for base64 and carrier metadata
	MaxSourceClients              = 10000
	MaxSourceRecordSets           = 10000
	MaxSourceGroups               = 256
	MaxSourceZones                = 256
	MaxSourceCount                = 128
	MaxSourceAllocations          = 10000
	MaxClusterAllocations         = 50000
	MaxEffectiveMembers           = 1000
)

// SourceKinds lists the source carriers included in the full private Kinds set.
var SourceKinds = []string{KindIntegrationSourceHead, KindIntegrationSourceChunk, KindIntegrationNameAllocation}

// AddSourceToScheme registers source carriers without changing public CRDs.
func AddSourceToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(public.GroupVersion, &IntegrationSourceHead{}, &IntegrationSourceHeadList{}, &IntegrationSourceChunk{}, &IntegrationSourceChunkList{}, &IntegrationNameAllocation{}, &IntegrationNameAllocationList{})
	metav1.AddToGroupVersion(s, public.GroupVersion)
	return nil
}

// SourceChunkRef binds ordered immutable bytes to one complete source version.
type SourceChunkRef struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Bytes  int    `json:"bytes"`
}

// IntegrationSourceHeadSpec points to a complete original observation/evidence.
// Retained heads preserve finite DNS evidence but confer no inventory/group grant.
type IntegrationSourceHeadSpec struct {
	FormatVersion    int              `json:"formatVersion"`
	Owner            Owner            `json:"owner"`
	Sequence         uint64           `json:"sequence,string"`
	ConfigDigest     string           `json:"configDigest"`
	CredentialDigest string           `json:"credentialDigest"`
	RootDigest       string           `json:"rootDigest"`
	AcceptedAt       time.Time        `json:"acceptedAt"`
	State            string           `json:"state"` // active, withdrawn, retained
	WithdrawnAt      time.Time        `json:"withdrawnAt,omitempty"`
	Chunks           []SourceChunkRef `json:"chunks"`
}

// IntegrationSourceChunkSpec contains opaque codec bytes, never credentials.
type IntegrationSourceChunkSpec struct {
	FormatVersion int    `json:"formatVersion"`
	Owner         Owner  `json:"owner"`
	Sequence      uint64 `json:"sequence,string"`
	Index         int    `json:"index"`
	Digest        string `json:"digest"`
	Data          []byte `json:"data"`
}

// AllocationScope never authorizes a recreated target UID.
type AllocationScope struct {
	ClusterID string `json:"clusterID"`
	ZoneUID   string `json:"zoneUID"`
	Apex      string `json:"apex"`
}

// IntegrationNameAllocationSpec permanently reserves a label for an incarnation.
// Omission is not reclamation. Base changes retain the prior reservation.
type IntegrationNameAllocationSpec struct {
	FormatVersion int             `json:"formatVersion"`
	Scope         AllocationScope `json:"scope"`
	Label         string          `json:"label"`
	Owner         Owner           `json:"owner"`
	ClientID      string          `json:"clientID"`
	Base          string          `json:"base"`
	CreatedAt     time.Time       `json:"createdAt"`
}

// IntegrationSourceHead is the private whole-source commit/publication pointer.
type IntegrationSourceHead struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntegrationSourceHeadSpec `json:"spec"`
}

// IntegrationSourceHeadList is the private head carrier list.
type IntegrationSourceHeadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntegrationSourceHead `json:"items"`
}

// IntegrationSourceChunk stores immutable bounded source bytes.
type IntegrationSourceChunk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntegrationSourceChunkSpec `json:"spec"`
}

// IntegrationSourceChunkList is the private chunk carrier list.
type IntegrationSourceChunkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntegrationSourceChunk `json:"items"`
}

// IntegrationNameAllocation reserves one relative name under immutable ownership.
type IntegrationNameAllocation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntegrationNameAllocationSpec `json:"spec"`
}

// IntegrationNameAllocationList is the private allocation carrier list.
type IntegrationNameAllocationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntegrationNameAllocation `json:"items"`
}

// SourceHeadName binds a durable head to one Integration incarnation.
func SourceHeadName(o Owner) string { return objectName("ish-", o) }

// SourceChunkName binds immutable chunks to owner, version, index, and content.
func SourceChunkName(o Owner, sequence uint64, index int, digest string) string {
	return objectName("isc-", struct {
		Owner    Owner
		Sequence uint64
		Index    int
		Digest   string
	}{o, sequence, index, digest})
}

// NameAllocationName binds reservations to target incarnation, apex, and label.
func NameAllocationName(scope AllocationScope, label string) string {
	return objectName("ina-", struct {
		Scope AllocationScope
		Label string
	}{scope, label})
}

// Validate rejects noncanonical target identity and wildcard/escaped apices.
func (s AllocationScope) Validate() error {
	if !bounded(s.ClusterID, 256) || !bounded(s.ZoneUID, 256) || s.Apex == "." || s.Apex != dns.CanonicalName(s.Apex) || len(s.Apex) > 254 {
		return fmt.Errorf("invalid allocation scope")
	}
	// Restrict the wire name to ordinary ASCII schema labels.
	for _, label := range dns.SplitDomainName(s.Apex) {
		if len(validation.IsDNS1123Label(label)) != 0 {
			return fmt.Errorf("invalid allocation apex")
		}
	}
	if _, ok := dns.IsDomainName(s.Apex); !ok {
		return fmt.Errorf("invalid allocation apex")
	}
	return nil
}

// Validate rejects malformed source head identity, references, and lifecycle.
func (s *IntegrationSourceHead) Validate() error {
	if s == nil {
		return fmt.Errorf("nil source head")
	}
	p := s.Spec
	if err := objectValid(s, p.FormatVersion, s.Name, s.Namespace, SourceHeadName(p.Owner)); err != nil {
		return err
	}
	if err := p.Owner.Validate(); err != nil {
		return err
	}
	if p.Sequence == 0 || !ValidDigest(p.ConfigDigest) || !ValidDigest(p.CredentialDigest) || !ValidDigest(p.RootDigest) || p.AcceptedAt.IsZero() || len(p.Chunks) == 0 || len(p.Chunks) > MaxSourceChunks {
		return fmt.Errorf("invalid source head")
	}
	if p.State != "active" && p.State != "withdrawn" && p.State != "retained" || (p.State == "active") != p.WithdrawnAt.IsZero() || !p.WithdrawnAt.IsZero() && p.WithdrawnAt.Before(p.AcceptedAt) {
		return fmt.Errorf("invalid source lifecycle")
	}
	total := 0
	for i, c := range p.Chunks {
		if !ValidDigest(c.Digest) || c.Bytes <= 0 || c.Bytes > MaxSourceChunkBytes || c.Name != SourceChunkName(p.Owner, p.Sequence, i, c.Digest) {
			return fmt.Errorf("invalid source chunk reference")
		}
		total += c.Bytes
	}
	if total > MaxSourceBytes {
		return fmt.Errorf("source size exceeded")
	}
	return nil
}

// Validate rejects malformed or tampered immutable source chunks.
func (s *IntegrationSourceChunk) Validate() error {
	if s == nil {
		return fmt.Errorf("nil source chunk")
	}
	p := s.Spec
	if err := objectValid(s, p.FormatVersion, s.Name, s.Namespace, SourceChunkName(p.Owner, p.Sequence, p.Index, p.Digest)); err != nil {
		return err
	}
	if err := p.Owner.Validate(); err != nil {
		return err
	}
	if p.Sequence == 0 || p.Index < 0 || p.Index >= MaxSourceChunks || len(p.Data) == 0 || len(p.Data) > MaxSourceChunkBytes || !ValidDigest(p.Digest) || Digest(p.Data) != p.Digest {
		return fmt.Errorf("invalid source chunk")
	}
	return nil
}

// Validate rejects malformed allocation identity or undocumented suffixes.
func (s *IntegrationNameAllocation) Validate() error {
	if s == nil {
		return fmt.Errorf("nil name allocation")
	}
	p := s.Spec
	if err := objectValid(s, p.FormatVersion, s.Name, s.Namespace, NameAllocationName(p.Scope, p.Label)); err != nil {
		return err
	}
	if err := p.Owner.Validate(); err != nil {
		return err
	}
	if err := p.Scope.Validate(); err != nil {
		return err
	}
	if p.Owner.ClusterID != p.Scope.ClusterID || !bounded(p.ClientID, 256) || len(validation.IsDNS1123Label(p.Label)) != 0 || len(validation.IsDNS1123Label(p.Base)) != 0 || p.CreatedAt.IsZero() || !AllocationLabelMatches(p.Base, p.Label) {
		return fmt.Errorf("invalid name allocation")
	}
	return nil
}

// AllocationLabelMatches accepts base or its canonical bounded numeric suffix.
func AllocationLabelMatches(base, label string) bool {
	if len(validation.IsDNS1123Label(base)) != 0 || len(validation.IsDNS1123Label(label)) != 0 {
		return false
	}
	if label == base {
		return true
	}
	i := strings.LastIndexByte(label, '-')
	if i < 0 {
		return false
	}
	n, err := strconv.ParseUint(label[i+1:], 10, 32)
	if err != nil || n < 2 || strconv.FormatUint(n, 10) != label[i+1:] {
		return false
	}
	suffix := label[i:]
	if len(base) > 63-len(suffix) {
		base = strings.TrimRight(base[:63-len(suffix)], "-")
	}
	return label == base+suffix
}
