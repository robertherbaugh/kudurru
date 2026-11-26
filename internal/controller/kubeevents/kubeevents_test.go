package kubeevents

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"

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
	s.Event(zone, controller.EventNormal, controller.EventSerialBumped, "serial 1 -> 2")

	tests := []struct {
		ns, kind, name, uid, reason, typ string
		count                            int32
	}{
		{"apps", "DNSRecordSet", "web", "u1", controller.EventDuplicated, "Warning", 1},
		{"default", "DNSZone", "home", "u2", controller.EventSerialBumped, "Normal", 2},
	}
	for _, tt := range tests {
		deadline := time.Now().Add(10 * time.Second)
		for {
			list, err := cs.CoreV1().Events(tt.ns).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(list.Items) == 1 && list.Items[0].Count == tt.count {
				ev := list.Items[0]
				if ev.InvolvedObject.Kind != tt.kind || ev.InvolvedObject.Name != tt.name || ev.Reason != tt.reason || ev.Type != tt.typ ||
					string(ev.InvolvedObject.UID) != tt.uid || ev.InvolvedObject.APIVersion != v1alpha1.GroupVersion.String() ||
					ev.Source.Component != DefaultComponent || ev.Source.Host != "pod-a" {
					t.Fatalf("event %+v", ev)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no matching event in %s: %+v", tt.ns, list.Items)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestDeliverRecreatesExpiredEvent(t *testing.T) {
	client := fake.NewClientset()
	events := client.CoreV1().Events("default")
	sink := &Sink{ctx: t.Context()}
	correlator := record.NewEventCorrelator(clock.RealClock{})
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "default", Name: "home.first"},
		InvolvedObject: corev1.ObjectReference{Kind: "DNSZone", Name: "home", UID: "zone"},
		Type:           corev1.EventTypeNormal,
		Reason:         controller.EventSerialBumped,
		Message:        "serial 1 -> 2",
		Source:         corev1.EventSource{Component: DefaultComponent},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	if err := sink.deliver(client.CoreV1(), correlator, event.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := events.Delete(t.Context(), event.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	repeat := event.DeepCopy()
	repeat.Name = "home.repeat"
	if err := sink.deliver(client.CoreV1(), correlator, repeat); err != nil {
		t.Fatal(err)
	}
	actions := client.Actions()
	if len(actions) != 2 || !actions[0].Matches("patch", "events") || !actions[1].Matches("create", "events") {
		t.Fatalf("delivery actions = %v, want patch then create", actions)
	}
	list, err := events.List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Name != event.Name || list.Items[0].Count != 2 {
		t.Fatalf("stored Events = %+v, want one correlated Event with name %s and count 2", list.Items, event.Name)
	}
}

func TestSinkFiltersRepeatSpam(t *testing.T) {
	client := fake.NewClientset()
	sink, err := New(client.CoreV1(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Shutdown)
	zone := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home", UID: "zone"}}
	const offered = 40
	for range offered {
		sink.Event(zone, controller.EventNormal, controller.EventSerialBumped, "serial 1 -> 2")
	}
	marker := &v1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "marker", Namespace: "apps", UID: "marker"}}
	sink.Event(marker, controller.EventWarning, controller.EventDuplicated, "marker")
	for until := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		list, err := client.CoreV1().Events("apps").List(t.Context(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Items) == 1 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("worker did not process the marker after the repeated events")
		}
	}
	writes := 0
	for _, action := range client.Actions() {
		if action.GetNamespace() == "default" && (action.Matches("create", "events") || action.Matches("patch", "events")) {
			writes++
		}
	}
	if writes <= 1 || writes >= offered {
		t.Errorf("%d repeated Events produced %d writes; want aggregation followed by spam filtering", offered, writes)
	}
	if got := testutil.ToFloat64(sink.drops.WithLabelValues("queue_full")); got != 0 {
		t.Errorf("spam filtering recorded %v queue drops, want 0", got)
	}
}

type blockedEvents struct {
	typedcorev1.EventInterface
	first   sync.Once
	entered chan struct{}
}

func (client *blockedEvents) Events(string) typedcorev1.EventInterface { return client }

func (client *blockedEvents) Create(ctx context.Context, _ *corev1.Event, _ metav1.CreateOptions) (*corev1.Event, error) {
	client.first.Do(func() { close(client.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBlockedClientDropsOnlyOverflowAndShutdownCancels(t *testing.T) {
	client := &blockedEvents{entered: make(chan struct{})}
	registry := prometheus.NewRegistry()
	sink, err := New(client, Options{Registerer: registry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sink.Shutdown)
	zone := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home", UID: "u2"}}
	sink.Event(zone, controller.EventNormal, controller.EventSerialBumped, "in flight")
	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not reach the client")
	}
	const overflow = 19
	offered := make(chan struct{})
	go func() {
		defer close(offered)
		for range queueCapacity + overflow {
			sink.Event(zone, controller.EventNormal, controller.EventSerialBumped, "queued")
		}
	}()
	select {
	case <-offered:
	case <-time.After(2 * time.Second):
		t.Fatal("Event blocked behind the client")
	}
	if got := len(sink.queue); got != queueCapacity {
		t.Errorf("queue length = %d, want %d", got, queueCapacity)
	}
	if got := testutil.ToFloat64(sink.drops.WithLabelValues("queue_full")); got != overflow {
		t.Errorf("queue_full = %v, want %d", got, overflow)
	}
	if count, err := testutil.GatherAndCount(registry, "kudurru_controller_events_dropped_total"); err != nil || count != 3 {
		t.Errorf("drop metrics = %d, %v; want three reason series", count, err)
	}
	stopped := make(chan struct{})
	go func() {
		sink.Shutdown()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not cancel the blocked write")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("nil client accepted")
	}
}
