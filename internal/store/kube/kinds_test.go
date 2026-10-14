package kube

import (
	"fmt"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestRegistryDerivesKindsFromScheme(t *testing.T) {
	r, err := newRegistry(testScheme())
	if err != nil {
		t.Fatal(err)
	}
	// BlockList ends in "List" but is a kind (its list is BlockListList);
	// WatchEvent, ListOptions and core/v1 types are not kudurru kinds.
	want := []store.Kind{"BlockList", "ClientGroup", "DHCPScope", "DNSRecordSet", "DNSZone", "Integration", "Policy", "UnblockRequest"}
	if fmt.Sprint(r.kinds) != fmt.Sprint(want) {
		t.Fatalf("kinds = %v, want %v", r.kinds, want)
	}
	tests := []struct {
		kind     store.Kind
		listKind string
	}{
		{"BlockList", "BlockListList"},
		{"DNSZone", "DNSZoneList"},
		{"Policy", "PolicyList"},
	}
	for _, tt := range tests {
		ki, err := r.get(tt.kind)
		if err != nil {
			t.Fatal(err)
		}
		obj := ki.newObject()
		if store.KindOf(obj) != tt.kind || obj.GetObjectKind().GroupVersionKind() != v1alpha1.GroupVersion.WithKind(string(tt.kind)) {
			t.Errorf("%s: newObject = %T %v", tt.kind, obj, obj.GetObjectKind().GroupVersionKind())
		}
		if got := ki.newList().GetObjectKind().GroupVersionKind().Kind; got != tt.listKind {
			t.Errorf("%s: list kind %s, want %s", tt.kind, got, tt.listKind)
		}
	}
	for _, k := range []store.Kind{"WatchEvent", "BlockListList", "DNSZoneList", "Namespace"} {
		if _, err := r.get(k); err == nil {
			t.Errorf("%s must not be a kind", k)
		}
	}
}
