package controller

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	zonemodel "github.com/robertherbaugh/kudurru/internal/zone"
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
	// Conflicts are endpoints the zone model refused although they are
	// valid and in the zone: CNAME exclusivity (Duplicated against another
	// object, Invalid within one), data below a DNAME (Occluded), apex
	// CNAME/NS and records in a forward zone (Invalid). They are dropped
	// from the zone. They come from the zone build (withConflicts), not
	// from buildZoneView.
	Conflicts []conflict
	// Endpoints and Kept count the endpoints in the spec and those that
	// survive (served, or disabled on purpose).
	Endpoints int
	Kept      int
}

// conflict is one endpoint the zone model refused.
type conflict struct {
	Index  int    // the endpoint's index in spec.endpoints
	Reason string // v1alpha1.ReasonDuplicated, ReasonOccluded or ReasonInvalid
	Text   string
}

// conflicts returns the texts of the conflicts with reason.
func (v *rrsetVerdict) conflicts(reason string) []string {
	var out []string
	for _, c := range v.Conflicts {
		if c.Reason == reason {
			out = append(out, c.Text)
		}
	}
	return out
}

// withConflicts returns v with the zone build's conflicts for its record
// set applied: a copy when there are any (views are shared), v otherwise.
// Blocked record sets were not built, so they have none.
func (v *rrsetVerdict) withConflicts(cs []conflict) *rrsetVerdict {
	if len(cs) == 0 || v.Blocked != "" {
		return v
	}
	c := *v
	c.Conflicts = cs
	dropped := map[int]bool{}
	for _, x := range cs {
		if !dropped[x.Index] {
			dropped[x.Index] = true
			c.Kept--
		}
	}
	return &c
}

// problem returns the verdict's condition reason, or "" when the record
// set is fully served.
func (v *rrsetVerdict) problem() string {
	switch {
	case v.Blocked != "":
		return v.Blocked
	case len(v.Outside) > 0:
		return v1alpha1.ReasonOutsideZone
	case len(v.Duplicates) > 0 || len(v.conflicts(v1alpha1.ReasonDuplicated)) > 0:
		return v1alpha1.ReasonDuplicated
	case len(v.conflicts(v1alpha1.ReasonOccluded)) > 0:
		return v1alpha1.ReasonOccluded
	case len(v.Conflicts) > 0:
		return v1alpha1.ReasonInvalid
	}
	return ""
}

// zoneView is the zone as the controller sees it: the winning tuples and
// a verdict for every record set that references the zone. It is cheap to
// build (no record parsing) and rebuilt on every change; the zone model's
// build (zoneBuild) supplies the hash, the record count and the conflicts.
type zoneView struct {
	Zone        *v1alpha1.DNSZone // nil when the zone does not exist
	Origin      string
	SelectorErr error
	Records     []RecordTuple
	Verdicts    map[store.Key]*rrsetVerdict
}

// zoneBuild is the zone model's verdict on a zone: the zone every replica
// builds from the same objects (ZoneContentHash).
type zoneBuild struct {
	// Hash is zone.Zone.ContentHash, "" when there is no zone or it does
	// not build.
	Hash string
	// RecordCount is the number of RRs the zone serves besides the SOA
	// (zone.Zone.RecordCount, apex NS included).
	RecordCount int
	// ZoneErr is set when the DNSZone itself does not build (a bad zone
	// name, query ACL or nameserver): no replica serves it.
	ZoneErr error
	// PolicyErr reports transfer/notify policy errors: the zone is served
	// with transfers denied and bad notify targets dropped.
	PolicyErr error
	// Conflicts holds, per record set, the endpoints the zone model
	// refused that the controller's own rules do not report.
	Conflicts map[store.Key][]conflict
}

// buildZone builds zone from rrsets with the zone model. A nil zone has
// an empty build.
func buildZone(zone *v1alpha1.DNSZone, rrsets []*v1alpha1.DNSRecordSet) *zoneBuild {
	b := &zoneBuild{}
	if zone == nil {
		return b
	}
	z, errs := ZoneContentHash(zone, rrsets, time.Time{})
	if z != nil {
		b.Hash, b.RecordCount = z.ContentHash(), z.RecordCount()
	} else {
		b.ZoneErr = errs.Zone.ToAggregate()
	}
	if len(errs.Policy) > 0 {
		b.PolicyErr = errs.Policy.ToAggregate()
	}
	byKey := make(map[string]*v1alpha1.DNSRecordSet, len(rrsets))
	for _, rs := range rrsets {
		byKey[rs.Namespace+"/"+rs.Name] = rs
	}
	for src, list := range errs.RecordSets {
		rs := byKey[src]
		if rs == nil {
			continue
		}
		var cs []conflict
		for _, e := range list {
			if c, ok := conflictOf(rs, e); ok {
				cs = append(cs, c)
			}
		}
		if len(cs) > 0 {
			if b.Conflicts == nil {
				b.Conflicts = map[store.Key][]conflict{}
			}
			b.Conflicts[store.KeyOf(rs)] = cs
		}
	}
	return b
}

// ZoneContentHash builds the zone exactly as the zone cache does on every
// replica (zone.ServedRecordSets, then zone.Build at now; zero means the
// current time) and returns it with the build errors. Its ContentHash is
// status.contentHash (D12), so the serial moves exactly when what is
// served changes, SOA and apex NS fields included. The zone is nil when
// the DNSZone itself does not build.
func ZoneContentHash(dz *v1alpha1.DNSZone, rrsets []*v1alpha1.DNSRecordSet, now time.Time) (*zonemodel.Zone, zonemodel.Errors) {
	return zonemodel.Build(dz, zonemodel.ServedRecordSets(dz, rrsets), zonemodel.BuildOptions{Now: now})
}

// endpointIndex extracts i from a field path "spec.endpoints[i]...".
var endpointIndex = regexp.MustCompile(`^spec\.endpoints\[(\d+)\]`)

// conflictOf turns a zone-model error into a conflict, unless the
// controller's own rules already report it: identity duplicates (the only
// errors of type field.ErrorTypeDuplicate) and OutsideZone.
func conflictOf(rs *v1alpha1.DNSRecordSet, e *field.Error) (conflict, bool) {
	reason := zonemodel.ErrorReason(e)
	if e.Type == field.ErrorTypeDuplicate || reason == v1alpha1.ReasonOutsideZone {
		return conflict{}, false
	}
	text := e.Detail
	for _, p := range []string{v1alpha1.ReasonDuplicated, v1alpha1.ReasonOccluded, v1alpha1.ReasonInvalid} {
		text = strings.TrimPrefix(text, p+": ")
	}
	c := conflict{Index: -1, Reason: reason, Text: e.Field + " (" + text + ")"}
	if m := endpointIndex.FindStringSubmatch(e.Field); m != nil {
		if i, err := strconv.Atoi(m[1]); err == nil && i < len(rs.Spec.Endpoints) {
			c.Index = i
			c.Text = describe(rs.Spec.Endpoints[i]) + " (" + text + ")"
		}
	}
	return c, true
}

// conflictsChanged returns the record sets whose conflicts differ between
// two builds of a zone (prev may be nil).
func conflictsChanged(prev, next *zoneBuild) []store.Key {
	var out []store.Key
	var before map[store.Key][]conflict
	if prev != nil {
		before = prev.Conflicts
	}
	for k, cs := range next.Conflicts {
		if !slices.Equal(before[k], cs) {
			out = append(out, k)
		}
	}
	for k := range before {
		if _, ok := next.Conflicts[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// identity is the duplicate-detection key of an endpoint within a zone; it
// is the zone model's (zone.EndpointIdentity), so both agree on what a
// duplicate is.
func identity(ep v1alpha1.Endpoint) string { return zonemodel.EndpointIdentity(ep) }

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
// name, so the earlier object keeps a contested identity. It is the zone
// model's order (zone.CompareRecordSets).
func olderFirst(a, b *v1alpha1.DNSRecordSet) int { return zonemodel.CompareRecordSets(a, b) }

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
//
// The zone model's own refusals (CNAME exclusivity, data below a DNAME,
// apex CNAME/NS) come from buildZone and are applied with withConflicts.
func buildZoneView(zone *v1alpha1.DNSZone, rrsets []*v1alpha1.DNSRecordSet, validate EndpointValidator) *zoneView {
	if validate == nil {
		validate = zonemodel.ValidateEndpoint
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
		}
	}
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
