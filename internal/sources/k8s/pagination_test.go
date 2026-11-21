package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	zonemodel "github.com/robertherbaugh/kudurru/internal/zone"
)

func TestExternalSourceLargePageRestartsWithSmallerPages(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	const objects = 128
	padding := strings.Repeat("x", 70<<10)
	var normalPages, smallPages atomic.Int32
	controller := sourceHTTPFixture(t, memory.New(), func(w http.ResponseWriter, r *http.Request) {
		limit, offset := listFixturePage(t, r)
		snapshot := "old"
		if limit == listPageSize {
			normalPages.Add(1)
		} else {
			if smallPages.Add(1) == 1 && r.URL.Query().Get("continue") != "" {
				t.Errorf("retry used the old continuation token: %s", r.URL)
			}
			snapshot = "new"
		}
		pad := ""
		if offset >= listPageSize {
			pad = padding
		}
		writeIngressPage(w, snapshot, pad, offset, limit, objects)
	})
	kind := kindOf("ingress")
	items, observation, err := controller.list(ctx, kind)
	if err != nil || len(items) != objects || observation.rv != "new" || normalPages.Load() != 2 || smallPages.Load() != 16 {
		t.Fatalf("retry did not complete a fresh snapshot: %d objects, observation=%+v, normal=%d small=%d, err=%v", len(items), observation, normalPages.Load(), smallPages.Load(), err)
	}
	for index, item := range items {
		if want := "new-" + strconv.Itoa(index); item.GetName() != want {
			t.Fatalf("item %d: got %s, want %s", index, item.GetName(), want)
		}
	}
	for _, result := range []listResult{listTruncated, listOK} {
		if got := testutil.ToFloat64(controller.metrics.lists.WithLabelValues(kind.label, string(result))); got != 1 {
			t.Errorf("metric %s = %v, want 1", result, got)
		}
	}
}

func TestExternalSourceSmallPageRetryStillEnforcesByteCapAndKeepsLastGood(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	st := memory.New()
	zone := externalZone()
	if _, err := st.Apply(ctx, zone, store.ApplyOptions{Manager: "test"}); err != nil {
		t.Fatal(err)
	}
	kind := kindOf("ingress")
	padding := strings.Repeat("x", 480<<10)
	var gets atomic.Int32
	controller := sourceHTTPFixture(t, st, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/web") {
			gets.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		limit, offset := listFixturePage(t, r)
		writeIngressPage(w, "snapshot", padding, offset, limit, 80)
	})
	source := strip(kind, object(kind, map[string]any{annotationPrefix + "hostname": "web.ext.example.", annotationPrefix + "target": "192.0.2.42"}, nil), nil)
	sets, err := controller.project(ctx, source, []*v1alpha1.DNSZone{zone})
	if err != nil || len(sets) != 1 {
		t.Fatalf("could not build fixture record set: %v, %v", sets, err)
	}
	name := RecordSetName(kind.label, source.uid, zone.Name)
	if _, err := st.Apply(ctx, sets[name], store.ApplyOptions{Manager: zonemodel.ExternalSourceManager(source.uid)}); err != nil {
		t.Fatal(err)
	}
	watches, err := controller.reconcile(ctx)
	if !errors.Is(err, store.ErrTooLarge) || len(watches) != 0 || gets.Load() != 0 {
		t.Fatalf("byte-capped retry: watches=%v lookups=%d err=%v", watches, gets.Load(), err)
	}
	var retained v1alpha1.DNSRecordSet
	if err := st.Get(ctx, store.KeyOf(sets[name]), &retained); err != nil {
		t.Fatal("partial list lost the last-good record set:", err)
	}
	if !reflect.DeepEqual(retained.Spec, sets[name].Spec) {
		t.Fatal("partial list changed the last-good record set:", retained.Spec)
	}
	if got := testutil.ToFloat64(controller.metrics.lists.WithLabelValues(kind.label, string(listTooLarge))); got != 1 {
		t.Fatalf("byte cap count = %v, want 1", got)
	}
}

func TestExternalSourceMalformedResponseDoesNotRetry(t *testing.T) {
	var requests atomic.Int32
	controller := sourceHTTPFixture(t, memory.New(), func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"kind":"IngressList","items":[`)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := controller.list(ctx, kindOf("ingress")); err == nil || errors.Is(err, errTruncatedPage) || requests.Load() != 1 {
		t.Fatalf("unrelated decode error retried or misclassified: requests=%d err=%v", requests.Load(), err)
	}
}

func sourceHTTPFixture(t *testing.T, st store.Store, handler http.HandlerFunc) *Controller {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := New(&rest.Config{Host: server.URL}, Options{Store: st, Validator: v, Admit: func(context.Context) error { return nil }, Registerer: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func listFixturePage(t *testing.T, r *http.Request) (limit, offset int) {
	t.Helper()
	query := r.URL.Query()
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil || (limit != listPageSize && limit != smallListPageSize) {
		t.Errorf("invalid fixture page limit: %s", r.URL)
		return listPageSize, 0
	}
	if token := query.Get("continue"); token != "" {
		offset, err = strconv.Atoi(token)
		if err != nil || offset <= 0 {
			t.Errorf("invalid fixture page token: %s", r.URL)
		}
	}
	return limit, offset
}

func writeIngressPage(writer http.ResponseWriter, snapshot, padding string, offset, limit, total int) {
	writer.Header().Set("Content-Type", "application/json")
	end := min(offset+limit, total)
	next := ""
	if end < total {
		next = strconv.Itoa(end)
	}
	_, _ = fmt.Fprintf(writer, `{"apiVersion":"networking.k8s.io/v1","kind":"IngressList","metadata":{"resourceVersion":%q,"continue":%q},"items":[`, snapshot, next)
	encoder := json.NewEncoder(writer)
	for index := offset; index < end; index++ {
		if index != offset {
			_, _ = io.WriteString(writer, ",")
		}
		if err := encoder.Encode(map[string]any{"metadata": map[string]any{
			"name":        snapshot + "-" + strconv.Itoa(index),
			"annotations": map[string]string{"fixture": padding},
		}}); err != nil {
			return
		}
	}
	_, _ = io.WriteString(writer, "]}")
}
