package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

// Record publication over the external-dns record routes (D380).
//
// The external-dns protocol has no row IDs, no versions, no owner field and
// no conditional write, and its 204 says only that a peer accepted a body.
// Ownership therefore lives in a kudurru TXT registry at the peer: every
// RRset this instance manages has one companion TXT row at
// registryName(key), with the same set identifier, whose single value is
// "heritage=kudurru,kudurru/owner=<digest of cluster ID and Integration
// UID>". A data RRset is this instance's when its registry row carries this
// instance's marker, another owner's when it carries another kudurru marker
// or an external-dns heritage claims it, and unowned otherwise. Registry
// rows are never shown to the planner.
//
// IDs and versions are synthesized: one stable ID per RRset key and a
// version that digests the RRset's content and its registry marker. Apply
// rereads the complete list, requires the intent's exact baseline, sends one
// Changes body with the data and registry rows, and returns a complete
// receipt only when a second complete reread shows the intended RRset and
// this instance's registry row. Anything else is a partial receipt, so the
// ledger stays pending and the host settles it from a later reread. Nothing
// closes the window between the baseline reread and the POST: another
// writer may change the peer in between, and the verifying reread is what
// detects it.

// maxRecordItems bounds one record list: the planner's 10,000 RRsets, a
// registry row for each, and room for foreign registries.
const maxRecordItems = 32768

// maxRecordSets is the planner's default RRset bound.
const maxRecordSets = 10000

// registryPrefix starts the first label of every kudurru registry row. Data
// records whose first label starts with it are never published.
const registryPrefix = "_kudurru-"

var errDrift = errors.New("webhook: remote records changed")

// webhookDestination is the instance's single record destination: the peer
// endpoint's digest stands for the account, and the zone is every
// configured domain.
func webhookDestination(endpoint string) kinds.Destination {
	return kinds.Destination{Provider: "webhook", Account: kinds.Digest([]byte(endpoint)), Zone: "*"}
}

func registryMarker(owner host.InstanceID) string {
	b, _ := json.Marshal([]string{owner.ClusterID, owner.UID})
	return kinds.Digest(b)
}

func registryValue(marker string) string {
	return `"heritage=kudurru,kudurru/owner=` + marker + `"`
}

// registryName maps an RRset key to its registry row name. A wildcard's
// leading "*" label becomes a "-wildcard" suffix of the registry label, so
// the registry row is an ordinary name every peer can store.
func registryName(key kinds.RecordKey) (string, error) {
	label := registryPrefix + strings.ToLower(key.Type)
	name := key.Name
	if rest, ok := strings.CutPrefix(name, "*."); ok {
		label += "-wildcard"
		name = rest
	}
	out := label + "." + name
	if name == "." {
		out = label + "."
	}
	if _, ok := dns.IsDomainName(out); !ok || len(out) > 254 {
		return "", errInput
	}
	return out, nil
}

// registryKey parses a registry row's name back to the data key it marks.
func registryKey(name, setIdentifier string) (kinds.RecordKey, bool) {
	label, rest, ok := strings.Cut(name, ".")
	if !ok || !strings.HasPrefix(label, registryPrefix) {
		return kinds.RecordKey{}, false
	}
	typ := strings.TrimPrefix(label, registryPrefix)
	if base, wildcard := strings.CutSuffix(typ, "-wildcard"); wildcard {
		typ = base
		rest = "*." + rest
	}
	if rest == "" {
		rest = "."
	}
	key := kinds.RecordKey{Name: dns.CanonicalName(rest), Type: strings.ToUpper(typ), SetIdentifier: setIdentifier}
	if key.Validate() != nil {
		return kinds.RecordKey{}, false
	}
	if again, err := registryName(key); err != nil || again != name {
		return kinds.RecordKey{}, false
	}
	return key, true
}

// registryOwner parses a registry value and returns the marker it names.
func registryOwner(e public.Endpoint) (string, bool) {
	if e.RecordType != "TXT" || len(e.Targets) != 1 {
		return "", false
	}
	rr, err := dns.NewRR("owner.example. 1 IN TXT " + e.Targets[0])
	if err != nil {
		return "", false
	}
	txt, ok := rr.(*dns.TXT)
	if !ok || len(txt.Txt) != 1 {
		return "", false
	}
	labels := map[string]string{}
	for _, part := range strings.Split(txt.Txt[0], ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || labels[key] != "" || value == "" {
			return "", false
		}
		labels[key] = value
	}
	if len(labels) != 2 || labels["heritage"] != "kudurru" || !kinds.ValidDigest(labels["kudurru/owner"]) {
		return "", false
	}
	return labels["kudurru/owner"], true
}

// remoteRow is one data RRset as the peer listed it.
type remoteRow struct {
	record plan.RemoteRecord
	wire   *externaldns.Endpoint
}

// remoteState is one complete, decoded record list.
type remoteState struct {
	rows     []plan.RemoteRecord
	data     map[kinds.RecordKey]remoteRow
	registry map[kinds.RecordKey]string
	wires    map[kinds.RecordKey]*externaldns.Endpoint
}

func (s remoteState) owned(key kinds.RecordKey, marker string) bool {
	return s.registry[key] == marker
}

func recordID(key kinds.RecordKey) string {
	b, _ := json.Marshal([]string{"webhook", key.Name, key.Type, key.SetIdentifier})
	return kinds.Digest(b)
}

func endpointKey(e public.Endpoint) kinds.RecordKey {
	return kinds.RecordKey{Name: e.DNSName, Type: e.RecordType, SetIdentifier: e.SetIdentifier}
}

func toPublic(e *externaldns.Endpoint) public.Endpoint {
	out := public.Endpoint{DNSName: e.DNSName, Targets: slices.Clone(e.Targets), RecordType: e.RecordType, SetIdentifier: e.SetIdentifier, RecordTTL: e.RecordTTL}
	for _, p := range e.ProviderSpecific {
		out.ProviderSpecific = append(out.ProviderSpecific, public.ProviderSpecific{Name: p.Name, Value: p.Value})
	}
	return out
}

func toWire(e public.Endpoint) *externaldns.Endpoint {
	out := &externaldns.Endpoint{DNSName: strings.TrimSuffix(e.DNSName, "."), Targets: slices.Clone(e.Targets), RecordType: e.RecordType, SetIdentifier: e.SetIdentifier, RecordTTL: e.RecordTTL}
	for _, p := range e.ProviderSpecific {
		out.ProviderSpecific = append(out.ProviderSpecific, externaldns.ProviderSpecificProperty{Name: p.Name, Value: p.Value})
	}
	return out
}

// records is the captured publication state of one provider incarnation.
type records struct {
	domains []string
	policy  plan.Policy
	account kinds.Destination
	marker  string
}

func (p *provider) publication(ctx context.Context) (*transport, *advertisement, records, error) {
	tr, a, err := p.captured(ctx, "records")
	if err != nil {
		return nil, nil, records{}, err
	}
	p.mu.RLock()
	r := p.records
	p.mu.RUnlock()
	if len(r.domains) == 0 {
		return nil, nil, records{}, host.ErrUnsupported
	}
	return tr, a, r, nil
}

// Destinations names the one destination of a publishing instance.
func (p *provider) Destinations(ctx context.Context) ([]host.Publication, error) {
	_, _, r, err := p.publication(ctx)
	if err != nil {
		return nil, err
	}
	return []host.Publication{{Destination: r.account, Domains: slices.Clone(r.domains), Policy: r.policy}}, nil
}

// Records returns one complete list without the registry rows.
func (p *provider) Records(ctx context.Context, d kinds.Destination) (host.RemoteSnapshot, error) {
	tr, a, r, err := p.publication(ctx)
	if err != nil {
		return host.RemoteSnapshot{}, err
	}
	if d != r.account {
		return host.RemoteSnapshot{}, errInput
	}
	state, err := p.read(ctx, tr, a, r)
	if err != nil {
		return host.RemoteSnapshot{}, err
	}
	return host.RemoteSnapshot{Records: state.rows, Complete: true}, nil
}

func (p *provider) read(ctx context.Context, tr *transport, a *advertisement, r records) (remoteState, error) {
	reply, err := tr.request(ctx, http.MethodGet, "/records", a.media, nil, nil)
	if err != nil {
		return remoteState{}, err
	}
	if reply.status != http.StatusOK || reply.media != a.media || reply.partial || !arrayJSON(reply.body) || validJSONItems(reply.body, maxRecordItems) != nil || p.echo(reply.body) {
		return remoteState{}, errProtocol
	}
	var wire []*externaldns.Endpoint
	if decodeItems(reply.body, &wire, maxRecordItems) != nil || len(wire) > maxRecordItems {
		return remoteState{}, errProtocol
	}
	state := remoteState{data: map[kinds.RecordKey]remoteRow{}, registry: map[kinds.RecordKey]string{}, wires: map[kinds.RecordKey]*externaldns.Endpoint{}}
	type candidate struct {
		endpoint public.Endpoint
		wire     *externaldns.Endpoint
	}
	var data []candidate
	seen := map[kinds.RecordKey]bool{}
	for _, w := range wire {
		if w == nil {
			return remoteState{}, errProtocol
		}
		n, err := plan.Normalize(toPublic(w))
		if err != nil {
			return remoteState{}, errProtocol
		}
		key := endpointKey(n)
		// external-dns lists one endpoint per RRset; a repeated key is a
		// list this adapter cannot attribute.
		if seen[key] {
			return remoteState{}, errProtocol
		}
		seen[key] = true
		if n.RecordType == "TXT" {
			if marked, ok := registryKey(n.DNSName, n.SetIdentifier); ok {
				if owner, ok := registryOwner(n); ok {
					state.registry[marked] = owner
					state.wires[marked] = w
					continue
				}
			}
		}
		// The peer may serve more than this instance publishes to; rows
		// outside the configured domains are not its destination.
		if inDomains(n.DNSName, r.domains) {
			data = append(data, candidate{n, w})
		}
	}
	if len(data) > maxRecordSets {
		return remoteState{}, plan.ErrCapacity
	}
	for _, c := range data {
		key := endpointKey(c.endpoint)
		row := plan.RemoteRecord{Endpoint: c.endpoint, IDs: []string{recordID(key)}}
		switch owner, marked := state.registry[key]; {
		case marked && owner == r.marker:
			row.Owner = plan.RemoteOwner{ClusterID: p.owner.ClusterID, IntegrationUID: p.owner.UID}
		case marked:
			row.Owner = plan.RemoteOwner{ClusterID: "foreign", IntegrationUID: "kudurru-registry"}
		}
		state.rows = append(state.rows, row)
	}
	if plan.AttachHeritage(state.rows) != nil {
		return remoteState{}, errProtocol
	}
	for i := range state.rows {
		row := &state.rows[i]
		key := endpointKey(row.Endpoint)
		b, _ := json.Marshal(struct {
			Endpoint public.Endpoint
			Owner    plan.RemoteOwner
			Marker   string
		}{row.Endpoint, row.Owner, state.registry[key]})
		row.Version = kinds.Digest(b)
		state.data[key] = remoteRow{record: *row, wire: data[i].wire}
	}
	slices.SortFunc(state.rows, func(a, b plan.RemoteRecord) int { return strings.Compare(a.IDs[0], b.IDs[0]) })
	return state, nil
}

// Adjust lets the peer adjust the selected endpoints, as external-dns does
// before planning, and refuses what the instance must not publish: a name
// the peer's filter excludes, a name in the reserved registry namespace and
// a key whose registry name is not a valid DNS name.
func (p *provider) Adjust(ctx context.Context, pub host.Publication, endpoints []public.Endpoint) ([]public.Endpoint, error) {
	tr, a, r, err := p.publication(ctx)
	if err != nil {
		return nil, err
	}
	if pub.Destination != r.account || len(endpoints) > maxRecordSets {
		return nil, errInput
	}
	if len(endpoints) == 0 {
		return []public.Endpoint{}, nil
	}
	wire := make([]*externaldns.Endpoint, 0, len(endpoints))
	keys := make([]kinds.RecordKey, 0, len(endpoints))
	for _, e := range endpoints {
		n, err := plan.Normalize(e)
		if err != nil {
			return nil, errInput
		}
		if err := r.publishable(a, endpointKey(n)); err != nil {
			return nil, err
		}
		wire = append(wire, toWire(n))
		keys = append(keys, endpointKey(n))
	}
	body, err := json.Marshal(wire)
	if err != nil || p.echo(body) {
		return nil, errInput
	}
	reply, err := tr.request(ctx, http.MethodPost, "/adjustendpoints", a.media, body, nil)
	if err != nil {
		return nil, err
	}
	var adjusted []*externaldns.Endpoint
	if reply.status != http.StatusOK || reply.media != a.media || reply.partial || !arrayJSON(reply.body) || validJSONItems(reply.body, maxRecordItems) != nil || p.echo(reply.body) || decodeItems(reply.body, &adjusted, maxRecordItems) != nil || len(adjusted) != len(keys) {
		return nil, errProtocol
	}
	out := make([]public.Endpoint, 0, len(adjusted))
	for i, w := range adjusted {
		if w == nil || i >= len(keys) {
			return nil, errProtocol
		}
		n, err := plan.Normalize(toPublic(w))
		if err != nil || endpointKey(n) != keys[i] {
			return nil, errProtocol
		}
		out = append(out, n)
	}
	return out, nil
}

func (r records) publishable(a *advertisement, key kinds.RecordKey) error {
	label, _, _ := strings.Cut(key.Name, ".")
	if strings.HasPrefix(label, registryPrefix) || !inDomains(key.Name, r.domains) || !a.admits(key.Name) {
		return errInput
	}
	name, err := registryName(key)
	if err != nil || !a.admits(name) {
		return errInput
	}
	return nil
}

func inDomains(name string, domains []string) bool {
	for _, d := range domains {
		if dns.IsSubDomain(d, name) {
			return true
		}
	}
	return false
}

// admits reports whether the peer's negotiated domain filter covers name.
// Include and exclude entries match a name and its subdomains (an entry
// with a leading dot only subdomains); both regular expressions apply when
// set. This is stricter than external-dns, where a regex replaces the lists.
func (a *advertisement) admits(name string) bool {
	n := strings.ToLower(strings.TrimSuffix(name, "."))
	match := func(entry string) bool {
		entry = strings.ToLower(strings.TrimSuffix(entry, "."))
		if strings.HasPrefix(entry, ".") {
			return strings.HasSuffix(n, entry)
		}
		return n == entry || strings.HasSuffix(n, "."+entry)
	}
	include, exclude := a.filter.Include, a.filter.Exclude
	if a.media == integrationMedia {
		include, exclude = a.declaration.Filters, nil
	}
	if len(include) > 0 && !slices.ContainsFunc(include, match) || slices.ContainsFunc(exclude, match) {
		return false
	}
	if a.regexInclude != nil && !a.regexInclude.MatchString(n) || a.regexExclude != nil && a.regexExclude.MatchString(n) {
		return false
	}
	return true
}

// Apply settles one reserved intent: it requires the exact baseline, sends
// one Changes body, and confirms only from a complete reread.
func (p *provider) Apply(ctx context.Context, op *kinds.IntegrationOperation) (kinds.Receipt, error) {
	return p.execute(ctx, op, false)
}

// Recover rereads independently of the host's snapshot, which is no remote
// compare-and-swap: the peer may have changed since.
func (p *provider) Recover(ctx context.Context, op *kinds.IntegrationOperation, snapshot host.RemoteSnapshot) (kinds.Receipt, error) {
	if !snapshot.Complete {
		return kinds.Receipt{}, errInput
	}
	return p.execute(ctx, op, true)
}

func (p *provider) execute(ctx context.Context, op *kinds.IntegrationOperation, recovering bool) (kinds.Receipt, error) {
	tr, a, r, err := p.publication(ctx)
	if err != nil {
		return kinds.Receipt{}, err
	}
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	if op == nil || op.Validate() != nil || !op.Spec.AbandonedAt.IsZero() || op.Spec.Record.Owner != p.owner || op.Spec.Record.Destination != r.account || p.now().Before(op.Spec.CreatedAt) {
		return kinds.Receipt{}, errInput
	}
	switch op.Spec.Action {
	case "create", "update", "delete":
	default:
		// Adoption needs a spec field the webhook type does not have.
		return kinds.Receipt{}, errInput
	}
	if op.Spec.Policy == "create-only" && op.Spec.Action != "create" || op.Spec.Policy == "upsert-only" && op.Spec.Action == "delete" {
		return kinds.Receipt{}, errInput
	}
	key := op.Spec.Record.Key
	if err := r.publishable(a, key); err != nil {
		return kinds.Receipt{}, err
	}
	state, err := p.read(ctx, tr, a, r)
	if err != nil {
		return kinds.Receipt{}, err
	}
	facts := []kinds.TargetResult{}
	if op.Spec.Receipt != nil {
		facts = slices.Clone(op.Spec.Receipt.Targets)
	}
	receipt := p.receipt(op, state, r, facts)
	if receipt.Complete {
		return receipt, nil
	}
	current, present := state.data[key]
	owned := state.owned(key, r.marker)
	if marker, marked := state.registry[key]; marked && marker != r.marker {
		return receipt, errDrift
	}
	if present && !owned {
		return receipt, errDrift
	}
	if !recovering {
		var before *public.Endpoint
		var ids, versions []string
		if present {
			before = &current.record.Endpoint
			ids, versions = current.record.IDs, []string{current.record.Version}
		}
		expectedIDs := slices.Clone(op.Spec.ProviderIDs)
		slices.Sort(expectedIDs)
		expectedVersions := slices.Clone(op.Spec.RemoteVersions)
		slices.Sort(expectedVersions)
		if kinds.EndpointDigest(before) != op.Spec.BeforeHash || !slices.Equal(ids, expectedIDs) || !slices.Equal(versions, expectedVersions) {
			return receipt, errDrift
		}
	} else if present && kinds.EndpointDigest(&current.record.Endpoint) != op.Spec.BeforeHash && kinds.EndpointDigest(&current.record.Endpoint) != op.Spec.AfterHash {
		return receipt, errDrift
	}
	changes, err := r.changes(op, state, present, current)
	if err != nil {
		return receipt, err
	}
	body, err := json.Marshal(changes)
	if err != nil || len(body) > maxBody || p.echo(body) {
		return receipt, errInput
	}
	reply, postErr := tr.request(ctx, http.MethodPost, "/records", a.media, body, nil)
	if postErr == nil && reply.status != http.StatusNoContent && reply.status != http.StatusOK {
		postErr = errProtocol
	}
	if postErr != nil {
		// A body that may have reached the peer cannot become untouched
		// work because a later read shows the old state or also fails.
		for _, target := range attempted(op) {
			facts = addFact(facts, kinds.TargetResult{Target: target, ErrorClass: "ambiguous"})
		}
	}
	// The answer is wire evidence only (D298): confirm from a reread.
	observed, readErr := p.read(ctx, tr, a, r)
	if readErr != nil {
		if postErr != nil {
			return partial(op, facts, p.now()), postErr
		}
		return partial(op, facts, p.now()), readErr
	}
	receipt = p.receipt(op, observed, r, facts)
	if receipt.Complete {
		return receipt, nil
	}
	if postErr != nil {
		return receipt, postErr
	}
	return receipt, errDrift
}

// changes builds the one Changes body that moves key from what the reread
// showed to the intent's After state, with its registry row.
func (r records) changes(op *kinds.IntegrationOperation, state remoteState, present bool, current remoteRow) (*externaldns.Changes, error) {
	key := op.Spec.Record.Key
	name, err := registryName(key)
	if err != nil {
		return nil, err
	}
	registry := toWire(public.Endpoint{DNSName: name, RecordType: "TXT", SetIdentifier: key.SetIdentifier, RecordTTL: 300, Targets: []string{registryValue(r.marker)}})
	_, marked := state.registry[key]
	ch := &externaldns.Changes{}
	switch {
	case op.Spec.After == nil && present:
		ch.Delete = append(ch.Delete, current.wire)
		if marked {
			ch.Delete = append(ch.Delete, state.wires[key])
		}
	case op.Spec.After == nil:
		// A delete of absent data is complete before any change is built.
		return nil, errDrift
	case present:
		ch.UpdateOld = append(ch.UpdateOld, current.wire)
		ch.UpdateNew = append(ch.UpdateNew, toWire(*op.Spec.After))
		if !marked {
			ch.Create = append(ch.Create, registry)
		}
	default:
		ch.Create = append(ch.Create, toWire(*op.Spec.After))
		if !marked {
			ch.Create = append(ch.Create, registry)
		}
	}
	return ch, nil
}

// receipt is complete only when a complete list shows the intended RRset
// under this instance's registry row (or, for a delete, no RRset at all).
func (p *provider) receipt(op *kinds.IntegrationOperation, state remoteState, r records, facts []kinds.TargetResult) kinds.Receipt {
	key := op.Spec.Record.Key
	current, present := state.data[key]
	owned := state.owned(key, r.marker)
	if present && owned && op.Spec.After != nil && kinds.EndpointDigest(&current.record.Endpoint) == op.Spec.AfterHash {
		for _, target := range current.record.Endpoint.Targets {
			facts = addFact(facts, kinds.TargetResult{Target: target, ProviderID: current.record.IDs[0], Applied: true})
		}
		out := partial(op, facts, p.now())
		out.Complete, out.ContentHash, out.ProviderIDs = true, op.Spec.AfterHash, slices.Clone(current.record.IDs)
		return out
	}
	if op.Spec.After == nil && !present {
		out := partial(op, facts, p.now())
		out.Complete = true
		return out
	}
	return partial(op, facts, p.now())
}

func attempted(op *kinds.IntegrationOperation) []string {
	if op.Spec.After != nil {
		return op.Spec.After.Targets
	}
	if op.Spec.Before != nil {
		return op.Spec.Before.Targets
	}
	return nil
}

func partial(op *kinds.IntegrationOperation, facts []kinds.TargetResult, at time.Time) kinds.Receipt {
	if op.Spec.Receipt != nil && at.Before(op.Spec.Receipt.ObservedAt) {
		at = op.Spec.Receipt.ObservedAt
	}
	return kinds.Receipt{ObservedAt: at.UTC(), Targets: slices.Clone(facts)}
}

func addFact(facts []kinds.TargetResult, fact kinds.TargetResult) []kinds.TargetResult {
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

var (
	_ host.Publisher         = (*provider)(nil)
	_ host.RecoveryPublisher = (*provider)(nil)
)
