package plan

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

type currentSet struct {
	endpoint         public.Endpoint
	rows             []RemoteRecord
	owned, adoptable bool
}

// Build refuses incomplete snapshots and never removes records at a conflicted
// owner. Every operation is private and deterministic under reordered inputs.
func Build(in Input) (*Plan, error) {
	if !in.DesiredComplete || !in.CurrentComplete {
		return nil, ErrIncomplete
	}
	if err := in.Owner.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := in.Destination.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !kinds.ValidDigest(in.ConfigDigest) || !kinds.ValidDigest(in.CredentialDigest) || !kinds.ValidDigest(in.SourceDigest) {
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
	if limit > 100000 || len(in.Desired) > limit || len(in.Current) > limit || len(in.Ledger) > limit || len(in.RecordPolicies) > limit {
		return nil, ErrCapacity
	}
	encoded := 0
	checkSize := func(v any) bool {
		b, err := json.Marshal(v)
		encoded += len(b)
		return err == nil && encoded <= MaxSnapshotBytes
	}
	overrides := map[kinds.RecordKey]Policy{}
	for _, p := range in.RecordPolicies {
		if p.Key.Validate() != nil || !validPolicy(p.Policy) {
			return nil, ErrInvalid
		}
		if _, exists := overrides[p.Key]; exists {
			return nil, ErrInvalid
		}
		overrides[p.Key] = p.Policy
	}
	retainedPolicies := map[kinds.RecordKey]Policy{}
	evidence := map[kinds.RecordKey]map[string]bool{}
	ledgerTokens := map[string]string{}
	for _, l := range in.Ledger {
		if !checkSize(l) {
			return nil, ErrCapacity
		}
		if l == nil || l.Validate() != nil || l.Spec.Record.Owner != in.Owner || l.Spec.Record.Destination != in.Destination || l.ResourceVersion == "" {
			return nil, ErrInvalid
		}
		if l.Spec.Pending != nil {
			return nil, ErrIncomplete
		}
		if _, exists := ledgerTokens[l.Name]; exists {
			return nil, ErrInvalid
		}
		ledgerTokens[l.Name] = l.ResourceVersion
		ids := map[string]bool{}
		for _, id := range l.Spec.ProviderIDs {
			ids[id] = true
		}
		evidence[l.Spec.Record.Key] = ids
		if l.Spec.Policy != "" {
			retainedPolicies[l.Spec.Record.Key] = Policy(l.Spec.Policy)
		}
	}
	desired := map[kinds.RecordKey]public.Endpoint{}
	for _, e := range in.Desired {
		if !checkSize(e) {
			return nil, ErrCapacity
		}
		n, err := Normalize(e)
		if err != nil {
			return nil, err
		}
		k := key(n)
		if old, ok := desired[k]; ok {
			n, err = merge(old, n)
			if err != nil {
				return nil, err
			}
		}
		desired[k] = n
	}
	policyFor := func(k kinds.RecordKey) Policy {
		if p, ok := overrides[k]; ok {
			return p
		}
		if _, have := desired[k]; !have {
			if p, ok := retainedPolicies[k]; ok {
				return p
			}
		}
		return in.Policy
	}
	current := map[kinds.RecordKey]*currentSet{}
	seenIDs := map[string]bool{}
	for _, row := range in.Current {
		if !checkSize(row) {
			return nil, ErrCapacity
		}
		n, err := Normalize(row.Endpoint)
		if err != nil {
			return nil, err
		}
		if len(row.IDs) == 0 || len(row.IDs) > kinds.MaxTargets || len(row.Version) > 512 {
			return nil, ErrInvalid
		}
		row.Endpoint = n
		row.IDs = slices.Clone(row.IDs)
		slices.Sort(row.IDs)
		for _, id := range row.IDs {
			if id == "" || len(id) > 512 || strings.ContainsAny(id, "\x00\r\n") || seenIDs[id] {
				return nil, ErrInvalid
			}
			seenIDs[id] = true
		}
		k := key(n)
		owned := row.Owner.ClusterID == in.Owner.ClusterID && row.Owner.IntegrationUID == in.Owner.UID && row.Owner.IntegrationUID != ""
		if !owned && row.Owner.ClusterID == in.Owner.ClusterID && row.Owner.IntegrationUID == "" && row.Owner.ExternalDNSOwner == "" {
			owned = true
			for _, id := range row.IDs {
				owned = owned && evidence[k][id]
			}
		}
		unowned := row.Owner.ClusterID == "" && row.Owner.IntegrationUID == "" && row.Owner.ExternalDNSOwner == ""
		adoptable := !owned && (unowned && in.AdoptUnowned || row.Owner.ClusterID == "" && row.Owner.IntegrationUID == "" && in.AdoptExternalDNSOwner != "" && row.Owner.ExternalDNSOwner == in.AdoptExternalDNSOwner)
		if old, ok := current[k]; ok {
			old.endpoint, err = merge(old.endpoint, n)
			if err != nil {
				return nil, err
			}
			old.rows = append(old.rows, row)
			old.owned = old.owned && owned
			old.adoptable = old.adoptable && (owned || adoptable)
		} else {
			current[k] = &currentSet{n, []RemoteRecord{row}, owned, owned || adoptable}
		}
	}
	p := &Plan{Owner: in.Owner, Destination: in.Destination, ConfigDigest: in.ConfigDigest, CredentialDigest: in.CredentialDigest, SourceDigest: in.SourceDigest}
	blocked := map[string]bool{}
	conflict := func(k kinds.RecordKey, why string) {
		blocked[k.Name] = true
		p.Conflicts = append(p.Conflicts, Conflict{k.Name, k.Type, why})
	}
	cname := map[string]bool{}
	nonAlias := map[string]bool{}
	for k := range desired {
		if k.Type == "CNAME" {
			cname[k.Name] = true
		} else if k.Type != "RRSIG" && k.Type != "NSEC" {
			nonAlias[k.Name] = true
		}
	}
	for k := range desired {
		if cname[k.Name] && nonAlias[k.Name] {
			conflict(k, "desired CNAME coexistence")
		}
	}
	for k, c := range current {
		if !c.owned && !c.adoptable {
			conflict(k, "foreign or unowned owner")
		}
		if policyFor(k) != PolicySync {
			if cname[k.Name] && k.Type != "CNAME" && k.Type != "RRSIG" && k.Type != "NSEC" || k.Type == "CNAME" && nonAlias[k.Name] {
				conflict(k, "policy cannot remove CNAME conflict")
			}
		}
	}
	keys := map[kinds.RecordKey]bool{}
	for k := range desired {
		keys[k] = true
	}
	for k := range current {
		keys[k] = true
	}
	ordered := make([]kinds.RecordKey, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	slices.SortFunc(ordered, func(a, b kinds.RecordKey) int { return strings.Compare(keyString(a), keyString(b)) })
	for _, k := range ordered {
		if blocked[k.Name] {
			continue
		}
		d, have := desired[k]
		policy := policyFor(k)
		c := current[k]
		var action string
		var before, after *public.Endpoint
		if c == nil {
			if have {
				action, after = "create", d.DeepCopy()
			}
		} else if !have {
			if c.owned && policy == PolicySync {
				action, before = "delete", c.endpoint.DeepCopy()
			}
		} else if !c.owned {
			if c.adoptable && policy != PolicyCreateOnly {
				action, before, after = "adopt", c.endpoint.DeepCopy(), d.DeepCopy()
			}
		} else if kinds.EndpointDigest(&c.endpoint) == kinds.EndpointDigest(&d) {
			p.Unchanged++
		} else if policy != PolicyCreateOnly {
			action, before, after = "update", c.endpoint.DeepCopy(), d.DeepCopy()
		}
		if action == "" {
			continue
		}
		op := Operation{Key: k, Action: action, Policy: policy, Before: before, After: after}
		if c != nil {
			for _, row := range c.rows {
				op.ProviderIDs = append(op.ProviderIDs, row.IDs...)
				if row.Version != "" {
					op.RemoteVersions = append(op.RemoteVersions, row.Version)
				}
			}
			if len(op.ProviderIDs) > kinds.MaxTargets {
				return nil, ErrCapacity
			}
			slices.Sort(op.ProviderIDs)
			slices.Sort(op.RemoteVersions)
			op.RemoteVersions = slices.Compact(op.RemoteVersions)
		}
		p.Operations = append(p.Operations, op)
	}
	slices.SortFunc(p.Conflicts, func(a, b Conflict) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		if n := strings.Compare(a.Type, b.Type); n != 0 {
			return n
		}
		return strings.Compare(a.Reason, b.Reason)
	})
	p.Conflicts = slices.Compact(p.Conflicts)
	// Include observations, not merely the delta: unchanged remote versions or
	// ledger CAS changes must invalidate a previously previewed mutation.
	observed := make([]RemoteRecord, 0, len(in.Current))
	for _, c := range current {
		observed = append(observed, c.rows...)
	}
	slices.SortFunc(observed, func(a, b RemoteRecord) int {
		aJSON, _ := json.Marshal(a)
		bJSON, _ := json.Marshal(b)
		return strings.Compare(string(aJSON), string(bJSON))
	})
	desiredOrdered := make([]public.Endpoint, 0, len(desired))
	for _, k := range ordered {
		if d, ok := desired[k]; ok {
			desiredOrdered = append(desiredOrdered, d)
		}
	}
	sortedPolicies := make([]RecordPolicy, 0, len(overrides))
	for k, p := range overrides {
		sortedPolicies = append(sortedPolicies, RecordPolicy{k, p})
	}
	slices.SortFunc(sortedPolicies, func(a, b RecordPolicy) int { return strings.Compare(keyString(a.Key), keyString(b.Key)) })
	b, _ := json.Marshal(struct {
		RecordPolicies []RecordPolicy
		Plan           *Plan
		Policy         Policy
		Adopt          bool
		External       string
		Desired        []public.Endpoint
		Current        []RemoteRecord
		Ledger         map[string]string
	}{sortedPolicies, p, in.Policy, in.AdoptUnowned, in.AdoptExternalDNSOwner, desiredOrdered, observed, ledgerTokens})
	p.Digest = kinds.Digest(b)
	for i := range p.Operations {
		op := &p.Operations[i]
		b, _ := json.Marshal(struct {
			Plan   string
			Key    kinds.RecordKey
			Action string
		}{p.Digest, op.Key, op.Action})
		op.ID = kinds.Digest(b)
	}
	// Deletes precede replacements and creates, while each group is stable.
	slices.SortStableFunc(p.Operations, func(a, b Operation) int {
		rank := func(action string) int {
			switch action {
			case "delete":
				return 0
			case "update", "adopt":
				return 1
			default:
				return 2
			}
		}
		return rank(a.Action) - rank(b.Action)
	})
	for i := range p.Operations {
		if p.Intent(i, time.Unix(1, 0)).Validate() != nil {
			return nil, fmt.Errorf("%w: durable operation exceeds bounds", ErrCapacity)
		}
	}
	return p, nil
}
