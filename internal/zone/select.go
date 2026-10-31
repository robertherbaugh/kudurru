package zone

import (
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// The functions in this file decide which DNSRecordSets a zone is built
// from. The zone cache (internal/zoneview) and the controller
// (status.contentHash, D12) both call ServedRecordSets and then Build, so
// the hash the leader stamps is the hash of the zone every replica serves.

// ValidateEndpoint reports why an endpoint can never be served in any zone:
// a bad owner name, type, TTL or target. It is ParseEndpoint against the
// root origin, so it does not check that the owner is inside a particular
// zone (Build reports that as OutsideZone). It satisfies the controller's
// EndpointValidator seam.
func ValidateEndpoint(ep v1alpha1.Endpoint) error {
	_, errs := ParseEndpoint(".", DefaultTTL, &ep, field.NewPath("endpoint"))
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s %s: %w", ep.DNSName, ep.RecordType, errs.ToAggregate())
}

// CompareRecordSets orders record sets oldest first: creationTimestamp,
// then namespace, then name. The earlier object keeps a contested
// identity, so this order makes Duplicated deterministic.
func CompareRecordSets(a, b *v1alpha1.DNSRecordSet) int {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c
	}
	if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// RecordSetSelector compiles a zone's spec.recordSetSelector. A nil
// selector admits everything.
type RecordSetSelector struct {
	namespaces []string
	labels     labels.Selector
}

// NewRecordSetSelector compiles dz.Spec.RecordSetSelector. A selector that
// does not compile is returned as an error; callers admit nothing then.
func NewRecordSetSelector(dz *v1alpha1.DNSZone) (*RecordSetSelector, error) {
	rs := dz.Spec.RecordSetSelector
	if rs == nil {
		return nil, nil
	}
	s := &RecordSetSelector{namespaces: rs.Namespaces}
	if rs.LabelSelector != nil {
		sel, err := metav1.LabelSelectorAsSelector(rs.LabelSelector)
		if err != nil {
			return nil, err
		}
		s.labels = sel
	}
	return s, nil
}

// Admits reports whether the selector admits rs (namespaces, then labels).
func (s *RecordSetSelector) Admits(rs *v1alpha1.DNSRecordSet) bool {
	if s == nil {
		return true
	}
	if len(s.namespaces) > 0 && !slices.Contains(s.namespaces, rs.Namespace) {
		return false
	}
	return s.labels == nil || s.labels.Matches(labels.Set(rs.Labels))
}

// ServedRecordSets returns the record sets of sets that zone dz is built
// from, oldest first (CompareRecordSets): those the zone's
// recordSetSelector admits and that are valid as a whole, meaning every
// endpoint passes ValidateEndpoint and no two endpoints of the set share
// an identity (owner, type, setIdentifier). An invalid record set is left
// out entirely, as the apiserver would have rejected it; Build then
// decides endpoint by endpoint (OutsideZone, Duplicated, CNAME and DNAME
// conflicts). sets is not modified. A recordSetSelector that does not
// compile admits nothing.
func ServedRecordSets(dz *v1alpha1.DNSZone, sets []*v1alpha1.DNSRecordSet) []*v1alpha1.DNSRecordSet {
	return ServedRecordSetsCached(dz, sets, nil, time.Time{})
}

// ServedRecordSetsCached applies the same selection with optional parsed reuse.
func ServedRecordSetsCached(dz *v1alpha1.DNSZone, sets []*v1alpha1.DNSRecordSet, cache *RecordCache, now time.Time) []*v1alpha1.DNSRecordSet {
	if now.IsZero() {
		now = time.Now()
	}
	sel, err := NewRecordSetSelector(dz)
	if err != nil {
		return nil
	}
	out := make([]*v1alpha1.DNSRecordSet, 0, len(sets))
	for _, rs := range sets {
		if sel.Admits(rs) && cache.RecordSetValid(dz, rs, now) {
			out = append(out, rs)
		}
	}
	slices.SortStableFunc(out, CompareRecordSets)
	return out
}

// RecordSetValid reports whether every endpoint of rs passes
// ValidateEndpoint and no two endpoints share an identity.
func RecordSetValid(rs *v1alpha1.DNSRecordSet) bool {
	seen := make(map[string]struct{}, len(rs.Spec.Endpoints))
	for i := range rs.Spec.Endpoints {
		ep := &rs.Spec.Endpoints[i]
		if ValidateEndpoint(*ep) != nil {
			return false
		}
		id := EndpointIdentity(*ep)
		if _, dup := seen[id]; dup {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

// EndpointIdentity returns the duplicate-detection key of an endpoint as
// text: the canonical owner, the upper-case type and the set identifier.
// Two endpoints with the same identity in one record set make it invalid;
// across record sets the later one is Duplicated.
func EndpointIdentity(ep v1alpha1.Endpoint) string {
	owner := strings.ToLower(strings.TrimSpace(ep.DNSName))
	if k, err := canonicalKey(owner); err == nil {
		owner = nameFromKey(k)
	}
	return owner + " " + strings.ToUpper(strings.TrimSpace(ep.RecordType)) + " " + ep.SetIdentifier
}
