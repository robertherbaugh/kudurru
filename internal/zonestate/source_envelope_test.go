package zonestate

import (
	"encoding/json"
	"fmt"
	"testing"

	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
)

func TestSourceAcknowledgmentHistoryCompactsAndNewTargetsRemain(t *testing.T) {
	a := fixture(t)
	id := a.Zone
	source := Source{ID: "DNSRecordSet/dns/source", UID: "source-uid", ResourceVersion: "observed", Manager: "api"}
	digest := zonekinds.Digest([]byte("intent"))
	e := SourceEnvelope{FormatVersion: 1, State: "present", Object: json.RawMessage(`{"spec":{}}`), IntentDigest: digest, Projection: "served"}
	for i := 0; i < 300; i++ {
		target := DesiredTarget{Zone: id, Source: SourceToken{ID: source.ID, UID: source.UID, ResourceVersion: fmt.Sprintf("opaque-%d", i), IntentDigest: digest}, Operation: "apply"}
		if err := AcknowledgeSource(&e, source, []DesiredTarget{target}); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ack := range e.Acknowledged {
			if ack.Source == target.Source {
				found = true
				if ack.Outcome != "committedEquivalent" {
					t.Fatalf("outcome %+v", ack)
				}
			}
		}
		if !found || len(e.Acknowledged) > 256 {
			t.Fatal("new target lost or history unbounded")
		}
	}
	if len(e.Acknowledged) != 256 {
		t.Fatal("unexpected receipt bound")
	}
	raw, err := EncodeSourceEnvelope(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSourceEnvelope(raw); err != nil {
		t.Fatal(err)
	}
}
