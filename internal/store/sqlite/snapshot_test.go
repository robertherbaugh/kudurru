package sqlite

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openTemp(t)
	z := mustApply(t, src, zone("home", "home.example."), store.ManagerFile("objects.d/zones.yaml")).(*v1alpha1.DNSZone)
	z.Status.Serial = 99
	if err := src.UpdateStatus(ctx, z); err != nil {
		t.Fatal(err)
	}
	mustApply(t, src, recordSet("apps", "nas", map[string]string{"team": "infra"}), store.ManagerAPI)
	mustApply(t, src, recordSet("dns", "ns1", nil), store.ManagerIntegration("unifi"))
	mustApply(t, src, clientGroup("lan", "kids"), store.ManagerAPI)

	var buf bytes.Buffer
	if err := src.Snapshot(ctx, &buf, store.SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		APIVersion string           `json:"apiVersion"`
		Kind       string           `json:"kind"`
		Items      []map[string]any `json:"items"`
	}
	if err := yaml.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "List" || doc.APIVersion != "v1" || len(doc.Items) != 4 {
		t.Fatalf("snapshot shape: %s %s with %d items", doc.APIVersion, doc.Kind, len(doc.Items))
	}
	var order []string
	for _, it := range doc.Items {
		md := it["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		order = append(order, it["kind"].(string)+"/"+ns+"/"+md["name"].(string))
	}
	if got := strings.Join(order, ","); got != "ClientGroup/lan/kids,DNSRecordSet/apps/nas,DNSRecordSet/dns/ns1,DNSZone//home" {
		t.Fatalf("snapshot order: %s", got)
	}

	dst := openTemp(t)
	if err := dst.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []store.Kind{"DNSZone", "DNSRecordSet", "ClientGroup"} {
		a, _ := src.List(ctx, kind, store.ListOptions{})
		b, _ := dst.List(ctx, kind, store.ListOptions{})
		if len(a.Items) != len(b.Items) {
			t.Fatalf("%s: %d restored, want %d", kind, len(b.Items), len(a.Items))
		}
		for i := range a.Items {
			x, y := a.Items[i], b.Items[i]
			if store.KeyOf(x) != store.KeyOf(y) || x.GetUID() != y.GetUID() || !x.GetCreationTimestamp().Time.Equal(y.GetCreationTimestamp().Time) ||
				store.ManagedBy(x) != store.ManagedBy(y) || specHash(x) != specHash(y) || len(x.GetLabels()) != len(y.GetLabels()) {
				t.Fatalf("%s restored differently:\n%+v\n%+v", store.KeyOf(x), x, y)
			}
		}
	}
	var rz v1alpha1.DNSZone
	if err := dst.Get(ctx, store.Key{Kind: "DNSZone", Name: "home"}, &rz); err != nil {
		t.Fatal(err)
	}
	if rz.Status.Serial != 0 {
		t.Fatal("status is not restored; the controller recomputes it")
	}
	if store.ManagedBy(&rz) != "file:objects.d/zones.yaml" {
		t.Fatalf("restore keeps the recorded owner: %q", store.ManagedBy(&rz))
	}

	// Restoring the same snapshot again is a no-op.
	rv := dst.ResourceVersion()
	if err := dst.Restore(ctx, bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	if dst.ResourceVersion() != rv {
		t.Fatal("re-restoring an identical snapshot must not write")
	}
}

func TestRestoreDefaultsToBootstrapAndJoinsErrors(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	in := `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: ok}
spec: {zoneName: ok.example., type: primary}
---
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: bad}
spec: {zoneName: no-dot, type: primary}
`
	err := s.Restore(ctx, strings.NewReader(in))
	if err == nil || !strings.Contains(err.Error(), "DNSZone/bad") {
		t.Fatalf("expected the invalid document to be reported, got %v", err)
	}
	var z v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "ok"}, &z); err != nil {
		t.Fatal("valid documents are applied even when others fail")
	}
	if store.ManagedBy(&z) != store.ManagerBootstrap {
		t.Fatalf("documents without an owner restore as bootstrap, got %q", store.ManagedBy(&z))
	}
	if err := s.Restore(ctx, strings.NewReader("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n")); err == nil {
		t.Fatal("foreign kinds must be rejected")
	}
}

func TestSnapshotSafeStripsInternalKinds(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, func(o *Options) { o.InternalKinds = []store.Kind{"ClientGroup"} })
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	mustApply(t, s, clientGroup("lan", "kids"), store.ManagerAPI)
	var full, safe bytes.Buffer
	if err := s.Snapshot(ctx, &full, store.SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Snapshot(ctx, &safe, store.SnapshotOptions{Safe: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.String(), "kind: ClientGroup") {
		t.Fatal("a full snapshot includes internal kinds")
	}
	if strings.Contains(safe.String(), "kind: ClientGroup") || !strings.Contains(safe.String(), "kind: DNSZone") {
		t.Fatalf("safe snapshot:\n%s", safe.String())
	}
}
