package main

import (
	"encoding/json"
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// adoptionPendingPrimary inspects only one complete image and its matching
// captured runtime owner. It never joins a newer repository head to that owner.
func adoptionPendingPrimary(t *testing.T, image *replication.Image, owner *servingbuild.RuntimeOwner) (records, notifications int) {
	t.Helper()
	heads := map[zk.ZoneID]*zk.ZoneState{}
	chunks := map[string]*zk.ZoneChunk{}
	var zones []*public.DNSZone
	var desired []*public.DNSRecordSet
	for _, raw := range image.Payload.Objects {
		var header metav1.TypeMeta
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatal(err)
		}
		switch header.Kind {
		case zk.KindZoneState:
			var head zk.ZoneState
			if err := json.Unmarshal(raw, &head); err != nil {
				t.Fatal(err)
			}
			heads[head.Spec.Zone] = &head
		case zk.KindZoneChunk:
			var chunk zk.ZoneChunk
			if err := json.Unmarshal(raw, &chunk); err != nil {
				t.Fatal(err)
			}
			chunks[chunk.Spec.Digest] = &chunk
		case "DNSZone":
			var zone public.DNSZone
			if err := json.Unmarshal(raw, &zone); err != nil {
				t.Fatal(err)
			}
			zones = append(zones, &zone)
		case "DNSRecordSet":
			var record public.DNSRecordSet
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			desired = append(desired, &record)
		}
	}
	// A historical/orphaned head can intentionally retain undeliverable
	// notification intent. Only current live primary members have work that
	// can race this fixture's next request. Match the captured lifecycle's
	// actual head, never a former source UID or a newer independently read head.
	live := map[string]*zk.ZoneState{}
	for _, zone := range zones {
		if zone.Spec.Type != public.ZoneTypePrimary || zone.DeletionTimestamp != nil {
			continue
		}
		observation, ok := owner.Primary().CommittedPrimaryStatus(zone.Name)
		if !ok || observation.Member.Phase != zk.PhaseActive || observation.Member.Type != public.ZoneTypePrimary ||
			observation.Member.Zone.UID != string(zone.UID) || observation.Member.Zone.Apex != dns.CanonicalName(zone.Spec.ZoneName) {
			continue
		}
		head := heads[observation.HeadZone]
		if head == nil || head.Spec.CommitSequence != observation.Member.Version.CommitSequence || head.Spec.RootDigest != observation.Member.Version.RootDigest {
			t.Fatal("captured primary head does not match its complete image")
		}
		live[zone.Name] = head
		if head.Spec.Lifecycle == zk.LifecycleActive && head.Spec.NotifySequence != 0 {
			notifications++
		}
	}
	for _, record := range desired {
		head, ok := live[record.Spec.ZoneRef.Name]
		if !ok {
			// Orphaned records and records outside current primary membership
			// do not imply reconciliation work for this serving fixture.
			continue
		}
		aggregate, err := zonestate.Decode(head, func(digest string) (*zk.ZoneChunk, error) {
			if chunk := chunks[digest]; chunk != nil {
				return chunk, nil
			}
			return nil, store.ErrNotFound
		}, zonestate.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		token, err := primaryzone.SourceToken(record)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, source := range aggregate.Sources {
			if source.ID != token.ID || source.UID != token.UID {
				continue
			}
			envelope, err := zonestate.DecodeSourceEnvelope(source.Desired)
			if err != nil {
				t.Fatal(err)
			}
			// RV is opaque; status-only updates may preserve the committed
			// intent receipt. UID and intent must still match exactly.
			found = envelope.State == "present" && envelope.IntentDigest == token.IntentDigest
		}
		if !found {
			records++
		}
	}
	return records, notifications
}
