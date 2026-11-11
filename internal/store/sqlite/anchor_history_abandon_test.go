package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// A member that refuses the current primary's branch (D369) stays fenced: it
// is not a follower, so it cannot be promoted, and it records no conflict or
// divergence, so an ordinary resolution refuses it. When that primary and
// every member that followed it are lost for good, the member was stranded
// with no writer and no command to appoint one. These tests pin the bounded
// operator action that ends that state, an audited resolution that names the
// lost term exactly (ResolveRequest.AbandonTerm), and that it keeps every
// revocation the member holds.

// anchorAbandonRequest is the request `cluster resolve --abandon-term` builds
// from the offline review of a fenced member.
func anchorAbandonRequest(t *testing.T, member *Store, request string) (context.Context, *replication.Validated, replication.ResolveRequest) {
	t.Helper()
	ctx := captureReplica(t, member)
	image, err := member.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := member.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	a := member.Authority()
	term := a.Term
	return ctx, chosen, replication.ResolveRequest{ExpectedTerm: a.Term, ExpectedInstalled: image.Generation(), AbandonTerm: &term, RequestID: request}
}

func anchorAbandonAudit(t *testing.T) AuditEntry {
	t.Helper()
	return AuditEntry{Actor: "host:offline-cli", Authz: "host-local data directory and member identity; abandons unreachable term", Reason: "primary and its followers are lost"}
}

// anchorAbandonRefused runs one resolution attempt and requires it to refuse
// with want, leaving the member's authority, image, owner, carrier and
// quarantine as they were.
func anchorAbandonRefused(t *testing.T, member *Store, want error, attempt func() error) {
	t.Helper()
	before, installed, cursor, local := member.Authority(), imageOf(t, member).Generation(), anchorHistoryCursor(t, member), anchorLocalTestRead(t, member)
	if err := attempt(); !errors.Is(err, want) {
		t.Fatalf("abandonment: got %v, want %v", err, want)
	}
	if !reflect.DeepEqual(before, member.Authority()) || !installed.Same(imageOf(t, member).Generation()) || anchorHistoryCursor(t, member) != cursor || !sameAnchorLocal(local, anchorLocalTestRead(t, member)) {
		t.Fatal("a refused abandonment changed the member")
	}
	if _, err := member.ReadQuarantined(t.Context(), installed); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a refused abandonment quarantined the member's branch", err)
	}
}

// The old primary recorded a REVOKE that its promoted follower never pulled,
// so it refuses the promoted branch. The promoted primary is then lost for
// good. The audited abandonment makes the old primary the writer of a new
// term on its own branch, which still revokes the key; it publishes completely,
// keeps that across a restart, continues the history as the writer, and the
// lost primary, if it ever returns, adopts the revocation instead of losing it.
func TestAnchorRecheckStrandedMemberAbandonsTheLostTerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	old, root := anchorHistoryPrimary(t, path)
	lost := anchorHistoryMember(t, old)
	revoked := anchorHistoryPoll(t, old, root, true)
	key, ds := root.firstID(t, revoked), anchorRecheckDS(root.first)
	promoted := anchorHistoryPromote(t, lost, "abandon-lost-term")
	anchorHistoryPoll(t, lost, root, false)
	fenced := anchorHistoryObserve(t, old, promoted.Term)
	anchorHistoryRefused(t, old, lost)
	kept := imageOf(t, old).Generation()
	if kept.Term.Epoch >= fenced.Term.Epoch {
		t.Fatal("fixture member holds an image of its current term")
	}

	// Without a named abandonment, a resolution still refuses this member.
	ctx, chosen, req := anchorAbandonRequest(t, old, "abandon-unnamed")
	req.AbandonTerm = nil
	anchorAbandonRefused(t, old, replication.ErrCAS, func() error {
		_, err := old.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, anchorAbandonAudit(t))
		return err
	})
	// A term that is not the exact one that fenced the member is refused.
	other := fenced.Term
	other.PromotionID = uuid.NewString()
	ctx, chosen, req = anchorAbandonRequest(t, old, "abandon-other-term")
	req.AbandonTerm = &other
	anchorAbandonRefused(t, old, replication.ErrCAS, func() error {
		_, err := old.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, anchorAbandonAudit(t))
		return err
	})
	// The abandonment keeps only the member's own installed branch.
	ctx, _, req = anchorAbandonRequest(t, old, "abandon-remote-branch")
	remote, err := old.ValidateImage(ctx, *imageOf(t, lost))
	if err != nil {
		t.Fatal(err)
	}
	anchorAbandonRefused(t, old, replication.ErrCAS, func() error {
		_, err := old.ResolveAudited(ctx, remote, req, func(context.Context) error { return nil }, anchorAbandonAudit(t))
		return err
	})

	ctx, chosen, req = anchorAbandonRequest(t, old, "abandon-promoted-term")
	resolved, err := old.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, anchorAbandonAudit(t))
	if err != nil {
		t.Fatal("the stranded member cannot abandon the lost term", err)
	}
	if !resolved.Writable() || resolved.Mode != replication.Primary || resolved.Term.Epoch != fenced.Term.Epoch+1 || resolved.Term.PrimaryID != fenced.MemberID ||
		resolved.Parent == nil || !resolved.Parent.Same(kept) || resolved.Conflict != nil || resolved.Divergence != nil {
		t.Fatal("abandonment did not allocate a new term over the member's own branch", resolved.Mode)
	}
	if anchorHistoryCursor(t, old).owner.Version() != revoked.Version() {
		t.Fatal("abandonment changed the member's owner")
	}
	anchorRecheckHolds(t, old, key, ds)
	if _, err := old.ReadQuarantined(t.Context(), kept); err != nil {
		t.Fatal("the member's prior image was not retained as private quarantine", err)
	}
	audit, err := old.ReadAudit(t.Context(), AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range audit {
		if entry.Action == "cluster.resolve" && entry.RequestID == "abandon-promoted-term" && entry.Actor == "host:offline-cli" && entry.Authz == anchorAbandonAudit(t).Authz {
			n++
		}
	}
	if n != 1 {
		t.Fatal("abandonment audit count", n)
	}
	// The same decision replays as the committed result, without a second audit.
	ctx = captureReplica(t, old)
	again, err := old.ResolveAudited(ctx, chosen, replication.ResolveRequest{ExpectedTerm: resolved.Term, ExpectedInstalled: ctx.Value(anchorInstallKey{}).(*anchorInstall).installed,
		AbandonTerm: req.AbandonTerm, RequestID: "abandon-promoted-term"}, func(context.Context) error { return nil }, anchorAbandonAudit(t))
	if err != nil || !reflect.DeepEqual(again, resolved) {
		t.Fatal("a duplicate abandonment changed the result", err)
	}
	// The member publishes completely as the writer, also after a restart.
	writer := anchorTestContext(t.Context(), t, old)
	anchorPublicationTestRead(writer, t, anchorPublicationTestPublish(writer, t, old))
	version, local := anchorHistoryCursor(t, old).owner.Version(), anchorLocalTestRead(t, old)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	old = openAt(t, path, internalOptions)
	if _, err := old.BindAnchorLifecycle(anchorTestContext(t.Context(), t, old)); err != nil {
		t.Fatal("restart refused the new writer's committed cut", err)
	}
	if anchorHistoryCursor(t, old).owner.Version() != version || !sameAnchorLocal(local, anchorLocalTestRead(t, old)) {
		t.Fatal("restart changed the member's owner or local carrier")
	}
	if a := old.Authority(); !a.Writable() || !a.Term.Same(resolved.Term) {
		t.Fatal("restart lost the new term", a.Mode)
	}
	anchorRecheckHolds(t, old, key, ds)
	writer = anchorTestContext(t.Context(), t, old)
	anchorPublicationTestRead(writer, t, anchorPublicationTestPublish(writer, t, old))
	// It continues the history as the writer of the new term.
	next := anchorHistoryPoll(t, old, root, false)
	if next.Version().Generation != revoked.Version().Generation+1 {
		t.Fatal("the new writer did not continue its own history")
	}
	anchorRecheckHolds(t, old, key, ds)

	// The lost primary returns after all. It is fenced by the new term and
	// adopts the kept branch, revocation included, quarantining its own.
	lostImage := imageOf(t, lost).Generation()
	anchorHistoryObserve(t, lost, resolved.Term)
	if err := anchorHistoryInstall(t, lost, old); err != nil {
		t.Fatal("the returning primary cannot adopt the kept branch", err)
	}
	anchorHistoryAdopted(t, lost, old, []string{key})
	anchorRecheckHolds(t, lost, key, ds)
	if a := lost.Authority(); a.Mode != replication.Follower || !a.Term.Same(resolved.Term) {
		t.Fatal("the returning primary does not follow the new term", a.Mode)
	}
	if _, err := lost.ReadQuarantined(t.Context(), lostImage); err != nil {
		t.Fatal("the abandoned term's branch was not quarantined", err)
	}
}

// The abandonment applies only to a fenced member that holds no image of its
// current term and records no conflict or divergence. Every other role
// refuses it with nothing changed.
func TestAnchorRecheckAbandonmentRefusesEveryOtherRole(t *testing.T) {
	t.Run("follower", func(t *testing.T) {
		origin, _ := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
		member := anchorHistoryMember(t, origin)
		ctx, chosen, req := anchorAbandonRequest(t, member, "abandon-follower")
		anchorAbandonRefused(t, member, replication.ErrCAS, func() error {
			_, err := member.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, anchorAbandonAudit(t))
			return err
		})
	})
	t.Run("primary", func(t *testing.T) {
		origin, _ := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
		ctx, chosen, req := anchorAbandonRequest(t, origin, "abandon-primary")
		anchorAbandonRefused(t, origin, replication.ErrCAS, func() error {
			_, err := origin.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, anchorAbandonAudit(t))
			return err
		})
	})
	t.Run("conflict", func(t *testing.T) {
		origin, _ := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
		first, second := anchorHistoryMember(t, origin), anchorHistoryMember(t, origin)
		a := anchorHistoryPromote(t, first, "abandon-conflict-first")
		anchorHistoryPromote(t, second, "abandon-conflict-second")
		if _, err := second.ObserveAuthority(t.Context(), a.Term); !errors.Is(err, replication.ErrPromotionConflict) {
			t.Fatal("fixture did not record the promotion conflict", err)
		}
		ctx, chosen, req := anchorAbandonRequest(t, second, "abandon-conflict")
		anchorAbandonRefused(t, second, replication.ErrCAS, func() error {
			_, err := second.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, anchorAbandonAudit(t))
			return err
		})
	})
	t.Run("fenced-after-installing-its-term", func(t *testing.T) {
		origin, _ := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
		old := origin
		successor := anchorHistoryMember(t, old)
		promoted := anchorHistoryPromote(t, successor, "abandon-installed")
		anchorHistoryObserve(t, old, promoted.Term)
		if err := anchorHistoryInstall(t, old, successor); err != nil {
			t.Fatal(err)
		}
		if a := old.Authority(); a.Mode != replication.Follower {
			t.Fatal("fixture old primary does not follow", a.Mode)
		}
		ctx, chosen, req := anchorAbandonRequest(t, old, "abandon-installed")
		anchorAbandonRefused(t, old, replication.ErrCAS, func() error {
			_, err := old.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, anchorAbandonAudit(t))
			return err
		})
	})
}

// A follower whose own deny-only check saw a revocation is fenced by a newer
// term and stranded there. The abandonment does not clear its denial: as the
// writer of the new term it still answers SERVFAIL and can commit no
// observation that omits the REVOKE, exactly as after a promotion, and its
// own observation of the REVOKE ends the state.
func TestAnchorRecheckAbandonmentKeepsAPendingDenial(t *testing.T) {
	origin, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
	member, successor := anchorHistoryMember(t, origin), anchorHistoryMember(t, origin)
	shared := anchorHistoryCursor(t, member).owner
	anchorRecheckDeny(t, member, root.signed(t, true))
	key, ds := root.firstID(t, shared), anchorRecheckDS(root.first)
	anchorRecheckRetired(t, member)
	promoted := anchorHistoryPromote(t, successor, "abandon-denial-successor")
	anchorHistoryPoll(t, successor, root, false)
	anchorHistoryObserve(t, member, promoted.Term)
	anchorHistoryRefused(t, member, successor)
	ctx, chosen, req := anchorAbandonRequest(t, member, "abandon-denial")
	resolved, err := member.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, anchorAbandonAudit(t))
	if err != nil || !resolved.Writable() {
		t.Fatal("the stranded member cannot abandon the lost term", err)
	}
	if anchorHistoryCursor(t, member).owner.Version() != shared.Version() || !slices.Contains(anchorHistoryDeniedIDs(anchorLocalTestRead(t, member)), key) {
		t.Fatal("abandonment changed the owner or cleared the denial")
	}
	anchorRecheckRetired(t, member)
	anchorHistoryPoll(t, member, root, true)
	anchorRecheckHolds(t, member, key, ds)
	writer := anchorTestContext(t.Context(), t, member)
	anchorPublicationTestRead(writer, t, anchorPublicationTestPublish(writer, t, member))
}
