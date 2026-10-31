package plan

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Source is an ORIGINAL admitted record set and its captured owning zone.
// Root performs source admission; Select does not read serving trees or stores.
type Source struct {
	Zone    *public.DNSZone
	Records *public.DNSRecordSet
}

// SelectionInput binds admitted original sources and publication filters.
type SelectionInput struct {
	Owner      kinds.Owner
	Sources    []Source
	Select     public.PublicationSelection
	Domains    []string
	Policy     Policy
	Complete   bool
	Now        time.Time
	MaxRecords int
}

// SourceIdentity pins the exact original record-set and zone incarnations.
type SourceIdentity struct{ Namespace, Name, UID, ResourceVersion, ZoneUID, ZoneResourceVersion string }

// Selected retains endpoint metadata for provider adjustment and source reporting.
type Selected struct {
	Endpoint public.Endpoint
	Policy   Policy
	Source   SourceIdentity
}

// Selection is a complete deterministic selection; it is never truncated.
type Selection struct {
	Records []Selected
	Digest  string
}

// Select includes original public-only endpoints. Global label matches and
// matching zone publish rules opt in independently; global types/domain/name
// exclusions still constrain both. Endpoint labels override record-set labels.
// Empty global labels match all records in configured domains. Publication
// rule policy overrides the integration default; conflicting policies fail.
func Select(in SelectionInput) (*Selection, error) {
	if !in.Complete {
		return nil, ErrIncomplete
	}
	if in.Owner.Validate() != nil || in.Now.IsZero() || len(in.Select.Labels) > 64 || len(in.Select.Types) > 64 || len(in.Select.Exclude) > 256 || len(in.Domains) > 256 {
		return nil, ErrInvalid
	}
	if in.Policy == "" {
		in.Policy = PolicySync
	}
	if !validPolicy(in.Policy) {
		return nil, ErrInvalid
	}
	limit := in.MaxRecords
	if limit <= 0 {
		limit = 10000
	}
	if limit > 100000 || len(in.Sources) > limit {
		return nil, ErrCapacity
	}
	domains, err := patterns(in.Domains, false)
	if err != nil {
		return nil, err
	}
	exclude, err := patterns(in.Select.Exclude, true)
	if err != nil {
		return nil, err
	}
	types, err := typeFilter(in.Select.Types)
	if err != nil {
		return nil, err
	}
	global := labels.SelectorFromSet(in.Select.Labels)
	out := &Selection{}
	count, encoded := 0, 0
	for _, src := range in.Sources {
		z, r := src.Zone, src.Records
		sourceJSON, err := json.Marshal(src)
		if err != nil {
			return nil, ErrInvalid
		}
		encoded += len(sourceJSON)
		if encoded > MaxSnapshotBytes {
			return nil, ErrCapacity
		}
		if z == nil || r == nil || z.UID == "" || z.ResourceVersion == "" || r.UID == "" || r.ResourceVersion == "" {
			return nil, ErrInvalid
		}
		ns := r.Spec.ZoneRef.Namespace
		if ns == "" && z.Namespace != "" {
			ns = r.Namespace
		}
		if r.Spec.ZoneRef.Name != z.Name || ns != z.Namespace {
			return nil, ErrInvalid
		}
		apex, err := pattern(z.Spec.ZoneName, false)
		if err != nil {
			return nil, err
		}
		type rule struct {
			selectLabels labels.Selector
			types        map[string]bool
			policy       Policy
		}
		var rules []rule
		for _, p := range z.Spec.Publish {
			ns := p.IntegrationRef.Namespace
			if ns == "" {
				ns = z.Namespace
			}
			if p.IntegrationRef.Name != in.Owner.Name || ns != in.Owner.Namespace {
				continue
			}
			selector, err := metav1.LabelSelectorAsSelector(p.Selector)
			if err != nil {
				return nil, ErrInvalid
			}
			if p.Selector == nil {
				selector = labels.Everything()
			}
			t, err := typeFilter(p.Types)
			if err != nil {
				return nil, err
			}
			policy := Policy(p.Policy)
			if policy == "" {
				policy = in.Policy
			}
			if !validPolicy(policy) {
				return nil, ErrInvalid
			}
			rules = append(rules, rule{selector, t, policy})
		}
		for _, e := range r.Spec.Endpoints {
			count++
			if count > limit {
				return nil, ErrCapacity
			}

			if e.Kudurru != nil && (e.Kudurru.Disabled || e.Kudurru.ExpiresAt != nil && !in.Now.Before(e.Kudurru.ExpiresAt.Time)) {
				continue
			}
			n, err := Normalize(e)
			if err != nil {
				return nil, err
			}
			if !dns.IsSubDomain(apex, n.DNSName) {
				return nil, ErrInvalid
			}
			if !matchesDomain(domains, n.DNSName) || matchesExcluded(exclude, n.DNSName) || !matchesType(types, n.RecordType) {
				continue
			}
			effective := labels.Set{}
			for k, v := range r.Labels {
				effective[k] = v
			}
			for k, v := range e.Labels {
				effective[k] = v
			}
			selected := global.Matches(effective)
			policy := in.Policy
			var rulePolicy Policy
			for _, rule := range rules {
				if rule.selectLabels.Matches(effective) && matchesType(rule.types, n.RecordType) {
					selected = true
					if rulePolicy != "" && rulePolicy != rule.policy {
						return nil, ErrInvalid
					}
					rulePolicy = rule.policy
				}
			}
			if !selected {
				continue
			}
			if rulePolicy != "" {
				policy = rulePolicy
			}
			out.Records = append(out.Records, Selected{*e.DeepCopy(), policy, SourceIdentity{r.Namespace, r.Name, string(r.UID), r.ResourceVersion, string(z.UID), z.ResourceVersion}})
		}
	}
	slices.SortFunc(out.Records, func(a, b Selected) int {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return strings.Compare(string(x), string(y))
	})
	// Conflicting source policy cannot split one RRset across planners.
	policies := map[kinds.RecordKey]Policy{}
	for _, s := range out.Records {
		k := kinds.RecordKey{Name: dns.CanonicalName(s.Endpoint.DNSName), Type: strings.ToUpper(s.Endpoint.RecordType), SetIdentifier: s.Endpoint.SetIdentifier}
		if old, ok := policies[k]; ok && old != s.Policy {
			return nil, ErrInvalid
		}
		policies[k] = s.Policy
	}
	b, _ := json.Marshal(out.Records)
	out.Digest = kinds.Digest(b)
	return out, nil
}
func validPolicy(p Policy) bool {
	return p == PolicySync || p == PolicyUpsertOnly || p == PolicyCreateOnly
}
func typeFilter(in []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, t := range in {
		t = strings.ToUpper(t)
		if (kinds.RecordKey{Name: "example.", Type: t}).Validate() != nil {
			return nil, ErrInvalid
		}
		out[t] = true
	}
	return out, nil
}
func matchesType(types map[string]bool, t string) bool { return len(types) == 0 || types[t] }
func pattern(p string, wild bool) (string, error) {
	if p == "" || strings.TrimSpace(p) != p || strings.ContainsAny(p, "\x00\r\n\t ") {
		return "", ErrInvalid
	}
	prefix := ""
	if wild && strings.HasPrefix(p, "*.") {
		prefix = "*."
		p = p[2:]
	}
	p = dns.CanonicalName(p)
	if _, ok := dns.IsDomainName(p); !ok || strings.Contains(p, "*") {
		return "", ErrInvalid
	}
	return prefix + p, nil
}
func patterns(in []string, wild bool) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, p := range in {
		n, err := pattern(p, wild)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
func matchesDomain(in []string, n string) bool {
	if len(in) == 0 {
		return true
	}
	for _, p := range in {
		if dns.IsSubDomain(p, n) {
			return true
		}
	}
	return false
}
func matchesExcluded(in []string, n string) bool {
	for _, p := range in {
		if strings.HasPrefix(p, "*.") {
			apex := p[2:]
			if n != apex && dns.IsSubDomain(apex, n) {
				return true
			}
		} else if p == n {
			return true
		}
	}
	return false
}
