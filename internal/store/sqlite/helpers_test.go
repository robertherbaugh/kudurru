package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// sharedValidator is built once per test binary: compiling the CEL rules
// dominates the cost of opening a store.
var sharedValidator = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

// testOptions returns options with the shared validator and a fixed
// identity; mutate may adjust them.
func testOptions(mutate ...func(*Options)) Options {
	o := Options{Validator: sharedValidator(), Identity: "test-node"}
	for _, m := range mutate {
		m(&o)
	}
	return o
}

// openAt opens a store at path and closes it when the test ends.
func openAt(t testing.TB, path string, mutate ...func(*Options)) *Store {
	t.Helper()
	s, err := Open(path, testOptions(mutate...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openTemp opens a store in a fresh temporary directory.
func openTemp(t testing.TB, mutate ...func(*Options)) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), FileName), mutate...)
}

func zone(name, zoneName string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: zoneName, Type: v1alpha1.ZoneTypePrimary},
	}
}

func recordSet(ns, name string, labels map[string]string) *v1alpha1.DNSRecordSet {
	return &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: v1alpha1.DNSRecordSetSpec{
			ZoneRef:   v1alpha1.ObjectRef{Name: "home"},
			Endpoints: []v1alpha1.Endpoint{{DNSName: name + ".home.example", RecordType: "A", Targets: []string{"10.0.0.1"}}},
		},
	}
}

func clientGroup(ns, name string) *v1alpha1.ClientGroup {
	return &v1alpha1.ClientGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.ClientGroupSpec{Clients: []v1alpha1.ClientMatcher{{CIDR: "10.0.20.0/24"}}},
	}
}

func mustApply(t testing.TB, s store.Store, obj store.Object, manager string) store.Object {
	t.Helper()
	out, err := s.Apply(context.Background(), obj, store.ApplyOptions{Manager: manager})
	if err != nil {
		t.Fatalf("apply %s: %v", store.KeyOf(obj), err)
	}
	return out
}

// next reads one event or fails after a timeout.
func next(t testing.TB, w store.Watcher) store.Event {
	t.Helper()
	select {
	case ev, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("watch channel closed")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a watch event")
	}
	return store.Event{}
}
