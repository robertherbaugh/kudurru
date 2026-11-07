package primaryruntime

import (
	"slices"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PrimaryStatusObservation describes exact persisted lifecycle/head metadata
// from one complete image, before desired coverage overlays. It is evidence for
// a status CAS, never an acknowledgment of publication or a readiness verdict.
type PrimaryStatusObservation struct {
	Member                   Member
	LifecycleResourceVersion string
	Source                   zk.LifecycleSource
	HeadZone                 zk.ZoneID
	HeadResourceVersion      string
	CommittedSource          zonestate.SourceToken
	ConfigMatchesDesired     bool
	Serial                   uint32
	ContentHash              string
	RecordCount              int
	ExpiresAt                time.Time
}

type primaryModelStatus struct {
	serial uint32
	hash   string
	count  int
}

// CommittedPrimaryStatus looks up the exact cluster-scoped metadata.name. A
// former UID/apex remains observable even when new desired coverage hides it.
// An empty head RV proves head absence in this image; HeadZone still identifies
// that absence for CAS. Parents are detached and Published is always false.
// Callers must separately check current source, admission and protected serving
// coverage. Serial/hash/count describe durable data even after absolute expiry.
func (p *PreparedImage) CommittedPrimaryStatus(objectName string) (PrimaryStatusObservation, bool) {
	if p == nil {
		return PrimaryStatusObservation{}, false
	}
	o, ok := p.status[objectName]
	o.Member.Parents = slices.Clone(o.Member.Parents)
	return o, ok
}

func imageStatus(e *entry, desired *public.DNSZone, raw primaryModelStatus) PrimaryStatusObservation {
	l := e.lifecycle
	o := PrimaryStatusObservation{Member: e.member, LifecycleResourceVersion: l.ResourceVersion,
		Source: l.Spec.Source, HeadZone: l.Spec.HeadZone(), Serial: raw.serial,
		ContentHash: raw.hash, RecordCount: raw.count, ExpiresAt: e.expiry}
	o.Member.Parents = slices.Clone(o.Member.Parents)
	o.Member.Published = false
	if e.view == nil {
		return o
	}
	o.HeadResourceVersion = e.view.Head.ResourceVersion
	id := (store.Key{Kind: "DNSZone", Name: l.Spec.Zone.ObjectName}).String()
	for _, source := range e.view.Aggregate.Sources {
		if source.ID != id {
			continue
		}
		envelope, err := zonestate.DecodeSourceEnvelope(source.Desired)
		if err != nil {
			return o // Unknown receipt versions never attest to current config.
		}
		o.CommittedSource = zonestate.SourceToken{ID: source.ID, UID: source.UID,
			ResourceVersion: source.ResourceVersion, IntentDigest: envelope.IntentDigest}
		if desired == nil || envelope.State != "present" || envelope.Projection != "served" ||
			l.Spec.Source.Removed || l.Spec.Type != public.ZoneTypePrimary ||
			string(desired.UID) != l.Spec.Zone.UID || canonical(desired.Spec.ZoneName) != l.Spec.Zone.Apex {
			return o
		}
		token, err := primaryzone.SourceToken(desired)
		if err != nil || token.UID != source.UID || token.IntentDigest != envelope.IntentDigest {
			return o
		}
		if !imageStatusReceiptMatches(envelope, desired) {
			return o
		}
		var frozen public.DNSZone
		if imageStrictJSON(e.view.Aggregate.Config, &frozen) != nil ||
			frozen.Name != desired.Name || frozen.Namespace != desired.Namespace ||
			string(frozen.UID) != o.HeadZone.UID {
			return o
		}
		// RVs are equality tokens, never ordered or replaced here. Same UID and
		// intent may match despite a status-only RV change; NS addresses remain
		// a separate frozen serving dependency not included in desired intent.
		o.ConfigMatchesDesired = equality.Semantic.DeepEqual(frozen.Spec, desired.Spec) &&
			equality.Semantic.DeepEqual(frozen.Status.NameserverAddresses, desired.Status.NameserverAddresses)
		return o
	}
	return o
}

func imageStatusReceiptMatches(envelope zonestate.SourceEnvelope, desired *public.DNSZone) bool {
	// The version-one source envelope holds an intent object, not an API object
	// (manager is explicit, status/RV are absent). Unknown or malformed evidence
	// cannot attest to current configuration even if its claimed digest matches.
	var receipt struct {
		Metadata struct {
			Name      string            `json:"name"`
			Namespace string            `json:"namespace,omitempty"`
			UID       string            `json:"uid"`
			Created   metav1.Time       `json:"creationTimestamp"`
			Labels    map[string]string `json:"labels,omitempty"`
			Manager   string            `json:"manager"`
		} `json:"metadata"`
		Spec public.DNSZoneSpec `json:"spec"`
	}
	if imageStrictJSON(envelope.Object, &receipt) != nil {
		return false
	}
	m := receipt.Metadata
	return m.Name == desired.Name && m.Namespace == desired.Namespace && m.UID == string(desired.UID) &&
		m.Manager == store.ManagedBy(desired) && m.Created.Equal(&desired.CreationTimestamp) &&
		equality.Semantic.DeepEqual(m.Labels, desired.Labels) && equality.Semantic.DeepEqual(receipt.Spec, desired.Spec)
}

func imageStatusDeadlines(status map[string]PrimaryStatusObservation) {
	// Use persisted members, not desired-only placeholders. Ancestors first
	// propagates deadlines once, including a hidden retired former UID/apex.
	ordered := make([]string, 0, len(status))
	byApex := make(map[string]string, len(status))
	for name, o := range status {
		ordered = append(ordered, name)
		byApex[o.Member.Zone.Apex] = name
	}
	slices.SortFunc(ordered, func(a, b string) int { return len(status[a].Member.Zone.Apex) - len(status[b].Member.Zone.Apex) })
	for _, name := range ordered {
		o := status[name]
		for _, parent := range o.Member.Parents {
			expiry := status[byApex[parent.Zone.Apex]].ExpiresAt
			if !expiry.IsZero() && (o.ExpiresAt.IsZero() || expiry.Before(o.ExpiresAt)) {
				o.ExpiresAt = expiry
			}
		}
		status[name] = o
	}
}
