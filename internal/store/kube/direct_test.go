package kube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDirectHasNoBackgroundRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected request", http.StatusForbidden)
	}))
	defer server.Close()
	d, err := NewDirect(&rest.Config{Host: server.URL, Timeout: time.Second}, Options{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("direct constructor started discovery/informer work")
	}
	if err := d.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "example"}, &public.DNSZone{}); !errors.Is(err, store.ErrClosed) {
		t.Fatal("closed direct client remained usable")
	}
}

func TestDirectEnvtestOwnershipAndOriginalVersion(t *testing.T) {
	requireEnv(t)
	d, err := NewDirect(shared.cfg, Options{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "direct-proof"}, Spec: public.DNSZoneSpec{ZoneName: "direct.example.", Type: public.ZoneTypePrimary}}
	created, err := d.Apply(ctx, zone, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.admin.Delete(context.Background(), zone) })
	if _, err := d.Apply(ctx, zone, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("create-only replaced existing object")
	}
	changed := created.DeepCopyObject().(*public.DNSZone)
	changed.Labels = map[string]string{"team": "red"}
	updated, err := d.Apply(ctx, changed, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: created.GetResourceVersion()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Apply(ctx, created, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: created.GetResourceVersion()}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("stale original version was refreshed")
	}
	page, err := d.List(ctx, "DNSZone", store.ListOptions{Limit: 1}, "team in (red,blue)")
	if err != nil || len(page.Items) != 1 || page.Items[0].GetUID() != updated.GetUID() {
		t.Fatal("set-based selector not retained", err)
	}
	if _, err := d.Apply(ctx, zone, store.ApplyOptions{Manager: store.ManagerAPI}); err == nil {
		t.Fatal("unfenced direct apply accepted")
	}
	if _, err := d.Apply(ctx, zone, store.ApplyOptions{Manager: store.ManagerAPI, Force: true, CreateOnly: true}); err == nil {
		t.Fatal("force ownership exposed")
	}
	if err := d.Delete(ctx, store.KeyOf(zone), store.DeleteOptions{Manager: "file:objects.yaml"}); err == nil {
		t.Fatal("foreign manager deleted object")
	}
	if err := d.Delete(ctx, store.KeyOf(zone), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
}

// A replacement between ownership read and DELETE must survive. This uses the
// actual API server's UID/RV precondition, not a mocked conflict response.
func TestDirectEnvtestDeleteDoesNotFollowReplacement(t *testing.T) {
	requireEnv(t)
	d, err := NewDirect(shared.cfg, Options{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "direct-replacement"}, Spec: public.DNSZoneSpec{ZoneName: "replace.example.", Type: public.ZoneTypePrimary}}
	original, err := d.Apply(ctx, zone, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.admin.Delete(context.Background(), zone) })
	var replacement *public.DNSZone
	calls := 0
	d.store.c = &directDeleteBarrier{Client: d.store.c, before: func() {
		calls++
		if calls != 1 {
			return
		}
		if err := shared.admin.Delete(ctx, original); err != nil {
			t.Fatal(err)
		}
		replacement = zone.DeepCopy()
		replacement.Annotations = map[string]string{public.ManagedByAnnotation: store.ManagerAPI}
		if err := shared.admin.Create(ctx, replacement); err != nil {
			t.Fatal(err)
		}
	}}
	if err := d.Delete(ctx, store.KeyOf(zone), store.DeleteOptions{Manager: store.ManagerAPI}); !errors.Is(err, store.ErrConflict) {
		t.Fatal("replacement did not refuse original delete", err)
	}
	var current public.DNSZone
	if err := shared.admin.Get(ctx, client.ObjectKey{Name: zone.Name}, &current); err != nil {
		t.Fatal("replacement was deleted", err)
	}
	if calls != 1 || current.UID != replacement.UID || current.UID == original.GetUID() {
		t.Fatal("direct delete recaptured replacement authority")
	}
}

type directDeleteBarrier struct {
	client.Client
	before func()
}

func (c *directDeleteBarrier) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.before()
	return c.Client.Delete(ctx, obj, opts...)
}
