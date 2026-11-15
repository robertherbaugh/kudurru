package localdb

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestTrustAnchorSharedSchemaUpgradePreservesLocalLayoutAndBytes(t *testing.T) {
	for _, version := range []int{6, 7} {
		t.Run(strconv.Itoa(version), func(t *testing.T) { trustAnchorSharedSchemaUpgrade(t, version) })
	}
}

func trustAnchorSharedSchemaUpgrade(t *testing.T, version int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Build the actual existing schema6/7 layout2 tables, before opening the new
	// snapshot store. This owner has no trust-anchor rows or managed capability.
	if _, err := db.ExecContext(t.Context(), schemaV1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), schemaV2Blobs); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO meta(key,value) VALUES ('schema_version',?),('layout_version','2')`, strconv.Itoa(version)); err != nil {
		t.Fatal(err)
	}
	object := testObjects()[0]
	object.GetObjectKind().SetGroupVersionKind(v1alpha1.GroupVersion.WithKind("DNSZone"))
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).UnixNano()
	if _, err := db.ExecContext(t.Context(), `INSERT INTO kube_snapshot(id,written_at_ns,resource_version,cluster_id,objects) VALUES (1,?,'41','legacy-cluster',1)`, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO kube_snapshot_kinds(kind,resource_version,objects) VALUES ('DNSZone','41',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO kube_snapshot_objects(kind,namespace,name,api_version,resource_version,body) VALUES ('DNSZone','','home',?,'41',?)`, v1alpha1.GroupVersion.String(), body); err != nil {
		t.Fatal(err)
	}
	blob := []byte{0, 1, 0xff, 2}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO blobs(name,data,written_at_ns) VALUES ('original',?,?)`, blob, at); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := open(t, path)
	var schema, layout string
	if err := s.db.QueryRowContext(t.Context(), `SELECT value FROM meta WHERE key='schema_version'`).Scan(&schema); err != nil || schema != strconv.Itoa(store.SchemaVersion) {
		t.Fatal("shared local schema marker not advanced", err)
	}
	if err := s.db.QueryRowContext(t.Context(), `SELECT value FROM meta WHERE key='layout_version'`).Scan(&layout); err != nil || layout != "2" {
		t.Fatal("shared schema changed local layout", err)
	}
	var restored []byte
	if err := s.db.QueryRowContext(t.Context(), `SELECT body FROM kube_snapshot_objects WHERE kind='DNSZone' AND namespace='' AND name='home'`).Scan(&restored); err != nil || !bytes.Equal(restored, body) {
		t.Fatal("shared schema upgrade changed local object bytes", err)
	}
	got, err := s.GetBlob(t.Context(), "original")
	if err != nil || !bytes.Equal(got.Data, blob) || got.WrittenAt.UnixNano() != at {
		t.Fatal("shared schema upgrade changed local blob", err)
	}
	snapshot, err := s.Load(t.Context())
	if err != nil || snapshot.ClusterID != "legacy-cluster" || snapshot.WrittenAt.UnixNano() != at || snapshot.HighWaterMark() != "41" || len(snapshot.Objects) != 1 {
		t.Fatal("shared schema upgrade reset local snapshot", err)
	}
	// Exercise the production greater-than refusal without a helper that
	// duplicates its guard. This is not a compiled old-binary execution.
	future := strconv.Itoa(store.SchemaVersion + 1)
	if _, err := s.db.ExecContext(t.Context(), `UPDATE meta SET value=? WHERE key='schema_version'`, future); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSnapshotStore(t.Context(), path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatal("local downgrade gate accepted newer shared schema", err)
	}
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if err := raw.QueryRowContext(t.Context(), `SELECT body FROM kube_snapshot_objects WHERE kind='DNSZone'`).Scan(&restored); err != nil || !bytes.Equal(restored, body) {
		t.Fatal("refused local reopen changed old bytes", err)
	}
	if err := raw.QueryRowContext(t.Context(), `SELECT value FROM meta WHERE key='schema_version'`).Scan(&schema); err != nil || schema != future {
		t.Fatal("refused local reopen changed schema", err)
	}
}
