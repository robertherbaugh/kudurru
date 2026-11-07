package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/robertherbaugh/kudurru/internal/store"
)

const metaAuditLegacyMaxID = "audit_legacy_max_id"

func migrateAuditAttribution(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `ALTER TABLE audit ADD COLUMN chain_version INTEGER NOT NULL DEFAULT 1 CHECK (chain_version IN (1,2));
ALTER TABLE audit ADD COLUMN authz TEXT NOT NULL DEFAULT '';
ALTER TABLE audit ADD COLUMN request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE audit ADD COLUMN client_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE audit ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE audit ADD COLUMN request_method TEXT NOT NULL DEFAULT '';
ALTER TABLE audit ADD COLUMN request_path TEXT NOT NULL DEFAULT '';
INSERT INTO meta(key,value) SELECT 'audit_legacy_max_id', CAST(COALESCE(MAX(id),0) AS TEXT) FROM audit;`)
	return err
}

func auditLegacyBoundary(ctx context.Context, q txQueryer) (int64, error) {
	var value string
	if err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, metaAuditLegacyMaxID).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: missing audit format boundary", store.ErrAuditIntegrity)
		}
		return 0, fmt.Errorf("store/sqlite: read audit format boundary: %w", err)
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != value {
		return 0, fmt.Errorf("%w: invalid audit format boundary", store.ErrAuditIntegrity)
	}
	return n, nil
}

func validateAuditFormats(ctx context.Context, q txQueryer) error {
	boundary, err := auditLegacyBoundary(ctx, q)
	if err != nil {
		return err
	}
	var invalid int
	err = q.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE
 (id <= ? AND (chain_version != 1 OR authz != '' OR request_id != '' OR client_ip != '' OR user_agent != '' OR request_method != '' OR request_path != ''))
 OR (id > ? AND chain_version != 2)`, boundary, boundary).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("%w: unsupported audit chain format", store.ErrAuditIntegrity)
	}
	return nil
}

func validAuditFormat(e *AuditEntry, boundary int64) bool {
	if e.ID > boundary {
		return e.ChainVersion == 2
	}
	return e.ChainVersion == 1 && e.Authz == "" && e.RequestID == "" && e.ClientIP == "" && e.UserAgent == "" && e.RequestMethod == "" && e.RequestPath == ""
}
