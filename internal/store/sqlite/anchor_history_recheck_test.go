package sqlite

import (
	"context"
	"crypto"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// These tests re-examine the term-ordered adoption of a managed owner (D369)
// the way an adversary would: sequences of promotion, rejoin, resolution and
// restart that try to make a member trust a root key it saw revoked, forget a
// revocation, lose managed history or adopt a branch it must refuse. They also
// pin, for every state that leaves a member fenced or retired, what ends that
// state. As in anchor_history_test.go, members are real databases and every
// owner comes from the pure state machine and signed DNSKEY responses; the two
// installs of a re-sealed image say so.

// anchorRecheckDS names a fixture key the way the resolver projection does.
func anchorRecheckDS(proof anchorTestProof) string {
	ds := proof.key.ToDS(dns.SHA256)
	ds.Hdr.Ttl = 0
	return ds.String()
}

// anchorRecheckHolds fails if the member's committed owner would give the
// resolver a key the member saw revoked, or if the member forgot the
// revocation in its owner or in its node-local carrier.
func anchorRecheckHolds(t *testing.T, member *Store, key, ds string) {
	t.Helper()
	p := anchorHistoryProjection(t, member)
	if slices.Contains(p.DS(), ds) {
		t.Fatal("member trusts a root key it saw revoked")
	}
	if !slices.Contains(p.RevokedIDs(), key) {
		t.Fatal("member's owner lost the revocation")
	}
	if !slices.Contains(anchorHistoryDeniedIDs(anchorLocalTestRead(t, member)), key) {
		t.Fatal("member's local carrier lost the revocation")
	}
}

// anchorRecheckServes publishes the member's own committed cut as a fenced
// member does, DNS-only, and reads through the registered resolver input.
func anchorRecheckServes(t *testing.T, member *Store) *AnchorResolverInput {
	t.Helper()
	ctx := anchorLocalTestFinite(t.Context(), t)
	_, token, err := member.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !token.DNSOnly() {
		t.Fatal("fixture member is not on a DNS-only cut")
	}
	publication, err := member.CaptureAnchorPublication(ctx, token)
	if err != nil {
		t.Fatal("fenced member cannot publish its own branch", err)
	}
	if err := member.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: publication, DNSOnly: true}, publication.Apply); err != nil {
		t.Fatal(err)
	}
	anchorPublicationTestRead(ctx, t, publication.Input())
	return publication.Input()
}

// anchorRecheckRetired requires that the member's committed local floor
// retires its own owner generation, so that it can register no resolver input
// for that cut: it answers SERVFAIL. Admission itself stays open.
func anchorRecheckRetired(t *testing.T, member *Store) {
	t.Helper()
	cursor := anchorHistoryCursor(t, member)
	if record := anchorLocalTestRead(t, member).record; record.Terminal || record.RetiredThrough != cursor.owner.Version().Generation {
		t.Fatalf("local floor %d does not retire exactly owner generation %d", record.RetiredThrough, cursor.owner.Version().Generation)
	}
	ctx := anchorLocalTestFinite(t.Context(), t)
	if member.Authority().Writable() {
		ctx = anchorTestContext(t.Context(), t, member)
	}
	_, token, err := member.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.CaptureAnchorPublication(ctx, token); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatalf("a member with a pending local denial captured a publication: got %v, want %v", err, ErrAnchorUnavailable)
	}
	if anchorHistoryCursor(t, member) != cursor {
		t.Fatal("the refused publication changed the member")
	}
}

// anchorRecheckReplacement joins a new receiver database from the current
// primary: the documented recovery for a member that can never follow. It
// takes the primary's owner as its first image and publishes it.
func anchorRecheckReplacement(t *testing.T, primary *Store) *Store {
	t.Helper()
	replacement := anchorHistoryMember(t, primary)
	want := anchorHistoryCursor(t, primary)
	cursor := anchorHistoryCursor(t, replacement)
	if cursor.owner.Version() != want.owner.Version() {
		t.Fatal("replacement receiver does not hold the primary's owner")
	}
	revoked, err := anchorRevoked(want)
	if err != nil {
		t.Fatal(err)
	}
	if revoked == nil {
		revoked = []string{}
	}
	local := anchorLocalTestRead(t, replacement)
	if got := anchorHistoryDeniedIDs(local); !slices.Equal(got, revoked) || local.record.Terminal || local.record.RetiredThrough != want.owner.Version().Generation-1 {
		t.Fatalf("replacement carrier denies %v, want exactly the primary's revoked keys %v", got, revoked)
	}
	if a := replacement.Authority(); a.Mode != replication.Follower || !a.Term.Same(primary.Authority().Term) {
		t.Fatal("replacement receiver does not follow the primary", a.Mode)
	}
	if want.mark != (retirement.Mark{}) {
		ctx := anchorLocalTestFinite(t.Context(), t)
		anchorPublicationTestRead(ctx, t, anchorPublicationTestPublish(ctx, t, replacement))
	}
	return replacement
}

// anchorRecheckRestart closes the member and opens the same database again.
func anchorRecheckRestart(t *testing.T, member *Store, path string) *Store {
	t.Helper()
	version, local := anchorHistoryCursor(t, member).owner.Version(), anchorLocalTestRead(t, member)
	if err := member.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, path, internalOptions)
	if _, err := reopened.BindAnchorLifecycle(anchorLocalTestFinite(t.Context(), t)); err != nil {
		t.Fatal("restart refused the member's committed cut", err)
	}
	if anchorHistoryCursor(t, reopened).owner.Version() != version || !sameAnchorLocal(local, anchorLocalTestRead(t, reopened)) {
		t.Fatal("restart changed the member's owner or local carrier")
	}
	return reopened
}

// A revocation the old primary recorded is held through every later term. The
// member refuses each primary whose branch still trusts the key, across
// restarts, a second promotion and that primary's ordinary observations; it
// adopts a branch only after that branch's writer has recorded the same REVOKE.
// A follower that is promoted later from an image without the tombstone is
// refused again. The member's own branch keeps answering DNS-only throughout,
// and a member without the revocation follows every term undisturbed.
func TestAnchorRecheckRevocationHeldAcrossTermsRestartsAndStalePromotion(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	old, root := anchorHistoryPrimary(t, path)
	second, third := anchorHistoryMember(t, old), anchorHistoryMember(t, old)
	revoked := anchorHistoryPoll(t, old, root, true)
	key, ds := root.firstID(t, revoked), anchorRecheckDS(root.first)
	anchorRecheckHolds(t, old, key, ds)

	// Term 2: a follower that never pulled the tombstone is promoted.
	t2 := anchorHistoryPromote(t, second, "recheck-term-2")
	if !slices.Contains(anchorHistoryProjection(t, second).DS(), ds) {
		t.Fatal("fixture successor does not trust the first key")
	}
	anchorHistoryObserve(t, old, t2.Term)
	anchorHistoryRefused(t, old, second)
	// A publication of the member's own branch that was captured before a
	// refused pull still completes after it.
	serving := anchorLocalTestFinite(t.Context(), t)
	_, token, err := old.ReadServingCandidate(serving)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := old.CaptureAnchorPublication(serving, token)
	if err != nil {
		t.Fatal(err)
	}
	anchorHistoryRefused(t, old, second)
	if err := old.PublishIfCurrent(serving, token, PublicationOptions{Anchor: publication, DNSOnly: true}, publication.Apply); err != nil {
		t.Fatal("a refused adoption ended a publication captured before it", err)
	}
	input := publication.Input()
	anchorPublicationTestRead(serving, t, input)
	// The refusing member cannot make its own branch the cluster's: it is not
	// a follower, so it cannot be promoted, and it holds no conflict or
	// divergence that a resolution could keep.
	fenced, installed := old.Authority(), imageOf(t, old).Generation()
	if _, err := old.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: fenced.Term, ExpectedInstalled: installed, RequestID: "recheck-refusing-member"}); !errors.Is(err, replication.ErrFenced) {
		t.Fatalf("promotion of a fenced member: got %v, want %v", err, replication.ErrFenced)
	}
	resolving, chosen, req := resolutionInputs(t, old)
	if _, err := old.ResolveAudited(resolving, chosen, req, func(context.Context) error { return nil }, AuditEntry{Actor: "host:offline-cli", Authz: "host-local data directory and member identity", Reason: "recheck"}); !errors.Is(err, replication.ErrCAS) {
		t.Fatalf("resolution on a member without a conflict: got %v, want %v", err, replication.ErrCAS)
	}
	if anchorAuthority(fenced) != anchorAuthority(old.Authority()) || !installed.Same(imageOf(t, old).Generation()) {
		t.Fatal("a refused promotion or resolution changed the member")
	}
	// A member that saw no revocation follows the same primary at once.
	anchorHistoryObserve(t, third, t2.Term)
	if err := anchorHistoryInstall(t, third, second); err != nil {
		t.Fatal("a member without the revocation cannot follow the promoted primary", err)
	}
	// The promoted writer observes the root without the REVOKE: its branch
	// moves on and still trusts the key.
	anchorHistoryPoll(t, second, root, false)
	if err := anchorHistoryInstall(t, third, second); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		anchorHistoryRefused(t, old, second)
	}
	// Refusals leave the member's own registration current and readable.
	if !input.Current() {
		t.Fatal("a refused adoption retired the member's own resolver input")
	}
	anchorPublicationTestRead(anchorLocalTestFinite(t.Context(), t), t, input)
	member := anchorRecheckRestart(t, old, path)
	anchorRecheckHolds(t, member, key, ds)
	anchorRecheckServes(t, member)

	// Term 3: another follower of the trusting branch is promoted.
	t3 := anchorHistoryPromote(t, third, "recheck-term-3")
	anchorHistoryObserve(t, member, t3.Term)
	anchorHistoryRefused(t, member, third)
	anchorHistoryPoll(t, third, root, false)
	anchorHistoryRefused(t, member, third)
	member = anchorRecheckRestart(t, member, path)
	anchorHistoryRefused(t, member, third)
	anchorRecheckHolds(t, member, key, ds)
	// A follower of term 3 that holds the image from before the tombstone.
	stale := anchorHistoryMember(t, third)

	// The term-3 writer records the same signed REVOKE. Only now does the
	// member follow, and it publishes the adopted owner without the key.
	tombstone := anchorHistoryPoll(t, third, root, true)
	if err := anchorHistoryInstall(t, member, third); err != nil {
		t.Fatal("the canonical tombstone was not adopted", err)
	}
	anchorHistoryAdopted(t, member, third, []string{key})
	anchorRecheckHolds(t, member, key, ds)
	if anchorHistoryCursor(t, member).owner.Version() != tombstone.Version() {
		t.Fatal("member did not adopt the owner that carries the tombstone")
	}

	// Term 4: the stale follower is promoted from the image that still trusts
	// the key. The member, now a follower with the tombstone, refuses it too.
	if !slices.Contains(anchorHistoryProjection(t, stale).DS(), ds) {
		t.Fatal("fixture stale follower already holds the tombstone")
	}
	t4 := anchorHistoryPromote(t, stale, "recheck-term-4")
	anchorHistoryObserve(t, member, t4.Term)
	anchorHistoryRefused(t, member, stale)
	anchorRecheckHolds(t, member, key, ds)
	anchorRecheckServes(t, member)
	member = anchorRecheckRestart(t, member, path)
	anchorHistoryRefused(t, member, stale)

	// The same owner under a still higher term and at a higher generation is
	// refused as well: neither the term nor the count outranks a revocation.
	// (Re-sealed: no honest writer holds this owner in this term.)
	for range 3 {
		anchorHistoryPoll(t, second, root, false)
	}
	trusting := anchorHistoryCursor(t, second).owner
	if trusting.Version().Generation <= anchorHistoryCursor(t, member).owner.Version().Generation {
		t.Fatal("fixture trusting owner is not at a higher generation")
	}
	image := imageOf(t, stale)
	before := anchorHistoryCursor(t, member)
	if err := anchorHistoryInstallImage(t, member, anchorHistoryReseal(t, image, trusting, image.Revision+1)); !errors.Is(err, accepted.ErrCAS) {
		t.Fatalf("a higher generation that trusts a revoked key: got %v, want %v", err, accepted.ErrCAS)
	}
	if anchorHistoryCursor(t, member) != before {
		t.Fatal("refused image changed the member")
	}

	anchorHistoryPoll(t, stale, root, true)
	if err := anchorHistoryInstall(t, member, stale); err != nil {
		t.Fatal("the term-4 tombstone was not adopted", err)
	}
	anchorHistoryAdopted(t, member, stale, []string{key})
	anchorRecheckHolds(t, member, key, ds)
	// Inside the adopted term the tombstone binds every later image too.
	// (Re-sealed for the same reason.)
	image, before = imageOf(t, member), anchorHistoryCursor(t, member)
	if err := anchorHistoryInstallImage(t, member, anchorHistoryReseal(t, image, trusting, image.Revision+1)); !errors.Is(err, accepted.ErrCAS) {
		t.Fatalf("a same-term successor that trusts a revoked key: got %v, want %v", err, accepted.ErrCAS)
	}
	if anchorHistoryCursor(t, member) != before {
		t.Fatal("refused image changed the member")
	}
	anchorRecheckHolds(t, member, key, ds)
}

// Conflict resolution cannot lose a revocation in either direction. Keeping
// the branch that still trusts the key leaves the member that revoked it
// fenced on its own branch until the kept branch's writer records the REVOKE;
// keeping the branch that revoked it hands the tombstone to the other member.
func TestAnchorRecheckResolutionKeepsRevocationOnEitherBranch(t *testing.T) {
	conflict := func(t *testing.T) (trusting, revoking *Store, root anchorHistoryRoot, key, ds string) {
		t.Helper()
		origin, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
		trusting, revoking = anchorHistoryMember(t, origin), anchorHistoryMember(t, origin)
		first, second := anchorHistoryPromote(t, trusting, "recheck-trusting"), anchorHistoryPromote(t, revoking, "recheck-revoking")
		anchorHistoryPoll(t, trusting, root, false)
		key = root.firstID(t, anchorHistoryPoll(t, revoking, root, true))
		for _, m := range []struct {
			s     *Store
			other replication.Term
		}{{trusting, second.Term}, {revoking, first.Term}} {
			if _, err := m.s.ObserveAuthority(t.Context(), m.other); !errors.Is(err, replication.ErrPromotionConflict) {
				t.Fatal("fixture did not record the promotion conflict", err)
			}
		}
		return trusting, revoking, root, key, anchorRecheckDS(root.first)
	}
	resolve := func(t *testing.T, kept *Store) replication.Authority {
		t.Helper()
		ctx, chosen, req := resolutionInputs(t, kept)
		resolved, err := kept.ResolveAudited(ctx, chosen, req, func(context.Context) error { return nil }, AuditEntry{Actor: "host:offline-cli", Authz: "host-local data directory and member identity", Reason: "recheck"})
		if err != nil || !resolved.Writable() {
			t.Fatal("audited resolution", err)
		}
		return resolved
	}
	t.Run("kept-branch-trusts-the-key", func(t *testing.T) {
		trusting, revoking, root, key, ds := conflict(t)
		// While conflicted, the member that revoked the key serves its own branch.
		anchorRecheckServes(t, revoking)
		resolved := resolve(t, trusting)
		anchorHistoryObserve(t, revoking, resolved.Term)
		anchorHistoryRefused(t, revoking, trusting)
		anchorRecheckHolds(t, revoking, key, ds)
		anchorRecheckServes(t, revoking)
		anchorHistoryPoll(t, trusting, root, true)
		if err := anchorHistoryInstall(t, revoking, trusting); err != nil {
			t.Fatal("the kept branch's tombstone was not adopted", err)
		}
		anchorHistoryAdopted(t, revoking, trusting, []string{key})
		anchorRecheckHolds(t, revoking, key, ds)
	})
	t.Run("kept-branch-revoked-the-key", func(t *testing.T) {
		trusting, revoking, _, key, ds := conflict(t)
		resolved := resolve(t, revoking)
		anchorRecheckHolds(t, revoking, key, ds)
		anchorHistoryObserve(t, trusting, resolved.Term)
		if err := anchorHistoryInstall(t, trusting, revoking); err != nil {
			t.Fatal("the member that still trusted the key cannot adopt the kept branch", err)
		}
		anchorHistoryAdopted(t, trusting, revoking, []string{key})
		anchorRecheckHolds(t, trusting, key, ds)
	})
}

// Two members replace each other as primary three times. Each returning
// member adopts the other's branch whatever the two generation counts are,
// including an owner at its own generation with another digest, and no
// comparison of the two writers' clocks takes part. Every abandoned branch
// stays in quarantine, the carrier advances once per adoption, and the member
// that was primary first restarts on the cut it adopted last.
func TestAnchorRecheckAlternatingPrimariesAdoptEachOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	a, root := anchorHistoryPrimary(t, path)
	b := anchorHistoryMember(t, a)
	adopt := func(t *testing.T, member, primary *Store) {
		t.Helper()
		own, lost, revision := anchorHistoryCursor(t, member).owner.Version(), imageOf(t, member), anchorLocalTestRead(t, member).record.Revision
		anchorHistoryObserve(t, member, primary.Authority().Term)
		if err := anchorHistoryInstall(t, member, primary); err != nil {
			t.Fatalf("member at generation %d cannot adopt the primary's generation %d: %v", own.Generation, anchorHistoryCursor(t, primary).owner.Version().Generation, err)
		}
		anchorHistoryAdopted(t, member, primary, []string{})
		if anchorLocalTestRead(t, member).record.Revision != revision+1 {
			t.Fatal("adoption did not advance the local carrier exactly once")
		}
		quarantined, err := member.ReadQuarantined(t.Context(), lost.Generation())
		if err != nil {
			t.Fatal("the abandoned branch was not retained in quarantine", err)
		}
		if owner, err := accepted.DecodeRows(quarantined.Payload.Runtime, quarantined.Term.ClusterID); err != nil || owner.Version() != own {
			t.Fatal("quarantine does not hold the abandoned owner", err)
		}
	}
	// A runs two observations ahead; B is promoted on the shared owner, whose
	// last update is earlier than A's.
	anchorHistoryPoll(t, a, root, false)
	ahead := anchorHistoryPoll(t, a, root, false)
	anchorHistoryPromote(t, b, "recheck-b-1")
	if canonical := anchorHistoryCursor(t, b).owner; canonical.Version().Generation >= ahead.Version().Generation || !canonical.Inspect().UpdatedAt.Before(ahead.Inspect().UpdatedAt) {
		t.Fatal("fixture canonical owner is not lower and older than the abandoned one")
	}
	adopt(t, a, b)
	// A is promoted over B and observes once; B kept observing as the writer
	// of its own term and is three generations ahead when it returns.
	anchorHistoryPromote(t, a, "recheck-a-2")
	anchorHistoryPoll(t, a, root, false)
	for range 3 {
		anchorHistoryPoll(t, b, root, false)
	}
	if anchorHistoryCursor(t, a).owner.Version().Generation >= anchorHistoryCursor(t, b).owner.Version().Generation {
		t.Fatal("fixture returning member is not ahead")
	}
	adopt(t, b, a)
	// B is promoted again and observes once; A observed once more as well, so
	// both hold one generation with different owners.
	anchorHistoryPromote(t, b, "recheck-b-3")
	other := anchorHistoryPoll(t, b, root, false).Version()
	if own := anchorHistoryPoll(t, a, root, false).Version(); own.Generation != other.Generation || own == other {
		t.Fatal("fixture branches did not diverge at one generation")
	}
	adopt(t, a, b)
	reopened := anchorRecheckRestart(t, a, path)
	anchorHistoryAdopted(t, reopened, b, []string{})
	// The canonical branch moves on inside its term: an ordinary successor.
	next := anchorHistoryPoll(t, b, root, false).Version()
	if err := anchorHistoryInstall(t, reopened, b); err != nil || anchorHistoryCursor(t, reopened).owner.Version() != next {
		t.Fatal("member did not follow the canonical successor after three adoptions", err)
	}
}

// anchorRecheckSingle is a real primary with one seed key, the fixture in
// which a revocation loses every anchor.
type anchorRecheckSingle struct {
	primary *Store
	proof   anchorTestProof
}

func newAnchorRecheckSingle(t *testing.T, path string) anchorRecheckSingle {
	t.Helper()
	s := openAt(t, path, internalOptions)
	_, dormant := anchorLocalTestDormant(t, s)
	proof := anchorTestManaged(t, s, dormant)
	if _, err := s.EnablePrimary(t.Context(), imageOf(t, s).Generation()); err != nil {
		t.Fatal(err)
	}
	return anchorRecheckSingle{primary: s, proof: proof}
}

func (f anchorRecheckSingle) poll(t *testing.T, s *Store, revoke bool) accepted.Snapshot {
	t.Helper()
	return anchorHistoryPollSigned(t, s, anchorTestSigned(t, f.proof.key, f.proof.signer, revoke), revoke)
}

// anchorRecheckKey is one more root key with its signer, for example the
// replacement key of a recovery.
func anchorRecheckKey(t *testing.T) anchorTestProof {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	return anchorTestProof{key: key, signer: private.(crypto.Signer)}
}

// anchorRecheckRecover runs a real `anchors recover` transition on a
// quarantined writer: the replacement key is authenticated and recorded as
// recovery history at the next pure generation.
func anchorRecheckRecover(t *testing.T, s *Store, replacement anchorTestProof) accepted.Snapshot {
	t.Helper()
	quarantined := anchorHistoryCursor(t, s).owner
	lost, err := quarantined.State()
	if err != nil || !lost.Inspect().Quarantined {
		t.Fatal("fixture writer is not quarantined", err)
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	capture, err := s.CaptureAnchorUpdate(anchorTestContext(t.Context(), t, s), quarantined.Version())
	if err != nil {
		t.Fatal(err)
	}
	recovered, receipt, err := trustanchors.Recover(lost, []string{replacement.key.ToDS(dns.SHA256).String()}, anchorTestSigned(t, replacement.key, replacement.signer, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	records := recovered.Inspect().Recoveries
	owner, err := accepted.ProposeRecovery(quarantined, recovered, records[len(records)-1].At, receipt.ValidUntil())
	if err != nil {
		t.Fatal(err)
	}
	if completion, err := s.AnchorCommand(capture, func(ctx context.Context, tx *CommandTx) error {
		if err := receipt.CheckRecovery(lost, recovered, clock.Sample()); err != nil {
			return err
		}
		return anchorTestStage(ctx, tx, owner)
	}); err != nil || completion.Outcome() != AnchorCommitted {
		t.Fatal("fixture recovery did not commit", err)
	}
	if p := anchorHistoryProjection(t, s); p.Disposition() != resolverstate.Usable {
		t.Fatal("fixture recovery is not usable")
	}
	return anchorHistoryCursor(t, s).owner
}

// anchorRecheckStuck requires the adoption refusal and that neither a later
// canonical image nor a restart ends it; the member keeps its own branch.
func anchorRecheckStuck(t *testing.T, member *Store, path string, primary *Store) *Store {
	t.Helper()
	anchorHistoryRefused(t, member, primary)
	member = anchorRecheckRestart(t, member, path)
	anchorHistoryRefused(t, member, primary)
	return member
}

// A returning member and the current primary disagree about `anchors recover`
// history. The member refuses the primary's branch for good: no later image,
// restart or second recovery reconciles the two, the member keeps answering
// from its own branch, and a replacement receiver joined from the primary
// follows it. A member whose own state had not reached the primary's recovery
// is not in that state and adopts the recovered branch.
func TestAnchorRecheckRecoveryHistoryDisagreement(t *testing.T) {
	// The current primary lost every anchor after the split and the operator
	// recovered it. RetainsRecoveryHistory still compares the pure generation
	// of that record with the returning member's own pure generation.
	for _, test := range []struct {
		name  string
		ahead int
		adopt bool
	}{{"primary-recovered/member-behind-the-record", 0, true}, {"primary-recovered/member-level-with-the-record", 2, false}, {"primary-recovered/member-past-the-record", 3, false}} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			f := newAnchorRecheckSingle(t, path)
			old, successor, replacement := f.primary, anchorHistoryMember(t, f.primary), anchorRecheckKey(t)
			for range test.ahead {
				f.poll(t, old, false)
			}
			promoted := anchorHistoryPromote(t, successor, "recheck-recovered")
			if lost, err := f.poll(t, successor, true).State(); err != nil || !lost.Inspect().Quarantined {
				t.Fatal("fixture successor did not lose every anchor", err)
			}
			state, err := anchorRecheckRecover(t, successor, replacement).State()
			if err != nil {
				t.Fatal(err)
			}
			own, err := anchorHistoryCursor(t, old).owner.State()
			if err != nil {
				t.Fatal(err)
			}
			if record := state.Inspect().Recoveries[0].Generation; (record > own.Inspect().Generation) != test.adopt {
				t.Fatalf("fixture recovery at pure generation %d against member generation %d does not exercise the case", record, own.Inspect().Generation)
			}
			anchorHistoryObserve(t, old, promoted.Term)
			if test.adopt {
				if err := anchorHistoryInstall(t, old, successor); err != nil {
					t.Fatal("a member behind the recovery cannot adopt the recovered branch", err)
				}
				anchorHistoryAdopted(t, old, successor, anchorHistoryProjection(t, successor).RevokedIDs())
				return
			}
			member := anchorRecheckStuck(t, old, path, successor)
			// A later canonical observation does not help: the record keeps
			// its generation.
			anchorHistoryPollSigned(t, successor, anchorTestSigned(t, replacement.key, replacement.signer, false), false)
			anchorHistoryRefused(t, member, successor)
			anchorRecheckServes(t, member)
			anchorRecheckReplacement(t, successor)
		})
	}
	// The returning member holds a recovery the current primary never
	// accepted. A recovery on the primary with the same replacement key writes
	// another record, so the two histories never agree.
	t.Run("member-recovered", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		f := newAnchorRecheckSingle(t, path)
		old, successor, replacement := f.primary, anchorHistoryMember(t, f.primary), anchorRecheckKey(t)
		f.poll(t, old, true)
		anchorRecheckRecover(t, old, replacement)
		promoted := anchorHistoryPromote(t, successor, "recheck-member-recovered")
		anchorHistoryObserve(t, old, promoted.Term)
		member := anchorRecheckStuck(t, old, path, successor)
		// The primary observes the same REVOKE, loses every anchor and is
		// recovered as the runbook says.
		f.poll(t, successor, true)
		anchorRecheckRecover(t, successor, replacement)
		if anchorHistoryProjection(t, member).DS()[0] != anchorHistoryProjection(t, successor).DS()[0] {
			t.Fatal("fixture members did not recover with the same key")
		}
		anchorHistoryRefused(t, member, successor)
		anchorRecheckServes(t, member)
		anchorRecheckReplacement(t, successor)
	})
}

// anchorRecheckSignedSet is one root DNSKEY RRset signed by every listed key.
// The keys named in revoked carry the REVOKE bit and sign with their changed
// key tag, as a root does while it retires a key.
func anchorRecheckSignedSet(t *testing.T, keys []anchorTestProof, revoked ...int) *dns.Msg {
	t.Helper()
	var set []dns.RR
	for i, proof := range keys {
		rr := dns.Copy(proof.key).(*dns.DNSKEY)
		if slices.Contains(revoked, i) {
			rr.Flags |= dns.REVOKE
		}
		set = append(set, rr)
	}
	query := new(dns.Msg)
	query.SetQuestion(".", dns.TypeDNSKEY)
	message := new(dns.Msg)
	message.SetReply(query)
	message.Authoritative = true
	message.Answer = slices.Clone(set)
	for i, proof := range keys {
		// A signature made for another RRset of the same key gives the header.
		sig := anchorTestSigned(t, proof.key, proof.signer, slices.Contains(revoked, i)).Answer[1].(*dns.RRSIG)
		if err := sig.Sign(proof.signer, set); err != nil {
			t.Fatal(err)
		}
		message.Answer = append(message.Answer, sig)
	}
	return message
}

// A member carries a revocation that the current primary can never record:
// the root has stopped publishing the revoked key, or the primary's branch
// never knew the key at all. The member refuses that primary for good and keeps
// answering from its own branch, which treats the key as revoked. What ends
// the state is a replacement receiver, and it holds the cluster's trust-anchor
// state: it trusts the first key again in the first case.
func TestAnchorRecheckRevocationThePrimaryCanNeverRecord(t *testing.T) {
	t.Run("root-stopped-publishing-the-key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		old, root := anchorHistoryPrimary(t, path)
		successor := anchorHistoryMember(t, old)
		key, ds := root.firstID(t, anchorHistoryPoll(t, old, root, true)), anchorRecheckDS(root.first)
		promoted := anchorHistoryPromote(t, successor, "recheck-key-gone")
		anchorHistoryObserve(t, old, promoted.Term)
		// The root now serves only the second key. The primary records the
		// first as missing, and a missing key stays trusted (RFC 5011).
		gone := anchorRecheckSignedSet(t, []anchorTestProof{root.second})
		for range 2 {
			state, err := anchorHistoryPollSigned(t, successor, gone, false).State()
			if err != nil {
				t.Fatal(err)
			}
			if p := anchorHistoryProjection(t, successor); !slices.Contains(p.DS(), ds) || len(p.RevokedIDs()) != 0 {
				t.Fatal("fixture primary stopped trusting the key it can no longer see")
			}
			if !slices.ContainsFunc(state.Inspect().Keys, func(k trustanchors.Key) bool { return k.ID == key && k.Status == trustanchors.Missing }) {
				t.Fatal("fixture primary did not record the key as missing")
			}
			old = anchorRecheckStuck(t, old, path, successor)
		}
		anchorRecheckHolds(t, old, key, ds)
		anchorRecheckServes(t, old)
		replacement := anchorRecheckReplacement(t, successor)
		if !slices.Contains(anchorHistoryProjection(t, replacement).DS(), ds) || len(anchorLocalTestRead(t, replacement).record.Denied) != 0 {
			t.Fatal("the replacement receiver does not hold the primary's trust-anchor state")
		}
		anchorHistoryRefused(t, old, successor)
		anchorRecheckHolds(t, old, key, ds)
	})
	t.Run("primary-never-knew-the-key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), FileName)
		old, root := anchorHistoryPrimary(t, path)
		successor := anchorHistoryMember(t, old)
		third := anchorRecheckKey(t)
		keys := []anchorTestProof{root.first, root.second, third}
		// The old primary sees a new key, which starts its hold-down, and then
		// that key's signed REVOKE. Neither reaches the follower.
		pending, err := anchorHistoryPollSigned(t, old, anchorRecheckSignedSet(t, keys), false).State()
		if err != nil {
			t.Fatal(err)
		}
		index := slices.IndexFunc(pending.Inspect().Keys, func(k trustanchors.Key) bool { return k.Status == trustanchors.AddPending })
		if index < 0 {
			t.Fatal("fixture old primary did not start the new key's hold-down")
		}
		key := pending.Inspect().Keys[index].ID
		retired := anchorRecheckSignedSet(t, keys, 2)
		anchorHistoryPollSigned(t, old, retired, true)
		if p := anchorHistoryProjection(t, old); !slices.Equal(p.RevokedIDs(), []string{key}) || len(p.DS()) != 2 {
			t.Fatal("fixture old primary did not record the new key as revoked")
		}
		promoted := anchorHistoryPromote(t, successor, "recheck-key-unknown")
		anchorHistoryObserve(t, old, promoted.Term)
		// The primary reads the very same response. A REVOKE of a key it never
		// held is not a revocation for it, and it records nothing about the key.
		for range 2 {
			state, err := anchorHistoryPollSigned(t, successor, retired, false).State()
			if err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(state.Inspect().Keys, func(k trustanchors.Key) bool { return k.ID == key }) {
				t.Fatal("fixture primary recorded a key it never held")
			}
			old = anchorRecheckStuck(t, old, path, successor)
		}
		anchorRecheckServes(t, old)
		// Nobody ever trusted that key, and the replacement does not either.
		replacement := anchorRecheckReplacement(t, successor)
		if p := anchorHistoryProjection(t, replacement); len(p.DS()) != 2 || len(p.RevokedIDs()) != 0 {
			t.Fatal("the replacement receiver does not hold the primary's trust-anchor state")
		}
	})
}

// anchorRecheckBootstrap starts managed anchors on a writer whose owner is
// still dormant, from the listed seed keys and one signed response.
func anchorRecheckBootstrap(t *testing.T, s *Store, signed *dns.Msg, seeds ...anchorTestProof) accepted.Snapshot {
	t.Helper()
	dormant := anchorHistoryCursor(t, s).owner
	if dormant.Inspect().Phase != accepted.Dormant {
		t.Fatal("fixture writer is not dormant")
	}
	capture, err := s.CaptureAnchorUpdate(anchorTestContext(t.Context(), t, s), dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	seed := trustanchors.Seed{ClusterID: s.Authority().Term.ClusterID, PolicyID: "explicit-test-root"}
	for _, proof := range seeds {
		seed.DS = append(seed.DS, proof.key.ToDS(dns.SHA256).String())
	}
	state, original, err := trustanchors.Bootstrap(seed, signed, clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	managed, err := accepted.ProposeManaged(dormant, state, state.Inspect().Checkpoint.Wall, original.ValidUntil())
	if err != nil {
		t.Fatal(err)
	}
	if completion, err := s.AnchorCommand(capture, func(ctx context.Context, tx *CommandTx) error {
		if err := original.CheckBootstrap(state, clock.Sample()); err != nil {
			return err
		}
		return anchorTestStage(ctx, tx, managed)
	}); err != nil || completion.Outcome() != AnchorCommitted {
		t.Fatal("fixture bootstrap did not commit", err)
	}
	return anchorHistoryCursor(t, s).owner
}

// The old primary started managed anchors after its follower's last pull, and
// the follower was promoted with a dormant owner. Managed history is never
// replaced by a dormant owner, so the old primary refuses that primary and
// keeps answering from its own branch. The state ends without a replacement
// when the primary starts managed anchors with the same seeds: its first
// managed owner is another branch of the same history and is adopted by term.
// With other seeds it is another trust domain and stays refused.
func TestAnchorRecheckManagedMemberAndDormantPrimary(t *testing.T) {
	fixture := func(t *testing.T) (old, successor *Store, path string, root anchorHistoryRoot, own accepted.Snapshot) {
		t.Helper()
		path = filepath.Join(t.TempDir(), FileName)
		old = openAt(t, path, internalOptions)
		_, dormant := anchorLocalTestDormant(t, old)
		if _, err := old.EnablePrimary(t.Context(), imageOf(t, old).Generation()); err != nil {
			t.Fatal(err)
		}
		successor = anchorHistoryMember(t, old)
		if anchorHistoryCursor(t, successor).owner.Version() != dormant.Version() {
			t.Fatal("fixture follower does not hold the dormant owner")
		}
		root = anchorHistoryRoot{first: anchorTestBootstrap(t, old, dormant), second: anchorTestBootstrap(t, old, dormant)}
		own = anchorRecheckBootstrap(t, old, root.signed(t, false), root.first, root.second)
		promoted := anchorHistoryPromote(t, successor, "recheck-dormant-primary")
		anchorHistoryObserve(t, old, promoted.Term)
		old = anchorRecheckStuck(t, old, path, successor)
		anchorRecheckServes(t, old)
		return old, successor, path, root, own
	}
	t.Run("same-seeds", func(t *testing.T) {
		old, successor, path, root, own := fixture(t)
		canonical := anchorRecheckBootstrap(t, successor, root.signed(t, false), root.first, root.second)
		if canonical.Version() == own.Version() || canonical.Inspect().SeedDigest != own.Inspect().SeedDigest || canonical.Inspect().HistoryID != own.Inspect().HistoryID {
			t.Fatal("fixture primary did not start another branch of the same history")
		}
		lost := imageOf(t, old)
		if err := anchorHistoryInstall(t, old, successor); err != nil {
			t.Fatal("the member cannot adopt the primary's first managed owner", err)
		}
		anchorHistoryAdopted(t, old, successor, []string{})
		if _, err := old.ReadQuarantined(t.Context(), lost.Generation()); err != nil {
			t.Fatal("the abandoned branch was not retained in quarantine", err)
		}
		reopened := anchorRecheckRestart(t, old, path)
		anchorHistoryAdopted(t, reopened, successor, []string{})
	})
	t.Run("other-seeds", func(t *testing.T) {
		old, successor, path, root, own := fixture(t)
		canonical := anchorRecheckBootstrap(t, successor, root.signed(t, false), root.first)
		if canonical.Inspect().SeedDigest == own.Inspect().SeedDigest {
			t.Fatal("fixture primary did not start from other seeds")
		}
		old = anchorRecheckStuck(t, old, path, successor)
		anchorRecheckServes(t, old)
		anchorRecheckReplacement(t, successor)
	})
}

// A follower whose own root check saw a signed REVOKE answers SERVFAIL until
// the canonical tombstone arrives. Promoting it does not end that: it keeps
// its owner, which still trusts the key, publishes nothing as the primary, and
// cannot commit an observation that omits the REVOKE. Its own observation of
// the REVOKE commits the tombstone and it publishes at once. A member that
// follows it holds no such denial and publishes meanwhile.
func TestAnchorRecheckPromotedMemberWithPendingDenial(t *testing.T) {
	origin, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
	member := anchorHistoryMember(t, origin)
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
	key, ds := root.firstID(t, shared), anchorRecheckDS(root.first)
	anchorRecheckRetired(t, member)
	anchorHistoryPromote(t, member, "recheck-pending-denial")
	if anchorHistoryCursor(t, member).owner.Version() != shared.Version() || !slices.Contains(anchorHistoryProjection(t, member).DS(), ds) {
		t.Fatal("promotion changed the member's owner")
	}
	anchorRecheckRetired(t, member)
	// A member that follows this primary takes the owner without the local
	// denial and publishes it.
	follower := anchorRecheckReplacement(t, member)
	// An observation of the root without the REVOKE cannot commit.
	state, err := shared.State()
	if err != nil {
		t.Fatal(err)
	}
	before, local := anchorHistoryCursor(t, member), anchorLocalTestRead(t, member)
	writer := anchorTestContext(t.Context(), t, member)
	update, err := member.CaptureAnchorUpdate(writer, shared.Version())
	if err != nil {
		t.Fatal(err)
	}
	observation, err := trustanchors.Verify(state, root.signed(t, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	completion, err := member.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
		next, err := trustanchors.Advance(state, observation, clock.Sample())
		if err != nil {
			return err
		}
		owner, err := accepted.ProposeManaged(shared, next, next.Inspect().Checkpoint.Wall, observation.ValidUntil())
		if err != nil {
			return err
		}
		return anchorTestStage(ctx, tx, owner)
	})
	if !errors.Is(err, accepted.ErrCAS) || completion.Outcome() != AnchorRolledBack {
		t.Fatalf("an observation that omits a locally denied key: got %v outcome %d, want %v rolled back", err, completion.Outcome(), accepted.ErrCAS)
	}
	if anchorHistoryCursor(t, member) != before || !sameAnchorLocal(local, anchorLocalTestRead(t, member)) {
		t.Fatal("the refused observation changed the member")
	}
	anchorRecheckRetired(t, member)
	// The primary's own observation of the REVOKE ends the state.
	tombstone := anchorHistoryPoll(t, member, root, true)
	if p := anchorHistoryProjection(t, member); slices.Contains(p.DS(), ds) || !slices.Equal(p.RevokedIDs(), []string{key}) {
		t.Fatal("the tombstone was not recorded")
	}
	writer = anchorTestContext(t.Context(), t, member)
	anchorPublicationTestRead(writer, t, anchorPublicationTestPublish(writer, t, member))
	if err := anchorHistoryInstall(t, follower, member); err != nil || anchorHistoryCursor(t, follower).owner.Version() != tombstone.Version() {
		t.Fatal("the follower did not install the tombstone", err)
	}
	if got := anchorHistoryDeniedIDs(anchorLocalTestRead(t, follower)); !slices.Equal(got, []string{key}) {
		t.Fatalf("follower carrier denies %v, want %v", got, []string{key})
	}
}

// anchorRecheckDeny is one deny-only root check by a member that is not the
// writer: it verifies the signed response against its own committed owner and
// commits what that response revokes to its node-local carrier.
func anchorRecheckDeny(t *testing.T, member *Store, signed *dns.Msg) {
	t.Helper()
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	denial, err := member.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), anchorHistoryCursor(t, member).owner.Version(), clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(signed)
	if err != nil {
		t.Fatal(err)
	}
	if completion, _, err := member.CommitAnchorDenial(verified); err != nil || completion.Outcome() != AnchorCommitted {
		t.Fatal("deny-only root check did not commit", err)
	}
}

// A member that refuses the primary keeps checking the root on its own. When
// it then sees a second key revoked it retires its own branch as well, and it
// follows the primary only after the primary has recorded both revocations:
// one of the two is not enough.
func TestAnchorRecheckRefusingMemberDeniesAnotherKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	old := openAt(t, path, internalOptions)
	_, dormant := anchorLocalTestDormant(t, old)
	if _, err := old.EnablePrimary(t.Context(), imageOf(t, old).Generation()); err != nil {
		t.Fatal(err)
	}
	keys := []anchorTestProof{anchorTestBootstrap(t, old, dormant), anchorTestBootstrap(t, old, dormant), anchorTestBootstrap(t, old, dormant)}
	shared := anchorRecheckBootstrap(t, old, anchorRecheckSignedSet(t, keys), keys...)
	successor := anchorHistoryMember(t, old)
	id := func(proof anchorTestProof) string {
		t.Helper()
		return anchorHistoryRoot{first: proof}.firstID(t, shared)
	}
	first, second := id(keys[0]), id(keys[1])
	want := []string{first, second}
	slices.Sort(want)
	// The old primary records the first key's REVOKE; the follower never pulls.
	anchorHistoryPollSigned(t, old, anchorRecheckSignedSet(t, keys, 0), true)
	promoted := anchorHistoryPromote(t, successor, "recheck-second-key")
	anchorHistoryObserve(t, old, promoted.Term)
	anchorHistoryRefused(t, old, successor)
	anchorRecheckServes(t, old)
	// Its own root check, in the fenced role, sees the second key revoked too.
	anchorRecheckDeny(t, old, anchorRecheckSignedSet(t, keys, 0, 1))
	if got := anchorHistoryDeniedIDs(anchorLocalTestRead(t, old)); !slices.Equal(got, want) {
		t.Fatalf("local denial %v, want %v", got, want)
	}
	anchorRecheckRetired(t, old)
	anchorHistoryRefused(t, old, successor)
	// The primary records the first revocation only: still refused, and the
	// member's own branch stays retired across a restart.
	anchorHistoryPollSigned(t, successor, anchorRecheckSignedSet(t, keys, 0), true)
	member := anchorRecheckStuck(t, old, path, successor)
	anchorRecheckRetired(t, member)
	// The primary records the second one. The member follows and publishes
	// the adopted owner, which trusts the third key alone.
	anchorHistoryPollSigned(t, successor, anchorRecheckSignedSet(t, keys, 0, 1), true)
	if err := anchorHistoryInstall(t, member, successor); err != nil {
		t.Fatal("the owner that revokes both carried keys was not adopted", err)
	}
	anchorHistoryAdopted(t, member, successor, want)
	if p := anchorHistoryProjection(t, member); !slices.Equal(p.DS(), []string{anchorRecheckDS(keys[2])}) || !slices.Equal(p.RevokedIDs(), want) {
		t.Fatal("member does not trust exactly the third key")
	}
}

// A member's own root check verifies a signed REVOKE while an adoption
// commits. The denial was captured for the abandoned owner and cannot be
// written for the adopted one. The member does not go on as if it had seen
// nothing: anchor admission closes for the process and it registers no
// resolver input. A restart restores the adopted cut, and the next root check
// records the denial against it, which retires that cut until the primary
// records the same REVOKE.
func TestAnchorRecheckRevocationVerifiedWhileAdoptionCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	old, root := anchorHistoryPrimary(t, path)
	successor := anchorHistoryMember(t, old)
	anchorHistoryPoll(t, old, root, false)
	promoted := anchorHistoryPromote(t, successor, "recheck-racing-denial")
	anchorHistoryPoll(t, successor, root, false)
	anchorHistoryObserve(t, old, promoted.Term)
	own := anchorHistoryCursor(t, old).owner
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	denial, err := old.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), own.Version(), clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(root.signed(t, true))
	if err != nil {
		t.Fatal(err)
	}
	// The scheduled pull installs the promoted branch in between.
	if err := anchorHistoryInstall(t, old, successor); err != nil {
		t.Fatal(err)
	}
	adopted := anchorHistoryCursor(t, old).owner.Version()
	if adopted != anchorHistoryCursor(t, successor).owner.Version() {
		t.Fatal("fixture member did not adopt the promoted branch")
	}
	completion, receipt, err := old.CommitAnchorDenial(verified)
	if err == nil || receipt != nil || completion.Outcome() != AnchorRolledBack {
		t.Fatalf("a denial captured for the abandoned owner: got %v outcome %d, want a refusal that wrote nothing", err, completion.Outcome())
	}
	if source := old.anchors.Load(); !source.closed.Load() || !old.anchorBlocked.Load() {
		t.Fatal("a verified REVOKE that could not be recorded left anchor admission open")
	}
	ctx := anchorLocalTestFinite(t.Context(), t)
	_, token, err := old.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.CaptureAnchorPublication(ctx, token); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatalf("publication after a lost denial: got %v, want %v", err, ErrAnchorUnavailable)
	}
	if got := anchorLocalTestRead(t, old).record; got.Owner != adopted || len(got.Denied) != 0 || got.RetiredThrough != adopted.Generation-1 {
		t.Fatal("the refused denial changed the committed carrier")
	}
	// Restart: the adopted cut binds and publishes, and the next root check
	// records the denial against the adopted owner.
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	member := openAt(t, path, internalOptions)
	if _, err := member.BindAnchorLifecycle(anchorLocalTestFinite(t.Context(), t)); err != nil {
		t.Fatal("restart refused the adopted cut", err)
	}
	anchorHistoryAdopted(t, member, successor, []string{})
	anchorRecheckDeny(t, member, root.signed(t, true))
	anchorRecheckRetired(t, member)
	key := root.firstID(t, own)
	// A canonical successor without the tombstone is refused; the one with it
	// installs and the member publishes again.
	anchorHistoryPoll(t, successor, root, false)
	if err := anchorHistoryInstall(t, member, successor); !errors.Is(err, accepted.ErrCAS) {
		t.Fatalf("a successor that omits a locally denied key: got %v, want %v", err, accepted.ErrCAS)
	}
	anchorRecheckRetired(t, member)
	anchorHistoryPoll(t, successor, root, true)
	if err := anchorHistoryInstall(t, member, successor); err != nil {
		t.Fatal("the canonical tombstone was not installed", err)
	}
	anchorHistoryAdopted(t, member, successor, []string{key})
}

// The adoption rule reads what a member knows to be revoked from two places:
// the owner it abandons and its node-local denial. Every carrier a member can
// commit lists the owner's revoked keys in the denial as well, except a
// terminal one, whose bounded list no longer holds them all. Each source alone
// must refuse an owner that trusts the key.
func TestAnchorRecheckAdoptionCoversBothSourcesOfRevocation(t *testing.T) {
	old, root := anchorHistoryPrimary(t, filepath.Join(t.TempDir(), FileName))
	successor, follower := anchorHistoryMember(t, old), anchorHistoryMember(t, old)
	anchorHistoryPromote(t, successor, "recheck-second-writer")
	anchorHistoryPoll(t, successor, root, false)
	trusting := anchorHistoryCursor(t, successor)
	key := root.firstID(t, anchorHistoryPoll(t, old, root, true))
	revoking := anchorHistoryCursor(t, old)
	member := old.Authority().MemberID
	// Only the abandoned owner names the key: a terminal carrier whose list is
	// empty still passes the restart check for that owner.
	r := revoking.local.record
	r.Denied, r.Terminal, r.RetiredThrough = []anchorLocalRevocation{}, true, ^uint64(0)
	terminal, err := encodeAnchorLocal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkAnchorLocal(terminal, revoking, member); err != nil {
		t.Fatal("fixture terminal carrier is not a committable cut", err)
	}
	if err := anchorAdoptionCovers(terminal, revoking, trusting); !errors.Is(err, accepted.ErrCAS) {
		t.Fatalf("an owner that trusts a key the abandoned owner revoked: got %v, want %v", err, accepted.ErrCAS)
	}
	if _, err := anchorLocalForAdoption(terminal, revoking, trusting, member); !errors.Is(err, accepted.ErrCAS) {
		t.Fatalf("a carrier for an owner that trusts a key the abandoned owner revoked: got %v, want %v", err, accepted.ErrCAS)
	}
	// Only the local denial names the key: a follower's own deny-only check.
	anchorRecheckDeny(t, follower, root.signed(t, true))
	denying := anchorHistoryCursor(t, follower)
	if known, err := anchorRevoked(denying); err != nil || len(known) != 0 || !slices.Equal(anchorHistoryDeniedIDs(denying.local), []string{key}) {
		t.Fatal("fixture follower does not hold the key in its local denial alone", err)
	}
	if err := anchorAdoptionCovers(denying.local, denying, trusting); !errors.Is(err, accepted.ErrCAS) {
		t.Fatalf("an owner that trusts a locally denied key: got %v, want %v", err, accepted.ErrCAS)
	}
	// An owner that revokes the key satisfies both.
	anchorHistoryPoll(t, successor, root, true)
	covering := anchorHistoryCursor(t, successor)
	if err := anchorAdoptionCovers(terminal, revoking, covering); err != nil {
		t.Fatal(err)
	}
	if err := anchorAdoptionCovers(denying.local, denying, covering); err != nil {
		t.Fatal(err)
	}
	if err := anchorAdoptionCovers(nil, revoking, covering); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("adoption without a carrier", err)
	}
}
