package localdb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestBlobsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := OpenSnapshotStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBlob(ctx, "cache"); !errors.Is(err, ErrNoBlob) {
		t.Fatalf("missing blob: want ErrNoBlob, got %v", err)
	}
	if err := s.PutBlob(ctx, "", []byte("x")); err == nil {
		t.Fatal("an empty blob name must be refused")
	}
	big := bytes.Repeat([]byte{0, 1, 2, 3, 0xff}, 2<<20) // 10 MiB
	before := time.Now()
	if err := s.PutBlob(ctx, "cache", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlob(ctx, "cache", big); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlob(ctx, "empty", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, path)
	b, err := s.GetBlob(ctx, "cache")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "cache" || !bytes.Equal(b.Data, big) {
		t.Fatalf("blob after reopen: name %q, %d bytes (want %d)", b.Name, len(b.Data), len(big))
	}
	if b.WrittenAt.Before(before.Add(-time.Second)) || b.WrittenAt.After(time.Now()) {
		t.Fatalf("written_at %s outside the write window", b.WrittenAt)
	}
	if e, err := s.GetBlob(ctx, "empty"); err != nil || len(e.Data) != 0 {
		t.Fatalf("empty blob: %v %v", e, err)
	}
	if err := s.DeleteBlob(ctx, "cache"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBlob(ctx, "cache"); err != nil {
		t.Fatalf("deleting a missing blob: %v", err)
	}
	if _, err := s.GetBlob(ctx, "cache"); !errors.Is(err, ErrNoBlob) {
		t.Fatalf("deleted blob: want ErrNoBlob, got %v", err)
	}
}

// TestLayoutMigration: a local.db written before layout_version existed
// (snapshot tables only) gains the blobs table and keeps its snapshot; a
// newer layout refuses to open.
func TestLayoutMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, `INSERT INTO meta(key, value) VALUES ('schema_version', '1')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()
	s := open(t, path)
	if err := s.Write(ctx, &Snapshot{Objects: testObjects(), ClusterID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBlob(ctx, "cache", []byte("dump")); err != nil {
		t.Fatalf("blobs after migrating a layout-1 database: %v", err)
	}
	var layout string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'layout_version'`).Scan(&layout); err != nil || layout != strconv.Itoa(LayoutVersion) {
		t.Fatalf("layout_version = %q (%v), want %d", layout, err, LayoutVersion)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening at the current layout is a no-op that keeps the data.
	s = open(t, path)
	if snap, err := s.Load(ctx); err != nil || len(snap.Objects) != 2 {
		t.Fatalf("snapshot after reopen: %v %v", snap, err)
	}
	if b, err := s.GetBlob(ctx, "cache"); err != nil || string(b.Data) != "dump" {
		t.Fatalf("blob after reopen: %v %v", b, err)
	}
	if _, err := s.db.Exec(`UPDATE meta SET value = ? WHERE key = 'layout_version'`, strconv.Itoa(LayoutVersion+1)); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := OpenSnapshotStore(ctx, path); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("newer layout: want ErrSchemaTooNew, got %v", err)
	}
}
