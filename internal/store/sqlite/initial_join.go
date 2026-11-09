package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	anchors "github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// InitialJoin retains an exact never-installed local cut BEFORE contacting the
// invitation's primary. Its context and anchor carrier cannot be recaptured
// after network acquisition. It never grants authority to overwrite a database.
type InitialJoin struct{ original *initialJoin }

type initialJoin struct {
	s          *Store
	ctx        context.Context
	invitation enrollment.Invitation
	local      memberidentity.Trust
	life       anchorOriginalLifetime
	used       atomic.Bool
}

type initialJoinKey struct{}

// CreateJoinReceiver exclusively creates a never-enrolled database under the
// caller's original finite context. The parent must already be an explicitly
// created private directory. Existing files refuse; errors preserve evidence.
func CreateJoinReceiver(ctx context.Context, path string, opts Options) (*Store, error) {
	life, err := captureAnchorLifetime(ctx)
	if err != nil {
		return nil, err
	}
	// #nosec G304 G703 -- explicit offline operator path; exclusive creation, never HTTP input
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	opts.InitialMode = replication.Unenrolled
	s, err := openStore(ctx, path, opts, false)
	if err != nil {
		return nil, err
	}
	if err := life.check(); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

// CaptureInitialJoin requires an explicitly unenrolled pristine receiver whose
// physical migration carrier was already bound. Missing historical data and
// standalone/follower databases refuse, including previously installed images.
func (s *Store) CaptureInitialJoin(ctx context.Context, invitation enrollment.Invitation, local memberidentity.Trust) (*InitialJoin, error) {
	life, err := captureAnchorLifetime(ctx)
	if err != nil {
		return nil, err
	}
	if err := invitation.CheckLifetime(local); err != nil {
		return nil, err
	}
	o := &initialJoin{s: s, invitation: invitation, local: local, life: life}
	ctx = context.WithValue(ctx, initialJoinKey{}, o)
	captured, err := s.CaptureAnchorInstall(ctx)
	if err != nil {
		return nil, err
	}
	cell := captured.Value(anchorInstallKey{}).(*anchorInstall)
	if !anchorAwaitingInstallCursor(cell.cursor) || cell.installed != (replication.Generation{}) {
		return nil, replication.ErrCAS
	}
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := readAuthority(ctx, tx)
	if err != nil {
		return nil, err
	}
	if anchorAuthority(a) != cell.authority || a.MemberID != local.MemberID() {
		return nil, replication.ErrCAS
	}
	if err := pristineJoin(ctx, tx, a); err != nil {
		return nil, err
	}
	o.ctx = captured
	return &InitialJoin{original: o}, o.check()
}

func pristineJoin(ctx context.Context, q *sql.Tx, a replication.Authority) error {
	if a.Mode != replication.Unenrolled || a.Term != (replication.Term{}) || a.Installed != (replication.Generation{}) || a.InstallSequence != 0 || a.MaxObservedEpoch != 0 || a.Parent != nil || a.Conflict != nil || a.Divergence != nil {
		return replication.ErrCAS
	}
	var occupied bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM objects) OR EXISTS(SELECT 1 FROM changes) OR EXISTS(SELECT 1 FROM replication_quarantine) OR EXISTS(SELECT 1 FROM audit) OR EXISTS(SELECT 1 FROM replication_sections WHERE NOT ((section=? AND key=?) OR (section=? AND key=?)))`, accepted.Section, accepted.RootKey, anchors.Section, anchors.RootKey).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return replication.ErrCAS
	}
	var zeroCounters int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key IN (?,?) AND value='0'`, metaResourceVersion, metaFeedCompacted).Scan(&zeroCounters); err != nil {
		return err
	}
	if zeroCounters != 2 {
		return replication.ErrCAS
	}
	// Exact sentinel validation rejects absent, altered and historical owners.
	rows, err := readEnrollmentRows(ctx, q)
	if err != nil {
		return err
	}
	if !accepted.IsAwaiting(rows) {
		return replication.ErrCAS
	}
	return nil
}

func (o *initialJoin) check() error {
	if o == nil || o.s == nil {
		return replication.ErrInvalid
	}
	if err := o.life.check(); err != nil {
		return err
	}
	return o.invitation.CheckLifetime(o.local)
}

// InstallJoinedReplica atomically pins the original primary term and installs
// its complete first image, including local anchor binding. The caller must have
// authenticated acquisition with the invitation's exact CA. A successful primary
// acceptance alone never changes this receiver's authority or serving state.
func (s *Store) InstallJoinedReplica(request *InitialJoin, validated *replication.Validated) (replication.Generation, error) {
	if request == nil || request.original == nil {
		return replication.Generation{}, replication.ErrInvalid
	}
	o := request.original
	if o.s != s || validated == nil || !o.used.CompareAndSwap(false, true) {
		return replication.Generation{}, replication.ErrInvalid
	}
	if err := o.check(); err != nil {
		return replication.Generation{}, err
	}
	i := validated.Image()
	issuer, _, _ := o.invitation.IssuingPeer()
	if i.Term.ClusterID != o.invitation.ClusterID() || i.Term.PrimaryID != issuer.MemberID || i.Term.Epoch != issuer.Epoch || i.Term.PromotionID != issuer.PromotionID {
		return replication.Generation{}, replication.ErrCAS
	}
	owner, err := accepted.DecodeRows(i.Payload.Membership, i.Term.ClusterID)
	if err != nil {
		return replication.Generation{}, err
	}
	if err := o.invitation.CheckAccepted(owner.State(), o.local); err != nil {
		return replication.Generation{}, err
	}
	return s.InstallReplica(o.ctx, validated, replication.InstallOptions{ExpectedTerm: i.Term})
}
