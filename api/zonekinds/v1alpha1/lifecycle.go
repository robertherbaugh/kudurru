package v1alpha1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Lifecycle constants bound the private ledger and name its finite phases.
const (
	KindZoneLifecycle = "ZoneLifecycle"
	MaxLifecycleBytes = 128 << 10
	MaxParentFences   = 128
	PhasePreparing    = "preparing"
	PhaseFencing      = "fencing"
	PhaseActive       = "active"
	PhaseRetiring     = "retiring"
	PhaseRetired      = "retired"
)

// LifecycleSource is an actually observed equality token, never an ordered RV.
type LifecycleSource struct {
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	IntentDigest    string `json:"intentDigest"`
	Removed         bool   `json:"removed,omitempty"`
}

// ParentFence binds an ancestor head and incarnation to this child's boundary.
// Sequence/root are empty until installation; Released is durable removal proof.
type ParentFence struct {
	Zone           ZoneID `json:"zone"`
	Incarnation    uint64 `json:"incarnation"`
	BoundaryID     string `json:"boundaryID"`
	CommitSequence uint64 `json:"commitSequence,omitempty"`
	RootDigest     string `json:"rootDigest,omitempty"`
	Released       bool   `json:"released,omitempty"`
}

// ZoneLifecycle is a private CAS ledger. ZoneState alone owns the RR data.
type ZoneLifecycle struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ZoneLifecycleSpec `json:"spec"`
}

// ZoneLifecycleSpec binds membership to source equality and ancestor fences.
type ZoneLifecycleSpec struct {
	FormatVersion int             `json:"formatVersion"`
	Zone          ZoneID          `json:"zone"`
	Type          public.ZoneType `json:"type"`
	Incarnation   uint64          `json:"incarnation"`
	Sequence      uint64          `json:"sequence"`
	Phase         string          `json:"phase"`
	Source        LifecycleSource `json:"source"`
	Parents       []ParentFence   `json:"parents,omitempty"`
}

// ZoneLifecycleList carries private ledgers through existing object drivers.
type ZoneLifecycleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZoneLifecycle `json:"items"`
}

// LifecycleName is stable across UID/type/apex replacements of one source key.
func LifecycleName(cluster, object string) string {
	return digestName("zl-", Digest([]byte(cluster+"\x00"+object)))
}

// HeadZone allocates a fresh RR authority for every later incarnation. The
// first retains the existing UID for migration. Never reactivate a tombstone.
// Runtime adapters freeze this synthetic UID into the generation's config;
// Spec.Zone and Spec.Source continue to bind the actual desired object UID.
func (s ZoneLifecycleSpec) HeadZone() ZoneID {
	id := s.Zone
	if s.Incarnation > 1 {
		id.UID = "zl-" + Digest([]byte(s.Zone.UID+"\x00"+strconv.FormatUint(s.Incarnation, 10)))
	}
	return id
}

func validLifecycleID(id ZoneID) bool {
	for _, s := range []string{id.ClusterID, id.ObjectName, id.UID, id.Apex} {
		if s == "" || len(s) > 512 || strings.ContainsRune(s, 0) {
			return false
		}
	}
	_, ok := dns.IsDomainName(id.Apex)
	return ok && dns.IsFqdn(id.Apex) && dns.CanonicalName(id.Apex) == id.Apex
}
func phaseRank(phase string) int {
	switch phase {
	case PhasePreparing:
		return 1
	case PhaseFencing:
		return 2
	case PhaseActive:
		return 3
	case PhaseRetiring:
		return 4
	case PhaseRetired:
		return 5
	}
	return 0
}

// Validate checks the complete bounded lifecycle carrier.
func (s *ZoneLifecycle) Validate() error {
	if s == nil || s.Spec.FormatVersion != FormatVersion || !validLifecycleID(s.Spec.Zone) || s.Namespace != "" || s.Name != LifecycleName(s.Spec.Zone.ClusterID, s.Spec.Zone.ObjectName) || s.Spec.Incarnation == 0 || s.Spec.Sequence == 0 || phaseRank(s.Spec.Phase) == 0 || len(s.Spec.Parents) > MaxParentFences {
		return fmt.Errorf("invalid ZoneLifecycle identity/version/phase/bounds")
	}
	switch s.Spec.Type {
	case public.ZoneTypePrimary, public.ZoneTypeSecondary, public.ZoneTypeStub, public.ZoneTypeForward, public.ZoneTypeExternal:
	default:
		return fmt.Errorf("invalid ZoneLifecycle type")
	}
	t := s.Spec.Source
	if t.UID != s.Spec.Zone.UID || t.ResourceVersion == "" || len(t.ResourceVersion) > 512 || strings.ContainsRune(t.ResourceVersion, 0) || !ValidDigest(t.IntentDigest) || (t.Removed && phaseRank(s.Spec.Phase) < phaseRank(PhaseRetiring)) {
		return fmt.Errorf("invalid ZoneLifecycle source token")
	}
	seen := map[ZoneID]bool{}
	for _, p := range s.Spec.Parents {
		if !validLifecycleID(p.Zone) || p.Zone.ClusterID != s.Spec.Zone.ClusterID || p.Zone.Apex == s.Spec.Zone.Apex || !dns.IsSubDomain(p.Zone.Apex, s.Spec.Zone.Apex) || p.Incarnation == 0 || !ValidDigest(p.BoundaryID) || seen[p.Zone] || (p.CommitSequence == 0) != (p.RootDigest == "") || (p.RootDigest != "" && !ValidDigest(p.RootDigest)) || (p.Released && s.Spec.Phase != PhaseRetired) || (s.Spec.Phase == PhaseActive && (p.CommitSequence == 0 || p.Released)) {
			return fmt.Errorf("invalid ZoneLifecycle parent obligation")
		}
		seen[p.Zone] = true
	}
	b, err := json.Marshal(s)
	if err != nil || len(b) > MaxLifecycleBytes {
		return fmt.Errorf("ZoneLifecycle exceeds byte bound")
	}
	return nil
}

// ValidateLifecycleTransition checks actual current identity AND exact RV. The
// admission driver must run it inside the same source/HA/object CAS boundary.
func ValidateLifecycleTransition(current *ZoneLifecycle, expectedRV string, next *ZoneLifecycle) error {
	if next.Validate() != nil {
		return fmt.Errorf("invalid next lifecycle")
	}
	if current == nil {
		if expectedRV != "" || next.Spec.Sequence != 1 || next.Spec.Incarnation != 1 || next.Spec.Phase != PhasePreparing {
			return fmt.Errorf("invalid lifecycle initialization")
		}
		return nil
	}
	if current.Validate() != nil || expectedRV == "" || current.ResourceVersion != expectedRV || current.Name != next.Name || current.Spec.Sequence == math.MaxUint64 || next.Spec.Sequence != current.Spec.Sequence+1 {
		return fmt.Errorf("lifecycle equality CAS mismatch")
	}
	a, b := current.Spec, next.Spec
	if b.Incarnation == a.Incarnation {
		if b.Zone != a.Zone || b.Type != a.Type || phaseRank(b.Phase) < phaseRank(a.Phase) || (phaseRank(b.Phase) > phaseRank(a.Phase)+1 && b.Phase != PhaseRetiring) {
			return fmt.Errorf("invalid lifecycle phase transition")
		}
		if a.Phase == PhaseRetired && b.Phase != PhaseRetired {
			return fmt.Errorf("retired incarnation")
		}
		parents := make(map[ZoneID]ParentFence, len(b.Parents))
		for _, p := range b.Parents {
			parents[p.Zone] = p
		}
		for _, p := range a.Parents {
			q, ok := parents[p.Zone]
			if !ok {
				return fmt.Errorf("parent obligation removed within incarnation")
			}
			if p.Zone != q.Zone || p.Incarnation != q.Incarnation || p.BoundaryID != q.BoundaryID || q.CommitSequence < p.CommitSequence || (q.CommitSequence == p.CommitSequence && q.RootDigest != p.RootDigest) || (p.Released && !q.Released) {
				return fmt.Errorf("parent obligation regressed")
			}
		}
	} else if a.Incarnation == math.MaxUint64 || b.Incarnation != a.Incarnation+1 || a.Phase != PhaseRetired || b.Phase != PhasePreparing {
		return fmt.Errorf("invalid replacement incarnation")
	} else {
		for _, p := range a.Parents {
			if !p.Released {
				return fmt.Errorf("replacement before ancestor release")
			}
		}
	}
	return nil
}

// DecodeZoneLifecycle rejects unknown versions/fields, trailing data and
// oversized carriers before allocation. Invalid carriers never authorize prune.
func DecodeZoneLifecycle(raw []byte) (*ZoneLifecycle, error) {
	if len(raw) > MaxLifecycleBytes {
		return nil, fmt.Errorf("ZoneLifecycle exceeds byte bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var s ZoneLifecycle
	if err := d.Decode(&s); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing lifecycle data")
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// UnmarshalJSON enforces the same strict carrier contract through BOTH existing
// generic object drivers, before unknown fields can be silently discarded.
func (s *ZoneLifecycle) UnmarshalJSON(raw []byte) error {
	if len(raw) > MaxLifecycleBytes {
		return fmt.Errorf("ZoneLifecycle exceeds byte bound")
	}
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(raw)), 0); err != nil {
		return err
	}
	type plain ZoneLifecycle
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var out plain
	if err := d.Decode(&out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing lifecycle data")
	}
	decoded := ZoneLifecycle(out)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*s = decoded
	return nil
}
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("lifecycle JSON nesting exceeded")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return fmt.Errorf("duplicate lifecycle JSON key")
				}
				seen[name] = true
				if err := uniqueJSON(d, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := uniqueJSON(d, depth+1); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("invalid lifecycle JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	return nil
}

// DeepCopy returns detached lifecycle metadata and obligations.
func (s *ZoneLifecycle) DeepCopy() *ZoneLifecycle {
	if s == nil {
		return nil
	}
	out := *s
	out.ObjectMeta = *s.ObjectMeta.DeepCopy()
	out.Spec.Parents = append([]ParentFence(nil), s.Spec.Parents...)
	return &out
}

// DeepCopyObject implements runtime.Object.
func (s *ZoneLifecycle) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	return s.DeepCopy()
}

// DeepCopyObject implements runtime.Object.
func (s *ZoneLifecycleList) DeepCopyObject() runtime.Object {
	if s == nil {
		return nil
	}
	out := *s
	s.DeepCopyInto(&out.ListMeta)
	out.Items = make([]ZoneLifecycle, len(s.Items))
	for i := range s.Items {
		out.Items[i] = *s.Items[i].DeepCopy()
	}
	return &out
}
