package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v7"
	sdk "github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

type effect struct {
	kind, id, target string
	body             sdk.RecordBatchParamsPost
}

// Apply reconciles a reserved intent after rereading its exact remote baseline.
func (p *Provider) Apply(ctx context.Context, op *kinds.IntegrationOperation) (kinds.Receipt, error) {
	return p.execute(ctx, op, false)
}

// Recover rereads independently of the host observation. An observation is not
// remote CAS, and another actor may change the provider before this callback.
func (p *Provider) Recover(ctx context.Context, op *kinds.IntegrationOperation, snapshot host.RemoteSnapshot) (kinds.Receipt, error) {
	if !snapshot.Complete {
		return kinds.Receipt{}, ErrInvalid
	}
	return p.execute(ctx, op, true)
}
func (p *Provider) execute(ctx context.Context, op *kinds.IntegrationOperation, recovering bool) (kinds.Receipt, error) {
	if err := p.available(ctx); err != nil {
		return kinds.Receipt{}, err
	}
	p.applyMu.Lock()
	defer p.applyMu.Unlock()
	if err := p.available(ctx); err != nil {
		return kinds.Receipt{}, err
	}
	if op == nil || op.Validate() != nil || !op.Spec.AbandonedAt.IsZero() || op.Spec.Record.Owner != p.owner || op.Spec.Policy == "create-only" && op.Spec.Action != "create" || op.Spec.Policy == "upsert-only" && op.Spec.Action == "delete" || p.now().Before(op.Spec.CreatedAt) {
		return kinds.Receipt{}, ErrInvalid
	}
	z, err := p.destination(op.Spec.Record.Destination)
	if err != nil || !dns.IsSubDomain(z.apex, op.Spec.Record.Key.Name) {
		return kinds.Receipt{}, ErrInvalid
	}
	snapshot, err := p.Records(ctx, z.destination)
	if err != nil {
		return kinds.Receipt{}, err
	}
	rows, err := p.rows(snapshot.Records, op, z)
	if err != nil {
		return kinds.Receipt{}, err
	}
	facts := []kinds.TargetResult{}
	if op.Spec.Receipt != nil {
		facts = slices.Clone(op.Spec.Receipt.Targets)
	}
	receipt, err := p.receipt(op, rows, facts)
	if err != nil {
		return kinds.Receipt{}, err
	}
	if receipt.Complete {
		return receipt, nil
	}
	if !recovering {
		before, ids, versions, _, err := aggregate(rows, p.owner)
		if err != nil {
			return receipt, err
		}
		expectedIDs := slices.Clone(op.Spec.ProviderIDs)
		slices.Sort(expectedIDs)
		expectedVersions := slices.Clone(op.Spec.RemoteVersions)
		slices.Sort(expectedVersions)
		if kinds.EndpointDigest(before) != op.Spec.BeforeHash || !slices.Equal(ids, expectedIDs) || !slices.Equal(versions, expectedVersions) {
			return receipt, ErrDrift
		}
	} else if !compatible(rows, op) {
		return receipt, ErrDrift
	}
	// Validate every desired wire body before the first deletion, not halfway
	// through a batch when an unsupported type/option would leave partial data.
	effects, err := p.effects(op, rows, z)
	if err != nil {
		return receipt, err
	}
	for len(effects) > 0 {
		chunk := effects[:min(200, len(effects))]
		if err := p.pause(ctx); err != nil {
			return receipt, err
		}
		result, batchErr := p.batch(ctx, z, chunk)
		if batchErr == nil {
			var factsErr error
			facts, factsErr = p.batchFacts(op, z, chunk, result, facts)
			if factsErr != nil {
				batchErr = factsErr
			}
		}
		if batchErr != nil {
			// An attempted batch with no acknowledgment cannot become "untouched"
			// merely because a later read sees its old baseline or also fails.
			for _, e := range chunk {
				if e.kind == "deletes" && op.Spec.After != nil {
					continue
				}
				facts = addFact(facts, kinds.TargetResult{Target: e.target, ErrorClass: "ambiguous"})
			}
		}
		observed, readErr := p.Records(ctx, z.destination)
		if readErr != nil {
			return partial(op, facts, p.now()), safe(ctx, readErr)
		}
		rows, rowErr := p.rows(observed.Records, op, z)
		if rowErr != nil {
			return partial(op, facts, p.now()), rowErr
		}
		receipt, err = p.receipt(op, rows, facts)
		if err != nil {
			return partial(op, facts, p.now()), err
		}
		if receipt.Complete {
			return receipt, nil
		}
		if batchErr != nil {
			return receipt, safe(ctx, batchErr)
		}
		if !observedEffects(chunk, rows) {
			return receipt, ErrDrift
		}
		// Remaining effects were never attempted. Newly created IDs cannot be
		// blindly replayed if an acknowledgment/read was ambiguous.
		effects = effects[len(chunk):]
		if len(effects) > 0 {
			fresh, err := p.effects(op, rows, z)
			if err != nil {
				return receipt, err
			}
			if !sameEffects(effects, fresh) {
				return receipt, ErrDrift
			}
		}
	}
	return receipt, ErrDrift
}
func (p *Provider) rows(all []plan.RemoteRecord, op *kinds.IntegrationOperation, z resolvedZone) ([]plan.RemoteRecord, error) {
	var out []plan.RemoteRecord
	for _, row := range all {
		if row.Endpoint.DNSName != op.Spec.Record.Key.Name {
			continue
		}
		if row.Endpoint.RecordType != op.Spec.Record.Key.Type && (op.Spec.After == nil || op.Spec.After.RecordType != "CNAME") && row.Endpoint.RecordType != "CNAME" {
			continue
		}
		own := row.Owner.ClusterID == p.owner.ClusterID && row.Owner.IntegrationUID == p.owner.UID
		adopt := op.Spec.Action == "adopt" && (row.Owner == (plan.RemoteOwner{}) && z.config.AdoptUnowned || z.config.AdoptFrom != nil && row.Owner.ClusterID == "" && row.Owner.IntegrationUID == "" && row.Owner.ExternalDNSOwner == z.config.AdoptFrom.ExternalDNSOwner)
		if !own && !adopt {
			return nil, ErrDrift
		}
		if op.Spec.After != nil && (op.Spec.After.RecordType == "CNAME" && row.Endpoint.RecordType != "CNAME" || row.Endpoint.RecordType == "CNAME" && op.Spec.After.RecordType != "CNAME") {
			return nil, ErrDrift
		}
		if row.Endpoint.RecordType == op.Spec.Record.Key.Type && row.Endpoint.SetIdentifier == op.Spec.Record.Key.SetIdentifier {
			out = append(out, row)
		}
	}
	return out, nil
}
func compatible(rows []plan.RemoteRecord, op *kinds.IntegrationOperation) bool {
	for _, row := range rows {
		matched := false
		for _, e := range []*public.Endpoint{op.Spec.Before, op.Spec.After} {
			if e == nil {
				continue
			}
			for _, t := range e.Targets {
				if row.Endpoint.Targets[0] != t {
					continue
				}
				single := e.DeepCopy()
				single.Targets = []string{t}
				if kinds.EndpointDigest(single) == kinds.EndpointDigest(&row.Endpoint) {
					matched = true
				}
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
func aggregate(rows []plan.RemoteRecord, owner kinds.Owner) (*public.Endpoint, []string, []string, bool, error) {
	var e *public.Endpoint
	var ids, versions []string
	owned := true
	for _, row := range rows {
		if len(row.IDs) != 1 || len(row.Endpoint.Targets) != 1 {
			return nil, nil, nil, false, ErrInvalid
		}
		owned = owned && row.Owner.ClusterID == owner.ClusterID && row.Owner.IntegrationUID == owner.UID
		if e == nil {
			e = row.Endpoint.DeepCopy()
		} else {
			if e.RecordTTL != row.Endpoint.RecordTTL || !slices.Equal(e.ProviderSpecific, row.Endpoint.ProviderSpecific) {
				return nil, nil, nil, false, ErrDrift
			}
			e.Targets = append(e.Targets, row.Endpoint.Targets...)
		}
		ids = append(ids, row.IDs...)
		versions = append(versions, row.Version)
	}
	if e != nil {
		slices.Sort(e.Targets)
		e.Targets = slices.Compact(e.Targets)
	}
	slices.Sort(ids)
	slices.Sort(versions)
	if len(ids) > kinds.MaxTargets {
		return nil, nil, nil, false, ErrInvalid
	}
	return e, ids, versions, owned, nil
}
func (p *Provider) receipt(op *kinds.IntegrationOperation, rows []plan.RemoteRecord, facts []kinds.TargetResult) (kinds.Receipt, error) {
	e, ids, _, owned, err := aggregate(rows, p.owner)
	// A partial batch may leave individually valid records at both old and new
	// TTL/options. This is incomplete observation, never a homogeneous RRset.
	if err != nil && err != ErrDrift {
		return kinds.Receipt{}, err
	}
	desired := map[string]bool{}
	if op.Spec.After != nil {
		for _, t := range op.Spec.After.Targets {
			desired[t] = true
		}
	}
	for _, row := range rows {
		if row.Owner.ClusterID == p.owner.ClusterID && row.Owner.IntegrationUID == p.owner.UID && desired[row.Endpoint.Targets[0]] {
			single := op.Spec.After.DeepCopy()
			single.Targets = slices.Clone(row.Endpoint.Targets)
			if kinds.EndpointDigest(single) != kinds.EndpointDigest(&row.Endpoint) {
				continue
			}
			facts = addFact(facts, kinds.TargetResult{Target: row.Endpoint.Targets[0], ProviderID: row.IDs[0], Applied: true})
		}
	}
	r := partial(op, facts, p.now())
	if err == nil && owned && kinds.EndpointDigest(e) == op.Spec.AfterHash {
		r.Complete = true
		r.ContentHash = op.Spec.AfterHash
		r.ProviderIDs = ids
	}
	return r, nil
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
func (p *Provider) effects(op *kinds.IntegrationOperation, rows []plan.RemoteRecord, z resolvedZone) ([]effect, error) {
	desired := map[string]bool{}
	if op.Spec.After != nil {
		for _, target := range op.Spec.After.Targets {
			desired[target] = true
			if _, err := p.post(*op.Spec.After, target, z); err != nil {
				return nil, err
			}
		}
	}
	existing := map[string]bool{}
	var deletes, patches, puts, posts []effect
	for _, row := range rows {
		target := row.Endpoint.Targets[0]
		if !desired[target] {
			deletes = append(deletes, effect{kind: "deletes", id: row.IDs[0], target: target})
			continue
		}
		existing[target] = true
		current := row.Endpoint
		after := *op.Spec.After.DeepCopy()
		after.Targets = []string{target}
		own := row.Owner.ClusterID == p.owner.ClusterID && row.Owner.IntegrationUID == p.owner.UID
		if kinds.EndpointDigest(&current) == kinds.EndpointDigest(&after) && own {
			continue
		}
		body, err := p.post(*op.Spec.After, target, z)
		if err != nil {
			return nil, err
		}
		kind := "puts"
		if !own {
			kind = "patches"
		}
		e := effect{kind: kind, id: row.IDs[0], target: target, body: body}
		if kind == "patches" {
			patches = append(patches, e)
		} else {
			puts = append(puts, e)
		}
	}
	if op.Spec.After != nil {
		for _, target := range op.Spec.After.Targets {
			if existing[target] {
				continue
			}
			body, err := p.post(*op.Spec.After, target, z)
			if err != nil {
				return nil, err
			}
			posts = append(posts, effect{kind: "posts", target: target, body: body})
		}
	}
	sort := func(es []effect) {
		slices.SortFunc(es, func(a, b effect) int {
			if a.target != b.target {
				return strings.Compare(a.target, b.target)
			}
			return strings.Compare(a.id, b.id)
		})
	}
	sort(deletes)
	sort(patches)
	sort(puts)
	sort(posts)
	out := append(deletes, patches...)
	out = append(out, puts...)
	return append(out, posts...), nil
}
func (p *Provider) pause(ctx context.Context) error {
	if !p.lastBatch.IsZero() {
		wait := p.cadence - p.now().Sub(p.lastBatch)
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	p.lastBatch = p.now()
	return ctx.Err()
}
func (p *Provider) batch(ctx context.Context, z resolvedZone, effects []effect) (sdk.RecordBatchResponse, error) {
	params := sdk.RecordBatchParams{ZoneID: cf.F(z.destination.Zone)}
	var deletes []sdk.RecordBatchParamsDelete
	var posts []sdk.RecordBatchParamsPostUnion
	var patches, puts []map[string]any
	for _, e := range effects {
		switch e.kind {
		case "deletes":
			deletes = append(deletes, sdk.RecordBatchParamsDelete{ID: cf.F(e.id)})
		case "posts":
			posts = append(posts, e.body)
		case "patches", "puts":
			encoded, err := e.body.MarshalJSON()
			if err != nil {
				return sdk.RecordBatchResponse{}, ErrInvalid
			}
			var body map[string]any
			if json.Unmarshal(encoded, &body) != nil {
				return sdk.RecordBatchResponse{}, ErrInvalid
			}
			body["id"] = e.id
			if e.kind == "patches" {
				patches = append(patches, body)
			} else {
				puts = append(puts, body)
			}
		}
	}
	if len(deletes) > 0 {
		params.Deletes = cf.F(deletes)
	}
	if len(posts) > 0 {
		params.Posts = cf.F(posts)
	}
	// SDK Raw fields preserve the typed Post serialization when adding IDs across
	// the generated batch union types; routing/auth/retry/decoding remain SDK-owned.
	if len(patches) > 0 {
		params.Patches = cf.Raw[[]sdk.BatchPatchUnionParam](patches)
	}
	if len(puts) > 0 {
		params.Puts = cf.Raw[[]sdk.BatchPutUnionParam](puts)
	}
	var env envelope[sdk.RecordBatchResponse]
	_, err := p.records.Batch(ctx, params, option.WithResponseBodyInto(&env))
	if err != nil {
		return sdk.RecordBatchResponse{}, safe(ctx, err)
	}
	if env.valid() != nil {
		return sdk.RecordBatchResponse{}, ErrInvalid
	}
	return env.Result, nil
}
func (p *Provider) batchFacts(op *kinds.IntegrationOperation, z resolvedZone, effects []effect, result sdk.RecordBatchResponse, facts []kinds.TargetResult) ([]kinds.TargetResult, error) {
	groups := map[string][]sdk.RecordResponse{"deletes": result.Deletes, "patches": result.Patches, "puts": result.Puts, "posts": result.Posts}
	indexes := map[string]int{}
	for _, e := range effects {
		list := groups[e.kind]
		i := indexes[e.kind]
		if i >= len(list) || !validID(list[i].ID) || e.id != "" && e.id != list[i].ID {
			return facts, ErrInvalid
		}
		indexes[e.kind]++
		if e.kind != "deletes" {
			row, err := p.convert(list[i], z)
			if err != nil || row.Owner.ClusterID != p.owner.ClusterID || row.Owner.IntegrationUID != p.owner.UID {
				return facts, ErrInvalid
			}
			want := op.Spec.After.DeepCopy()
			want.Targets = []string{e.target}
			if kinds.EndpointDigest(want) != kinds.EndpointDigest(&row.Endpoint) {
				return facts, ErrInvalid
			}
		} else if op.Spec.After != nil {
			// Update facts are bounded by After targets; old+new may total 256.
			continue
		}
		// The authenticated batch response records an applied fact even if a later
		// list fails. Recovery must not abandon it as apparently untouched work.
		facts = addFact(facts, kinds.TargetResult{Target: e.target, ProviderID: list[i].ID, Applied: true})
	}
	for key, rows := range groups {
		if len(rows) != indexes[key] {
			return facts, ErrInvalid
		}
	}
	return facts, nil
}
func observedEffects(effects []effect, rows []plan.RemoteRecord) bool {
	ids := map[string]bool{}
	targets := map[string]bool{}
	for _, row := range rows {
		ids[row.IDs[0]] = true
		targets[row.Endpoint.Targets[0]] = true
	}
	for _, e := range effects {
		if e.kind == "deletes" && ids[e.id] || e.kind != "deletes" && !targets[e.target] {
			return false
		}
	}
	return true
}
func sameEffects(a, b []effect) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].kind != b[i].kind || a[i].id != b[i].id || a[i].target != b[i].target {
			return false
		}
		if a[i].kind == "deletes" {
			continue
		}
		x, err := a[i].body.MarshalJSON()
		if err != nil {
			return false
		}
		y, err := b[i].body.MarshalJSON()
		if err != nil || !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}

var _ host.RecoveryPublisher = (*Provider)(nil)
