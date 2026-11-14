package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ErrMemberEnrollmentPending reports only the exact migration sentinel on a
// read-only receiver that has never installed an image. It grants no membership
// or TLS trust; startup can keep ordinary listeners alive but unavailable.
var ErrMemberEnrollmentPending = errors.New("store/sqlite: member enrollment awaiting first image")

// ReadMemberEnrollment reads bounded complete membership and local authority
// from one transaction, without loading the complete replica image. It is an
// observation for listener setup and TLS trust, never request or write admission.
// Callers must bound setup/handshake I/O; each request still needs its original
// CaptureMemberRequest before body acquisition and through final output.
func (s *Store) ReadMemberEnrollment(ctx context.Context) (replication.Authority, accepted.Owner, error) {
	if s.isClosed() {
		return replication.Authority{}, accepted.Owner{}, store.ErrClosed
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return replication.Authority{}, accepted.Owner{}, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return replication.Authority{}, accepted.Owner{}, err
	}
	rows, err := readEnrollmentRows(ctx, tx)
	if err != nil {
		return replication.Authority{}, accepted.Owner{}, err
	}
	if err := ctx.Err(); err != nil {
		return replication.Authority{}, accepted.Owner{}, err
	}
	if accepted.IsAwaiting(rows) &&
		(a.Mode == replication.Unenrolled || a.Mode == replication.Follower) &&
		a.Installed == (replication.Generation{}) && a.InstallSequence == 0 &&
		a.Parent == nil && a.Conflict == nil && a.Divergence == nil {
		return a, accepted.Owner{}, ErrMemberEnrollmentPending
	}
	owner, err := accepted.DecodeRows(rows, a.Term.ClusterID)
	if err != nil {
		return replication.Authority{}, accepted.Owner{}, err
	}
	return a, owner, ctx.Err()
}
