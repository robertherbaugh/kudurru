package localdb

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func open(t *testing.T, path string) *SnapshotStore {
	t.Helper()
	s, err := OpenSnapshotStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testObjects() []store.Object {
	z := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "home", ResourceVersion: "41", Annotations: map[string]string{v1alpha1.ManagedByAnnotation: store.ManagerAPI}},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary, DefaultTTL: 300},
		Status:     v1alpha1.DNSZoneStatus{Serial: 2026101101},
	}
	r := &v1alpha1.DNSRecordSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSRecordSet"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "nas", ResourceVersion: "42", Labels: map[string]string{"team": "infra"}},
		Spec: v1alpha1.DNSRecordSetSpec{
			ZoneRef:   v1alpha1.ObjectRef{Name: "home"},
			Endpoints: []v1alpha1.Endpoint{{DNSName: "nas.home.example.", RecordType: "A", Targets: []string{"10.0.0.5"}}},
		},
	}
	return []store.Object{r, z}
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s := open(t, path)
	if _, err := s.Load(ctx); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("fresh database: want ErrNoSnapshot, got %v", err)
	}
	written := time.Now().Add(-90 * time.Second).Truncate(time.Microsecond)
	in := &Snapshot{
		Objects:          testObjects(),
		ResourceVersions: map[store.Kind]string{"DNSZone": "41", "DNSRecordSet": "57", "BlockList": "39"},
		ClusterID:        "c0ffee",
		WrittenAt:        written,
	}
	if err := s.Write(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A fresh process (here: a fresh handle) reads it back.
	r := open(t, path)
	out, err := r.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Objects) != 2 || out.ClusterID != "c0ffee" || !out.WrittenAt.Equal(written) {
		t.Fatalf("got %d objects, cluster %q, written %s", len(out.Objects), out.ClusterID, out.WrittenAt)
	}
	if out.HighWaterMark() != "57" || out.ResourceVersions["BlockList"] != "39" {
		t.Fatalf("resource versions: %v (hwm %s)", out.ResourceVersions, out.HighWaterMark())
	}
	// Ordered by kind, namespace, name: DNSRecordSet before DNSZone.
	rs, ok := out.Objects[0].(*v1alpha1.DNSRecordSet)
	if !ok || rs.Spec.Endpoints[0].Targets[0] != "10.0.0.5" || rs.Labels["team"] != "infra" || rs.Kind != "DNSRecordSet" {
		t.Fatalf("record set did not round-trip: %#v", out.Objects[0])
	}
	z, ok := out.Objects[1].(*v1alpha1.DNSZone)
	if !ok || z.Spec.ZoneName != "home.example." || z.Status.Serial != 2026101101 || store.ManagedBy(z) != store.ManagerAPI || z.APIVersion != v1alpha1.GroupVersion.String() {
		t.Fatalf("zone did not round-trip: %#v", out.Objects[1])
	}
	if got := out.ByKind("DNSZone"); len(got) != 1 || got[0].GetName() != "home" {
		t.Fatalf("ByKind: %v", got)
	}
	now := written.Add(2 * time.Minute)
	if out.Age(now) != 2*time.Minute || !out.Fresh(24*time.Hour, now) || out.Fresh(time.Minute, now) {
		t.Fatalf("age %s", out.Age(now))
	}
}

func TestWriteReplaces(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), FileName))
	if err := s.Write(ctx, &Snapshot{Objects: testObjects(), ResourceVersions: map[store.Kind]string{"DNSZone": "1"}}); err != nil {
		t.Fatal(err)
	}
	only := testObjects()[1:]
	if err := s.Write(ctx, &Snapshot{Objects: only, ResourceVersions: map[store.Kind]string{"DNSZone": "2"}}); err != nil {
		t.Fatal(err)
	}
	out, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Objects) != 1 || out.ResourceVersions["DNSZone"] != "2" || len(out.ResourceVersions) != 1 {
		t.Fatalf("second write must replace the first: %d objects, %v", len(out.Objects), out.ResourceVersions)
	}
	if out.WrittenAt.IsZero() || out.Age(time.Now()) > time.Minute {
		t.Fatalf("zero WrittenAt must be stamped with now, got %s", out.WrittenAt)
	}
	if err := s.Write(ctx, &Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if out, err = s.Load(ctx); err != nil || len(out.Objects) != 0 {
		t.Fatalf("empty snapshot: %v %v", out, err)
	}
}

func TestSchemaVersioning(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spaces and ?query# chars.db")
	s := open(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil || v != strconv.Itoa(store.SchemaVersion) {
		t.Fatalf("schema_version = %q (%v)", v, err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q (%v), want wal from the DSN pragmas", mode, err)
	}
	if _, err := db.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, strconv.Itoa(store.SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := OpenSnapshotStore(ctx, path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("newer on-disk schema: want ErrSchemaTooNew, got %v", err)
	}
}

func TestUnknownKindsAreSkipped(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s := open(t, path)
	if err := s.Write(ctx, &Snapshot{Objects: testObjects()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO kube_snapshot_objects(kind, namespace, name, api_version, resource_version, body) VALUES ('Integration', 'apps', 'unifi', 'kudurru.io/v1alpha2', '9', X'7b7d')`); err != nil {
		t.Fatal(err)
	}
	out, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Objects) != 2 || len(out.Skipped) != 1 || out.Skipped[0] != "kudurru.io/v1alpha2/Integration apps/unifi" {
		t.Fatalf("objects %d, skipped %v", len(out.Objects), out.Skipped)
	}
}

func TestHighWaterMark(t *testing.T) {
	tests := []struct {
		rvs  map[store.Kind]string
		want string
	}{
		{nil, ""},
		{map[store.Kind]string{"A": "7"}, "7"},
		{map[store.Kind]string{"A": "9", "B": "10", "C": "2"}, "10"},
		{map[store.Kind]string{"A": "9", "B": "opaque"}, ""},
		{map[store.Kind]string{"A": "18446744073709551615", "B": "1"}, "18446744073709551615"},
	}
	for _, tt := range tests {
		s := &Snapshot{ResourceVersions: tt.rvs}
		if got := s.HighWaterMark(); got != tt.want {
			t.Errorf("HighWaterMark(%v) = %q, want %q", tt.rvs, got, tt.want)
		}
	}
}

func TestOpenRejectsUnwritablePath(t *testing.T) {
	if _, err := OpenSnapshotStore(context.Background(), filepath.Join(t.TempDir(), "missing-dir", FileName)); err == nil {
		t.Fatal("opening a database in a missing directory must fail")
	}
}

func TestTouch(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), FileName))
	if err := s.Touch(ctx, time.Now()); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("touch before any write: want ErrNoSnapshot, got %v", err)
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Microsecond)
	if err := s.Write(ctx, &Snapshot{Objects: testObjects(), ResourceVersions: map[store.Kind]string{"DNSZone": "41"}, WrittenAt: old}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Microsecond)
	if err := s.Touch(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WrittenAt.Equal(now) || len(got.Objects) != 2 || got.ResourceVersions["DNSZone"] != "41" {
		t.Fatalf("after touch: writtenAt %s (want %s), %d objects, rvs %v", got.WrittenAt, now, len(got.Objects), got.ResourceVersions)
	}
}
