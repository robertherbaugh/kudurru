package sqlite

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// These tests build divergent managed owner branches the way a cluster does:
// two real member databases share one accepted owner, each then commits its own
// authenticated observations as the elected writer of its own term, and one of
// them later installs the other's image. Every owner is produced by the pure
// state machine from signed DNSKEY responses; only the same-term refusal tests
// re-seal an image, and they say so.

// anchorHistoryRoot is one signed root DNSKEY set with two seed keys, so that
// revoking the first leaves a usable owner.
type anchorHistoryRoot struct{ first, second anchorTestProof }

func (r anchorHistoryRoot) signed(t *testing.T, revokeFirst bool) *dns.Msg {
	t.Helper()
	return anchorPollTestSignedPair(t, r.first, r.second, revokeFirst)
}

func (r anchorHistoryRoot) firstID(t *testing.T, owner accepted.Snapshot) string {
	t.Helper()
	state, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	want := dns.Copy(r.first.key).(*dns.DNSKEY)
	want.Hdr.Ttl = 0
	for _, key := range state.Inspect().Keys {
		rr, err := dns.NewRR(key.DNSKEY)
		if err != nil {
			t.Fatal(err)
		}
		got := rr.(*dns.DNSKEY)
		if got.PublicKey == want.PublicKey {
			return key.ID
		}
	}
	t.Fatal("fixture owner does not contain the first seed key")
	return ""
}

// anchorHistoryPrimary is a real primary whose managed owner was bootstrapped
// from both seeds at envelope generation 2.
func anchorHistoryPrimary(t *testing.T, path string) (*Store, anchorHistoryRoot) {
	t.Helper()
	s := openAt(t, path, internalOptions)
	_, dormant := anchorLocalTestDormant(t, s)
	ctx := anchorTestContext(t.Context(), t, s)
	bootstrap, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	root := anchorHistoryRoot{first: anchorTestBootstrap(t, s, dormant), second: anchorTestBootstrap(t, s, dormant)}
	seed := trustanchors.Seed{ClusterID: s.Authority().Term.ClusterID, PolicyID: "explicit-test-root", DS: []string{root.first.key.ToDS(dns.SHA256).String(), root.second.key.ToDS(dns.SHA256).String()}}
	state, original, err := trustanchors.Bootstrap(seed, root.signed(t, false), root.first.clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	managed, err := accepted.ProposeManaged(dormant, state, state.Inspect().Checkpoint.Wall, original.ValidUntil())
	if err != nil {
		t.Fatal(err)
	}
	completion, err := s.AnchorCommand(bootstrap, func(ctx context.Context, tx *CommandTx) error {
		if err := original.CheckBootstrap(state, root.first.clock.Sample()); err != nil {
			return err
		}
		return anchorTestStage(ctx, tx, managed)
	})
	if err != nil || completion.Outcome() != AnchorCommitted {
		t.Fatal("two-seed bootstrap did not commit", err)
	}
	if _, err := s.EnablePrimary(t.Context(), imageOf(t, s).Generation()); err != nil {
		t.Fatal(err)
	}
	return s, root
}

func anchorHistoryCursor(t *testing.T, s *Store) *anchorCursor {
	t.Helper()
	source := s.anchors.Load()
	if source == nil || source.closed.Load() || s.anchorBlocked.Load() {
		t.Fatal("anchor admission closed or unbound")
	}
	return source.cursor.Load()
}

// anchorHistoryMember is a second member database with the source's current
// image installed.
func anchorHistoryMember(t *testing.T, source *Store) *Store {
	t.Helper()
	member, _ := anchorLocalTestFollower(t, source)
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, member, source); err != nil {
		t.Fatal(err)
	}
	return member
}

func anchorHistoryPromote(t *testing.T, s *Store, request string) replication.Authority {
	t.Helper()
	a := s.Authority()
	promoted, err := s.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: a.Term, ExpectedInstalled: imageOf(t, s).Generation(), RequestID: request})
	if err != nil || !promoted.Writable() || promoted.Term.Epoch <= a.Term.Epoch {
		t.Fatal("promotion", err)
	}
	return promoted
}

// anchorHistoryPoll commits one authenticated observation as the elected
// writer, in a new clock session as every writer run uses. revoke serves the
// first key with its self-signed REVOKE bit, which takes the two-phase lane.
func anchorHistoryPoll(t *testing.T, s *Store, root anchorHistoryRoot, revoke bool) accepted.Snapshot {
	t.Helper()
	return anchorHistoryPollSigned(t, s, root.signed(t, revoke), revoke)
}

// anchorHistoryPollSigned is anchorHistoryPoll for any signed root DNSKEY
// response. revoke says that the response carries a self-signed REVOKE of a key
// the writer knows, which takes the two-phase lane.
func anchorHistoryPollSigned(t *testing.T, s *Store, signed *dns.Msg, revoke bool) accepted.Snapshot {
	t.Helper()
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	current := anchorHistoryCursor(t, s).owner
	state, err := current.State()
	if err != nil {
		t.Fatal(err)
	}
	stage := func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
		next, err := trustanchors.Advance(state, observation, clock.Sample())
		if err != nil {
			return err
		}
		owner, err := accepted.ProposeManaged(current, next, next.Inspect().Checkpoint.Wall, observation.ValidUntil())
		if err != nil {
			return err
		}
		return anchorTestStage(ctx, tx, owner)
	}
	ctx := anchorTestContext(t.Context(), t, s)
	var receipt AnchorCommit
	if revoke {
		denial, update, err := s.CaptureAnchorRevocation(ctx, current.Version(), clock)
		if err != nil {
			t.Fatal(err)
		}
		verified, err := denial.Verify(signed)
		if err != nil {
			t.Fatal(err)
		}
		_, own, err := s.CommitAnchorDenial(verified)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err = s.AnchorCommandAfterDenial(update, own, stage)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		update, err := s.CaptureAnchorUpdate(ctx, current.Version())
		if err != nil {
			t.Fatal(err)
		}
		observation, err := trustanchors.Verify(state, signed, clock.Sample())
		if err != nil {
			t.Fatal(err)
		}
		receipt, err = s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error { return stage(ctx, tx, observation) })
		if err != nil {
			t.Fatal(err)
		}
	}
	next := anchorHistoryCursor(t, s).owner
	if version, ok := receipt.Version(); receipt.Outcome() != AnchorCommitted || !ok || version != next.Version() || version.Generation != current.Version().Generation+1 {
		t.Fatal("writer observation did not commit exactly one successor")
	}
	return next
}

func anchorHistoryObserve(t *testing.T, s *Store, term replication.Term) replication.Authority {
	t.Helper()
	if _, err := s.ObserveAuthority(t.Context(), term); err != nil {
		t.Fatal(err)
	}
	a := s.Authority()
	if a.Mode != replication.Fenced || !a.Term.Same(term) || a.Conflict != nil || a.Divergence != nil {
		t.Fatal("member was not fenced by the higher term", a.Mode)
	}
	return a
}

func anchorHistoryInstall(t *testing.T, member, source *Store) error {
	t.Helper()
	return anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, member, source)
}

func anchorHistoryDeniedIDs(local *anchorLocal) []string {
	ids := []string{}
	for _, denied := range local.record.Denied {
		ids = append(ids, denied.ID)
	}
	return ids
}

// anchorHistoryAdopted checks the complete committed and published cut of a
// member that adopted the canonical owner. denied lists the keys the carrier
// keeps; the adopted owner revokes every one of them (D369), so the member
// publishes the adopted generation at once.
func anchorHistoryAdopted(t *testing.T, member, canonical *Store, denied []string) {
	t.Helper()
	cursor, want := anchorHistoryCursor(t, member), anchorHistoryCursor(t, canonical).owner.Version()
	if cursor.owner.Version() != want {
		t.Fatalf("member owner generation %d, canonical generation %d: the canonical owner was not adopted", cursor.owner.Version().Generation, want.Generation)
	}
	durable := anchorLocalTestRead(t, member)
	if !sameAnchorLocal(cursor.local, durable) {
		t.Fatal("published local carrier is not the committed one")
	}
	record := durable.record
	if record.Phase != "managed" || record.Owner != want || record.Member != member.Authority().MemberID || record.Terminal {
		t.Fatal("local carrier is not bound to the adopted owner")
	}
	if record.RetiredThrough != want.Generation-1 {
		t.Fatalf("local floor %d, want %d for adopted generation %d", record.RetiredThrough, want.Generation-1, want.Generation)
	}
	if got := anchorHistoryDeniedIDs(durable); !slices.Equal(got, denied) {
		t.Fatalf("local denial %v, want %v", got, denied)
	}
	for _, entry := range record.Denied {
		if entry.FirstOwner.Generation > want.Generation {
			t.Fatal("a denial receipt names a generation above the adopted owner")
		}
	}
	if err := checkAnchorLocal(durable, cursor, member.Authority().MemberID); err != nil {
		t.Fatal("adopted cut does not pass the restart check", err)
	}
	if !cursor.gate.MatchesInstalled(cursor.mark) {
		t.Fatal("retirement gate disagrees with the committed local floor")
	}
	ctx := anchorLocalTestFinite(t.Context(), t)
	_, token, err := member.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := member.CaptureAnchorPublication(ctx, token)
	if err != nil {
		t.Fatal("adopted owner cannot publish", err)
	}
	if err := member.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: publication}, publication.Apply); err != nil {
		t.Fatal(err)
	}
	anchorPublicationTestRead(ctx, t, publication.Input())
}

func anchorHistoryProjection(t *testing.T, s *Store) resolverstate.Projection {
	t.Helper()
	projection, err := resolverstate.Project(anchorHistoryCursor(t, s).owner)
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

// The predicted defect from the F28-HA-2 map entry. An old primary commits one
// observation its follower never pulls, the follower is promoted, and the old
// primary must install the promoted branch although that branch's owner
// generation is lower than, or equal to, its own.
func TestAnchorHistoryRejoiningPrimaryAdoptsPromotedBranch(t *testing.T) {
	for _, branch := range []string{"lower-generation", "equal-generation"} {
		t.Run(branch, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			old, root := anchorHistoryPrimary(t, path)
			successor := anchorHistoryMember(t, old)
			shared := anchorHistoryCursor(t, old).owner.Version()
			// A registration and a captured publication attempt of the shared
			// owner. The lower-generation branch installs that exact owner again
			// later, and neither may come back to life with it.
			earlier := anchorPublicationTestPublish(anchorTestContext(t.Context(), t, old), t, old)
			staleCtx := anchorTestContext(t.Context(), t, old)
			staleToken, stale := anchorPublicationTestCapture(staleCtx, t, old)
			ahead := anchorHistoryPoll(t, old, root, false).Version()
			writer := anchorTestContext(t.Context(), t, old)
			input := anchorPublicationTestPublish(writer, t, old)
			anchorPublicationTestRead(writer, t, input)
			promoted := anchorHistoryPromote(t, successor, "history-"+branch)
			if branch == "equal-generation" {
				other := anchorHistoryPoll(t, successor, root, false).Version()
				if other.Generation != ahead.Generation || other == ahead {
					t.Fatal("fixture branches did not diverge at one generation")
				}
			} else if anchorHistoryCursor(t, successor).owner.Version() != shared {
				t.Fatal("fixture successor left the shared owner")
			}
			canonical := anchorHistoryCursor(t, successor).owner.Version()
			if canonical.Generation > ahead.Generation {
				t.Fatal("fixture does not exercise a non-increasing generation")
			}
			anchorHistoryObserve(t, old, promoted.Term)
			lost := imageOf(t, old)
			revision := anchorLocalTestRead(t, old).record.Revision
			if err := anchorHistoryInstall(t, old, successor); err != nil {
				t.Fatalf("old primary at generation %d cannot install the promoted branch at generation %d: %v", ahead.Generation, canonical.Generation, err)
			}
			a := old.Authority()
			if a.Mode != replication.Follower || !a.Term.Same(promoted.Term) || !imageOf(t, old).Generation().Same(imageOf(t, successor).Generation()) {
				t.Fatal("old primary did not become a follower of the promoted term", a.Mode)
			}
			if input.Current() {
				t.Fatal("resolver input of the abandoned branch is still current")
			}
			if _, err := input.CaptureRead(anchorLocalTestFinite(t.Context(), t)); err == nil {
				t.Fatal("a registration of the abandoned branch admitted a new read")
			}
			if earlier.Current() {
				t.Fatal("an earlier registration is current again under the adopted owner")
			}
			if _, err := earlier.CaptureRead(anchorLocalTestFinite(t.Context(), t)); err == nil {
				t.Fatal("an earlier registration admitted a new read under the adopted owner")
			}
			// In the lower-generation branch the gate holds the stale attempt's
			// exact mark again. The gate still refuses it, because it counts
			// rebases; the source's cursor identity would refuse it next.
			if err := old.PublishIfCurrent(staleCtx, staleToken, PublicationOptions{Anchor: stale}, stale.Apply); !errors.Is(err, retirement.ErrMismatch) {
				t.Fatalf("a publication attempt captured before the split: got %v, want %v", err, retirement.ErrMismatch)
			}
			if stale.Input().Current() {
				t.Fatal("a stale publication attempt left current input")
			}
			anchorHistoryAdopted(t, old, successor, []string{})
			if anchorLocalTestRead(t, old).record.Revision != revision+1 {
				t.Fatal("adoption did not advance the local carrier exactly once")
			}
			quarantined, err := old.ReadQuarantined(t.Context(), lost.Generation())
			if err != nil {
				t.Fatal("the abandoned branch was not retained in quarantine", err)
			}
			if owner, err := accepted.DecodeRows(quarantined.Payload.Runtime, quarantined.Term.ClusterID); err != nil || owner.Version() != ahead {
				t.Fatal("quarantine does not hold the abandoned owner", err)
			}
			// A physical restart restores the same rebased cut and publishes it.
			durable := anchorLocalTestRead(t, old)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openAt(t, path, internalOptions)
			if _, err := reopened.BindAnchorLifecycle(anchorLocalTestFinite(t.Context(), t)); err != nil {
				t.Fatal("restart refused the adopted cut", err)
			}
			if !sameAnchorLocal(durable, anchorLocalTestRead(t, reopened)) {
				t.Fatal("restart changed the local carrier")
			}
			anchorHistoryAdopted(t, reopened, successor, []string{})
			// The canonical branch then moves on inside its own term: an ordinary
			// strict successor install.
			next := anchorHistoryPoll(t, successor, root, false).Version()
			if err := anchorHistoryInstall(t, reopened, successor); err != nil {
				t.Fatal("same-term successor after adoption", err)
			}
			if anchorHistoryCursor(t, reopened).owner.Version() != next {
				t.Fatal("rejoined member did not follow the canonical successor")
			}
			anchorHistoryAdopted(t, reopened, successor, []string{})
		})
	}
}

// Two members are promoted from one image into the same epoch and both poll.
// The operator keeps the first branch with the audited resolution (D361); the
// losing member holds the higher owner generation and must still adopt the
// kept branch through an ordinary install under the new term.
func TestAnchorHistoryLosingBranchAdoptsAuditedResolution(t *testing.T) {
	origin, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
	kept, losing := anchorHistoryMember(t, origin), anchorHistoryMember(t, origin)
	first, second := anchorHistoryPromote(t, kept, "history-kept"), anchorHistoryPromote(t, losing, "history-losing")
	if first.Term.Epoch != second.Term.Epoch || first.Term.Same(second.Term) {
		t.Fatal("fixture promotions are not one equal-epoch conflict")
	}
	anchorHistoryPoll(t, kept, root, false)
	anchorHistoryPoll(t, losing, root, false)
	higher := anchorHistoryPoll(t, losing, root, false).Version()
	for _, m := range []struct {
		s     *Store
		other replication.Term
	}{{kept, second.Term}, {losing, first.Term}} {
		if _, err := m.s.ObserveAuthority(t.Context(), m.other); !errors.Is(err, replication.ErrPromotionConflict) {
			t.Fatal("fixture did not record the promotion conflict", err)
		}
	}
	// A conflicted member installs nothing.
	if err := anchorHistoryInstall(t, losing, kept); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("conflicted member installed the other branch without a resolution", err)
	}
	before := anchorHistoryCursor(t, kept)
	ctx, chosen, req := resolutionInputs(t, kept)
	checks := 0
	resolved, err := kept.ResolveAudited(ctx, chosen, req, func(context.Context) error { checks++; return nil }, AuditEntry{Actor: "host:offline-cli", Authz: "host-local data directory and member identity", Reason: "keep the first branch"})
	if err != nil || !resolved.Writable() || resolved.Term.Epoch != first.Term.Epoch+1 || checks < 2 {
		t.Fatal("audited resolution", err)
	}
	if anchorHistoryCursor(t, kept) != before {
		t.Fatal("keeping the local branch changed its anchor cut")
	}
	canonical := before.owner.Version()
	if canonical.Generation >= higher.Generation {
		t.Fatal("fixture losing branch does not hold the higher generation")
	}
	anchorHistoryObserve(t, losing, resolved.Term)
	lost := imageOf(t, losing)
	if err := anchorHistoryInstall(t, losing, kept); err != nil {
		t.Fatalf("losing branch at generation %d cannot adopt the resolved branch at generation %d: %v", higher.Generation, canonical.Generation, err)
	}
	if a := losing.Authority(); a.Mode != replication.Follower || !a.Term.Same(resolved.Term) {
		t.Fatal("losing member did not follow the resolved term", a.Mode)
	}
	anchorHistoryAdopted(t, losing, kept, []string{})
	if _, err := losing.ReadQuarantined(t.Context(), lost.Generation()); err != nil {
		t.Fatal("the losing branch was not retained in quarantine", err)
	}
}

// The Store resolution API can also keep the other member's branch. That is
// the same adoption, committed with the new epoch in one transaction.
func TestAnchorHistoryResolutionKeepingRemoteBranchAdopts(t *testing.T) {
	origin, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
	remote, local := anchorHistoryMember(t, origin), anchorHistoryMember(t, origin)
	first, second := anchorHistoryPromote(t, remote, "history-remote"), anchorHistoryPromote(t, local, "history-local")
	anchorHistoryPoll(t, remote, root, false)
	anchorHistoryPoll(t, local, root, false)
	higher := anchorHistoryPoll(t, local, root, false).Version()
	if _, err := local.ObserveAuthority(t.Context(), first.Term); !errors.Is(err, replication.ErrPromotionConflict) {
		t.Fatal("fixture did not record the promotion conflict", err)
	}
	ctx := captureReplica(t, local)
	image := imageOf(t, remote)
	chosen, err := local.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	a := local.Authority()
	resolved, err := local.ResolveConflict(ctx, chosen, replication.ResolveRequest{ExpectedTerm: a.Term, ExpectedInstalled: imageOf(t, local).Generation(), ExpectedConflict: a.Conflict, RequestID: "history-keep-remote"})
	if err != nil {
		t.Fatalf("resolution cannot keep a remote branch at a lower owner generation than the local %d: %v", higher.Generation, err)
	}
	if !resolved.Writable() || resolved.Term.Epoch != second.Term.Epoch+1 || resolved.Parent == nil || !resolved.Parent.Same(image.Generation()) {
		t.Fatal("resolution did not allocate a new term over the kept branch")
	}
	anchorHistoryAdopted(t, local, remote, []string{})
	// The resolving member is the writer of the new term and continues the
	// adopted history from there.
	next := anchorHistoryPoll(t, local, root, false).Version()
	if next.Generation != anchorHistoryCursor(t, remote).owner.Version().Generation+1 {
		t.Fatal("new writer did not continue the adopted history")
	}
}

// anchorHistoryRefused installs the source's image into the member and requires
// the adoption refusal (D369): accepted.ErrCAS before any write, the member's
// cut, image, carrier and authority unchanged and nothing quarantined. The
// member keeps serving its own branch DNS-only.
// It returns the classified refusal (every refusal of an image's owner is an
// *AnchorRefusal that still matches accepted.ErrCAS).
func anchorHistoryRefused(t *testing.T, member, source *Store) *AnchorRefusal {
	t.Helper()
	before, installed, local, authority := anchorHistoryCursor(t, member), imageOf(t, member), anchorLocalTestRead(t, member), member.Authority()
	err := anchorHistoryInstall(t, member, source)
	if !errors.Is(err, accepted.ErrCAS) {
		t.Fatalf("adoption of a branch this member must refuse: got %v, want %v", err, accepted.ErrCAS)
	}
	var refusal *AnchorRefusal
	if !errors.As(err, &refusal) || refusal.Reason == "" {
		t.Fatalf("refusal of an image's owner is not classified: %v", err)
	}
	if anchorHistoryCursor(t, member) != before || !installed.Generation().Same(imageOf(t, member).Generation()) || !sameAnchorLocal(local, anchorLocalTestRead(t, member)) || anchorAuthority(authority) != anchorAuthority(member.Authority()) {
		t.Fatal("refused adoption changed the member")
	}
	if _, err := member.ReadQuarantined(t.Context(), installed.Generation()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("refused adoption quarantined the member's own branch", err)
	}
	return refusal
}

// A member never loses a revocation by adopting a branch that has not seen it.
// It refuses that branch, exactly as an install inside one term refuses an image
// that omits a pending denial, and keeps its own branch, which already revoked
// the key. Once the canonical writer records the same tombstone, the next
// install adopts the branch and the member publishes it at once.
func TestAnchorHistoryAdoptionKeepsLocalRevocationDenied(t *testing.T) {
	t.Run("revoked-by-old-primary", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		old, root := anchorHistoryPrimary(t, path)
		successor := anchorHistoryMember(t, old)
		revoked := anchorHistoryPoll(t, old, root, true)
		key := root.firstID(t, revoked)
		if p := anchorHistoryProjection(t, old); p.Disposition() != resolverstate.Usable || !slices.Equal(p.RevokedIDs(), []string{key}) || len(p.DS()) != 1 {
			t.Fatal("fixture old primary did not revoke exactly the first key")
		}
		promoted := anchorHistoryPromote(t, successor, "history-revoked")
		if p := anchorHistoryProjection(t, successor); len(p.RevokedIDs()) != 0 || len(p.DS()) != 2 {
			t.Fatal("fixture successor already saw the revocation")
		}
		anchorHistoryObserve(t, old, promoted.Term)
		lost := imageOf(t, old)
		// The promoted branch still trusts the key: refused, nothing changes.
		anchorHistoryRefused(t, old, successor)
		// The refusal survives a restart; the member's own branch still binds.
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		member := openAt(t, path, internalOptions)
		if _, err := member.BindAnchorLifecycle(anchorLocalTestFinite(t.Context(), t)); err != nil {
			t.Fatal("restart refused the member's own cut", err)
		}
		if anchorHistoryCursor(t, member).owner.Version() != revoked.Version() {
			t.Fatal("restart did not keep the member's own branch")
		}
		// A canonical successor that still trusts the key is refused as well.
		anchorHistoryPoll(t, successor, root, false)
		anchorHistoryRefused(t, member, successor)
		// The canonical writer observes the same signed REVOKE. Only then does
		// the member adopt the branch, without the revoked key.
		tombstone := anchorHistoryPoll(t, successor, root, true)
		if err := anchorHistoryInstall(t, member, successor); err != nil {
			t.Fatal("canonical tombstone was not adopted", err)
		}
		anchorHistoryAdopted(t, member, successor, []string{key})
		if p := anchorHistoryProjection(t, member); !slices.Equal(p.RevokedIDs(), []string{key}) || len(p.DS()) != 1 || anchorHistoryCursor(t, member).owner.Version() != tombstone.Version() {
			t.Fatal("member adopted an owner with the revoked key")
		}
		if a := member.Authority(); a.Mode != replication.Follower || !a.Term.Same(promoted.Term) {
			t.Fatal("member did not follow the promoted term", a.Mode)
		}
		quarantined, err := member.ReadQuarantined(t.Context(), lost.Generation())
		if err != nil {
			t.Fatal("the abandoned branch was not retained in quarantine", err)
		}
		if owner, err := accepted.DecodeRows(quarantined.Payload.Runtime, quarantined.Term.ClusterID); err != nil || owner.Version() != revoked.Version() {
			t.Fatal("quarantine does not hold the abandoned owner", err)
		}
	})
	t.Run("denied-by-follower", func(t *testing.T) {
		origin, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
		successor, member := anchorHistoryMember(t, origin), anchorHistoryMember(t, origin)
		// The follower's own deny-only observation of the signed REVOKE.
		shared := anchorHistoryCursor(t, member).owner
		clock, err := trustanchors.NewSessionClock()
		if err != nil {
			t.Fatal(err)
		}
		denial, err := member.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), shared.Version(), clock)
		if err != nil {
			t.Fatal(err)
		}
		verified, err := denial.Verify(root.signed(t, true))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := member.CommitAnchorDenial(verified); err != nil {
			t.Fatal(err)
		}
		key := root.firstID(t, shared)
		receipt := anchorLocalTestRead(t, member).record.Denied
		promoted := anchorHistoryPromote(t, successor, "history-follower-denial")
		anchorHistoryPoll(t, successor, root, false)
		anchorHistoryObserve(t, member, promoted.Term)
		anchorHistoryRefused(t, member, successor)
		anchorHistoryPoll(t, successor, root, true)
		if err := anchorHistoryInstall(t, member, successor); err != nil {
			t.Fatal("canonical tombstone was not adopted", err)
		}
		anchorHistoryAdopted(t, member, successor, []string{key})
		if !slices.Equal(anchorLocalTestRead(t, member).record.Denied, receipt) {
			t.Fatal("adoption replaced a first denial receipt it could keep")
		}
	})
	t.Run("resolution-keeping-a-branch-that-trusts-it", func(t *testing.T) {
		// The resolution API refuses to keep a remote branch that still trusts
		// a key the resolving member revoked; the conflict stays as it was.
		origin, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
		remote, local := anchorHistoryMember(t, origin), anchorHistoryMember(t, origin)
		first := anchorHistoryPromote(t, remote, "history-remote-trusts")
		anchorHistoryPromote(t, local, "history-local-revokes")
		anchorHistoryPoll(t, remote, root, false)
		anchorHistoryPoll(t, local, root, true)
		if _, err := local.ObserveAuthority(t.Context(), first.Term); !errors.Is(err, replication.ErrPromotionConflict) {
			t.Fatal("fixture did not record the promotion conflict", err)
		}
		ctx := captureReplica(t, local)
		chosen, err := local.ValidateImage(ctx, *imageOf(t, remote))
		if err != nil {
			t.Fatal(err)
		}
		before, installed, carrier, a := anchorHistoryCursor(t, local), imageOf(t, local), anchorLocalTestRead(t, local), local.Authority()
		if _, err := local.ResolveConflict(ctx, chosen, replication.ResolveRequest{ExpectedTerm: a.Term, ExpectedInstalled: installed.Generation(), ExpectedConflict: a.Conflict, RequestID: "history-keep-trusting"}); !errors.Is(err, accepted.ErrCAS) {
			t.Fatalf("resolution kept a branch that trusts a revoked key: got %v, want %v", err, accepted.ErrCAS)
		}
		if anchorHistoryCursor(t, local) != before || !installed.Generation().Same(imageOf(t, local).Generation()) || !sameAnchorLocal(carrier, anchorLocalTestRead(t, local)) || anchorAuthority(a) != anchorAuthority(local.Authority()) {
			t.Fatal("refused resolution changed the member")
		}
	})
	t.Run("revoked-by-canonical-branch", func(t *testing.T) {
		old, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
		successor := anchorHistoryMember(t, old)
		anchorHistoryPoll(t, old, root, false)
		promoted := anchorHistoryPromote(t, successor, "history-canonical-revoke")
		tombstone := anchorHistoryPoll(t, successor, root, true)
		key := root.firstID(t, tombstone)
		anchorHistoryObserve(t, old, promoted.Term)
		if err := anchorHistoryInstall(t, old, successor); err != nil {
			t.Fatal("old primary cannot install a promoted branch that revoked a key", err)
		}
		anchorHistoryAdopted(t, old, successor, []string{key})
		if p := anchorHistoryProjection(t, old); !slices.Equal(p.RevokedIDs(), []string{key}) || len(p.DS()) != 1 {
			t.Fatal("adopted owner lost the canonical tombstone")
		}
	})
}

// anchorHistoryReseal returns the image with its anchor owner rows replaced
// and a new revision. It models an image no honest writer can produce, to pin
// the refusals that do not depend on how the image came to exist.
func anchorHistoryReseal(t *testing.T, image *replication.Image, owner accepted.Snapshot, revision uint64) *replication.Image {
	t.Helper()
	rows, err := accepted.EncodeRows(owner)
	if err != nil {
		t.Fatal(err)
	}
	forged := *image
	forged.Payload.Runtime = nil
	for _, entry := range image.Payload.Runtime {
		if !anchorKey(accepted.Section, entry.Key) {
			forged.Payload.Runtime = append(forged.Payload.Runtime, entry)
		}
	}
	forged.Payload.Runtime = append(forged.Payload.Runtime, rows...)
	forged.Revision = revision
	if err := replication.Seal(&forged); err != nil {
		t.Fatal(err)
	}
	return &forged
}

func anchorHistoryInstallImage(t *testing.T, member *Store, image *replication.Image) error {
	t.Helper()
	captured, err := member.CaptureAnchorInstall(anchorLocalTestFinite(t.Context(), t))
	if err != nil {
		return err
	}
	validated, err := member.ValidateImage(captured, *image)
	if err != nil {
		return err
	}
	_, err = member.InstallReplica(captured, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: captured.Value(anchorInstallKey{}).(*anchorInstall).installed})
	return err
}

// Inside one term the strict successor rule is unchanged: a different owner
// whose generation is lower than or equal to the installed one is refused, on
// an ordinary follower and on a member that adopted a branch earlier.
func TestAnchorHistorySameTermInstallStaysStrict(t *testing.T) {
	primary, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
	base := anchorHistoryCursor(t, primary).owner
	member := anchorHistoryMember(t, primary)
	// A sibling of the next owner: the same authenticated transition from the
	// same base, proposed in another clock session and never committed anywhere.
	state, err := base.State()
	if err != nil {
		t.Fatal(err)
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := trustanchors.Verify(state, root.signed(t, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := trustanchors.Advance(state, observation, clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := accepted.ProposeManaged(base, advanced, advanced.Inspect().Checkpoint.Wall, observation.ValidUntil())
	if err != nil {
		t.Fatal(err)
	}
	next := anchorHistoryPoll(t, primary, root, false)
	if err := anchorHistoryInstall(t, member, primary); err != nil {
		t.Fatal(err)
	}
	if sibling.Version().Generation != next.Version().Generation || sibling.Version() == next.Version() {
		t.Fatal("fixture sibling is not a different owner at the installed generation")
	}
	refuse := func(t *testing.T, s *Store, owner accepted.Snapshot) {
		t.Helper()
		before, installed, local := anchorHistoryCursor(t, s), imageOf(t, s), anchorLocalTestRead(t, s)
		forged := anchorHistoryReseal(t, installed, owner, installed.Revision+1)
		if !forged.Term.Same(installed.Term) {
			t.Fatal("fixture image left the installed term")
		}
		if err := anchorHistoryInstallImage(t, s, forged); !errors.Is(err, accepted.ErrCAS) {
			t.Fatalf("same-term install accepted owner generation %d over installed generation %d: %v", owner.Version().Generation, before.owner.Version().Generation, err)
		}
		if anchorHistoryCursor(t, s) != before || !installed.Generation().Same(imageOf(t, s).Generation()) || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) {
			t.Fatal("refused same-term image changed the member")
		}
	}
	t.Run("follower-lower", func(t *testing.T) { refuse(t, member, base) })
	t.Run("follower-equal", func(t *testing.T) { refuse(t, member, sibling) })
	// The same member after a real adoption under a higher term: the successor
	// is promoted at the installed owner and both writers then poll once.
	successor := anchorHistoryMember(t, primary)
	promoted := anchorHistoryPromote(t, successor, "history-strict")
	adopted := anchorHistoryPoll(t, successor, root, false)
	ahead := anchorHistoryPoll(t, primary, root, false)
	if err := anchorHistoryInstall(t, member, primary); err != nil {
		t.Fatal(err)
	}
	if adopted.Version().Generation != ahead.Version().Generation || adopted.Version() == ahead.Version() {
		t.Fatal("fixture branches did not diverge at one generation")
	}
	anchorHistoryObserve(t, member, promoted.Term)
	if err := anchorHistoryInstall(t, member, successor); err != nil {
		t.Fatal("fixture adoption", err)
	}
	anchorHistoryAdopted(t, member, successor, []string{})
	t.Run("adopted-lower", func(t *testing.T) { refuse(t, member, next) })
	t.Run("adopted-equal", func(t *testing.T) { refuse(t, member, ahead) })
}

// A higher term changes which generation wins. It does not change which
// history a member may adopt: the recovery prefix it accepted, the history,
// policy and seed identity, and managed history itself all still bind.
func TestAnchorHistoryAdoptionKeepsIdentityAndRecoveryRefusals(t *testing.T) {
	// fenced returns an old primary one observation ahead and fenced by a
	// promoted successor that still holds the shared owner.
	fenced := func(t *testing.T) (old, successor *Store, root anchorHistoryRoot, shared accepted.Snapshot) {
		t.Helper()
		old, root = anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
		successor = anchorHistoryMember(t, old)
		shared = anchorHistoryCursor(t, old).owner
		anchorHistoryPoll(t, old, root, false)
		anchorHistoryObserve(t, old, anchorHistoryPromote(t, successor, "history-refusal").Term)
		return old, successor, root, shared
	}
	refuse := func(t *testing.T, old *Store, image *replication.Image, want error) {
		t.Helper()
		before, installed, local, authority := anchorHistoryCursor(t, old), imageOf(t, old), anchorLocalTestRead(t, old), old.Authority()
		if err := anchorHistoryInstallImage(t, old, image); !errors.Is(err, want) {
			t.Fatalf("install under a higher term: got %v, want %v", err, want)
		}
		if anchorHistoryCursor(t, old) != before || !installed.Generation().Same(imageOf(t, old).Generation()) || !sameAnchorLocal(local, anchorLocalTestRead(t, old)) || anchorAuthority(authority) != anchorAuthority(old.Authority()) {
			t.Fatal("refused image changed the member")
		}
		if _, err := old.ReadQuarantined(t.Context(), installed.Generation()); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("refused image quarantined the current branch", err)
		}
	}
	dormant := func(t *testing.T, cluster, history string, shared accepted.Snapshot) accepted.Snapshot {
		t.Helper()
		owner, err := accepted.NewDormant(cluster, history, shared.Inspect().InitializedAt)
		if err != nil {
			t.Fatal(err)
		}
		return owner
	}
	t.Run("other-history", func(t *testing.T) {
		old, successor, _, shared := fenced(t)
		state, err := shared.State()
		if err != nil {
			t.Fatal(err)
		}
		root := shared.Inspect()
		foreign, err := accepted.ProposeManaged(dormant(t, root.ClusterID, strings.Repeat("d", 32), shared), state, root.UpdatedAt, root.ObservationUntil)
		if err != nil {
			t.Fatal(err)
		}
		if foreign.Inspect().HistoryID == root.HistoryID {
			t.Fatal("fixture owner did not change history")
		}
		image := imageOf(t, successor)
		refuse(t, old, anchorHistoryReseal(t, image, foreign, image.Revision), accepted.ErrCAS)
	})
	t.Run("other-seeds", func(t *testing.T) {
		old, successor, _, shared := fenced(t)
		root := shared.Inspect()
		proof := anchorTestBootstrapForCluster(t, root.ClusterID, dormant(t, root.ClusterID, root.HistoryID, shared))
		if proof.next.Inspect().SeedDigest == root.SeedDigest || proof.next.Inspect().HistoryID != root.HistoryID {
			t.Fatal("fixture owner did not change only its seeds")
		}
		image := imageOf(t, successor)
		refuse(t, old, anchorHistoryReseal(t, image, proof.next, image.Revision), accepted.ErrCAS)
	})
	t.Run("managed-to-dormant", func(t *testing.T) {
		old, successor, _, shared := fenced(t)
		root := shared.Inspect()
		image := imageOf(t, successor)
		refuse(t, old, anchorHistoryReseal(t, image, dormant(t, root.ClusterID, root.HistoryID, shared), image.Revision), accepted.ErrCAS)
	})
	t.Run("recovery-prefix", func(t *testing.T) {
		// A real recovery: the old primary loses its only anchor, and the
		// operator recovers with a new key. The successor was promoted from the
		// image before both and never accepted that recovery.
		old, _, proof := anchorLocalTestManaged(t)
		if _, err := old.EnablePrimary(t.Context(), imageOf(t, old).Generation()); err != nil {
			t.Fatal(err)
		}
		successor := anchorHistoryMember(t, old)
		_, update, verified := anchorLocalTestRevoke(t, old, proof)
		_, own, err := old.CommitAnchorDenial(verified)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.AnchorCommandAfterDenial(update, own, func(ctx context.Context, tx *CommandTx, o trustanchors.Observation) error {
			return anchorLocalTestAdvance(ctx, tx, o, proof)
		}); err != nil {
			t.Fatal(err)
		}
		quarantined := anchorHistoryCursor(t, old).owner
		lost, err := quarantined.State()
		if err != nil || !lost.Inspect().Quarantined {
			t.Fatal("fixture old primary is not quarantined", err)
		}
		key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
		private, err := key.Generate(256)
		if err != nil {
			t.Fatal(err)
		}
		clock, err := trustanchors.NewSessionClock()
		if err != nil {
			t.Fatal(err)
		}
		capture, err := old.CaptureAnchorUpdate(anchorTestContext(t.Context(), t, old), quarantined.Version())
		if err != nil {
			t.Fatal(err)
		}
		recovered, receipt, err := trustanchors.Recover(lost, []string{key.ToDS(dns.SHA256).String()}, anchorTestSigned(t, key, private.(crypto.Signer), false), clock.Sample())
		if err != nil {
			t.Fatal(err)
		}
		records := recovered.Inspect().Recoveries
		owner, err := accepted.ProposeRecovery(quarantined, recovered, records[len(records)-1].At, receipt.ValidUntil())
		if err != nil {
			t.Fatal(err)
		}
		if completion, err := old.AnchorCommand(capture, func(ctx context.Context, tx *CommandTx) error {
			if err := receipt.CheckRecovery(lost, recovered, clock.Sample()); err != nil {
				return err
			}
			return anchorTestStage(ctx, tx, owner)
		}); err != nil || completion.Outcome() != AnchorCommitted {
			t.Fatal("fixture recovery did not commit", err)
		}
		if p := anchorHistoryProjection(t, old); p.Disposition() != resolverstate.Usable {
			t.Fatal("fixture recovery is not usable")
		}
		anchorHistoryObserve(t, old, anchorHistoryPromote(t, successor, "history-recovery").Term)
		refuse(t, old, imageOf(t, successor), accepted.ErrCAS)
	})
}

// The carrier computed for an adoption, at the bounds the member flows above
// do not reach: an adopted owner that does not revoke a carried key, a receipt
// that names a generation above the adopted owner, a terminal carrier, a full
// denial and a carrier bound to something else.
func TestAnchorHistoryAdoptionCarrierRules(t *testing.T) {
	old, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
	successor := anchorHistoryMember(t, old)
	// The old primary runs ahead before its revocation, so its receipt names a
	// generation the promoted writer's tombstone does not reach.
	anchorHistoryPoll(t, old, root, false)
	anchorHistoryPoll(t, old, root, false)
	key := root.firstID(t, anchorHistoryPoll(t, old, root, true))
	promoted := anchorHistoryPromote(t, successor, "history-carrier")
	before, trusting := anchorHistoryCursor(t, old), anchorHistoryCursor(t, successor)
	// The promoted writer records the same tombstone one generation later.
	anchorHistoryPoll(t, successor, root, true)
	next := anchorHistoryCursor(t, successor)
	member, adopted := old.Authority().MemberID, next.owner.Version()
	receipt := before.local.record.Denied
	if len(receipt) != 1 || receipt[0].ID != key || receipt[0].FirstOwner.Generation <= adopted.Generation {
		t.Fatal("fixture receipt does not name a generation above the adopted owner")
	}
	t.Run("pending-key-refused", func(t *testing.T) {
		if err := anchorAdoption(before, trusting); !errors.Is(err, accepted.ErrCAS) {
			t.Fatal("adoption admitted an owner that trusts a carried revoked key", err)
		}
		if _, err := anchorLocalForAdoption(before.local, before, trusting, member); !errors.Is(err, accepted.ErrCAS) {
			t.Fatal("carrier adopted an owner that trusts a carried revoked key", err)
		}
		if err := anchorAdoption(before, next); err != nil {
			t.Fatal("adoption refused an owner that revokes every carried key", err)
		}
	})
	t.Run("restated-receipt", func(t *testing.T) {
		local, err := anchorLocalForAdoption(before.local, before, next, member)
		if err != nil {
			t.Fatal(err)
		}
		r := local.record
		if !slices.Equal(r.Denied, []anchorLocalRevocation{{ID: key, FirstOwner: adopted}}) || r.RetiredThrough != adopted.Generation-1 || r.Owner != adopted || r.Revision != before.local.record.Revision+1 || r.Terminal || r.Carrier != before.local.record.Carrier {
			t.Fatal("carried denial was not restated at the adopted owner with the rebased floor")
		}
		rebased := *next
		rebased.local = local
		if err := checkAnchorLocal(local, &rebased, member); err != nil {
			t.Fatal("rebased carrier does not pass the restart check", err)
		}
	})
	t.Run("terminal-stays-terminal", func(t *testing.T) {
		r := before.local.record
		r.Terminal, r.RetiredThrough = true, math.MaxUint64
		terminal, err := encodeAnchorLocal(r)
		if err != nil {
			t.Fatal(err)
		}
		local, err := anchorLocalForAdoption(terminal, before, next, member)
		if err != nil || !local.record.Terminal || local.record.RetiredThrough != math.MaxUint64 || local.record.Owner != adopted {
			t.Fatal("adoption reopened a terminal carrier", err)
		}
	})
	t.Run("full-denial-refused", func(t *testing.T) {
		// A full carrier whose other denials the adopted owner does not revoke
		// is refused, not truncated.
		r := before.local.record
		r.Denied = []anchorLocalRevocation{{ID: key, FirstOwner: r.Owner}}
		for i := 0; len(r.Denied) < anchorLocalMaxRevocations; i++ {
			// Zero-padded hexadecimal counters are valid identities.
			if id := fmt.Sprintf("%064x", i+1); id != key {
				r.Denied = append(r.Denied, anchorLocalRevocation{ID: id, FirstOwner: r.Owner})
			}
		}
		slices.SortFunc(r.Denied, func(a, b anchorLocalRevocation) int { return strings.Compare(a.ID, b.ID) })
		full, err := encodeAnchorLocal(r)
		if err != nil || len(full.record.Denied) != anchorLocalMaxRevocations {
			t.Fatal("fixture carrier is not full", err)
		}
		if _, err := anchorLocalForAdoption(full, before, next, member); !errors.Is(err, accepted.ErrCAS) {
			t.Fatal("a full carrier with unrevoked denials adopted", err)
		}
	})
	t.Run("bound-to-something-else", func(t *testing.T) {
		if _, err := anchorLocalForAdoption(before.local, before, next, "another-member"); !errors.Is(err, accepted.ErrCAS) {
			t.Fatal("carrier of another member adopted", err)
		}
		if _, err := anchorLocalForAdoption(next.local, before, next, successor.Authority().MemberID); !errors.Is(err, accepted.ErrCAS) {
			t.Fatal("carrier bound to another owner adopted", err)
		}
		if _, err := anchorLocalForAdoption(before.local, before, before, member); !errors.Is(err, ErrAnchorUnavailable) {
			t.Fatal("an unchanged owner took the adoption lane", err)
		}
		if _, err := anchorLocalForAdoption(nil, before, next, member); !errors.Is(err, ErrAnchorUnavailable) {
			t.Fatal("a missing carrier adopted", err)
		}
	})
	// The same cut through the real install.
	anchorHistoryObserve(t, old, promoted.Term)
	if err := anchorHistoryInstall(t, old, successor); err != nil {
		t.Fatal(err)
	}
	anchorHistoryAdopted(t, old, successor, []string{key})
	if got := anchorLocalTestRead(t, old).record.Denied; !slices.Equal(got, []anchorLocalRevocation{{ID: key, FirstOwner: adopted}}) {
		t.Fatal("installed carrier differs from the computed one")
	}
}
