package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func lifecycleFixture() *ZoneLifecycle {
	id := ZoneID{ClusterID: "cluster", ObjectName: "child", UID: "source-uid", Apex: "child.example."}
	return &ZoneLifecycle{ObjectMeta: metav1.ObjectMeta{Name: LifecycleName(id.ClusterID, id.ObjectName), ResourceVersion: "opaque-z"}, Spec: ZoneLifecycleSpec{FormatVersion: 1, Zone: id, Type: public.ZoneTypePrimary, Incarnation: 1, Sequence: 1, Phase: PhasePreparing, Source: LifecycleSource{UID: id.UID, ResourceVersion: "source-opaque", IntentDigest: Digest([]byte("spec"))}}}
}
func TestLifecycleStrictBoundsAndCopies(t *testing.T) {
	s := lifecycleFixture()
	s.Spec.Parents = []ParentFence{{Zone: ZoneID{ClusterID: "cluster", ObjectName: "parent", UID: "parent-uid", Apex: "example."}, Incarnation: 1, BoundaryID: Digest([]byte("boundary"))}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(s)
	if _, err := DecodeZoneLifecycle(raw); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(string(raw), `"formatVersion":1`, `"formatVersion":2`, 1), strings.Replace(string(raw), `"phase":"preparing"`, `"phase":"mystery"`, 1), strings.Replace(string(raw), `"spec":{`, `"spec":{"secret":"value",`, 1), string(raw) + ` {}`, strings.Repeat(" ", MaxLifecycleBytes+1)} {
		if _, err := DecodeZoneLifecycle([]byte(bad)); err == nil {
			t.Fatal("accepted malformed/unknown carrier")
		}
	}
	s.Labels = map[string]string{"key": "old"}
	cp := s.DeepCopy()
	cp.Labels["key"] = "new"
	cp.Spec.Parents[0].BoundaryID = "new"
	if s.Labels["key"] != "old" || s.Spec.Parents[0].BoundaryID == "new" {
		t.Fatal("mutable alias")
	}
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if _, err := scheme.New(public.GroupVersion.WithKind(KindZoneLifecycle)); err != nil {
		t.Fatal(err)
	}
}
func TestLifecycleExactCASAndRetiredIncarnations(t *testing.T) {
	s := lifecycleFixture()
	next := s.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Phase = PhaseFencing
	if err := ValidateLifecycleTransition(s, "opaque-z", next); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLifecycleTransition(s, "opaque-a", next); err == nil {
		t.Fatal("fresh RV substitution")
	}
	next.Spec.Phase = PhaseActive
	if err := ValidateLifecycleTransition(s, s.ResourceVersion, next); err == nil {
		t.Fatal("phase skip")
	}
	s.Spec.Phase = PhaseRetired
	next = s.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Phase = PhaseActive
	if err := ValidateLifecycleTransition(s, s.ResourceVersion, next); err == nil {
		t.Fatal("tombstoned generation reactivated")
	}
	next.Spec.Phase = PhasePreparing
	next.Spec.Incarnation++
	if err := ValidateLifecycleTransition(s, s.ResourceVersion, next); err != nil {
		t.Fatal(err)
	}
	if next.Spec.HeadZone() == s.Spec.HeadZone() || next.Spec.Zone != s.Spec.Zone {
		t.Fatal("same-UID replacement reused RR authority")
	}
}
