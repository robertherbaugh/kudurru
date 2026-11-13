package plan

import (
	"slices"
	"testing"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func unmarked(e RemoteRecord) RemoteRecord {
	e.Owner = RemoteOwner{Unmarked: true}
	e.Version = ""
	return e
}

// A markerless provider row (D401) is the owner's exactly when the confirmed
// ledger lists all of its IDs. Every other unmarked row is a conflict, even
// with adoption enabled, and is never part of an operation.
func TestUnmarkedRowsOwnedOnlyByLedgerEvidence(t *testing.T) {
	in := fixture()
	ours := mustNormalize(t, endpoint("ours.example.com.", "A", "192.0.2.1"))
	foreign := mustNormalize(t, endpoint("foreign.example.com.", "A", "192.0.2.2"))
	gone := mustNormalize(t, endpoint("gone.example.com.", "A", "192.0.2.3"))
	in.Current = []RemoteRecord{unmarked(owned(in, ours, "id-ours")), unmarked(owned(in, foreign, "id-foreign")), unmarked(owned(in, gone, "id-gone"))}
	in.Ledger = []*kinds.IntegrationLedger{ledgerRow(in, ours, "sync", "id-ours"), ledgerRow(in, gone, "sync", "id-gone")}
	in.Desired = []public.Endpoint{endpoint("ours.example.com.", "A", "192.0.2.9"), endpoint("foreign.example.com.", "A", "192.0.2.2")}
	in.AdoptUnowned = true
	p := build(t, in)
	var got []string
	for _, op := range p.Operations {
		got = append(got, op.Action+" "+op.Key.Name+" "+op.ProviderIDs[0])
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"delete gone.example.com. id-gone", "update ours.example.com. id-ours"}) {
		t.Fatal("operations", got)
	}
	if len(p.Conflicts) != 1 || p.Conflicts[0].Name != "foreign.example.com." {
		t.Fatal("unproven unmarked row is not a conflict", p.Conflicts)
	}
	// A ledger row listing another ID does not prove a row at its key.
	in.Ledger = []*kinds.IntegrationLedger{ledgerRow(in, ours, "sync", "id-other")}
	in.Current = in.Current[:1]
	in.Desired = in.Desired[:1]
	p = build(t, in)
	if len(p.Operations) != 0 || len(p.Conflicts) != 1 {
		t.Fatal("ledger without the row's ID proved ownership", p.Operations, p.Conflicts)
	}
}

// The Unmarked field is omitted when false, so plans of marked providers keep
// the digests issued before it existed.
func TestUnmarkedOmittedFromMarkedDigests(t *testing.T) {
	in := fixture()
	e := mustNormalize(t, endpoint("www.example.com.", "A", "192.0.2.1"))
	in.Current = []RemoteRecord{owned(in, e, "id-1")}
	before := build(t, in).Digest
	in.Current[0].Owner.Unmarked = false
	if build(t, in).Digest != before {
		t.Fatal("digest changed")
	}
	in.Current[0] = unmarked(in.Current[0])
	if build(t, in).Digest == before {
		t.Fatal("unmarked row shares a marked row's digest")
	}
}
