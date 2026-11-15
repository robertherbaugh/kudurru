package sqlite

import (
	"context"
	"errors"
	"math"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ErrEnrollmentCompletionUnknown means the initial membership/authority command
// has no confirmed completion. The old writer is fenced; callers must not claim
// rollback, publish the candidate or retry the original operation as a refusal.
var ErrEnrollmentCompletionUnknown = errors.New("store/sqlite: initial enrollment completion unknown")

// enrollmentPrimary is an actual same-command proposal, never a receipt. The
// old in-memory authority stays installed until the entire SQL commit succeeds.
type enrollmentPrimary struct {
	after *replication.Authority
}

// StageEnrollmentPrimary stages first membership and the Standalone -> Primary
// transition in this command. expected must be the original Store-issued cut,
// captured before authorization/acquisition, and identity must be the local
// private identity. A complete Dormant owner is mandatory; neither migration nor
// missing history is initialization authority here. Stage this before other
// mutations, then append audit writes in the SAME command.
//
// This supplies no operator permission. The caller must authorize in this tx,
// register original final proof checks and retain dependency exclusion through
// Command completion. The original finite context must be at most 30 seconds.
// Only successful outer Command completion proves a durable transition. Output
// and publication require their own admission; the old writer is then fenced.
func (t *CommandTx) StageEnrollmentPrimary(ctx context.Context, expected GenerationToken, identity *memberidentity.Identity, name, address string) error {
	if t == nil {
		return ErrTransactionEnded
	}
	if ctx == nil {
		return t.poison(replication.ErrInvalid)
	}
	if err := t.valid(ctx); err != nil {
		return err
	}
	now := time.Now()
	deadline, finite := ctx.Deadline()
	writer, carried := WriterTokenFromContext(ctx)
	if !finite || !now.Before(deadline) || deadline.Sub(now) > 30*time.Second || !carried ||
		writer.member != t.authority.MemberID || writer.incarnation != t.authority.Incarnation || !writer.term.Same(t.authority.Term) ||
		identity == nil || expected.source != t.s || !expected.Matches(t.authority) {
		return t.poison(replication.ErrCAS)
	}
	if t.primary != nil || t.changed || t.localChanged || t.enrollmentBefore != nil || t.anchor != nil ||
		t.authority.Mode != replication.Standalone || t.authority.Incarnation == math.MaxUint64 {
		return t.poison(replication.ErrCAS)
	}
	trust := identity.Trust()
	if trust.MemberID() != t.authority.MemberID || !trust.ValidAt(now) {
		return t.poison(enrollment.ErrRefused)
	}
	image, err := t.s.readGeneration(ctx, t.tx)
	if err != nil {
		return t.poison(err)
	}
	if !image.Generation().Same(expected.generation) {
		return t.poison(replication.ErrCAS)
	}
	if _, err := t.s.ValidateImage(ctx, *image); err != nil {
		return t.poison(err)
	}
	rows, err := readEnrollmentRows(ctx, t.tx)
	if err != nil {
		return t.poison(err)
	}
	before, err := accepted.DecodeRows(rows, t.authority.Term.ClusterID)
	if err != nil {
		return t.poison(err)
	}
	if before.State() != nil {
		return t.poison(enrollment.ErrRefused)
	}
	state, err := enrollment.Initialize(before.ClusterID(), enrollment.Member{ID: trust.MemberID(), Name: name, Address: address, Trust: trust}, now)
	if err != nil {
		return t.poison(err)
	}
	next, err := accepted.Enrolled(state)
	if err != nil {
		return t.poison(err)
	}
	rows, err = next.Rows()
	if err != nil {
		return t.poison(err)
	}
	// Keep this exact context/certificate and clock origin even if later callback
	// work supplies another context. Nothing samples a replacement identity.
	if err := t.RequireCommitCheck(func(context.Context) error {
		at := time.Now()
		if err := ctx.Err(); err != nil {
			return err
		}
		if !at.Before(deadline) {
			return context.DeadlineExceeded
		}
		if at.UTC().Before(now.UTC()) || !trust.ValidAt(at) {
			return enrollment.ErrRefused
		}
		return nil
	}); err != nil {
		return err
	}
	t.primary = &enrollmentPrimary{}
	if err := t.PutSection(ctx, accepted.Section, rows[0]); err != nil {
		return err
	}
	return t.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix)
}

// preparePrimary runs after the ordinary original-writer check and revision
// advance. Its own new incarnation must not be mistaken for a foreign writer;
// final admission instead checks the exact planned SQL authority and unchanged
// original memory authority under the same wmu.
func (t *CommandTx) preparePrimary(ctx context.Context, a *replication.Authority) error {
	if t.primary == nil {
		return nil
	}
	a.Mode = replication.Primary
	a.Incarnation++ // StageEnrollmentPrimary checked overflow on the original cut.
	a.WatchLineage = uuid.NewString()
	if err := writeAuthority(ctx, t.tx, *a); err != nil {
		return err
	}
	after := *a
	t.primary.after = &after
	return nil
}

func (t *CommandTx) checkPrimaryWriter(ctx context.Context) error {
	writer, carried := WriterTokenFromContext(ctx)
	if t.primary.after == nil || !carried || writer.member != t.authority.MemberID ||
		writer.incarnation != t.authority.Incarnation || !writer.term.Same(t.authority.Term) ||
		t.s.isClosed() || t.s.writeBlocked.Load() || !reflect.DeepEqual(t.s.Authority(), t.authority) {
		return replication.ErrFenced
	}
	actual, err := readAuthority(ctx, t.tx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, *t.primary.after) {
		return replication.ErrFenced
	}
	return nil
}
