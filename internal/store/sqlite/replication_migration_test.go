package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestMigration2PreservesLegacyObjectsAndStatus(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", dsn(path, time.Second, true))
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations[:1], 1, time.Now); err != nil {
		t.Fatal(err)
	}
	z := zone("home", "home.example.")
	z.APIVersion, z.Kind = "kudurru.io/v1alpha1", "DNSZone"
	z.ResourceVersion, z.UID, z.Generation = "7", "legacy-stable-uid", 12
	z.SetCreationTimestamp(storedTime(time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)))
	z.Annotations = map[string]string{v1alpha1.ManagedByAnnotation: "file:legacy.yaml", "operator": "preserved"}
	z.Status.Serial = 2026101401
	z.Status.ContentHash = "source-hash"
	raw, err := json.Marshal(z)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, sqlUpsertObject, "DNSZone", "", "home", string(z.UID), 12, 7, "file:legacy.yaml", "2026-10-14T00:00:00Z", 0, specHash(z), "{}", 0, string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE meta SET value='7' WHERE key=?`, metaResourceVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path)
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, store.KeyOf(z), &got); err != nil {
		t.Fatal(err)
	}
	if got.UID != z.UID || got.Generation != 12 || got.Status.Serial != z.Status.Serial || got.Status.ContentHash != z.Status.ContentHash || got.ResourceVersion != "7" || store.ManagedBy(&got) != "file:legacy.yaml" {
		t.Fatal("migration changed complete legacy document")
	}
	image := imageOf(t, s)
	if image.ObjectResourceVersion != 7 || image.Revision != 0 {
		t.Fatal("migration confused source rv with image revision")
	}
	if _, err := s.ValidateImage(ctx, *image); err != nil {
		t.Fatal(err)
	}
}

type hookReader struct {
	io.Reader
	hook func()
}

func (r *hookReader) Read(b []byte) (int, error) {
	if r.hook != nil {
		f := r.hook
		r.hook = nil
		f()
	}
	return r.Reader.Read(b)
}

func TestRestoreBindsAuthorityBeforeDecodingInput(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	base := imageOf(t, s).Generation()
	input := &hookReader{Reader: strings.NewReader("apiVersion: kudurru.io/v1alpha1\nkind: DNSZone\nmetadata:\n  name: restored\nspec:\n  zoneName: restored.example.\n  type: primary\n"), hook: func() {
		if _, err := s.EnablePrimary(ctx, base); err != nil {
			t.Fatal(err)
		}
	}}
	if err := s.Restore(ctx, input); !errors.Is(err, replication.ErrFenced) {
		t.Fatalf("pre-transition restore crossed authority: %v", err)
	}
	var got v1alpha1.DNSZone
	if err := s.Get(ctx, store.Key{Kind: "DNSZone", Name: "restored"}, &got); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("restore committed after transition: %v", err)
	}
}
