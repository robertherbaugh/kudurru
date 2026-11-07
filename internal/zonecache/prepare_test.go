package zonecache

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPrepareCompleteCutCopiesInputsAndPreservesSourceStatus(t *testing.T) {
	instant := time.Date(2026, 10, 14, 1, 2, 3, 0, time.UTC)
	z := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home", ResourceVersion: "6", Labels: map[string]string{"pair": "old"}}, Spec: v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypePrimary, ZoneName: "home.example."}}
	z.Status.Serial = 42
	kinds := []store.Kind{"DNSZone", "DNSRecordSet"}
	snapshot, err := Prepare(PrepareOptions{Kinds: kinds, Now: instant}, []store.Object{z}, []Builder{
		NewBuilder("serial", func(objects ObjectSet) (any, error) {
			if !objects.Changed("DNSZone") || !objects.ListsChanged() || objects.Now() != instant {
				t.Fatal("incomplete prepare input")
			}
			return objects.Zones()[0].Status.Serial, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	z.Status.Serial, z.Labels["pair"], kinds[0] = 77, "new", "Policy"
	if snapshot.Objects.Zones()[0].Status.Serial != 42 || snapshot.Objects.Zones()[0].Labels["pair"] != "old" || !reflect.DeepEqual(snapshot.Objects.Kinds(), []store.Kind{"DNSRecordSet", "DNSZone"}) {
		t.Fatal("caller mutated prepared input")
	}
	if serial, ok := View[uint32](snapshot, "serial"); !ok || serial != 42 || !snapshot.Authoritative() || snapshot.Generation != 0 {
		t.Fatalf("prepared snapshot: %+v", snapshot)
	}
	// Absence in a subsequent complete cut deletes rather than retaining an index.
	empty, err := Prepare(PrepareOptions{Kinds: []store.Kind{"DNSZone", "DNSRecordSet"}}, nil, nil)
	if err != nil || empty.Objects.Len() != 0 || snapshot.Objects.Len() != 1 {
		t.Fatalf("absence replacement: %v", err)
	}
}

func TestPrepareRejectsWholeCutOnBuilderFailureOrPanic(t *testing.T) {
	want := errors.New("required compilation failed")
	for _, build := range []func(ObjectSet) (any, error){
		func(ObjectSet) (any, error) { return nil, want },
		func(ObjectSet) (any, error) { panic("owner panic") },
		func(ObjectSet) (any, error) { return nil, nil },
	} {
		snapshot, err := Prepare(PrepareOptions{}, nil, []Builder{NewBuilder("first", func(ObjectSet) (any, error) { return "new-first", nil }), NewBuilder("required", build)})
		if err == nil || snapshot != nil {
			t.Fatalf("partial builder cut accepted: %v", err)
		}
	}
	if s, err := Prepare(PrepareOptions{}, nil, []Builder{NewBuilder("same", func(ObjectSet) (any, error) { return "a", nil }), NewBuilder("same", func(ObjectSet) (any, error) { return "b", nil })}); err == nil || s != nil {
		t.Fatal("duplicate builder accepted")
	}
}

func TestPrepareRejectsInvalidObjectManifest(t *testing.T) {
	z := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}}
	var typedNil *v1alpha1.DNSZone
	for _, objects := range [][]store.Object{{nil}, {typedNil}, {z, z}, {&v1alpha1.Policy{ObjectMeta: metav1.ObjectMeta{Name: "policy"}}}} {
		if s, err := Prepare(PrepareOptions{Kinds: []store.Kind{"DNSZone"}}, objects, nil); err == nil || s != nil {
			t.Fatal("invalid complete object cut accepted")
		}
	}
	if _, err := Prepare(PrepareOptions{Kinds: []store.Kind{"DNSZone", "DNSZone"}}, nil, nil); err == nil {
		t.Fatal("duplicate kind manifest accepted")
	}
}
