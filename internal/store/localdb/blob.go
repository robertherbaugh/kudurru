package localdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// schemaV2Blobs is layout step 2: named opaque blobs. A rowid table rather
// than WITHOUT ROWID because a cache dump runs to megabytes.
const schemaV2Blobs = `
CREATE TABLE IF NOT EXISTS blobs (
	name          TEXT    PRIMARY KEY,
	data          BLOB    NOT NULL,
	written_at_ns INTEGER NOT NULL
) STRICT;
`

// ErrNoBlob means no blob of that name has been stored.
var ErrNoBlob = errors.New("localdb: no such blob")

// Blob is one stored blob.
type Blob struct {
	Name      string
	Data      []byte
	WrittenAt time.Time
}

// BlobStore keeps named opaque byte strings across restarts. *SnapshotStore
// implements it on the blobs table of local.db; internal/cache/persist
// stores the response cache dump through it.
type BlobStore interface {
	// PutBlob replaces the blob called name with data, stamped with the
	// current time, in one transaction.
	PutBlob(ctx context.Context, name string, data []byte) error
	// GetBlob returns the blob called name, or ErrNoBlob.
	GetBlob(ctx context.Context, name string) (Blob, error)
}

var _ BlobStore = (*SnapshotStore)(nil)

// PutBlob implements BlobStore.
func (s *SnapshotStore) PutBlob(ctx context.Context, name string, data []byte) error {
	if name == "" {
		return errors.New("localdb: blob name is empty")
	}
	if data == nil {
		data = []byte{}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO blobs(name, data, written_at_ns) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET data = excluded.data, written_at_ns = excluded.written_at_ns`,
		name, data, time.Now().UnixNano()); err != nil {
		return fmt.Errorf("localdb: put blob %s: %w", name, err)
	}
	return nil
}

// GetBlob implements BlobStore.
func (s *SnapshotStore) GetBlob(ctx context.Context, name string) (Blob, error) {
	b := Blob{Name: name}
	var ns int64
	err := s.db.QueryRowContext(ctx, `SELECT data, written_at_ns FROM blobs WHERE name = ?`, name).Scan(&b.Data, &ns)
	if errors.Is(err, sql.ErrNoRows) {
		return Blob{}, ErrNoBlob
	}
	if err != nil {
		return Blob{}, fmt.Errorf("localdb: get blob %s: %w", name, err)
	}
	b.WrittenAt = time.Unix(0, ns)
	return b, nil
}

// DeleteBlob removes the blob called name; a missing blob is not an error.
func (s *SnapshotStore) DeleteBlob(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM blobs WHERE name = ?`, name); err != nil {
		return fmt.Errorf("localdb: delete blob %s: %w", name, err)
	}
	return nil
}
