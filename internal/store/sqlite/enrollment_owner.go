package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// Migration alone can introduce the never-enrolled physical marker. Existing
// rows refuse; neither upgrade nor normal startup repairs historical membership.
func migrateEnrollmentOwner(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM replication_sections WHERE section=? AND substr(key,1,length(?))=?)`, accepted.Section, accepted.Prefix, accepted.Prefix).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return accepted.ErrOwner
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO replication_sections(section,key,data) VALUES(?,?,?)`, accepted.Section, accepted.RootKey, accepted.AwaitingOwnerJSON)
	return err
}

func readEnrollmentRows(ctx context.Context, tx *sql.Tx) ([]replication.Entry, error) {
	var count, size, keySize int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(CAST(data AS BLOB))),0),COALESCE(MAX(length(CAST(key AS BLOB))),0) FROM replication_sections WHERE section=? AND substr(key,1,length(?))=?`, accepted.Section, accepted.Prefix, accepted.Prefix).Scan(&count, &size, &keySize); err != nil {
		return nil, err
	}
	if count != 1 || size > accepted.MaxRowBytes || keySize > 512 {
		return nil, accepted.ErrOwner
	}
	var key, raw string
	if err := tx.QueryRowContext(ctx, `SELECT key,data FROM replication_sections WHERE section=? AND substr(key,1,length(?))=?`, accepted.Section, accepted.Prefix, accepted.Prefix).Scan(&key, &raw); err != nil {
		return nil, err
	}
	return []replication.Entry{{Key: key, Data: json.RawMessage(raw)}}, nil
}

func enrollmentTransition(before, after []replication.Entry, cluster string) error {
	if accepted.IsAwaiting(before) {
		if accepted.IsAwaiting(after) {
			return nil
		}
		_, err := accepted.DecodeRows(after, cluster)
		return err
	}
	old, err := accepted.DecodeRows(before, cluster)
	if err != nil {
		return err
	}
	next, err := accepted.DecodeRows(after, cluster)
	if err != nil {
		return err
	}
	if !next.RetainsHistory(old) {
		return accepted.ErrOwner
	}
	return nil
}

// Capture before the first owned mutation, avoiding membership reads/decodes on
// unrelated commands. The actual whole postimage is checked before any commit.
func (t *CommandTx) captureEnrollment(ctx context.Context, section, key string) error {
	if !enrollmentKey(section, key) || t.enrollmentBefore != nil {
		return nil
	}
	rows, err := readEnrollmentRows(ctx, t.tx)
	if err != nil {
		return t.poison(err)
	}
	t.enrollmentBefore = rows
	return nil
}

func (t *CommandTx) prepareEnrollment(ctx context.Context) error {
	if t.enrollmentBefore == nil {
		return nil
	}
	rows, err := readEnrollmentRows(ctx, t.tx)
	if err != nil {
		return err
	}
	if accepted.IsAwaiting(t.enrollmentBefore) && !accepted.IsAwaiting(rows) {
		next, err := accepted.DecodeRows(rows, t.authority.Term.ClusterID)
		if err != nil {
			return err
		}
		if next.State() != nil {
			return accepted.ErrOwner
		}
	}
	return enrollmentTransition(t.enrollmentBefore, rows, t.authority.Term.ClusterID)
}

func enrollmentKey(section, key string) bool {
	return section == accepted.Section && strings.HasPrefix(key, accepted.Prefix)
}

func validateEnrollmentInstall(ctx context.Context, tx *sql.Tx, image replication.Image) error {
	before, err := readEnrollmentRows(ctx, tx)
	if err != nil {
		return err
	}
	return enrollmentTransition(before, image.Payload.Membership, image.Term.ClusterID)
}
