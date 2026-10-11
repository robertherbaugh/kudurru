package controller

import (
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// defaultTTL applies when neither the endpoint nor the zone sets a TTL; it
// matches the DNSZone defaultTTL schema default.
const defaultTTL = 300

// rrsetVerdict is what the zone model concludes about one record set.
type rrsetVerdict struct {
	// Names is the number of distinct owner names in the spec.
	Names int
	// Blocked, when set, keeps the whole record set out of the zone:
	// ReasonZoneNotFound, ReasonNotSelected or v1alpha1.ReasonInvalid.
	Blocked    string
	BlockedMsg string
	// Outside lists "name TYPE" of endpoints whose owner is not in the
	// zone; Duplicates lists endpoints whose identity an earlier record
	// set holds. Both are dropped from the zone.
	Outside    []string
	Duplicates []string
	// Endpoints and Kept count the endpoints in the spec and those that
	// survive (served, or disabled on purpose).
	Endpoints int
	Kept      int
}

// problem returns the verdict's condition reason, or "" when the record
// set is fully served.
func (v *rrsetVerdict) problem() string {
	switch {
	case v.Blocked != "":
		return v.Blocked
	case len(v.Outside) > 0:
		return v1alpha1.ReasonOutsideZone
	case len(v.Duplicates) > 0:
		return v1alpha1.ReasonDuplicated
	}
	return ""
}

// zoneView is the zone as served: the winning tuples, their hash, and a
// verdict for every record set that references the zone.
type zoneView struct {
	Zone        *v1alpha1.DNSZone // nil when the zone does not exist
	Origin      string
	SelectorErr error
	Records     []RecordTuple
	RecordCount int // individual RRs (targets) across Records
	Hash        string
	Verdicts    map[store.Key]*rrsetVerdict
}

// identity is the duplicate-detection key of an endpoint within a zone.
func identity(ep v1alpha1.Endpoint) string {
	return canonicalName(ep.DNSName) + " " + strings.ToUpper(ep.RecordType) + " " + ep.SetIdentifier
}

// describe renders an identity for messages: "www.example.com. A" or
// "www.example.com. A/eu".
func describe(ep v1alpha1.Endpoint) string {
	s := canonicalName(ep.DNSName) + " " + strings.ToUpper(ep.RecordType)
	if ep.SetIdentifier != "" {
		s += "/" + ep.SetIdentifier
	}
	return s
}

// olderFirst orders record sets by creationTimestamp, then namespace and
// name, so the earlier object keeps a contested identity.
func olderFirst(a, b *v1alpha1.DNSRecordSet) int {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c
	}
	if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// buildZoneView evaluates every record set that references zone (which
// may be nil). It is a pure function of its inputs; rrsets is not
// modified.
//
// Rules, in order, per record set (oldest first):
//  1. no zone → blocked ZoneNotFound;
//  2. not admitted by spec.recordSetSelector → blocked NotSelected;
//  3. any endpoint failing validate, or two endpoints with one identity
//     (name, type, setIdentifier) → blocked Invalid (the whole object, as
//     the apiserver would reject it);
//  4. endpoints outside the zone are dropped (OutsideZone);
//  5. an identity already held by an earlier record set is dropped
//     (Duplicated, on the later object); disabled endpoints still hold
//     their identity so toggling them never moves ownership;
//  6. the rest, minus disabled endpoints, are the zone's records.
func buildZoneView(zone *v1alpha1.DNSZone, rrsets []*v1alpha1.DNSRecordSet, validate EndpointValidator) *zoneView {
	if validate == nil {
		validate = ValidateEndpoint
	}
	v := &zoneView{Zone: zone, Verdicts: make(map[store.Key]*rrsetVerdict, len(rrsets))}
	var sel labels.Selector
	ttl := int64(defaultTTL)
	if zone != nil {
		v.Origin = canonicalName(zone.Spec.ZoneName)
		if zone.Spec.DefaultTTL > 0 {
			ttl = int64(zone.Spec.DefaultTTL)
		}
		if rs := zone.Spec.RecordSetSelector; rs != nil && rs.LabelSelector != nil {
			s, err := metav1.LabelSelectorAsSelector(rs.LabelSelector)
			if err != nil {
				v.SelectorErr = err
			}
			sel = s
		}
	}
	sorted := slices.Clone(rrsets)
	slices.SortFunc(sorted, olderFirst)
	owner := map[string]*v1alpha1.DNSRecordSet{}
	for _, rs := range sorted {
		verdict := &rrsetVerdict{Endpoints: len(rs.Spec.Endpoints)}
		v.Verdicts[store.KeyOf(rs)] = verdict
		names := map[string]struct{}{}
		for _, ep := range rs.Spec.Endpoints {
			names[canonicalName(ep.DNSName)] = struct{}{}
		}
		verdict.Names = len(names)

		switch {
		case zone == nil:
			verdict.Blocked = ReasonZoneNotFound
			verdict.BlockedMsg = fmt.Sprintf("DNSZone %q does not exist", rs.Spec.ZoneRef.Name)
			continue
		case v.SelectorErr != nil:
			verdict.Blocked = ReasonNotSelected
			verdict.BlockedMsg = fmt.Sprintf("DNSZone %q has an invalid recordSetSelector: %v", zone.Name, v.SelectorErr)
			continue
		case !admitted(zone, sel, rs):
			verdict.Blocked = ReasonNotSelected
			verdict.BlockedMsg = fmt.Sprintf("DNSZone %q does not admit record sets from namespace %q with these labels (spec.recordSetSelector)", zone.Name, rs.Namespace)
			continue
		}
		if msg := invalid(rs, validate); msg != "" {
			verdict.Blocked = v1alpha1.ReasonInvalid
			verdict.BlockedMsg = msg
			continue
		}
		for _, ep := range rs.Spec.Endpoints {
			name := canonicalName(ep.DNSName)
			if !inZone(name, v.Origin) {
				verdict.Outside = append(verdict.Outside, describe(ep))
				continue
			}
			id := identity(ep)
			if prev, held := owner[id]; held {
				verdict.Duplicates = append(verdict.Duplicates, fmt.Sprintf("%s (held by %s/%s)", describe(ep), prev.Namespace, prev.Name))
				continue
			}
			owner[id] = rs
			verdict.Kept++
			if ep.Kudurru != nil && ep.Kudurru.Disabled {
				continue
			}
			t := RecordTuple{Name: name, Type: strings.ToUpper(ep.RecordType), SetIdentifier: ep.SetIdentifier, TTL: ttl, Targets: slices.Clone(ep.Targets)}
			if ep.RecordTTL > 0 {
				t.TTL = ep.RecordTTL
			}
			v.Records = append(v.Records, t)
			v.RecordCount += len(t.Targets)
		}
	}
	v.Hash = ContentHash(v.Records)
	return v
}

// admitted applies spec.recordSetSelector.
func admitted(zone *v1alpha1.DNSZone, sel labels.Selector, rs *v1alpha1.DNSRecordSet) bool {
	s := zone.Spec.RecordSetSelector
	if s == nil {
		return true
	}
	if len(s.Namespaces) > 0 && !slices.Contains(s.Namespaces, rs.Namespace) {
		return false
	}
	return sel == nil || sel.Matches(labels.Set(rs.Labels))
}

// invalid returns a message naming the first problems of a record set
// that cannot be served at all, or "".
func invalid(rs *v1alpha1.DNSRecordSet, validate EndpointValidator) string {
	var errs []string
	seen := map[string]bool{}
	for _, ep := range rs.Spec.Endpoints {
		if err := validate(ep); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		id := identity(ep)
		if seen[id] {
			errs = append(errs, "duplicate endpoint "+describe(ep)+" within the record set")
		}
		seen[id] = true
	}
	return joinLimited(errs, 5)
}

// joinLimited joins at most n items and says how many were left out.
func joinLimited(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, "; ")
	}
	return strings.Join(items[:n], "; ") + fmt.Sprintf("; and %d more", len(items)-n)
}
