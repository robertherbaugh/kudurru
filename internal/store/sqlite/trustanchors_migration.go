package sqlite

import (
	"context"
	"database/sql"

	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// migrateTrustAnchorOwner establishes only a non-granting format marker. Cluster
// identity and original writer admission are unavailable at this physical step;
// the explicit runtime-owner startup command must bind the normal dormant owner.
// Any preexisting owned row refuses rather than being adopted, erased or reset.
func migrateTrustAnchorOwner(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM replication_sections
		WHERE section=? AND substr(key,1,length(?))=?)`, accepted.Section, accepted.Prefix, accepted.Prefix).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return accepted.ErrInvalid
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, accepted.Section, accepted.RootKey, accepted.AwaitingOwnerJSON)
	return err
}
