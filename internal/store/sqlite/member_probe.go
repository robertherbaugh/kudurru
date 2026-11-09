package sqlite

import (
	"context"
	"crypto/tls"
	"errors"
	"reflect"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ErrMemberProbeBusy refuses an incoming metadata probe without waiting for an
// outgoing exchange's writer exclusion. Two nodes must not queue on each other's
// held wmu. A later scheduled probe is a new read, never mutation replay.
var ErrMemberProbeBusy = errors.New("store/sqlite: member authority observation busy")

// ObserveMemberProbe authenticates one actual enrolled primary's announcement
// and emits only this node's resulting term under the same membership exclusion.
// A higher or conflicting term can only fence; this never installs an image,
// promotes a node, chooses an installed branch or authorizes user work. The durable
// fence remains independent of a later output error. emit must synchronously
// write and flush under ctx, join cancellation and never reenter Store/owner gates.
func (s *Store) ObserveMemberProbe(ctx context.Context, cluster, member string, incoming replication.Term, connection *tls.ConnectionState,
	emit func(context.Context, string, replication.Term, func() error) error) error {
	if ctx == nil || emit == nil || HasWriterToken(ctx) || len(cluster) > 256 || len(member) > 256 || incoming.Validate() != nil ||
		incoming.ClusterID != cluster || incoming.PrimaryID != member {
		return enrollment.ErrRefused
	}
	deadline, finite := ctx.Deadline()
	if !finite || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second {
		return enrollment.ErrRefused
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.wmu.TryLock() {
		return ErrMemberProbeBusy
	}
	defer s.wmu.Unlock()
	a, owner, err := s.ReadMemberEnrollment(ctx)
	if err != nil {
		return err
	}
	if owner.State() == nil || a.Term.ClusterID != cluster || member == a.MemberID ||
		(a.Mode != replication.Primary && a.Mode != replication.Follower && a.Mode != replication.Fenced && a.Mode != replication.Conflict) {
		return enrollment.ErrRefused
	}
	var local, remote memberidentity.Trust
	for _, candidate := range owner.State().Members() {
		if candidate.Revoked {
			continue
		}
		if candidate.ID == a.MemberID {
			local = candidate.Trust
		}
		if candidate.ID == member {
			remote = candidate.Trust
		}
	}
	if !local.ValidAt(time.Now()) || !remote.ValidAt(time.Now()) {
		return enrollment.ErrRefused
	}
	peer, err := remote.CapturePeer(ctx, connection)
	if err != nil {
		return err
	}
	for _, until := range []time.Time{peer.Deadline(), local.ExpiresAt()} {
		if until.Before(deadline) {
			deadline = until
		}
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) || !local.ValidAt(time.Now()) || s.isClosed() || s.writeBlocked.Load() {
			return enrollment.ErrRefused
		}
		return peer.Check()
	}
	if err := check(); err != nil {
		return err
	}
	// Older authenticated announcements do not downgrade this node. Its current
	// term is returned so that the original sender can fence independently.
	if incoming.Epoch >= a.Term.Epoch {
		a, err = s.observeAuthorityLocked(ctx, incoming)
		if err != nil && !errors.Is(err, replication.ErrPromotionConflict) && !errors.Is(err, replication.ErrDivergent) {
			return err
		}
	}
	final := func() error {
		if err := check(); err != nil {
			return err
		}
		if !reflect.DeepEqual(a, s.Authority()) {
			return replication.ErrCAS
		}
		return nil
	}
	if err := final(); err != nil {
		return err
	}
	if err := emit(ctx, a.MemberID, a.Term, final); err != nil {
		return err
	}
	return final()
}
