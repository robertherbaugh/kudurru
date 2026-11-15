package inbound

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

func allocationInput() AllocationInput {
	return AllocationInput{Owner: kinds.Owner{ClusterID: "cluster", Namespace: "dns", Name: "unifi", UID: "original"}, Scope: kinds.AllocationScope{ClusterID: "cluster", ZoneUID: "zone-original", Apex: "lan.test."}, At: time.Unix(100000, 0).UTC()}
}
func persisted(rows []*kinds.IntegrationNameAllocation) []*kinds.IntegrationNameAllocation {
	out := make([]*kinds.IntegrationNameAllocation, len(rows))
	for i, r := range rows {
		out[i] = r.DeepCopy()
		out[i].UID = types.UID("saved-" + r.Name)
		out[i].ResourceVersion = "1"
	}
	return out
}
func TestAllocationStableSortedRestartOmissionAndBaseChange(t *testing.T) {
	in := allocationInput()
	in.Requests = []NameRequest{{"b", "phone"}, {"a", "phone"}}
	in.Reservations = []NameReservation{{Name: "phone.lan.test."}}
	before, _ := json.Marshal(in)
	first, err := AllocateNames(in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Labels["a"] != "phone-2" || first.Labels["b"] != "phone-3" {
		t.Fatal(first.Labels)
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("mutated input")
	}
	slices.Reverse(in.Requests)
	other, err := AllocateNames(in)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(other)
	if string(a) != string(b) {
		t.Fatal("nondeterministic allocation")
	}
	in.Existing = persisted(first.New)
	in.Requests = []NameRequest{{"b", "phone"}}
	again, err := AllocateNames(in)
	if err != nil || again.Labels["b"] != "phone-3" || len(again.New) != 0 {
		t.Fatal(again, err)
	}
	in.Requests = []NameRequest{{"c", "phone"}}
	next, err := AllocateNames(in)
	if err != nil || next.Labels["c"] != "phone-4" {
		t.Fatal(next, err)
	}
	in.Requests = []NameRequest{{"a", "tablet"}}
	next, err = AllocateNames(in)
	if err != nil || next.Labels["a"] != "tablet" || len(next.New) != 1 {
		t.Fatal(next, err)
	}
	in.Scope.ZoneUID = "recreated"
	in.Requests = []NameRequest{{"b", "phone"}}
	next, err = AllocateNames(in)
	if err != nil || next.Labels["b"] != "phone-2" || len(next.New) != 1 {
		t.Fatal(next, err)
	}
}
func TestAllocationForeignOwnershipManualConflictAndAtomicFailure(t *testing.T) {
	in := allocationInput()
	in.Requests = []NameRequest{{"a", "phone"}}
	first, err := AllocateNames(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Existing = persisted(first.New)
	in.Owner.UID = "new-incarnation"
	next, err := AllocateNames(in)
	if err != nil || next.Labels["a"] != "phone-2" {
		t.Fatal(next, err)
	}
	in.Owner.UID = "original"
	in.Reservations = []NameReservation{{Name: "phone.lan.test."}}
	if got, err := AllocateNames(in); err == nil || got.Labels != nil || got.New != nil {
		t.Fatal("silently resuffixed existing reservation")
	}
	in = allocationInput()
	in.Requests = []NameRequest{{"a", "valid"}, {"b", "INVALID"}}
	if got, err := AllocateNames(in); err == nil || got.Labels != nil || got.New != nil {
		t.Fatal("partial allocation escaped")
	}
	in = allocationInput()
	in.Requests = []NameRequest{{"a", "phone"}}
	in.Reservations = []NameReservation{{Name: "lan.test.", Subtree: true}}
	if _, err := AllocateNames(in); err == nil {
		t.Fatal("DNAME/boundary subtree bypassed")
	}
	in = allocationInput()
	in.Existing = first.New
	if _, err := AllocateNames(in); err == nil {
		t.Fatal("unpersisted evidence adopted")
	}
}
func TestAllocationSuffixLimitsAndQuota(t *testing.T) {
	in := allocationInput()
	base := strings.Repeat("a", 61) + "-a"
	in.Requests = []NameRequest{{"a", base}, {"b", base}}
	out, err := AllocateNames(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Labels["b"]) > 63 || !AllocatedLabel(base, out.Labels["b"]) {
		t.Fatal(out.Labels)
	}
	for _, label := range []string{"different", "phone-01", "phone-1", "phone-4294967296"} {
		if AllocatedLabel("phone", label) {
			t.Errorf("accepted %q", label)
		}
	}
	in = allocationInput()
	in.Requests = make([]NameRequest, kinds.MaxSourceAllocations+1)
	if _, err := AllocateNames(in); err == nil {
		t.Fatal("request cap bypassed")
	}
	in = allocationInput()
	in.Requests = []NameRequest{{"same", "phone"}, {"same", "tablet"}}
	if _, err := AllocateNames(in); err == nil {
		t.Fatal("duplicate stable id accepted")
	}
}
