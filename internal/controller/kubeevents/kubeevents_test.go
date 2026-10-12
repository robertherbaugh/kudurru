package kubeevents

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/controller"
)

func TestSinkRecordsEvents(t *testing.T) {
	cs := fake.NewClientset()
	s, err := New(cs.CoreV1(), Options{Host: "pod-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	rs := &v1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "web", UID: "u1"}}
	zone := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home", UID: "u2"}}
	s.Event(rs, controller.EventWarning, controller.EventDuplicated, "www.example.com. A (held by team-a/web)")
	s.Event(zone, controller.EventNormal, controller.EventSerialBumped, "serial 1 -> 2")

	tests := []struct {
		ns, kind, name, reason, typ string
	}{
		{"apps", "DNSRecordSet", "web", controller.EventDuplicated, "Warning"},
		{"default", "DNSZone", "home", controller.EventSerialBumped, "Normal"},
	}
	for _, tt := range tests {
		deadline := time.Now().Add(10 * time.Second)
		for {
			list, err := cs.CoreV1().Events(tt.ns).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Items) == 1 {
				ev := list.Items[0]
				if ev.InvolvedObject.Kind != tt.kind || ev.InvolvedObject.Name != tt.name || ev.Reason != tt.reason || ev.Type != tt.typ ||
					ev.InvolvedObject.APIVersion != v1alpha1.GroupVersion.String() || ev.Source.Component != DefaultComponent || ev.Source.Host != "pod-a" {
					t.Fatalf("event %+v", ev)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no event in %s (have %d)", tt.ns, len(list.Items))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("nil client accepted")
	}
}
