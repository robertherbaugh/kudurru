package unifi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

// Optional pushes (D401). Every write is an explicit operator command: the
// record mirror is a reviewed-only publisher (Roles.Reviewed), and the DHCP
// and client writes are advertised actions. Defaults advertise neither.

const (
	maxPolicies   = 10000
	maxPolicyName = 127
	enabledOption = "unifi/enabled"
)

func validatePush(c public.UniFiIntegration) error {
	push := c.Push
	if push.DHCPDNSServers {
		if len(push.DNSServers) == 0 || len(push.DNSServers) > 4 || len(push.Networks) == 0 || len(push.Networks) > 64 {
			return ErrInvalid
		}
	}
	seen := map[string]bool{}
	for _, raw := range push.DNSServers {
		ip, err := netip.ParseAddr(raw)
		if err != nil || !ip.Is4() || ip.String() != raw || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || seen[raw] {
			return ErrInvalid
		}
		seen[raw] = true
	}
	seen = map[string]bool{}
	for _, n := range push.Networks {
		if n == "" || len(n) > 253 || strings.TrimSpace(n) != n || strings.ContainsAny(n, "\x00\r\n\t") || seen[n] {
			return ErrInvalid
		}
		seen[n] = true
	}
	if push.ClientActions && !c.Legacy {
		// block-sta and unblock-sta exist only on the legacy controller API.
		return host.ErrUnsupported
	}
	m := push.MirrorRecords
	if m.Enabled && len(m.Domains) == 0 || len(m.Types) > 2 || len(m.Domains) > 64 || len(m.Labels) > 64 || len(m.Exclude) > 256 {
		return ErrInvalid
	}
	seen = map[string]bool{}
	for _, t := range m.Types {
		if t != "A" && t != "CNAME" || seen[t] {
			return ErrInvalid
		}
		seen[t] = true
	}
	seen = map[string]bool{}
	for _, d := range m.Domains {
		n, err := policyName(d)
		if err != nil || seen[n] {
			return ErrInvalid
		}
		seen[n] = true
	}
	for _, e := range m.Exclude {
		if _, err := policyName(strings.TrimPrefix(e, "*.")); err != nil {
			return ErrInvalid
		}
	}
	for k, v := range m.Labels {
		if k == "" || len(k) > 317 || len(v) > 63 {
			return ErrInvalid
		}
	}
	return nil
}

// policyName canonicalizes a DNS name that a UniFi DNS policy can hold.
func policyName(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " *\\\x00\r\n\t") {
		return "", ErrInvalid
	}
	n := dns.CanonicalName(raw)
	if _, ok := dns.IsDomainName(n); !ok || n == "." || len(n)-1 > maxPolicyName {
		return "", ErrInvalid
	}
	return n, nil
}

// Roles reports the fixed roles of this incarnation: always an observer, and
// a reviewed-only publisher exactly when the record mirror is enabled.
func (p *Provider) Roles() (host.Roles, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	mirror := p.host != nil && p.config.Push.MirrorRecords.Enabled
	return host.Roles{Observer: true, Publisher: mirror, Reviewed: mirror}, nil
}

// ObservationSchedule keeps the configured poll interval as the observation
// cadence; spec.resync may only shorten it.
func (p *Provider) ObservationSchedule() (time.Duration, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return pollInterval(p.config), nil
}

func (p *Provider) mirrorDestination() kinds.Destination {
	site := p.config.Site
	if site == "" {
		site = "default"
	}
	return kinds.Destination{Provider: "unifi", Account: p.bucket, Zone: site}
}

// Destinations names the one configured site without network I/O.
func (p *Provider) Destinations(ctx context.Context) ([]host.Publication, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.host == nil || p.stopped {
		return nil, host.ErrUnavailable
	}
	m := p.config.Push.MirrorRecords
	if !m.Enabled {
		return []host.Publication{}, nil
	}
	types := slices.Clone(m.Types)
	if len(types) == 0 {
		types = []string{"A", "CNAME"}
	}
	labels := map[string]string{}
	for k, v := range m.Labels {
		labels[k] = v
	}
	if len(labels) == 0 {
		labels = nil
	}
	return []host.Publication{{
		Destination: p.mirrorDestination(),
		Domains:     slices.Clone(m.Domains),
		Selection:   public.PublicationSelection{Labels: labels, Types: types, Exclude: slices.Clone(m.Exclude)},
		Policy:      plan.PolicySync,
	}}, nil
}

type policyRow struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Enabled      *bool  `json:"enabled"`
	Domain       string `json:"domain"`
	IPv4Address  string `json:"ipv4Address"`
	TargetDomain string `json:"targetDomain"`
	TTLSeconds   *int64 `json:"ttlSeconds"`
}

// remotePolicy is one A or CNAME policy in the plan's form.
type remotePolicy struct {
	id       string
	endpoint public.Endpoint
}

// Records lists every DNS policy of the site. A and CNAME policies are rows
// without any ownership marker (the API has no field for one); every other
// policy, a forwarded domain included, claims its whole name.
func (p *Provider) Records(ctx context.Context, d kinds.Destination) (host.RemoteSnapshot, error) {
	rows, claimed, err := p.resolvedPolicies(ctx, d, &pollBudget{})
	if err != nil {
		return host.RemoteSnapshot{}, err
	}
	out := host.RemoteSnapshot{Records: make([]plan.RemoteRecord, 0, len(rows)), Claimed: claimed, Complete: true}
	for _, row := range rows {
		out.Records = append(out.Records, plan.RemoteRecord{Endpoint: *row.endpoint.DeepCopy(), IDs: []string{row.id}, Owner: plan.RemoteOwner{Unmarked: true}})
	}
	return out, nil
}

func (p *Provider) policies(ctx context.Context, d kinds.Destination, budget *pollBudget) ([]remotePolicy, []kinds.RecordKey, siteRow, error) {
	p.mu.RLock()
	ready := p.host != nil && !p.stopped && p.config.Push.MirrorRecords.Enabled
	expected := p.mirrorDestination()
	p.mu.RUnlock()
	if !ready {
		return nil, nil, siteRow{}, host.ErrUnavailable
	}
	if d != expected {
		return nil, nil, siteRow{}, ErrInvalid
	}
	// The listing the host plans from resolves the site; the apply's own
	// rereads use that immutable site ID (a removed site answers 404).
	p.mu.RLock()
	cached := p.mirrorSite
	p.mu.RUnlock()
	var site siteRow
	if cached != nil {
		site = *cached
	} else {
		resolved, _, err := p.identity(ctx, budget)
		if err != nil {
			return nil, nil, siteRow{}, err
		}
		site = resolved
	}
	listed, err := list(ctx, p, budget, "/sites/"+site.ID+"/dns/policies", maxPolicies, func(r policyRow) string { return r.ID })
	if err != nil {
		return nil, nil, siteRow{}, err
	}
	var rows []remotePolicy
	var claimed []kinds.RecordKey
	seenClaims := map[kinds.RecordKey]bool{}
	for _, r := range listed {
		if r.Enabled == nil || r.TTLSeconds != nil && (*r.TTLSeconds < 0 || *r.TTLSeconds > 604800) {
			return nil, nil, siteRow{}, ErrInvalid
		}
		name, err := policyName(r.Domain)
		if err != nil {
			// A wildcard or otherwise unusual domain is foreign data this
			// adapter never writes; it cannot shadow a selectable name.
			continue
		}
		var e public.Endpoint
		switch r.Type {
		case "A_RECORD":
			ip, err := netip.ParseAddr(r.IPv4Address)
			if err != nil || !ip.Is4() {
				return nil, nil, siteRow{}, ErrInvalid
			}
			e = public.Endpoint{DNSName: name, RecordType: "A", Targets: []string{ip.String()}}
		case "CNAME_RECORD":
			target, err := policyName(r.TargetDomain)
			if err != nil {
				return nil, nil, siteRow{}, ErrInvalid
			}
			e = public.Endpoint{DNSName: name, RecordType: "CNAME", Targets: []string{target}}
		default:
			k := kinds.RecordKey{Name: name, Type: "NS"}
			if !seenClaims[k] {
				seenClaims[k] = true
				claimed = append(claimed, k)
			}
			continue
		}
		if r.TTLSeconds != nil {
			e.RecordTTL = *r.TTLSeconds
		}
		if !*r.Enabled {
			e.ProviderSpecific = []public.ProviderSpecific{{Name: enabledOption, Value: "false"}}
		}
		n, err := plan.Normalize(e)
		if err != nil {
			return nil, nil, siteRow{}, ErrInvalid
		}
		rows = append(rows, remotePolicy{id: r.ID, endpoint: n})
	}
	slices.SortFunc(claimed, func(a, b kinds.RecordKey) int { return strings.Compare(a.Name, b.Name) })
	return rows, claimed, site, nil
}

// resolvedPolicies lists the site's policies after resolving the site afresh
// and remembers it for the applies planned from this listing.
func (p *Provider) resolvedPolicies(ctx context.Context, d kinds.Destination, budget *pollBudget) ([]remotePolicy, []kinds.RecordKey, error) {
	p.mu.Lock()
	p.mirrorSite = nil
	p.mu.Unlock()
	rows, claimed, site, err := p.policies(ctx, d, budget)
	if err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	p.mirrorSite = &site
	p.mu.Unlock()
	return rows, claimed, nil
}

// Adjust maps selected records onto what a DNS policy can hold. Records
// scoped to a view are refused rather than exposed to every gateway client.
func (p *Provider) Adjust(ctx context.Context, pub host.Publication, endpoints []public.Endpoint) ([]public.Endpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	expected := p.mirrorDestination()
	p.mu.RUnlock()
	if pub.Destination != expected {
		return nil, ErrInvalid
	}
	out := make([]public.Endpoint, 0, len(endpoints))
	for _, e := range endpoints {
		e = *e.DeepCopy()
		e.RecordType = strings.ToUpper(e.RecordType)
		if e.SetIdentifier != "" || e.Kudurru != nil && e.Kudurru.View != "" || e.RecordType != "A" && e.RecordType != "CNAME" {
			return nil, ErrInvalid
		}
		if _, err := policyName(e.DNSName); err != nil {
			return nil, err
		}
		limit := int64(86400)
		if e.RecordType == "CNAME" {
			limit = 604800
		}
		e.RecordTTL = min(max(e.RecordTTL, 0), limit)
		e.ProviderSpecific = nil
		out = append(out, e)
	}
	return out, nil
}

type policyEffect struct {
	method, id, target string
}

// Apply executes one reserved intent with one attempt per request. It
// rereads the site first, refuses when the record set is not exactly the
// intent's ledger-proven baseline, and never touches a policy outside it.
func (p *Provider) Apply(ctx context.Context, op *kinds.IntegrationOperation) (kinds.Receipt, error) {
	p.mu.RLock()
	owner, destination := p.owner, p.mirrorDestination()
	p.mu.RUnlock()
	if op == nil || op.Validate() != nil || !op.Spec.AbandonedAt.IsZero() || op.Spec.Record.Owner != owner || op.Spec.Record.Destination != destination || op.Spec.Record.Key.SetIdentifier != "" || op.Spec.Policy != "" && op.Spec.Policy != string(plan.PolicySync) {
		return kinds.Receipt{}, ErrInvalid
	}
	key := op.Spec.Record.Key
	if key.Type != "A" && key.Type != "CNAME" || op.Spec.After != nil && (op.Spec.After.RecordType != key.Type || op.Spec.After.DNSName != key.Name) {
		return kinds.Receipt{}, ErrInvalid
	}
	budget := &pollBudget{}
	rows, claimed, site, err := p.policies(ctx, destination, budget)
	if err != nil {
		return kinds.Receipt{}, err
	}
	facts := []kinds.TargetResult{}
	if op.Spec.Receipt != nil {
		facts = slices.Clone(op.Spec.Receipt.Targets)
	}
	receipt := policyReceipt(op, rows, facts, p.now())
	if receipt.Complete {
		return receipt, nil
	}
	before := map[string]bool{}
	for _, id := range op.Spec.ProviderIDs {
		before[id] = true
	}
	var mine []remotePolicy
	for _, c := range claimed {
		if c.Name == key.Name {
			return receipt, ErrDrift
		}
	}
	for _, row := range rows {
		if row.endpoint.DNSName != key.Name {
			continue
		}
		if row.endpoint.RecordType != key.Type {
			if row.endpoint.RecordType == "CNAME" || key.Type == "CNAME" {
				return receipt, ErrDrift
			}
			continue
		}
		if !before[row.id] {
			return receipt, ErrDrift
		}
		mine = append(mine, row)
	}
	current, ids, homogeneous := aggregatePolicies(mine)
	expectedIDs := slices.Clone(op.Spec.ProviderIDs)
	slices.Sort(expectedIDs)
	if !homogeneous || kinds.EndpointDigest(current) != op.Spec.BeforeHash || !slices.Equal(ids, expectedIDs) {
		return receipt, ErrDrift
	}
	effects, err := policyEffects(op.Spec.After, mine)
	if err != nil {
		return receipt, err
	}
	base := p.prefix + "/v1/sites/" + site.ID + "/dns/policies"
	for _, e := range effects {
		var payload []byte
		path := base
		if e.method != http.MethodPost {
			path += "/" + e.id
		}
		if e.method != http.MethodDelete {
			payload, err = policyBody(op.Spec.After, e.target)
			if err != nil {
				return receipt, err
			}
		}
		body, writeErr := p.write(ctx, budget, e.method, path, payload)
		if writeErr == nil && e.method == http.MethodPost {
			var created policyRow
			if decode(body, &created, false) != nil || !validUUID(created.ID) {
				writeErr = ErrInvalid
			} else {
				facts = addPolicyFact(facts, kinds.TargetResult{Target: e.target, ProviderID: created.ID, Applied: true})
			}
		}
		if writeErr != nil {
			// A refused write changed nothing. An unacknowledged one may have:
			// the intent stays pending, a policy it may have created is never
			// proven ours (it can only become a conflict), and the host settles
			// the intent only from a later complete reread that shows exactly
			// the baseline (for example after the operator removed such a
			// policy). No write is replayed and polling never resumes one.
			return p.settle(ctx, op, destination, facts), writeErr
		}
	}
	receipt = p.settle(ctx, op, destination, facts)
	if !receipt.Complete {
		return receipt, ErrDrift
	}
	return receipt, nil
}

// settle rereads the site once and reports what this intent proves.
func (p *Provider) settle(ctx context.Context, op *kinds.IntegrationOperation, d kinds.Destination, facts []kinds.TargetResult) kinds.Receipt {
	rows, _, _, err := p.policies(ctx, d, &pollBudget{})
	if err != nil {
		return partialPolicy(op, facts, p.now())
	}
	return policyReceipt(op, rows, facts, p.now())
}

func policyEffects(after *public.Endpoint, mine []remotePolicy) ([]policyEffect, error) {
	var deletes, puts, posts []policyEffect
	var targets []string
	if after != nil {
		targets = after.Targets
	}
	kept := map[string]bool{}
	var spare []remotePolicy
	// A policy already holding a desired target keeps it; it is rewritten
	// only when its TTL or enabled state differs.
	for _, row := range mine {
		target := row.endpoint.Targets[0]
		if after != nil && slices.Contains(targets, target) && !kept[target] {
			kept[target] = true
			if !samePolicy(after, target, row.endpoint) {
				puts = append(puts, policyEffect{http.MethodPut, row.id, target})
			}
			continue
		}
		spare = append(spare, row)
	}
	// Remaining owned policies are rewritten to the remaining targets in
	// place (a changed address keeps its policy ID); the rest are deleted,
	// and targets left over are created.
	for _, t := range targets {
		if kept[t] {
			continue
		}
		kept[t] = true
		if len(spare) > 0 {
			puts = append(puts, policyEffect{http.MethodPut, spare[0].id, t})
			spare = spare[1:]
			continue
		}
		posts = append(posts, policyEffect{http.MethodPost, "", t})
	}
	for _, row := range spare {
		deletes = append(deletes, policyEffect{http.MethodDelete, row.id, row.endpoint.Targets[0]})
	}
	effects := append(append(deletes, puts...), posts...)
	if len(effects) > kinds.MaxTargets*2 {
		return nil, ErrCapacity
	}
	return effects, nil
}

func samePolicy(after *public.Endpoint, target string, row public.Endpoint) bool {
	single := after.DeepCopy()
	single.Targets = []string{target}
	return kinds.EndpointDigest(single) == kinds.EndpointDigest(&row)
}

func policyBody(after *public.Endpoint, target string) ([]byte, error) {
	if after == nil {
		return nil, ErrInvalid
	}
	body := map[string]any{"enabled": true, "domain": strings.TrimSuffix(after.DNSName, "."), "ttlSeconds": after.RecordTTL}
	switch after.RecordType {
	case "A":
		ip, err := netip.ParseAddr(target)
		if err != nil || !ip.Is4() {
			return nil, ErrInvalid
		}
		body["type"], body["ipv4Address"] = "A_RECORD", ip.String()
	case "CNAME":
		name, err := policyName(target)
		if err != nil {
			return nil, err
		}
		body["type"], body["targetDomain"] = "CNAME_RECORD", strings.TrimSuffix(name, ".")
	default:
		return nil, ErrInvalid
	}
	return json.Marshal(body)
}

// aggregatePolicies merges rows of one key the way the planner does.
func aggregatePolicies(rows []remotePolicy) (*public.Endpoint, []string, bool) {
	var e *public.Endpoint
	ids := []string{}
	for _, row := range rows {
		ids = append(ids, row.id)
		if e == nil {
			e = row.endpoint.DeepCopy()
			continue
		}
		if e.RecordTTL != row.endpoint.RecordTTL || !slices.Equal(e.ProviderSpecific, row.endpoint.ProviderSpecific) || e.RecordType == "CNAME" {
			return nil, nil, false
		}
		e.Targets = append(e.Targets, row.endpoint.Targets...)
	}
	if e != nil {
		slices.Sort(e.Targets)
		e.Targets = slices.Compact(e.Targets)
	}
	slices.Sort(ids)
	return e, ids, true
}

// policyReceipt is complete when every row at the key is proven this
// intent's (its baseline or a creation acknowledged to it) and the rows
// together are exactly After (absence for a delete).
func policyReceipt(op *kinds.IntegrationOperation, rows []remotePolicy, facts []kinds.TargetResult, at time.Time) kinds.Receipt {
	r := partialPolicy(op, facts, at)
	proven := map[string]bool{}
	for _, id := range op.Spec.ProviderIDs {
		proven[id] = true
	}
	for _, f := range facts {
		if f.Applied && f.ProviderID != "" {
			proven[f.ProviderID] = true
		}
	}
	key := op.Spec.Record.Key
	var mine []remotePolicy
	for _, row := range rows {
		if row.endpoint.DNSName != key.Name || row.endpoint.RecordType != key.Type {
			continue
		}
		if !proven[row.id] {
			return r
		}
		mine = append(mine, row)
	}
	e, ids, homogeneous := aggregatePolicies(mine)
	if !homogeneous || kinds.EndpointDigest(e) != op.Spec.AfterHash {
		return r
	}
	r.Complete, r.ContentHash = true, op.Spec.AfterHash
	if len(ids) > 0 {
		r.ProviderIDs = ids
	}
	return r
}

func partialPolicy(op *kinds.IntegrationOperation, facts []kinds.TargetResult, at time.Time) kinds.Receipt {
	if op.Spec.Receipt != nil && at.Before(op.Spec.Receipt.ObservedAt) {
		at = op.Spec.Receipt.ObservedAt
	}
	out := kinds.Receipt{ObservedAt: at.UTC()}
	if len(facts) > 0 {
		out.Targets = slices.Clone(facts)
	}
	return out
}

func addPolicyFact(facts []kinds.TargetResult, fact kinds.TargetResult) []kinds.TargetResult {
	for i, old := range facts {
		if old.Target == fact.Target {
			if old.Applied {
				return facts
			}
			facts[i] = fact
			return facts
		}
	}
	return append(facts, fact)
}
