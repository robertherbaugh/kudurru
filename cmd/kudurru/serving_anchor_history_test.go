package main

import (
	"context"
	"crypto"
	"errors"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
)

// anchorHistoryAuthority is an in-process signed root authority with two seed
// keys. With revoke set it serves the first key with its self-signed REVOKE
// bit, co-signed by the second key, as a root does while it retires a key. It
// keeps accepting, so every acquisition by any member is counted.
type anchorHistoryAuthority struct {
	endpoint string
	seeds    []string
	kept     string // canonical DS of the key that revoke leaves trusted
	reads    atomic.Int32
	revoke   atomic.Bool
}

func newAnchorHistoryAuthority(t *testing.T) *anchorHistoryAuthority {
	t.Helper()
	type signedKey struct {
		key    *dns.DNSKEY
		signer crypto.Signer
	}
	generate := func() signedKey {
		key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 60}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ED25519}
		private, err := key.Generate(256)
		if err != nil {
			t.Fatal(err)
		}
		return signedKey{key: key, signer: private.(crypto.Signer)}
	}
	sign := func(by signedKey, key *dns.DNSKEY, set []dns.RR) dns.RR {
		sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 60}, Algorithm: key.Algorithm, KeyTag: key.KeyTag(), SignerName: ".", Inception: uint32(time.Now().Add(-time.Minute).Unix() & 0xffffffff), Expiration: uint32(time.Now().Add(time.Hour).Unix() & 0xffffffff)}
		if err := sig.Sign(by.signer, set); err != nil {
			t.Fatal(err)
		}
		return sig
	}
	first, second := generate(), generate()
	// The resolver projection names a trusted key by its SHA-256 DS with TTL 0.
	kept := second.key.ToDS(dns.SHA256)
	kept.Hdr.Ttl = 0
	a := &anchorHistoryAuthority{seeds: []string{first.key.ToDS(dns.SHA256).String(), second.key.ToDS(dns.SHA256).String()}, kept: kept.String()}
	retired := dns.Copy(first.key).(*dns.DNSKEY)
	retired.Flags |= dns.REVOKE
	var answers [2][]dns.RR
	for i, lead := range []*dns.DNSKEY{first.key, retired} {
		set := []dns.RR{lead, second.key}
		answers[i] = []dns.RR{lead, second.key, sign(first, lead, set), sign(second, second.key, set)}
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	server := &dns.Server{Listener: listener, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		if q.RecursionDesired || len(q.Question) != 1 || q.Question[0] != (dns.Question{Name: ".", Qtype: dns.TypeDNSKEY, Qclass: dns.ClassINET}) || q.IsEdns0() == nil || !q.IsEdns0().Do() {
			t.Error("root caller changed the original DNSKEY acquisition")
			return
		}
		m := new(dns.Msg)
		m.SetReply(q)
		m.Authoritative = true
		source := answers[0]
		if a.revoke.Load() {
			source = answers[1]
		}
		for _, rr := range source {
			m.Answer = append(m.Answer, dns.Copy(rr))
		}
		a.reads.Add(1)
		_ = w.WriteMsg(m) // a canceled acquisition may have closed its socket
	})}
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.ShutdownContext(ctx); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("history root authority did not join")
		}
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("history root authority did not start")
	}
	a.endpoint = listener.Addr().String()
	return a
}

// anchorHistoryWriter is the elected writer of one term on an actual
// managed/direct runtime. It mounts no automatic updater: operate runs one
// explicit production root operation, the way the updater's step does.
type anchorHistoryWriter struct {
	s      *servingRuntime
	ctx    context.Context
	target serving.TermActivation
	host   providercoordinator.HostCapture
	clock  *trustanchors.SessionClock
}

func newAnchorHistoryWriter(ctx context.Context, t *testing.T, s *servingRuntime, target serving.TermActivation, host providercoordinator.HostCapture) *anchorHistoryWriter {
	t.Helper()
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	return &anchorHistoryWriter{s: s, ctx: ctx, target: target, host: host, clock: clock}
}

// operate acquires the root DNSKEY set once and commits what it authenticates.
func (w *anchorHistoryWriter) operate(t *testing.T, root *anchorHistoryAuthority, denial bool) accepted.Version {
	t.Helper()
	reads := root.reads.Load()
	op, err := w.s.captureAnchorOperation(w.ctx, w.target, w.host, w.clock, 0)
	if err != nil || op == nil {
		t.Fatal("root operation capture", err)
	}
	result, err := op.run()
	if err != nil || result.Update.Outcome() != sqlite.AnchorCommitted || (result.Denial.Outcome() == sqlite.AnchorCommitted) != denial {
		t.Fatal("root operation did not commit its expected branch", err, result.Update.Outcome(), result.Denial.Outcome())
	}
	version, ok := result.Update.Version()
	if !ok || version != anchorOperationOwner(w.ctx, t, w.s).Version() || root.reads.Load() != reads+1 {
		t.Fatal("root operation did not commit exactly one acquisition")
	}
	return version
}

// anchorHistoryPromote promotes the follower fixture's database for real.
func anchorHistoryPromote(ctx context.Context, t *testing.T, f *anchorFollowerFixture) replication.Authority {
	t.Helper()
	a := f.store.Authority()
	promoted, err := f.store.Promote(ctx, replication.PromoteRequest{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed, RequestID: "anchor-history-promotion"})
	if err != nil || !promoted.Writable() || promoted.Term.Epoch != a.Term.Epoch+1 {
		t.Fatal("successor promotion", err)
	}
	return promoted
}

// anchorHistoryElect physically reopens the promoted database under a running
// runtime and an actual elector, so the promoted term has a writer that can
// operate for the next 25 seconds.
func anchorHistoryElect(ctx context.Context, t *testing.T, f *anchorFollowerFixture, promoted replication.Authority) *anchorHistoryWriter {
	t.Helper()
	f.reopen(ctx, t)
	runServingFixture(t, f.s)
	original, target, host := anchorElectedWriter(t, f.s)
	if !target.Term.Same(promoted.Term) {
		t.Fatal("reopened successor is not the writer of its promoted term")
	}
	return newAnchorHistoryWriter(original, t, f.s, target, host)
}

// anchorHistorySource is the member transport reduced to its content: the
// current primary's complete image, or not-modified for the caller's ETag.
type anchorHistorySource struct{ primary func() *sqlite.Store }

func (s anchorHistorySource) Fetch(ctx context.Context, etag string) (follower.Response, error) {
	image, err := s.primary().ReadGeneration(ctx)
	if err != nil {
		return follower.Response{}, err
	}
	if image.Generation().ETag() == etag {
		return follower.Response{Term: image.Term, ETag: etag, NotModified: true}, nil
	}
	return follower.Response{Term: image.Term, Image: image, ETag: image.Generation().ETag()}, nil
}

// anchorHistoryPublished captures the current publication and reports what it
// carries. emit drives one answer through the production output guard.
func anchorHistoryPublished(ctx context.Context, t *testing.T, s *servingRuntime) (seal *recursiveRootSeal, dnsOnly bool, emit error) {
	t.Helper()
	lease, err := s.publisher.Capture()
	if err != nil {
		return nil, false, err
	}
	defer lease.Release()
	part, _ := lease.Bundle().Component(recursiveRootComponent)
	seal, _ = part.(*recursiveRootSeal)
	// A managed output needs a finite original lifetime of at most 30 s.
	query, done := context.WithTimeout(ctx, 10*time.Second)
	defer done()
	output, err := captureRootAnchorOutput(&pipeline.Request{Ctx: query, Serving: servingdns.Wrap(lease)})
	if err != nil {
		return seal, lease.Bundle().DNSOnly(), err
	}
	return seal, lease.Bundle().DNSOnly(), output.Emit(func() error { return nil })
}

func anchorHistoryAwait(t *testing.T, what string, s *servingRuntime, root *anchorHistoryAuthority, predicate func() bool) {
	t.Helper()
	until := time.Now().Add(20 * time.Second)
	for !predicate() {
		if time.Now().After(until) {
			status := s.publisher.Status()
			t.Fatalf("%s: reason=%q dnsOnly=%v protected=%v mode=%s root reads=%d", what, status.Reason, status.DNSOnly, status.ProtectedReady, status.Authority.Mode, root.reads.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func anchorHistoryProjection(ctx context.Context, t *testing.T, s *servingRuntime) resolverstate.Projection {
	t.Helper()
	projection, err := resolverstate.Project(anchorOperationOwner(ctx, t, s))
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

// The mounted form of the store-level history tests. An old primary runs one
// root operation its follower never pulls; the follower is promoted and, in the
// second case, its own writer runs one too. The old primary then learns of the
// promoted term and the production puller installs the promoted branch, whose
// owner generation is lower than or equal to the old primary's own. Before
// D369's history rule that install was refused for as long as the promoted
// writer's generation did not pass the old primary's, and the old primary
// stayed fenced and DNS-only.
func TestAnchorHistoryRejoiningPrimaryAdoptsPromotedBranchAndPublishes(t *testing.T) {
	for _, branch := range []string{"lower-generation", "equal-generation"} {
		t.Run(branch, func(t *testing.T) {
			root := newAnchorHistoryAuthority(t)
			s, original, target, host := anchorOperationFixtureTopology(t, root.endpoint, root.seeds, "direct")
			old := newAnchorHistoryWriter(original, t, s, target, host)
			shared := old.operate(t, root, false) // bootstrap
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			t.Cleanup(cancel)
			f := newAnchorFollowerFixture(ctx, t, s)
			ahead := old.operate(t, root, false) // never pulled by the successor
			if ahead.Generation != shared.Generation+1 || anchorOperationOwner(ctx, t, f.s).Version() != shared {
				t.Fatal("fixture old primary is not one generation ahead of its follower")
			}
			promoted := anchorHistoryPromote(ctx, t, f)
			canonical := shared
			if branch == "equal-generation" {
				canonical = anchorHistoryElect(ctx, t, f, promoted).operate(t, root, false)
				if canonical.Generation != ahead.Generation || canonical == ahead {
					t.Fatal("fixture branches did not diverge at one generation")
				}
			}
			polls := root.reads.Load()
			// The old primary learns of the promoted term, as a member exchange
			// does, and keeps serving its own branch as DNS-only last-good data.
			if _, err := s.opts.Store.ObserveAuthority(ctx, promoted.Term); err != nil {
				t.Fatal(err)
			}
			fenced := s.opts.Store.Authority()
			if fenced.Mode != replication.Fenced || !fenced.Term.Same(promoted.Term) {
				t.Fatal("old primary was not fenced by the promoted term", fenced.Mode)
			}
			lost := anchorFollowerImage(ctx, t, s.opts.Store)
			anchorHistoryAwait(t, "fenced old primary did not publish DNS-only with one supervision poll", s, root, func() bool {
				return s.publisher.Status().Reason == "dns_only" && root.reads.Load() == polls+1
			})
			seal, dnsOnly, err := anchorHistoryPublished(ctx, t, s)
			if err != nil || seal == nil || seal.managed == nil || seal.static != nil || !seal.current() || !dnsOnly || !seal.base.Same(lost.Generation()) {
				t.Fatal("fenced old primary did not serve its own last accepted managed owner", err)
			}
			// The scheduled pull, through the production puller.
			puller := &follower.Puller{Mirror: s.opts.Store, Source: anchorHistorySource{primary: func() *sqlite.Store { return f.store }}}
			if err := puller.Pull(ctx); err != nil {
				status := s.publisher.Status()
				t.Fatalf("old primary at owner generation %d cannot install the promoted branch at generation %d: %v (publication reason=%q dnsOnly=%v)", ahead.Generation, canonical.Generation, err, status.Reason, status.DNSOnly)
			}
			rejoined := s.opts.Store.Authority()
			image := anchorFollowerImage(ctx, t, s.opts.Store)
			if rejoined.Mode != replication.Follower || !rejoined.Term.Same(promoted.Term) || rejoined.MemberID != fenced.MemberID || !anchorFollowerSameImage(image, anchorFollowerImage(ctx, t, f.store)) {
				t.Fatal("old primary did not become a follower on the promoted image", rejoined.Mode)
			}
			if anchorOperationOwner(ctx, t, s).Version() != canonical || anchorOperationOwner(ctx, t, f.s).Version() != canonical {
				t.Fatal("members did not converge on the promoted branch's owner")
			}
			// Complete publication resumes on the adopted owner, with exactly one
			// supervision poll for the new follower role.
			anchorHistoryAwait(t, "rejoined old primary did not resume complete publication with one new supervision poll", s, root, func() bool {
				status := s.publisher.Status()
				return status.ProtectedReady && !status.DNSOnly && root.reads.Load() == polls+2
			})
			seal, dnsOnly, err = anchorHistoryPublished(ctx, t, s)
			if err != nil || seal == nil || seal.managed == nil || seal.static != nil || !seal.current() || dnsOnly || !seal.base.Same(image.Generation()) {
				t.Fatal("rejoined old primary did not publish the adopted managed owner", err)
			}
			// The abandoned branch is retained, and the next pull changes nothing.
			quarantined, err := s.opts.Store.ReadQuarantined(ctx, lost.Generation())
			if err != nil {
				t.Fatal("abandoned branch was not quarantined", err)
			}
			if owner, err := accepted.DecodeRows(quarantined.Payload.Runtime, quarantined.Term.ClusterID); err != nil || owner.Version() != ahead {
				t.Fatal("quarantine does not hold the abandoned owner", err)
			}
			if err := puller.Pull(ctx); err != nil {
				t.Fatal("converged pull", err)
			}
			time.Sleep(1500 * time.Millisecond)
			if root.reads.Load() != polls+2 || !reflect.DeepEqual(rejoined, s.opts.Store.Authority()) || !anchorFollowerSameImage(image, anchorFollowerImage(ctx, t, s.opts.Store)) {
				t.Fatal("converged member reacquired or changed", root.reads.Load())
			}
			if status := s.publisher.Status(); !status.ProtectedReady || status.DNSOnly {
				t.Fatal("converged member lost complete publication", status.Reason)
			}
		})
	}
}

// A revocation only the old primary saw must survive its return. The promoted
// branch still trusts the key, so the old primary refuses that branch (D369)
// and keeps answering from its own branch, which already revoked the key, as
// DNS-only last-good data; its own supervision cannot change that. Only when
// the promoted writer has observed the same signed REVOKE does the next pull
// adopt the branch, and the member publishes completely without the key.
func TestAnchorHistoryRejoiningPrimaryKeepsItsRevocationUntilCanonical(t *testing.T) {
	root := newAnchorHistoryAuthority(t)
	s, original, target, host := anchorOperationFixtureTopology(t, root.endpoint, root.seeds, "direct")
	old := newAnchorHistoryWriter(original, t, s, target, host)
	shared := old.operate(t, root, false) // bootstrap with both keys
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	f := newAnchorFollowerFixture(ctx, t, s)
	root.revoke.Store(true)
	revoked := old.operate(t, root, true)
	local := anchorHistoryProjection(ctx, t, s)
	if local.Disposition() != resolverstate.Usable || len(local.RevokedIDs()) != 1 || !reflect.DeepEqual(local.DS(), []string{root.kept}) {
		t.Fatal("fixture old primary did not revoke exactly the first key")
	}
	key := local.RevokedIDs()[0]
	// The successor is promoted from the image before the revocation. Its new
	// writer has not acquired anything yet.
	promoted := anchorHistoryPromote(ctx, t, f)
	if p := anchorHistoryProjection(ctx, t, f.s); len(p.RevokedIDs()) != 0 || len(p.DS()) != 2 || anchorOperationOwner(ctx, t, f.s).Version() != shared {
		t.Fatal("fixture successor already holds the revocation")
	}
	polls := root.reads.Load()
	if _, err := s.opts.Store.ObserveAuthority(ctx, promoted.Term); err != nil {
		t.Fatal(err)
	}
	fenced := s.opts.Store.Authority()
	if fenced.Mode != replication.Fenced || !fenced.Term.Same(promoted.Term) {
		t.Fatal("old primary was not fenced by the promoted term", fenced.Mode)
	}
	lost := anchorFollowerImage(ctx, t, s.opts.Store)
	// Fenced, it still serves its own branch, which no longer trusts the key.
	// Its supervision poll sees the REVOKE of a key that branch already revoked.
	anchorHistoryAwait(t, "fenced old primary did not publish DNS-only with one supervision poll", s, root, func() bool {
		return s.publisher.Status().Reason == "dns_only" && root.reads.Load() == polls+1
	})
	// While the promoted branch still trusts the key, every scheduled pull is
	// refused before any write. The member stays fenced on its own branch, and
	// the production output guard keeps emitting DNS-only answers from it.
	puller := &follower.Puller{Mirror: s.opts.Store, Source: anchorHistorySource{primary: func() *sqlite.Store { return f.store }}}
	refused := func(stage string) {
		t.Helper()
		if err := puller.Pull(ctx); !errors.Is(err, accepted.ErrCAS) {
			t.Fatalf("%s: old primary adopted a branch that trusts its revoked key: %v", stage, err)
		}
		if a := s.opts.Store.Authority(); !reflect.DeepEqual(a, fenced) || anchorOperationOwner(ctx, t, s).Version() != revoked || !anchorFollowerSameImage(lost, anchorFollowerImage(ctx, t, s.opts.Store)) {
			t.Fatal(stage, "refused pull changed the old primary", a.Mode)
		}
		if _, err := s.opts.Store.ReadQuarantined(ctx, lost.Generation()); !errors.Is(err, store.ErrNotFound) {
			t.Fatal(stage, "refused pull quarantined the old primary's own branch", err)
		}
		seal, dnsOnly, emit := anchorHistoryPublished(ctx, t, s)
		if emit != nil || seal == nil || seal.managed == nil || seal.static != nil || !seal.current() || !dnsOnly || !seal.base.Same(lost.Generation()) {
			t.Fatal(stage, "refused member does not answer DNS-only from its own managed owner", emit)
		}
		if p := anchorHistoryProjection(ctx, t, s); !reflect.DeepEqual(p.RevokedIDs(), []string{key}) || !reflect.DeepEqual(p.DS(), []string{root.kept}) {
			t.Fatal(stage, "refused member trusts the revoked key")
		}
		if status := s.publisher.Status(); status.ProtectedReady || status.Reason != "dns_only" {
			t.Fatal(stage, "refused member left DNS-only", status.Reason)
		}
		if _, err := s.publisher.CaptureHTTP(ctx); err == nil {
			t.Fatal(stage, "fenced member admitted protected HTTP")
		}
	}
	refused("first pull")
	time.Sleep(1500 * time.Millisecond)
	refused("second pull")
	if root.reads.Load() != polls+1 {
		t.Fatal("a refused pull started supervision again", root.reads.Load())
	}
	// The canonical writer observes the same signed REVOKE.
	tombstone := anchorHistoryElect(ctx, t, f, promoted).operate(t, root, true)
	if tombstone == revoked || tombstone.Generation != shared.Generation+1 {
		t.Fatal("fixture canonical tombstone is not the promoted branch's own successor")
	}
	if err := puller.Pull(ctx); err != nil {
		t.Fatal("canonical tombstone was not adopted", err)
	}
	image := anchorFollowerImage(ctx, t, s.opts.Store)
	if a := s.opts.Store.Authority(); a.Mode != replication.Follower || !a.Term.Same(promoted.Term) {
		t.Fatal("old primary did not follow the promoted term", a.Mode)
	}
	if anchorOperationOwner(ctx, t, s).Version() != tombstone || !anchorFollowerSameImage(image, anchorFollowerImage(ctx, t, f.store)) {
		t.Fatal("members did not converge on the canonical tombstone")
	}
	anchorHistoryAwait(t, "member did not publish completely after adopting the canonical tombstone", s, root, func() bool {
		status := s.publisher.Status()
		return status.ProtectedReady && !status.DNSOnly && status.Served.Base.Same(image.Generation())
	})
	seal, dnsOnly, err := anchorHistoryPublished(ctx, t, s)
	if err != nil || seal == nil || seal.managed == nil || seal.static != nil || !seal.current() || dnsOnly || !seal.base.Same(image.Generation()) {
		t.Fatal("member did not publish the canonical owner", err)
	}
	if p := anchorHistoryProjection(ctx, t, s); !reflect.DeepEqual(p.RevokedIDs(), []string{key}) || !reflect.DeepEqual(p.DS(), []string{root.kept}) {
		t.Fatal("member adopted an owner with the revoked key")
	}
	quarantined, err := s.opts.Store.ReadQuarantined(ctx, lost.Generation())
	if err != nil {
		t.Fatal("abandoned branch was not quarantined", err)
	}
	if owner, err := accepted.DecodeRows(quarantined.Payload.Runtime, quarantined.Term.ClusterID); err != nil || owner.Version() != revoked {
		t.Fatal("quarantine does not hold the abandoned owner", err)
	}
}

// The mounted form of the dormant-primary refusal and of the action that ends
// it. The old primary starts managed anchors after its follower's last pull and
// the follower is promoted with a dormant owner. The old primary refuses that
// primary's data, since managed history is never replaced by a dormant owner,
// and keeps answering DNS-only from its own branch; the offline review an
// operator can run on such a member shows a fenced member that holds no image
// of the term it follows. When the promoted primary starts managed anchors
// from the same configured seeds, through the production root operation, the
// next production pull adopts that branch and the member publishes completely.
func TestAnchorHistoryManagedMemberFollowsOnceThePrimaryStartsManagedAnchors(t *testing.T) {
	root := newAnchorHistoryAuthority(t)
	s, original, target, host := anchorOperationFixtureTopology(t, root.endpoint, root.seeds, "direct")
	old := newAnchorHistoryWriter(original, t, s, target, host)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	// The follower's image predates the old primary's bootstrap.
	f := newAnchorFollowerFixture(ctx, t, s)
	if owner := anchorOperationOwner(ctx, t, f.s); owner.Inspect().Phase != accepted.Dormant {
		t.Fatal("fixture follower does not hold a dormant owner")
	}
	own := old.operate(t, root, false)
	promoted := anchorHistoryPromote(ctx, t, f)
	polls := root.reads.Load()
	if _, err := s.opts.Store.ObserveAuthority(ctx, promoted.Term); err != nil {
		t.Fatal(err)
	}
	fenced := s.opts.Store.Authority()
	if fenced.Mode != replication.Fenced || !fenced.Term.Same(promoted.Term) {
		t.Fatal("old primary was not fenced by the promoted term", fenced.Mode)
	}
	lost := anchorFollowerImage(ctx, t, s.opts.Store)
	anchorHistoryAwait(t, "fenced old primary did not publish DNS-only with one supervision poll", s, root, func() bool {
		return s.publisher.Status().Reason == "dns_only" && root.reads.Load() == polls+1
	})
	puller := &follower.Puller{Mirror: s.opts.Store, Source: anchorHistorySource{primary: func() *sqlite.Store { return f.store }}}
	refused := func(stage string) {
		t.Helper()
		if err := puller.Pull(ctx); !errors.Is(err, accepted.ErrCAS) {
			t.Fatalf("%s: old primary replaced its managed history with a dormant owner: %v", stage, err)
		}
		if a := s.opts.Store.Authority(); !reflect.DeepEqual(a, fenced) || anchorOperationOwner(ctx, t, s).Version() != own || !anchorFollowerSameImage(lost, anchorFollowerImage(ctx, t, s.opts.Store)) {
			t.Fatal(stage, "refused pull changed the old primary", a.Mode)
		}
		seal, dnsOnly, emit := anchorHistoryPublished(ctx, t, s)
		if emit != nil || seal == nil || seal.managed == nil || seal.static != nil || !seal.current() || !dnsOnly || !seal.base.Same(lost.Generation()) {
			t.Fatal(stage, "refused member does not answer DNS-only from its own managed owner", emit)
		}
		if _, err := s.publisher.CaptureHTTP(ctx); err == nil {
			t.Fatal(stage, "fenced member admitted protected HTTP")
		}
		// What `cluster resolution --offline` reports for this database.
		review, err := offlineResolutionReview(ctx, s.opts.Store, "old-primary")
		if err != nil || review.Mode != string(replication.Fenced) || !review.Term.Same(promoted.Term) || !review.Installed.Same(lost.Generation()) || review.Installed.Term.Epoch >= review.Term.Epoch || review.Resolvable || review.Reason != "no_conflict" {
			t.Fatal(stage, "offline review does not show a fenced member without an image of its term", err, review.Mode, review.Reason)
		}
	}
	refused("first pull")
	// The promoted primary starts managed anchors from the same configuration.
	canonical := anchorHistoryElect(ctx, t, f, promoted).operate(t, root, false)
	if canonical == own || canonical.Generation != own.Generation {
		t.Fatal("fixture primary did not start another branch at the same generation")
	}
	if a, b := anchorOperationOwner(ctx, t, s).Inspect(), anchorOperationOwner(ctx, t, f.s).Inspect(); a.HistoryID != b.HistoryID || a.PolicyID != b.PolicyID || a.SeedDigest != b.SeedDigest {
		t.Fatal("fixture branches do not share history, policy and seeds")
	}
	if err := puller.Pull(ctx); err != nil {
		t.Fatal("old primary cannot adopt the primary's first managed owner", err)
	}
	image := anchorFollowerImage(ctx, t, s.opts.Store)
	if a := s.opts.Store.Authority(); a.Mode != replication.Follower || !a.Term.Same(promoted.Term) || a.MemberID != fenced.MemberID {
		t.Fatal("old primary did not follow the promoted term", a.Mode)
	}
	if anchorOperationOwner(ctx, t, s).Version() != canonical || !anchorFollowerSameImage(image, anchorFollowerImage(ctx, t, f.store)) {
		t.Fatal("members did not converge on the primary's managed owner")
	}
	anchorHistoryAwait(t, "member did not publish completely after adopting the primary's managed owner", s, root, func() bool {
		status := s.publisher.Status()
		return status.ProtectedReady && !status.DNSOnly && status.Served.Base.Same(image.Generation())
	})
	seal, dnsOnly, err := anchorHistoryPublished(ctx, t, s)
	if err != nil || seal == nil || seal.managed == nil || seal.static != nil || !seal.current() || dnsOnly || !seal.base.Same(image.Generation()) {
		t.Fatal("member did not publish the adopted managed owner", err)
	}
	quarantined, err := s.opts.Store.ReadQuarantined(ctx, lost.Generation())
	if err != nil {
		t.Fatal("abandoned branch was not quarantined", err)
	}
	if owner, err := accepted.DecodeRows(quarantined.Payload.Runtime, quarantined.Term.ClusterID); err != nil || owner.Version() != own {
		t.Fatal("quarantine does not hold the abandoned owner", err)
	}
	if review, err := offlineResolutionReview(ctx, s.opts.Store, "old-primary"); err != nil || review.Mode != string(replication.Follower) || !review.Installed.Term.Same(review.Term) {
		t.Fatal("offline review does not show a member that follows", err, review.Mode)
	}
}
