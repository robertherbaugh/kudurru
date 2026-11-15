package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

func externalZone() *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSZone"},
		ObjectMeta: metav1.ObjectMeta{Name: "ext"},
		Spec: v1alpha1.DNSZoneSpec{ZoneName: "ext.example.", Type: v1alpha1.ZoneTypeExternal, Nameservers: []string{"ns.ext.example."},
			External: &v1alpha1.ExternalSpec{Ingress: true}},
	}
}

// A status-only write of the zone (the zone controller stamping a new
// serial after each record set the pass wrote) must not refuse the rest of
// the pass; a change to anything the projection read must.
func TestCurrentZoneIgnoresStatusButNotSpec(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	if _, err := st.Apply(ctx, externalZone(), store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	c := &Controller{opts: Options{Store: st}}
	listed, err := c.zones(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatal(listed, err)
	}
	if err := c.currentZone(ctx, listed, "ext"); err != nil {
		t.Fatal("unchanged zone refused:", err)
	}
	for serial := uint32(1); serial <= 3; serial++ {
		var cur v1alpha1.DNSZone
		if err := st.Get(ctx, store.KeyOf(listed[0]), &cur); err != nil {
			t.Fatal(err)
		}
		cur.Status.Serial, cur.Status.RecordCount = serial, int(serial)
		if err := st.UpdateStatus(ctx, &cur); err != nil {
			t.Fatal(err)
		}
		if err := c.currentZone(ctx, listed, "ext"); err != nil {
			t.Fatalf("status write %d refused the pass: %v", serial, err)
		}
	}
	for name, change := range map[string]func(*v1alpha1.DNSZone){
		"spec":     func(z *v1alpha1.DNSZone) { z.Spec.External.LoadBalancerServices = true },
		"disabled": func(z *v1alpha1.DNSZone) { z.Annotations = map[string]string{v1alpha1.EnabledAnnotation: "false"} },
	} {
		st := memory.New()
		if _, err := st.Apply(ctx, externalZone(), store.ApplyOptions{Manager: "test"}); err != nil {
			t.Fatal(err)
		}
		c := &Controller{opts: Options{Store: st}}
		listed, err := c.zones(ctx)
		if err != nil || len(listed) != 1 {
			t.Fatal(listed, err)
		}
		next := externalZone()
		change(next)
		if _, err := st.Apply(ctx, next, store.ApplyOptions{Manager: "test"}); err != nil {
			t.Fatal(err)
		}
		if err := c.currentZone(ctx, listed, "ext"); !errors.Is(err, store.ErrConflict) {
			t.Errorf("%s change accepted: %v", name, err)
		}
	}
	// A different object under the same name.
	replaced := listed[0].DeepCopy()
	replaced.UID = "other"
	if sameZoneInput(replaced, listed[0]) {
		t.Error("a replaced zone was accepted")
	}
	deleting := listed[0].DeepCopy()
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	if sameZoneInput(listed[0], deleting) {
		t.Error("a deleting zone was accepted")
	}
}

// A steady stream of watch events (controllers updating Ingress status)
// coalesces into at most one pass per MinInterval instead of relisting
// back to back.
func TestRunPacesPassesUnderEventStorm(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := memory.New()
	if _, err := st.Apply(ctx, externalZone(), store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	ingresses := schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{ingresses: "IngressList"})
	var lists atomic.Int32
	client.PrependReactor("list", "ingresses", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		return false, nil, nil
	})
	c := &Controller{opts: Options{Store: st, Admit: func(context.Context) error { return nil }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resync: time.Minute, MinInterval: 500 * time.Millisecond}, client: client}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Run(ctx)
	}()
	// Every 10 ms another Ingress (without an address, so nothing is
	// published) wakes the watch.
	stop := time.After(2 * time.Second)
	for i := 0; ; i++ {
		select {
		case <-stop:
			cancel()
			<-done
			// 2 s at one pass per 500 ms is at most 5 passes (one list each).
			if n := lists.Load(); n < 2 || n > 5 {
				t.Fatalf("%d passes in 2 s with a 500 ms floor", n)
			}
			return
		case <-time.After(10 * time.Millisecond):
		}
		obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
			"metadata": map[string]any{"name": fmt.Sprintf("in-%d", i), "namespace": "apps"}}}
		if _, err := client.Resource(ingresses).Namespace("apps").Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}
