package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func externalZone() *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSZone"},
		ObjectMeta: metav1.ObjectMeta{Name: "ext"},
		Spec: v1alpha1.DNSZoneSpec{ZoneName: "ext.example.", Type: v1alpha1.ZoneTypeExternal, Nameservers: []string{"ns.ext.example."},
			External: &v1alpha1.ExternalSpec{Ingress: true}},
	}
}

func TestExternalSourceDeniedListsAreScopedAndRecover(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	st := memory.New()
	zone := externalZone()
	zone.Spec.External.Gateway = true
	zone.Spec.External.LoadBalancerServices = true
	if _, err := st.Apply(ctx, zone, store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	listKinds := make(map[schema.GroupVersionResource]string, len(healthKinds))
	for _, kind := range healthKinds {
		listKinds[kind.gvr] = kind.kind + "List"
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds)
	denied := true
	client.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if denied {
			return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "", errors.New("denied by fixture"))
		}
		list := &unstructured.UnstructuredList{}
		list.SetResourceVersion("1")
		return true, list, nil
	})
	reg := prometheus.NewRegistry()
	metrics, err := registerSourceMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{opts: Options{Store: st, Admit: func(context.Context) error { return nil }}, client: client, metrics: metrics}
	changed := controller.HealthChanged()
	if watched, err := controller.reconcile(ctx); !apierrors.IsForbidden(err) || len(watched) != 0 {
		t.Fatalf("denied pass: watches=%v err=%v", watched, err)
	}
	if problems, checked := controller.Problems(zone.Spec.External); !checked || len(problems) != len(healthKinds) {
		t.Fatalf("denied kinds: %v", problems)
	}
	if problems, checked := controller.Problems(&v1alpha1.ExternalSpec{LoadBalancerServices: true}); !checked || len(problems) != 1 || !strings.HasPrefix(problems[0], "Service: list denied; allow list on services") {
		t.Fatalf("Service-only zone: %v", problems)
	}
	select {
	case <-changed:
	default:
		t.Fatal("no signal on list failure")
	}
	for _, kind := range healthKinds {
		if count := testutil.ToFloat64(metrics.lists.WithLabelValues(kind.label, string(listForbidden))); count != 1 {
			t.Errorf("%s: forbidden count %v", kind.kind, count)
		}
	}
	denied = false
	if watched, err := controller.reconcile(ctx); err != nil || len(watched) != len(healthKinds) {
		t.Fatalf("recovered pass: watches=%v err=%v", watched, err)
	}
	if problems, checked := controller.Problems(zone.Spec.External); !checked || len(problems) != 0 {
		t.Fatalf("source did not recover: %v", problems)
	}
	zone.Spec.External = &v1alpha1.ExternalSpec{Ingress: true}
	if _, err := st.Apply(ctx, zone, store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "kudurru_external_zone_source_objects" {
			series := family.GetMetric()
			if len(series) != 1 || series[0].GetLabel()[0].GetValue() != "ingress" {
				t.Fatalf("object gauge retained deselected kinds: %v", series)
			}
			return
		}
	}
	t.Fatal("object gauge lost its selected kind")
}

func TestExternalSourceCanceledTermDoesNotPublishHealth(t *testing.T) {
	st := memory.New()
	zone := externalZone()
	if _, err := st.Apply(t.Context(), zone, store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	kind := kindOf("ingress")
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{kind.gvr: "IngressList"})
	term, cancelTerm := context.WithCancel(t.Context())
	defer cancelTerm()
	cancelListTerm := cancelTerm
	client.PrependReactor("list", kind.gvr.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		cancelListTerm()
		return true, nil, context.Canceled
	})
	controller := &Controller{opts: Options{Store: st, Admit: func(context.Context) error { return nil }}, client: client}
	controller.publishListHealth(map[*sourceKind]listResult{kind: listOK})
	if _, err := controller.reconcile(term); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted pass: %v", err)
	}
	if problems, checked := controller.Problems(zone.Spec.External); !checked || len(problems) != 0 {
		t.Fatalf("interrupted pass replaced observed health: %v", problems)
	}
	nextTerm, cancelNext := context.WithCancel(t.Context())
	defer cancelNext()
	cancelListTerm = cancelNext
	if err := controller.Run(nextTerm); err != nil {
		t.Fatal(err)
	}
	if problems, checked := controller.Problems(zone.Spec.External); checked || len(problems) != 0 {
		t.Fatalf("new term does not have an observation: checked=%t problems=%v", checked, problems)
	}
}

func TestExternalSourceWatchDeniedWithHealthyList(t *testing.T) {
	kind := kindOf("ingress")
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	denied := true
	client.PrependWatchReactor(kind.gvr.Resource, func(k8stesting.Action) (bool, watch.Interface, error) {
		if denied {
			return true, nil, apierrors.NewForbidden(kind.gvr.GroupResource(), "", errors.New("denied by fixture"))
		}
		stream := watch.NewRaceFreeFake()
		stream.Add(&unstructured.Unstructured{})
		return true, stream, nil
	})
	controller := &Controller{opts: Options{Resync: 50 * time.Millisecond}, client: client}
	controller.health.replaceLists(map[*sourceKind]listResult{kind: listOK})
	watches := []watchFrom{{kind: kind, rv: "1"}}
	controller.wait(t.Context(), watches)
	spec := externalZone().Spec.External
	if problems, _ := controller.Problems(spec); len(problems) != 1 || !strings.Contains(problems[0], "Ingress: watch denied") {
		t.Fatalf("watch denial disappeared behind the healthy list: %v", problems)
	}
	denied = false
	controller.wait(t.Context(), watches)
	if problems, _ := controller.Problems(spec); len(problems) != 0 {
		t.Fatalf("watch did not recover: %v", problems)
	}
}

func TestExternalSourceObjectCapReportsObservedObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	st := memory.New()
	zone := externalZone()
	if _, err := st.Apply(ctx, zone, store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	kind := kindOf("ingress")
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{kind.gvr: "IngressList"})
	client.PrependReactor("list", kind.gvr.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		list := &unstructured.UnstructuredList{Items: make([]unstructured.Unstructured, maxObjects+1)}
		list.SetResourceVersion("1")
		for i := range list.Items {
			list.Items[i] = *object(kind, nil, nil)
		}
		return true, list, nil
	})
	metrics, err := registerSourceMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{opts: Options{Store: st, Admit: func(context.Context) error { return nil }}, client: client, metrics: metrics}
	if watches, err := controller.reconcile(ctx); !errors.Is(err, store.ErrTooLarge) || len(watches) != 0 {
		t.Fatalf("over-cap pass: watches=%v err=%v", watches, err)
	}
	if problems, _ := controller.Problems(zone.Spec.External); len(problems) != 1 || !strings.Contains(problems[0], "4,096-object or 32 MiB") {
		t.Fatalf("cap diagnosis: %v", problems)
	}
	if got := testutil.ToFloat64(metrics.lists.WithLabelValues(kind.label, string(listTooLarge))); got != 1 {
		t.Fatalf("over-cap result count %v", got)
	}
	if got := testutil.ToFloat64(metrics.objects.WithLabelValues(kind.label)); got != maxObjects+1 {
		t.Fatalf("observed objects %v, want %d", got, maxObjects+1)
	}
}

func TestExternalSourceTruncatedPageIsDecodeFailureNotKindCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	st := memory.New()
	zone := externalZone()
	if _, err := st.Apply(ctx, zone, store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	largeAnnotation := strings.Repeat("x", maxResponseBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limit := r.URL.Query().Get("limit"); limit != "64" && limit != "8" {
			t.Errorf("expected a bounded list or smaller retry, got %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("continue") == "" {
			_, _ = io.WriteString(w, `{"apiVersion":"networking.k8s.io/v1","kind":"IngressList","metadata":{"resourceVersion":"1","continue":"next"},"items":[{"metadata":{"name":"seen"}}]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"apiVersion":"networking.k8s.io/v1","kind":"IngressList","metadata":{"resourceVersion":"1"},"items":[{"metadata":{"annotations":{"fixture":"%s"}}}]}`, largeAnnotation)
	}))
	defer server.Close()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := New(&rest.Config{Host: server.URL}, Options{Store: st, Validator: v, Admit: func(context.Context) error { return nil }, Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	watches, err := controller.reconcile(ctx)
	if !errors.Is(err, errTruncatedPage) || errors.Is(err, store.ErrTooLarge) || !strings.Contains(err.Error(), "unexpected end of JSON input") || len(watches) != 0 {
		t.Fatalf("truncated pass: watches=%v err=%v", watches, err)
	}
	if problems, _ := controller.Problems(zone.Spec.External); len(problems) != 1 || !strings.Contains(problems[0], "4 MiB response bound") {
		t.Fatalf("truncation diagnosis: %v", problems)
	}
	if got := testutil.ToFloat64(controller.metrics.lists.WithLabelValues("ingress", string(listTruncated))); got != 2 {
		t.Fatalf("truncation result count %v", got)
	}
	if got := testutil.ToFloat64(controller.metrics.objects.WithLabelValues("ingress")); got != 1 {
		t.Fatalf("per-kind count should include only the decoded prefix, got %v", got)
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
