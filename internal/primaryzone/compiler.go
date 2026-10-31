package primaryzone

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// CompilerOptions bounds desired metadata and removal receipts. Takeover is a
// root-supplied explicit audited authorization, never inferred from Store.Force.
type CompilerOptions struct {
	Limits                 zonestate.Limits
	MaxSources, MaxRemoved int
	RemovedRetention       time.Duration
	Takeover               func(context.Context, *zonestate.View, *zonestate.DesiredInputs) (bool, error)
}

// Compiler preserves the production selector/oldest/setIdentifier builder rules.
type Compiler struct {
	options CompilerOptions
	records *zone.RecordCache
}

// NewCompiler applies finite projection limits and a one-hour deletion receipt retention.
func NewCompiler(o CompilerOptions) *Compiler {
	o.Limits = o.Limits.Defaults()
	if o.MaxSources <= 0 {
		o.MaxSources = 100000
	}
	if o.MaxRemoved <= 0 {
		o.MaxRemoved = 256
	}
	if o.RemovedRetention <= 0 {
		o.RemovedRetention = time.Hour
	}
	return &Compiler{options: o, records: &zone.RecordCache{}}
}

// Project compiles the complete static projection, then preserves dynamic data
// and makes one content/serial decision against the exact committed base.
func (c *Compiler) Project(ctx context.Context, base *zonestate.View, in *zonestate.DesiredInputs, now time.Time) (*zonestate.Compilation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if base == nil || in == nil || !in.Complete || in.Zone == nil || len(in.Targets) > 4096 || len(in.RecordSets) > c.options.MaxSources {
		return nil, zonestate.ErrInvalid
	}
	id := base.Expected.Zone
	if base.Head != nil {
		id = base.Head.Spec.Zone
	}
	for _, target := range in.Targets {
		if target.Zone != id {
			return nil, zonestate.ErrInvalid
		}
	}
	if err := matchesZone(in.Zone, id); err != nil {
		return nil, err
	}
	left := c.options.Limits.MaxAggregateBytes
	for _, obj := range sourceObjects(in) {
		b, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}
		if int64(len(b)) > left || len(b) > store.MaxObjectBytes {
			return nil, zonestate.ErrCapacity
		}
		left -= int64(len(b))
	}
	if _, err := zone.NewRecordSetSelector(in.Zone); err != nil {
		return nil, err
	}
	policy := zonestate.Policy{AuthEpoch: 1}
	if base.Aggregate != nil {
		policy = base.Aggregate.Policy
	}
	if in.Policy != nil {
		policy = *in.Policy
	}
	if _, err := dynamicupdate.PolicyAllocation(policy.Credentials, policy.Grants, nil, left); err != nil {
		return nil, zonestate.ErrCapacity
	}
	if base.Head != nil {
		policy.AuthEpoch = base.Aggregate.Policy.AuthEpoch
		before := base.Aggregate.Policy
		before.AuthEpoch = 0
		after := policy
		after.AuthEpoch = 0
		x, _ := json.Marshal(before)
		y, _ := json.Marshal(after)
		if string(x) != string(y) {
			if policy.AuthEpoch == math.MaxUint64 {
				return nil, zonestate.ErrCapacity
			}
			policy.AuthEpoch++
		}
	}
	dz := in.Zone.DeepCopy()
	if base.Head != nil {
		dz.Status.Serial = base.Head.Spec.Serial
	} else if dz.Status.Serial == 0 {
		dz.Status.Serial = dynamicupdate.AutoSerial(0, now)
	}
	builder, errs := zone.NewBuilder(dz, zone.BuildOptions{Now: now, Records: c.records})
	if len(errs) > 0 {
		return nil, errs.ToAggregate()
	}
	if len(builder.PolicyErrors()) > 0 {
		return nil, builder.PolicyErrors().ToAggregate()
	}
	selector, _ := zone.NewRecordSetSelector(dz)
	c.records.Prune(dz, in.RecordSets)
	selected := zone.ServedRecordSetsCached(dz, in.RecordSets, c.records, now)
	owners := map[string]string{}
	rawRecords := map[string]zonestate.Record{}
	static := &zonestate.Aggregate{FormatVersion: zonekinds.FormatVersion, Zone: id, Policy: policy}
	projections := map[string]string{}
	reasons := map[string]string{}
	reserved := map[string]bool{}
	var diagnostics []zonestate.SourceDiagnostic
	for _, rs := range in.RecordSets {
		if rs == nil || rs.Spec.ZoneRef.Name != dz.Name {
			return nil, zonestate.ErrInvalid
		}
		key := store.KeyOf(rs).String()
		if selector.Admits(rs) && !c.records.RecordSetValid(dz, rs, now) {
			return nil, fmt.Errorf("%w: malformed desired source %s", zonestate.ErrInvalid, key)
		}
		projections[key] = "withheld"
		reasons[key] = "excluded by selector"
	}
	for _, rs := range selected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := store.KeyOf(rs).String()
		reasons[source] = "disabled, expired, view-scoped or outside zone"
		parsedSet := builder.ParseRecordSet(rs)
		for i := range rs.Spec.Endpoints {
			ep := &rs.Spec.Endpoints[i]
			parsed, parseErrs := parsedSet.Records(i)
			if len(parseErrs) > 0 {
				continue
			} // valid in root origin, outside this zone
			failures := parsedSet.Add(source, i)
			if len(failures) > 0 {
				reason := zone.ErrorReason(failures[0])
				if reason != public.ReasonDuplicated && reason != public.ReasonOccluded {
					return nil, failures.ToAggregate()
				}
				if projections[source] != "served" {
					projections[source] = "duplicated"
					reasons[source] = reason
				}
				continue
			}
			if !zone.Served(ep, now) {
				for _, rr := range parsed {
					name, err := dynamicupdate.CanonicalName(rr.Header().Name)
					if err != nil {
						return nil, err
					}
					key := fmt.Sprintf("%s/%d/%s", name, rr.Header().Rrtype, source)
					if !reserved[key] {
						static.Reservations = append(static.Reservations, dynamicupdate.Reservation{Name: name, Type: rr.Header().Rrtype, Subtree: rr.Header().Rrtype == dns.TypeDNAME, Source: source})
						reserved[key] = true
					}
				}
				continue
			}
			projections[source] = "served"
			reasons[source] = ""
			for _, rr := range parsed {
				identity, err := dynamicupdate.Identity(rr)
				if err != nil {
					return nil, err
				}
				if _, exists := owners[identity]; !exists {
					owners[identity] = source
				}
				rec, err := zonestate.FromRR(rr, zonestate.Provenance{SourceID: owners[identity]})
				if err != nil {
					return nil, err
				}
				if prior, exists := rawRecords[identity]; exists {
					old, _, err := dns.UnpackRR(prior.Wire, 0)
					if err != nil {
						return nil, err
					}
					if old.Header().Ttl <= rr.Header().Ttl {
						continue
					}
				}
				rawRecords[identity] = rec
			}
		}
	}
	// Finalize the genuine desired primary builder for generated apex/SOA data,
	// validation and expiry. Static endpoint raw wire is preserved separately.
	model := builder.Build()
	var recordError error
	model.RRsets(func(set *zone.RRset) bool {
		for _, rr := range set.RRs {
			identity, err := dynamicupdate.Identity(rr)
			if err != nil {
				recordError = err
				return false
			}
			if rec, exists := rawRecords[identity]; exists {
				static.Records = append(static.Records, rec)
			} else {
				rec, err := zonestate.FromRR(rr, zonestate.Provenance{SourceID: "system"})
				if err != nil {
					recordError = err
					return false
				}
				static.Records = append(static.Records, rec)
			}
		}
		return true
	})
	if recordError != nil {
		return nil, recordError
	}
	if base.Head == nil && in.Zone.Status.Serial != 0 && in.Zone.Status.ContentHash != model.ContentHash() {
		// Migration cannot reuse a formerly served serial for changed or
		// unconfirmed data. Preserve it only with an exact content-hash match.
		serial := dynamicupdate.AutoSerial(in.Zone.Status.Serial, now)
		for i, r := range static.Records {
			rr, _, err := dns.UnpackRR(r.Wire, 0)
			if err != nil {
				return nil, err
			}
			if soa, ok := rr.(*dns.SOA); ok {
				soa.Serial = serial
				static.Records[i], err = zonestate.FromRR(soa, r.Provenance)
				if err != nil {
					return nil, err
				}
				break
			}
		}
	}
	static.Expiry = model.NextExpiry()
	frozen := dz.DeepCopy()
	frozen.ResourceVersion = ""
	frozen.ManagedFields = nil
	frozen.Generation = 0
	frozen.Status = public.DNSZoneStatus{NameserverAddresses: append([]string(nil), dz.Status.NameserverAddresses...)}
	frozen.Annotations = nil
	frozen.Labels = nil
	frozen.CreationTimestamp = dz.CreationTimestamp
	config, err := json.Marshal(frozen)
	if err != nil {
		return nil, err
	}
	static.Config = config
	seenSources := map[string]bool{}
	for _, obj := range sourceObjects(in) {
		token, err := SourceToken(obj)
		if err != nil {
			return nil, err
		}
		if token.UID == "" || token.ResourceVersion == "" || seenSources[token.ID] {
			return nil, zonestate.ErrInvalid
		}
		seenSources[token.ID] = true
		projection, reason := "served", ""
		if _, isZone := obj.(*public.DNSZone); !isZone {
			projection = projections[token.ID]
			reason = reasons[token.ID]
		}
		object, err := intentObject(obj)
		if err != nil {
			return nil, err
		}
		e := zonestate.SourceEnvelope{FormatVersion: 1, State: "present", Object: object, IntentDigest: token.IntentDigest, Projection: projection, Reason: reason}
		source := zonestate.Source{ID: token.ID, UID: token.UID, ResourceVersion: token.ResourceVersion, Manager: store.ManagedBy(obj)}
		if base.Aggregate != nil {
			for _, prior := range base.Aggregate.Sources {
				if prior.ID == source.ID {
					old, err := zonestate.DecodeSourceEnvelope(prior.Desired)
					if err == nil {
						e.Acknowledged = old.Acknowledged
						if prior.UID == source.UID && old.IntentDigest == e.IntentDigest && old.State == e.State && old.Projection == e.Projection && old.Reason == e.Reason {
							source.ResourceVersion = prior.ResourceVersion
						}
					}
					break
				}
			}
		}
		if err := zonestate.AcknowledgeSource(&e, source, in.Targets); err != nil {
			return nil, err
		}
		envelope, err := zonestate.EncodeSourceEnvelope(e)
		if err != nil {
			return nil, err
		}
		source.Desired = envelope
		if source.Manager == "" {
			return nil, fmt.Errorf("%w: desired source ownership absent", zonestate.ErrInvalid)
		}
		// Status-only churn must not force commits. Keep the truthful prior observed
		// RV when the same UID/intent/projection was already committed.
		if base.Aggregate != nil {
			for _, prior := range base.Aggregate.Sources {
				if prior.ID == source.ID && prior.UID == source.UID && string(prior.Desired) == string(source.Desired) {
					source = prior
					break
				}
			}
		}
		static.Sources = append(static.Sources, source)
		diagnostics = append(diagnostics, zonestate.SourceDiagnostic{SourceID: token.ID, Outcome: projection, Reason: reason})
	}
	if err := c.retainRemovals(static, base, seenSources, in.Targets, now); err != nil {
		return nil, err
	}
	takeover := false
	if c.options.Takeover != nil {
		takeover, err = c.options.Takeover(ctx, base, in)
		if err != nil {
			return nil, err
		}
	}
	result, err := zonestate.ReplaceStatic(base, static, takeover, now, c.options.Limits)
	if err != nil {
		return nil, err
	}
	return &zonestate.Compilation{Aggregate: result, Diagnostics: diagnostics}, nil
}
func (c *Compiler) retainRemovals(a *zonestate.Aggregate, base *zonestate.View, seen map[string]bool, targets []zonestate.DesiredTarget, now time.Time) error {
	var previous []zonestate.Source
	if base.Aggregate != nil {
		previous = base.Aggregate.Sources
	}
	ledger := map[string]bool{}
	for _, s := range previous {
		ledger[s.ID] = true
	}
	// A registered deletion of a source that never reached the aggregate is
	// still observable from a complete post-registration source absence.
	for _, t := range targets {
		if seen[t.Source.ID] || ledger[t.Source.ID] || !strings.HasPrefix(t.Source.ID, "DNSRecordSet/") {
			continue
		}
		e := zonestate.SourceEnvelope{FormatVersion: 1, State: "removed", IntentDigest: t.Source.IntentDigest, Projection: "removed", RemovedAt: now}
		raw, err := zonestate.EncodeSourceEnvelope(e)
		if err != nil {
			return err
		}
		previous = append(previous, zonestate.Source{ID: t.Source.ID, UID: t.Source.UID, ResourceVersion: t.Source.ResourceVersion, Manager: "system", Desired: raw})
		ledger[t.Source.ID] = true
	}
	var removed []zonestate.Source
	for _, prior := range previous {
		if seen[prior.ID] {
			continue
		}
		envelope, err := zonestate.DecodeSourceEnvelope(prior.Desired)
		if err != nil {
			continue
		}
		if envelope.State != "removed" {
			envelope = zonestate.SourceEnvelope{FormatVersion: 1, State: "removed", IntentDigest: envelope.IntentDigest, Projection: "removed", RemovedAt: now, Acknowledged: envelope.Acknowledged}
		}
		if !now.Before(envelope.RemovedAt.Add(c.options.RemovedRetention)) {
			continue
		}
		if err := zonestate.AcknowledgeSource(&envelope, prior, targets); err != nil {
			return err
		}
		data, err := zonestate.EncodeSourceEnvelope(envelope)
		if err != nil {
			continue
		}
		prior.Desired = data
		removed = append(removed, prior)
	}
	sort.Slice(removed, func(i, j int) bool {
		a, _ := zonestate.DecodeSourceEnvelope(removed[i].Desired)
		b, _ := zonestate.DecodeSourceEnvelope(removed[j].Desired)
		if !a.RemovedAt.Equal(b.RemovedAt) {
			return a.RemovedAt.After(b.RemovedAt)
		}
		return strings.Compare(removed[i].ID, removed[j].ID) < 0
	})
	a.Sources = append(a.Sources, removed[:min(len(removed), c.options.MaxRemoved)]...)
	return nil
}
func sourceObjects(in *zonestate.DesiredInputs) []store.Object {
	out := make([]store.Object, 0, len(in.RecordSets)+1)
	out = append(out, in.Zone)
	for _, rs := range in.RecordSets {
		out = append(out, rs)
	}
	return out
}

// BuildCommitted is the actual immutable primary materializer adapter. It
// decodes only frozen config/raw RRs and never consults desired Store/cache data.
func BuildCommitted(ctx context.Context, v *zonestate.View) (*zone.Zone, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if v == nil || v.Aggregate == nil || v.Head == nil {
		return nil, zonestate.ErrInvalid
	}
	var config public.DNSZone
	if err := json.Unmarshal(v.Aggregate.Config, &config); err != nil {
		return nil, err
	}
	apex, err := dynamicupdate.CanonicalName(config.Spec.ZoneName)
	if err != nil || apex != v.Head.Spec.Zone.Apex || config.Name != v.Head.Spec.Zone.ObjectName || string(config.UID) != v.Head.Spec.Zone.UID {
		return nil, zonestate.ErrInvalid
	}
	records, err := v.Aggregate.RRs()
	if err != nil {
		return nil, err
	}
	apexOverride := false
	for i, rr := range records {
		if dns.CanonicalName(rr.Header().Name) == apex && v.Aggregate.Records[i].Provenance.SourceID != "system" {
			apexOverride = true
		}
	}
	return zone.BuildCommittedPrimary(&config, records, v.Head.Spec.Serial, v.Aggregate.Expiry, apexOverride)
}
