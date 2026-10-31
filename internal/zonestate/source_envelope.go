package zonestate

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
)

// SourceToken identifies a desired revision. ResourceVersion is an opaque equality token.
type SourceToken struct {
	ID              string `json:"id"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion"`
	IntentDigest    string `json:"intentDigest"`
}

// SourceEnvelope durably explains the projection of one actual desired source.
type SourceEnvelope struct {
	FormatVersion int                    `json:"formatVersion"`
	State         string                 `json:"state"`
	Object        json.RawMessage        `json:"object,omitempty"`
	IntentDigest  string                 `json:"intentDigest"`
	Projection    string                 `json:"projection"`
	Reason        string                 `json:"reason,omitempty"`
	RemovedAt     time.Time              `json:"removedAt,omitempty"`
	Acknowledged  []SourceAcknowledgment `json:"acknowledged,omitempty"`
}

// SourceAcknowledgment is an accepted target registered before a fresh actual
// source read. It can prove supersession without ordering opaque RVs.
type SourceAcknowledgment struct {
	Source    SourceToken `json:"source"`
	Operation string      `json:"operation"`
	Outcome   string      `json:"outcome"`
}

// AcknowledgeSource attaches bounded evidence for registered targets and prior
// durable targets. Caller must capture targets BEFORE reading actual sources.
func AcknowledgeSource(e *SourceEnvelope, source Source, targets []DesiredTarget) error {
	if len(targets) > 4096 || len(e.Acknowledged) > 256 {
		return ErrCapacity
	}
	acks := make(map[SourceAcknowledgment]bool)
	fresh := make(map[struct {
		Source    SourceToken
		Operation string
	}]bool)
	tokens := make(map[struct {
		Source    SourceToken
		Operation string
	}]bool)
	for _, a := range e.Acknowledged {
		tokens[struct {
			Source    SourceToken
			Operation string
		}{a.Source, a.Operation}] = true
	}
	for _, t := range targets {
		if t.Source.ID != source.ID {
			continue
		}
		if err := validateTarget(t); err != nil {
			return err
		}
		key := struct {
			Source    SourceToken
			Operation string
		}{t.Source, t.Operation}
		tokens[key] = true
		fresh[key] = true
	}
	if len(fresh) > 256 {
		return ErrCapacity
	}
	for t := range tokens {
		outcome := "superseded"
		if t.Source.UID == source.UID && t.Source.IntentDigest == e.IntentDigest {
			if t.Operation == "delete" {
				outcome = "pending"
				if e.State == "removed" {
					outcome = "committed"
				}
			} else if e.State == "present" {
				outcome = "committed"
			}
			if outcome == "committed" && t.Source.ResourceVersion != source.ResourceVersion {
				outcome = "committedEquivalent"
			}
			if outcome == "committed" || outcome == "committedEquivalent" {
				if e.Projection == "withheld" || e.Projection == "duplicated" {
					outcome = "withheld"
				}
			}
		}
		acks[SourceAcknowledgment{Source: t.Source, Operation: t.Operation, Outcome: outcome}] = true
	}
	e.Acknowledged = nil
	for a := range acks {
		e.Acknowledged = append(e.Acknowledged, a)
	}
	sort.Slice(e.Acknowledged, func(i, j int) bool {
		return acknowledgmentKey(e.Acknowledged[i]) < acknowledgmentKey(e.Acknowledged[j])
	})
	if len(e.Acknowledged) > 256 {
		retained := make([]SourceAcknowledgment, 0, 256)
		for _, a := range e.Acknowledged {
			if fresh[struct {
				Source    SourceToken
				Operation string
			}{a.Source, a.Operation}] {
				retained = append(retained, a)
			}
		}
		for _, a := range e.Acknowledged {
			if len(retained) == 256 {
				break
			}
			if !fresh[struct {
				Source    SourceToken
				Operation string
			}{a.Source, a.Operation}] {
				retained = append(retained, a)
			}
		}
		e.Acknowledged = retained
		sort.Slice(e.Acknowledged, func(i, j int) bool {
			return acknowledgmentKey(e.Acknowledged[i]) < acknowledgmentKey(e.Acknowledged[j])
		})
	}
	return nil
}

// Hash sorting is deterministic serialization/retention, never RV precedence.
func acknowledgmentKey(a SourceAcknowledgment) string {
	b, _ := json.Marshal(struct {
		Source    SourceToken
		Operation string
	}{a.Source, a.Operation})
	return zonekinds.Digest(b)
}

func validateTarget(t DesiredTarget) error {
	if validateZone(t.Zone) != nil || (t.Operation != "apply" && t.Operation != "delete") || !zonekinds.ValidDigest(t.Source.IntentDigest) {
		return ErrInvalid
	}
	for _, s := range []string{t.Source.ID, t.Source.UID, t.Source.ResourceVersion} {
		if s == "" || len(s) > 512 {
			return ErrInvalid
		}
	}
	if len(t.CorrelationID) > 512 {
		return ErrCapacity
	}
	return nil
}

// DesiredTarget describes a successfully persisted desired operation.
type DesiredTarget struct {
	Zone          zonekinds.ZoneID
	Source        SourceToken
	Operation     string
	CorrelationID string
}

// DesiredObservation keeps requested and observed revisions distinct, including equivalent intent.
type DesiredObservation struct {
	Target                  DesiredTarget
	Outcome                 string
	ObservedResourceVersion string
	IntentDigest            string
	CommitSequence          uint64
	Serial                  uint32
	RootDigest              string
	Reason                  string
	Published               bool
}

// EncodeSourceEnvelope validates and bounds projection evidence.
func EncodeSourceEnvelope(e SourceEnvelope) (json.RawMessage, error) {
	if e.FormatVersion != 1 || (e.State != "present" && e.State != "removed") || !zonekinds.ValidDigest(e.IntentDigest) || len(e.Reason) > 512 {
		return nil, ErrInvalid
	}
	if e.State == "present" && (len(e.Object) == 0 || len(e.Object) > 1536<<10) {
		return nil, ErrCapacity
	}
	if e.State == "removed" && (len(e.Object) != 0 || e.RemovedAt.IsZero()) {
		return nil, ErrInvalid
	}
	switch e.Projection {
	case "served", "withheld", "duplicated", "removed":
	default:
		return nil, ErrInvalid
	}
	if len(e.Acknowledged) > 256 {
		return nil, ErrCapacity
	}
	for _, a := range e.Acknowledged {
		if a.Source.ID == "" || len(a.Source.ID) > 512 || a.Source.UID == "" || len(a.Source.UID) > 512 || a.Source.ResourceVersion == "" || len(a.Source.ResourceVersion) > 512 || !zonekinds.ValidDigest(a.Source.IntentDigest) || (a.Operation != "apply" && a.Operation != "delete") {
			return nil, ErrInvalid
		}
		switch a.Outcome {
		case "pending", "committed", "committedEquivalent", "withheld", "superseded":
		default:
			return nil, ErrInvalid
		}
	}
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(b)
}

// DecodeSourceEnvelope accepts only the versioned receipt format, never legacy source payloads.
func DecodeSourceEnvelope(raw json.RawMessage) (SourceEnvelope, error) {
	var e SourceEnvelope
	if len(raw) > 2<<20 {
		return e, ErrCapacity
	}
	if err := strictDecode(raw, &e); err != nil {
		return e, err
	}
	if _, err := EncodeSourceEnvelope(e); err != nil {
		return e, fmt.Errorf("%w: envelope", err)
	}
	return e, nil
}

func observe(view *View, target DesiredTarget, published bool) DesiredObservation {
	out := DesiredObservation{Target: target, Outcome: "pending"}
	if view == nil || view.Head == nil || view.Aggregate == nil || view.Head.Spec.Zone != target.Zone {
		return out
	}
	for _, s := range view.Aggregate.Sources {
		if s.ID != target.Source.ID {
			continue
		}
		e, err := DecodeSourceEnvelope(s.Desired)
		if err != nil {
			out.Outcome = "unknown"
			out.Reason = "unrecognized source evidence"
			return out
		}
		out.ObservedResourceVersion = s.ResourceVersion
		out.IntentDigest = e.IntentDigest
		for _, a := range e.Acknowledged {
			if a.Source == target.Source && a.Operation == target.Operation && a.Outcome == "superseded" {
				out.Outcome = "superseded"
				out.CommitSequence = view.Head.Spec.CommitSequence
				out.Serial = view.Head.Spec.Serial
				out.RootDigest = view.Head.Spec.RootDigest
				out.Published = published
				return out
			}
		}
		if s.UID != target.Source.UID {
			return out
		}
		if target.Operation == "delete" {
			if e.State != "removed" || e.IntentDigest != target.Source.IntentDigest {
				return out
			}
		} else {
			if e.State == "removed" || e.IntentDigest != target.Source.IntentDigest {
				return out
			}
		}
		out.Outcome = "committed"
		if s.ResourceVersion != target.Source.ResourceVersion {
			out.Outcome = "committedEquivalent"
		}
		if e.Projection == "withheld" || e.Projection == "duplicated" {
			out.Outcome = "withheld"
		}
		out.Reason = e.Reason
		out.CommitSequence = view.Head.Spec.CommitSequence
		out.Serial = view.Head.Spec.Serial
		out.RootDigest = view.Head.Spec.RootDigest
		out.Published = published
		return out
	}
	return out
}
