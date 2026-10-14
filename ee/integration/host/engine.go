package host

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type publicationInput struct {
	publication Publication
	remote      RemoteSnapshot
	planInput   plan.Input
}

func (h *Host) publications(ctx context.Context, j *job, f frame, g ExecutionGuard) ([]publicationInput, error) {
	publisher, ok := j.provider.(Publisher)
	if !ok {
		return nil, ErrUnsupported
	}
	pubs, err := publisher.Destinations(ctx)
	if err != nil {
		return nil, safeError(ctx, err)
	}
	if len(pubs) > 256 {
		return nil, ErrCapacity
	}
	seen := map[kinds.Destination]bool{}
	out := make([]publicationInput, 0, len(pubs))
	for _, pub := range pubs {
		if pub.Destination.Validate() != nil || seen[pub.Destination] {
			return nil, ErrInvalid
		}
		seen[pub.Destination] = true
		selection, err := plan.Select(plan.SelectionInput{Owner: f.instance.owner, Sources: f.input.Sources, Select: pub.Selection, Domains: pub.Domains, Policy: pub.Policy, Complete: f.input.Complete, Now: h.opts.Now()})
		if err != nil {
			return nil, err
		}
		desired := make([]public.Endpoint, 0, len(selection.Records))
		for _, s := range selection.Records {
			desired = append(desired, *s.Endpoint.DeepCopy())
		}
		adjusted, err := publisher.Adjust(ctx, pub, desired)
		if err != nil {
			return nil, safeError(ctx, err)
		}
		// Strong adapters retain cardinality and order; this binds policy/provenance
		// even when a configured target override changes A to CNAME.
		if len(adjusted) != len(desired) {
			return nil, ErrProvider
		}
		policies := map[kinds.RecordKey]plan.Policy{}
		for i, e := range adjusted {
			n, err := plan.Normalize(e)
			if err != nil {
				return nil, err
			}
			original, err := plan.Normalize(selection.Records[i].Endpoint)
			if err != nil || n.DNSName != original.DNSName || n.SetIdentifier != original.SetIdentifier {
				return nil, ErrProvider
			}
			key := kinds.RecordKey{Name: n.DNSName, Type: n.RecordType, SetIdentifier: n.SetIdentifier}
			p := selection.Records[i].Policy
			if old, exists := policies[key]; exists && old != p {
				return nil, ErrInvalid
			}
			policies[key] = p
		}
		var recordPolicies []plan.RecordPolicy
		for key, p := range policies {
			recordPolicies = append(recordPolicies, plan.RecordPolicy{Key: key, Policy: p})
		}
		remote, err := publisher.Records(ctx, pub.Destination)
		if err != nil {
			return nil, safeError(ctx, err)
		}
		if !remote.Complete {
			return nil, plan.ErrIncomplete
		}
		ledgers, err := h.ledgers(ctx, f.instance.owner, pub.Destination)
		if err != nil {
			return nil, err
		}
		in := publicationInput{pub, remote, plan.Input{Owner: f.instance.owner, Destination: pub.Destination, ConfigDigest: g.Ledger.ConfigDigest, CredentialDigest: g.Ledger.CredentialDigest, SourceDigest: selection.Digest, Policy: pub.Policy, RecordPolicies: recordPolicies, Desired: adjusted, Current: remote.Records, Ledger: ledgers, DesiredComplete: true, CurrentComplete: true, AdoptUnowned: pub.AdoptUnowned, AdoptExternalDNSOwner: pub.AdoptExternalDNSOwner}}
		// Validate the complete observation even when a pending operation stops
		// ordinary planning early. This temporary plan grants no execution rights.
		if _, err := validatePendingObservation(in.planInput); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b publicationInput) int {
		x, _ := json.Marshal(a.publication.Destination)
		y, _ := json.Marshal(b.publication.Destination)
		return strings.Compare(string(x), string(y))
	})
	if err := h.fence(ctx, j, g); err != nil {
		return nil, err
	}
	return out, nil
}

func observationPlan(in plan.Input) (*plan.Plan, error) {
	in.Ledger = slices.Clone(in.Ledger)
	for i, l := range in.Ledger {
		in.Ledger[i] = l.DeepCopy()
		in.Ledger[i].Spec.Pending = nil
	}
	return plan.Build(in)
}

// validatePendingObservation exposes only conflicts, never a projected plan,
// operation or digest. Ordinary planning continues to consume all original rows.
func validatePendingObservation(in plan.Input) ([]plan.Conflict, error) {
	pending := map[kinds.RecordKey]bool{}
	for _, l := range in.Ledger {
		if l != nil && l.Spec.Pending != nil {
			pending[l.Spec.Record.Key] = true
		}
	}
	if len(pending) == 0 {
		checked, err := observationPlan(in)
		if err != nil {
			return nil, err
		}
		return checked.Conflicts, nil
	}
	limit := in.MaxRecords
	if limit <= 0 {
		limit = 10000
	}
	if len(in.Current) > limit {
		return nil, plan.ErrCapacity
	}
	bytes := 0
	for _, value := range []any{in.Desired, in.Current, in.Ledger} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, plan.ErrInvalid
		}
		bytes += len(encoded)
		if bytes > plan.MaxSnapshotBytes {
			return nil, plan.ErrCapacity
		}
	}
	seen := map[string]bool{}
	counts := map[kinds.RecordKey]int{}
	original := in.Current
	in.Current = make([]plan.RemoteRecord, 0, len(original))
	for _, row := range original {
		n, err := plan.Normalize(row.Endpoint)
		if err != nil {
			return nil, err
		}
		if len(row.IDs) == 0 || len(row.IDs) > kinds.MaxTargets || len(row.Version) > 512 {
			return nil, plan.ErrInvalid
		}
		for _, id := range row.IDs {
			if id == "" || len(id) > 512 || strings.ContainsAny(id, "\x00\r\n") || seen[id] {
				return nil, plan.ErrInvalid
			}
			seen[id] = true
		}
		key := kinds.RecordKey{Name: n.DNSName, Type: n.RecordType, SetIdentifier: n.SetIdentifier}
		if pending[key] {
			counts[key] += len(row.IDs)
			if counts[key] > kinds.MaxTargets {
				return nil, plan.ErrCapacity
			}
		} else {
			in.Current = append(in.Current, row)
		}
	}
	checked, err := observationPlan(in)
	if err != nil {
		return nil, err
	}
	return checked.Conflicts, nil
}

type pendingState struct {
	current                       *public.Endpoint
	ids                           []string
	homogeneous, owned, resumable bool
}

// pendingRows authorizes only an exact pending operation, never ownership for
// ordinary planning. An unstamped adoption row must still be its captured old
// ID/content/version; an ID created since intent requires current UID ownership
// and exact After content. A current marker alone cannot authorize extra data.
func pendingRows(in plan.Input, op *kinds.IntegrationOperation) (pendingState, error) {
	state := pendingState{homogeneous: true, owned: true, resumable: true}
	if !in.CurrentComplete || !in.DesiredComplete {
		return state, plan.ErrIncomplete
	}
	beforeIDs := map[string]bool{}
	for _, id := range op.Spec.ProviderIDs {
		beforeIDs[id] = true
	}
	versions := map[string]bool{}
	for _, version := range op.Spec.RemoteVersions {
		versions[version] = true
	}
	for _, row := range in.Current {
		n, err := plan.Normalize(row.Endpoint)
		if err != nil {
			return state, err
		}
		if n.DNSName != op.Spec.Record.Key.Name {
			continue
		}
		own := row.Owner.ClusterID == in.Owner.ClusterID && row.Owner.IntegrationUID == in.Owner.UID && row.Owner.ExternalDNSOwner == ""
		adoptable := explicitAdoption(in, row.Owner)
		// Preserve same-name foreign protection even when a sibling is also pending.
		if !own && !adoptable {
			state.resumable = false
		}
		if (n.RecordType == "CNAME" && op.Spec.Record.Key.Type != "CNAME" && op.Spec.Record.Key.Type != "RRSIG" && op.Spec.Record.Key.Type != "NSEC") || (op.Spec.Record.Key.Type == "CNAME" && n.RecordType != "CNAME" && n.RecordType != "RRSIG" && n.RecordType != "NSEC") {
			state.resumable = false
			return state, ledger.ErrPending
		}
		key := kinds.RecordKey{Name: n.DNSName, Type: n.RecordType, SetIdentifier: n.SetIdentifier}
		if key != op.Spec.Record.Key {
			continue
		}
		before, after := endpointPart(n, op.Spec.Before), endpointPart(n, op.Spec.After)
		oldIDs := len(row.IDs) > 0
		for _, id := range row.IDs {
			oldIDs = oldIDs && beforeIDs[id]
		}
		eligible := own && (before || after) && (oldIDs || after)
		if !own {
			eligible = op.Spec.Action == "adopt" && adoptable && oldIDs && before && (len(versions) == 0 || versions[row.Version])
		}
		state.resumable = state.resumable && eligible
		state.owned = state.owned && own
		state.ids = append(state.ids, row.IDs...)
		if state.current == nil {
			state.current = n.DeepCopy()
		} else {
			if state.current.RecordTTL != n.RecordTTL || !slices.Equal(state.current.ProviderSpecific, n.ProviderSpecific) {
				state.homogeneous = false
			}
			state.current.Targets = append(state.current.Targets, n.Targets...)
		}
	}
	slices.Sort(state.ids)
	if state.current != nil {
		slices.Sort(state.current.Targets)
		state.current.Targets = slices.Compact(state.current.Targets)
	}
	if !state.homogeneous {
		state.current = nil
	}
	return state, nil
}
func explicitAdoption(in plan.Input, owner plan.RemoteOwner) bool {
	if owner.ClusterID != "" || owner.IntegrationUID != "" {
		return false
	}
	return owner.ExternalDNSOwner == "" && in.AdoptUnowned || in.AdoptExternalDNSOwner != "" && owner.ExternalDNSOwner == in.AdoptExternalDNSOwner
}
func endpointPart(row public.Endpoint, whole *public.Endpoint) bool {
	if whole == nil {
		return false
	}
	expected, err := plan.Normalize(*whole)
	if err != nil {
		return false
	}
	for _, target := range row.Targets {
		if !slices.Contains(expected.Targets, target) {
			return false
		}
	}
	expected.Targets = slices.Clone(row.Targets)
	return kinds.EndpointDigest(&expected) == kinds.EndpointDigest(&row)
}

func (h *Host) ledgers(ctx context.Context, owner kinds.Owner, destination kinds.Destination) ([]*kinds.IntegrationLedger, error) {
	var out []*kinds.IntegrationLedger
	continuation := ""
	seen := map[string]bool{}
	count, bytes := 0, 0
	for {
		page, err := h.opts.Reader.List(ctx, kinds.KindIntegrationLedger, store.ListOptions{Limit: 256, Continue: continuation})
		if err != nil {
			return nil, err
		}
		for _, o := range page.Items {
			count++
			if count > 100000 {
				return nil, ErrCapacity
			}
			l, ok := o.(*kinds.IntegrationLedger)
			if !ok || l == nil {
				return nil, ErrInvalid
			}
			if l.Spec.Record.Owner != owner || l.Spec.Record.Destination != destination {
				continue
			}
			if l.Validate() != nil || l.ResourceVersion == "" || seen[l.Name] {
				return nil, ErrInvalid
			}
			b, _ := json.Marshal(l)
			bytes += len(b)
			if bytes > plan.MaxSnapshotBytes {
				return nil, ErrCapacity
			}
			seen[l.Name] = true
			out = append(out, l.DeepCopy())
		}
		if page.Continue == "" {
			break
		}
		if seen["page:"+page.Continue] || page.Continue == continuation {
			return nil, ErrInvalid
		}
		seen["page:"+page.Continue] = true
		continuation = page.Continue
	}
	return out, nil
}
func buildPreview(inputs []publicationInput) (*Preview, error) {
	p := &Preview{}
	for _, in := range inputs {
		built, err := plan.Build(in.planInput)
		if err != nil {
			return nil, err
		}
		p.Plans = append(p.Plans, built)
	}
	var digests []string
	for _, plan := range p.Plans {
		digests = append(digests, plan.Digest)
	}
	b, _ := json.Marshal(digests)
	p.Digest = kinds.Digest(b)
	return p, nil
}
func (h *Host) preview(ctx context.Context, j *job, f frame, g ExecutionGuard) (*Preview, error) {
	inputs, err := h.publications(ctx, j, f, g)
	if err != nil {
		return nil, err
	}
	return buildPreview(inputs)
}
func (h *Host) sync(ctx context.Context, j *job, f frame, g ExecutionGuard, expected string) (*Result, error) {
	inputs, err := h.publications(ctx, j, f, g)
	if err != nil {
		return nil, err
	}
	result := &Result{}
	// Recovery records only fresh remote facts. A nonempty obsolete preview is
	// rejected before recovery changes any durable token or external resource.
	for _, in := range inputs {
		for _, l := range in.planInput.Ledger {
			if l.Spec.Pending != nil {
				if expected != "" {
					return result, ErrStalePreview
				}
				if err := h.recover(ctx, j, g, in, l); err != nil {
					result.Pending++
					return result, err
				}
			}
		}
	}
	// Recovery changed ledger CAS tokens; rebuild complete observations.
	hadRecovery := false
	for _, in := range inputs {
		for _, l := range in.planInput.Ledger {
			hadRecovery = hadRecovery || l.Spec.Pending != nil
		}
	}
	if hadRecovery {
		inputs, err = h.publications(ctx, j, f, g)
		if err != nil {
			return result, err
		}
	}
	preview, err := buildPreview(inputs)
	if err != nil {
		return result, err
	}
	result.PreviewDigest = preview.Digest
	if expected != "" && expected != preview.Digest {
		return result, ErrStalePreview
	}
	publisher := j.provider.(Publisher)
	for _, p := range preview.Plans {
		result.Conflicts += len(p.Conflicts)
		result.Unchanged += p.Unchanged
		ledgers, err := h.ledgers(ctx, f.instance.owner, p.Destination)
		if err != nil {
			return result, err
		}
		tokens := map[kinds.RecordKey]string{}
		for _, l := range ledgers {
			tokens[l.Spec.Record.Key] = l.ResourceVersion
		}
		// Refuse to silently substitute fresh CAS tokens under this plan. The
		// planner bound exactly these ledgers; fresh changes require another Sync.
		original := map[kinds.RecordKey]string{}
		for _, in := range inputs {
			if in.planInput.Destination == p.Destination {
				for _, l := range in.planInput.Ledger {
					original[l.Spec.Record.Key] = l.ResourceVersion
				}
			}
		}
		if len(tokens) != len(original) {
			return result, ErrStalePreview
		}
		for key, rv := range tokens {
			if original[key] != rv {
				return result, ErrStalePreview
			}
		}
		for i := range p.Operations {
			if err := h.fence(ctx, j, g); err != nil {
				return result, err
			}
			intent := p.Intent(i, h.opts.Now())
			res, err := h.opts.Ledger.Begin(ctx, g.Ledger, intent, tokens[intent.Spec.Record.Key])
			if err != nil {
				return result, err
			}
			if res.Operation.Spec.Receipt != nil || !res.Operation.Spec.AbandonedAt.IsZero() {
				result.Pending++
				return result, ledger.ErrPending
			}
			if err := h.fence(ctx, j, g); err != nil {
				result.Pending++
				return result, err
			}
			receipt, applyErr := publisher.Apply(ctx, res.Operation.DeepCopy())
			if !receipt.ObservedAt.IsZero() {
				saved, saveErr := h.opts.Ledger.RecordReceipt(ctx, g.Ledger, res.Operation, receipt)
				if saveErr != nil {
					result.Pending++
					return result, saveErr
				}
				res.Operation = saved
			}
			if res.Operation.Spec.Receipt != nil && res.Operation.Spec.Receipt.Complete {
				confirmed, err := h.opts.Ledger.Confirm(ctx, g.Ledger, res)
				if err != nil {
					result.Pending++
					return result, err
				}
				tokens[intent.Spec.Record.Key] = confirmed.ResourceVersion
				result.Applied++
			} else {
				result.Pending++
				if applyErr != nil {
					return result, safeError(ctx, applyErr)
				}
				return result, ledger.ErrPending
			}
			if applyErr != nil {
				return result, safeError(ctx, applyErr)
			}
		}
	}
	return result, nil
}

func (h *Host) recover(ctx context.Context, j *job, g ExecutionGuard, in publicationInput, l *kinds.IntegrationLedger) error {
	op, err := h.opts.Ledger.ReadOperation(ctx, l.Spec.Pending.OperationID)
	if err != nil {
		return err
	}
	if op.Spec.Record != l.Spec.Record || op.Spec.Record.Owner != g.Ledger.Owner || in.planInput.Owner != op.Spec.Record.Owner || in.planInput.Destination != op.Spec.Record.Destination || op.Spec.PlanDigest != l.Spec.Pending.PlanDigest || op.Spec.AfterHash != l.Spec.Pending.AfterHash {
		return ErrInvalid
	}
	res := &ledger.Reservation{Ledger: l, Operation: op}
	if op.Spec.Receipt != nil && op.Spec.Receipt.Complete {
		_, err := h.opts.Ledger.Confirm(ctx, g.Ledger, res)
		return err
	}
	state, err := pendingRows(in.planInput, op)
	if err != nil {
		return err
	}
	conflicts, err := validatePendingObservation(in.planInput)
	if err != nil {
		return err
	}
	for _, conflict := range conflicts {
		if conflict.Name == op.Spec.Record.Key.Name {
			return ledger.ErrPending
		}
	}
	// Mixed rows have no whole-RRset hash. In particular, absence (empty hash)
	// must never be fabricated by discarding the inconsistent aggregate.
	hash := ""
	if state.homogeneous {
		hash = kinds.EndpointDigest(state.current)
	}
	if err := h.fence(ctx, j, g); err != nil {
		return err
	}
	if !op.Spec.AbandonedAt.IsZero() {
		if !state.homogeneous || !state.resumable || hash != op.Spec.BeforeHash {
			return ledger.ErrPending
		}
		_, err := h.opts.Ledger.Abandon(ctx, g.Ledger, res, ledger.Observation{BeforeHash: hash, ObservedAt: h.opts.Now()})
		return err
	}
	if state.homogeneous && state.resumable && hash == op.Spec.AfterHash && state.owned {
		receipt := kinds.Receipt{Complete: true, ContentHash: hash, ProviderIDs: state.ids, ObservedAt: h.opts.Now()}
		// Preserve durable target facts as required by the monotone receipt API.
		if op.Spec.Receipt != nil {
			receipt.Targets = slices.Clone(op.Spec.Receipt.Targets)
		}
		saved, err := h.opts.Ledger.RecordReceipt(ctx, g.Ledger, op, receipt)
		if err != nil {
			return err
		}
		res.Operation = saved
		_, err = h.opts.Ledger.Confirm(ctx, g.Ledger, res)
		return err
	}
	// Untouched work can be safely terminalized after this worker rereads a
	// complete snapshot. No provider call is in flight in this serial worker.
	beforeIDs := slices.Clone(op.Spec.ProviderIDs)
	slices.Sort(beforeIDs)
	if state.homogeneous && state.resumable && hash == op.Spec.BeforeHash && slices.Equal(state.ids, beforeIDs) && (op.Spec.Receipt == nil || len(op.Spec.Receipt.Targets) == 0) {
		_, err := h.opts.Ledger.Abandon(ctx, g.Ledger, res, ledger.Observation{BeforeHash: hash, ObservedAt: h.opts.Now()})
		return err
	}
	resume, ok := j.provider.(RecoveryPublisher)
	if !ok || !state.resumable || op.Spec.ConfigDigest != g.Ledger.ConfigDigest || op.Spec.CredentialDigest != g.Ledger.CredentialDigest || !recoveryDesired(in.planInput, op) {
		return ledger.ErrPending
	}
	if err := h.fence(ctx, j, g); err != nil {
		return err
	}
	receipt, applyErr := resume.Recover(ctx, op.DeepCopy(), in.remote)
	if !receipt.ObservedAt.IsZero() {
		saved, err := h.opts.Ledger.RecordReceipt(ctx, g.Ledger, op, receipt)
		if err != nil {
			return err
		}
		res.Operation = saved
	}
	if res.Operation.Spec.Receipt != nil && res.Operation.Spec.Receipt.Complete {
		_, err := h.opts.Ledger.Confirm(ctx, g.Ledger, res)
		return err
	}
	if applyErr != nil {
		return safeError(ctx, applyErr)
	}
	return ledger.ErrPending
}

func recoveryDesired(in plan.Input, op *kinds.IntegrationOperation) bool {
	var desired *public.Endpoint
	for _, e := range in.Desired {
		n, err := plan.Normalize(e)
		if err != nil {
			return false
		}
		key := kinds.RecordKey{Name: n.DNSName, Type: n.RecordType, SetIdentifier: n.SetIdentifier}
		if key != op.Spec.Record.Key {
			continue
		}
		if desired == nil {
			desired = n.DeepCopy()
		} else {
			if desired.RecordTTL != n.RecordTTL || !slices.Equal(desired.ProviderSpecific, n.ProviderSpecific) {
				return false
			}
			desired.Targets = append(desired.Targets, n.Targets...)
		}
	}
	if desired != nil {
		slices.Sort(desired.Targets)
		desired.Targets = slices.Compact(desired.Targets)
	}
	policy := in.Policy
	for _, p := range in.RecordPolicies {
		if p.Key == op.Spec.Record.Key {
			policy = p.Policy
		}
	}
	if policy == "" {
		policy = plan.PolicySync
	}
	if policy == plan.PolicyCreateOnly && op.Spec.Action != "create" || policy == plan.PolicyUpsertOnly && op.Spec.Action == "delete" {
		return false
	}
	return kinds.EndpointDigest(desired) == op.Spec.AfterHash
}
