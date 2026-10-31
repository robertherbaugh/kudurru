package main

import (
	"encoding/json"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/importers"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// offlineAcceptanceWaitRun observes the accepted job without repeating its
// mutation. A stalled job fails the fixture deadline; partial/interrupted
// results remain visible to the caller's ordinary CLI readback assertions.
func offlineAcceptanceWaitRun(t *testing.T, f *servingCommandFixture, id, source string) {
	t.Helper()
	awaitServingRuntime(t, f.s, func() bool {
		return offlineAcceptanceJournal(t, f, id, source).Spec.Status != "running"
	})
	adoptionWait(t, f)
}

// offlineAcceptanceWaitRecords waits for exact committed source evidence before
// the one-shot DNS assertion. Zone admission alone can legitimately serve an
// earlier head while a later desired RecordSet is still being reconciled.
func offlineAcceptanceWaitRecords(t *testing.T, f *servingCommandFixture, plan importers.Plan) {
	t.Helper()
	var records []*public.DNSRecordSet
	for _, doc := range plan.Objects {
		if doc["kind"] != "DNSRecordSet" {
			continue
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		var desired public.DNSRecordSet
		if err := json.Unmarshal(raw, &desired); err != nil {
			t.Fatal(err)
		}
		var actual public.DNSRecordSet
		if err := f.s.opts.Store.Get(t.Context(), store.KeyOf(&desired), &actual); err != nil {
			t.Fatal(err)
		}
		records = append(records, &actual)
	}
	backend, err := f.s.opts.Store.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	repository := zonestate.NewRepository(backend, zonestate.Limits{})
	awaitServingRuntime(t, f.s, func() bool {
		lease, err := f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		component, _ := lease.Bundle().Component("runtime")
		owner := component.(*servingbuild.RuntimeOwner)
		for _, record := range records {
			observation, ok := owner.Primary().CommittedPrimaryStatus(record.Spec.ZoneRef.Name)
			if !ok || observation.HeadResourceVersion == "" {
				return false
			}
			view, err := repository.Read(t.Context(), observation.HeadZone)
			if err != nil {
				t.Fatal(err)
			}
			// A current repository head cannot stand in for the captured owner.
			if view.Expected.CommitSequence != observation.Member.Version.CommitSequence || view.Expected.RootDigest != observation.Member.Version.RootDigest {
				return false
			}
			token, err := primaryzone.SourceToken(record)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, source := range view.Aggregate.Sources {
				if source.ID != token.ID || source.UID != token.UID || source.ResourceVersion != token.ResourceVersion {
					continue
				}
				envelope, err := zonestate.DecodeSourceEnvelope(source.Desired)
				if err != nil {
					t.Fatal(err)
				}
				found = envelope.State == "present" && envelope.IntentDigest == token.IntentDigest
			}
			if !found {
				return false
			}
		}
		status := f.s.publisher.Status()
		return status.ProtectedReady && status.Served == lease.Bundle().Identity()
	})
	adoptionWait(t, f)
}
