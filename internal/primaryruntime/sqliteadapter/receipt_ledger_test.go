package sqliteadapter

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// ledgerClock is the eviction clock of a fixture: the wall clock plus an
// offset that a test advances. Signature deadlines stay on the wall clock,
// which the adapter's own deadline check reads.
type ledgerClock struct{ offset atomic.Int64 }

func (c *ledgerClock) now() time.Time          { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *ledgerClock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// signed is f.request with a signature deadline and an optional
// prerequisite that the name is not in use, so that deciding the same
// request a second time could not answer NOERROR (RFC 2136 section 2.4.5).
func (f *fixture) signed(l *zk.ZoneLifecycle, retry, text string, notAfter time.Time, unused bool) pr.VerifiedUpdate {
	return f.signedUpdate(l, retry, text, notAfter, unused, !unused)
}

// signedUpdate is signed; replace makes the update replace the name's
// RRset of the record's type instead of adding to it.
func (f *fixture) signedUpdate(l *zk.ZoneLifecycle, retry, text string, notAfter time.Time, unused, replace bool) pr.VerifiedUpdate {
	f.t.Helper()
	m := new(dns.Msg)
	m.SetUpdate(l.Spec.Zone.Apex)
	rr, err := dns.NewRR(text)
	if err != nil {
		f.t.Fatal(err)
	}
	if unused {
		m.NameNotUsed([]dns.RR{rr})
	}
	if replace {
		m.RemoveRRset([]dns.RR{rr})
	}
	m.Insert([]dns.RR{rr})
	wire, err := m.Pack()
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := pr.SemanticDigest(wire)
	if err != nil {
		f.t.Fatal(err)
	}
	p := pr.ProofIdentity{Principal: dynamicupdate.Principal{Key: "key.", Version: "v1"}, ProviderVersion: "provider-v1", Algorithm: dns.HmacSHA256, Transport: "tcp", Domain: "dns-update:" + zk.HeadName(l.Spec.Zone), Subject: "key.", RetryID: retry, SemanticDigest: digest, Term: f.s.Authority().Term, NotAfter: notAfter, AuthorityIncarnation: f.s.Authority().Incarnation}
	return pr.VerifiedUpdate{Zone: l.Spec.Zone, Wire: wire, Proof: &issued{p: p}}
}

func (f *fixture) receipts(l *zk.ZoneLifecycle) []zonestate.Receipt {
	f.t.Helper()
	v, err := f.repo.Read(f.ctx(), l.Spec.HeadZone())
	if err != nil {
		f.t.Fatal(err)
	}
	return v.Aggregate.Receipts
}

// A zone's receipt ledger is bounded by the receipts that are still live,
// not by the zone's lifetime: a zone with room for 64 receipts takes 1,100
// committed UPDATEs, each commit dropping the receipts whose signature
// deadline (plus the clock margin) has passed. Before D409 the ledger was
// append-only and the 65th UPDATE answered SERVFAIL "retry ledger full",
// as did every later one.
func TestUpdateLedgerTakesUpdatesBeyondItsCapacity(t *testing.T) {
	clock := &ledgerClock{}
	f := setupAt(t, filepath.Join(t.TempDir(), "state.db"), fixtureOptions{Now: clock.now, Limits: zonestate.Limits{MaxReceipts: 64}})
	ctx := f.ctx()
	l := f.active("primary", "example.")
	const total = 1100
	// Each request is signed on the eviction clock (which runs ahead of the
	// wall clock the adapter checks deadlines on) and is valid for two
	// seconds; one is committed every two
	// seconds of the eviction clock, so with the one-minute margin about 32
	// receipts are live at once. Each update replaces one of 16 names, so
	// that the zone itself stays small.
	window := 2 * time.Second
	first, err := f.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	most := 0
	var v *zonestate.View
	for i := range total {
		req := f.signed(l, fmt.Sprintf("retry-%d", i), fmt.Sprintf("host-%d.example. 60 IN A 192.0.2.%d", i%16, 1+i%200), clock.now().Add(window), false)
		// A receipt that is not live before the commit is not live at it.
		before := clock.now()
		out := f.r.Apply(ctx, req)
		if out.Rcode != dns.RcodeSuccess || !out.Committed || !out.Published || out.Deduplicated {
			t.Fatalf("UPDATE %d of %d: %+v", i+1, total, out)
		}
		if v, err = f.repo.Read(ctx, l.Spec.HeadZone()); err != nil {
			t.Fatal(err)
		}
		most = max(most, len(v.Aggregate.Receipts))
		for _, receipt := range v.Aggregate.Receipts {
			if !receipt.Live(before) {
				t.Fatal("an UPDATE commit kept a receipt that is no longer live", receipt)
			}
		}
		clock.advance(2 * time.Second)
	}
	if most > 64 {
		t.Fatalf("ledger held %d receipts, more than its 64", most)
	}
	if commits := v.Head.Spec.CommitSequence - first.Head.Spec.CommitSequence; commits != total {
		t.Fatalf("%d commits for %d updates", commits, total)
	}
	t.Logf("%d committed UPDATEs, at most %d receipts held, %d at the end", total, most, len(v.Aggregate.Receipts))
}

// A repeat of a committed request (the same retry identity) inside its
// signature window is answered from the retained outcome: NOERROR, without
// a second decision (the request's own prerequisite would now refuse it)
// and without a durable change, also when other UPDATEs evicted older
// receipts meanwhile and after a restart. Once its window and the margin
// have passed, a later commit drops its receipt, and the request itself is
// refused by its signature deadline without any effect.
func TestUpdateLedgerRetainsLiveOutcomesAcrossEvictionAndRestart(t *testing.T) {
	clock := &ledgerClock{}
	path := filepath.Join(t.TempDir(), "state.db")
	f := setupAt(t, path, fixtureOptions{Now: clock.now, Limits: zonestate.Limits{MaxReceipts: 8}})
	ctx := f.ctx()
	l := f.active("primary", "example.")
	// short expires first; long stays live through the test.
	short := f.signed(l, "short", "short.example. 60 IN A 192.0.2.1", time.Now().Add(1500*time.Millisecond), true)
	long := f.signed(l, "long", "long.example. 60 IN A 192.0.2.2", time.Now().Add(time.Hour), true)
	for _, req := range []pr.VerifiedUpdate{short, long} {
		if out := f.r.Apply(ctx, req); out.Rcode != dns.RcodeSuccess || !out.Published {
			t.Fatal(out)
		}
	}
	// A new request with the same content is decided again, and its
	// prerequisite refuses it: that is what a repeat must not become.
	if out := f.r.Apply(ctx, f.signed(l, "fresh", "long.example. 60 IN A 192.0.2.2", time.Now().Add(time.Hour), true)); out.Rcode != dns.RcodeYXDomain || out.Committed {
		t.Fatal("a new request was not decided again", out)
	}
	before := snapshot(t, f.s)
	for _, req := range []pr.VerifiedUpdate{short, long} {
		if out := f.r.Apply(ctx, req); out.Rcode != dns.RcodeSuccess || !out.Deduplicated || !out.Published {
			t.Fatal("repeat inside its window was not answered from its retained outcome", out)
		}
	}
	if !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("a repeat changed durable state")
	}
	var shortSequence uint64
	for _, receipt := range f.receipts(l) {
		if receipt.NotAfter == zonestate.ReceiptNotAfter(short.Proof.(*issued).p.NotAfter) {
			shortSequence = receipt.CommitSequence
		}
	}
	if shortSequence == 0 {
		t.Fatal("no receipt with the request's signature deadline")
	}
	// Turn over the ledger with later updates while the clocks pass
	// short's deadline and margin, but not long's.
	waitUntil(t, time.Unix(zonestate.ReceiptNotAfter(short.Proof.(*issued).p.NotAfter), 0).Add(100*time.Millisecond))
	clock.advance(zonestate.ReceiptClockMargin + time.Second)
	for i := range 20 {
		if out := f.r.Apply(ctx, f.signed(l, fmt.Sprintf("later-%d", i), fmt.Sprintf("later-%d.example. 60 IN A 192.0.2.3", i), time.Now().Add(1500*time.Millisecond), false)); out.Rcode != dns.RcodeSuccess {
			t.Fatal("later update", i, out)
		}
		clock.advance(20 * time.Second)
	}
	for _, receipt := range f.receipts(l) {
		if receipt.CommitSequence == shortSequence {
			t.Fatal("an expired receipt was kept", receipt)
		}
	}
	if n := len(f.receipts(l)); n > 8 {
		t.Fatal("ledger over its capacity", n)
	}
	before = snapshot(t, f.s)
	if out := f.r.Apply(ctx, short); out.Rcode != dns.RcodeServerFailure || out.Committed || out.Deduplicated {
		t.Fatal("an expired request was decided again", out)
	}
	if !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("an expired repeat changed durable state")
	}
	// Restart: a cold store, adapter and runtime keep the rule from the
	// durable receipts alone.
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	cold := setupAt(t, path, fixtureOptions{Now: clock.now, Limits: zonestate.Limits{MaxReceipts: 8}})
	l, err := cold.r.ReadLifecycle(cold.ctx(), l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cold.r.Reconcile(cold.ctx(), l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	before = snapshot(t, cold.s)
	if out := cold.r.Apply(cold.ctx(), long); out.Rcode != dns.RcodeSuccess || !out.Deduplicated || !out.Published {
		t.Fatal("restart lost a live retained outcome", out)
	}
	if !bytes.Equal(before, snapshot(t, cold.s)) {
		t.Fatal("a repeat after restart changed durable state")
	}
	held := cold.receipts(l)
	var found bool
	for _, receipt := range held {
		if receipt.NotAfter == zonestate.ReceiptNotAfter(long.Proof.(*issued).p.NotAfter) {
			found = true
		}
	}
	if !found {
		t.Fatal("the live receipt was not retained across restart")
	}
}

// When the live receipts fill the ledger, an UPDATE is refused with
// SERVFAIL before anything is committed, and the refusal says why and when
// the ledger has room again; once receipts leave their window the zone
// takes UPDATEs again.
func TestUpdateLedgerRefusesOnlyWhileLiveReceiptsFillIt(t *testing.T) {
	clock := &ledgerClock{}
	f := setupAt(t, filepath.Join(t.TempDir(), "state.db"), fixtureOptions{Now: clock.now, Limits: zonestate.Limits{MaxReceipts: 4}})
	ctx := f.ctx()
	l := f.active("primary", "example.")
	deadline := time.Now().Add(time.Hour)
	for i := range 4 {
		if out := f.r.Apply(ctx, f.signed(l, fmt.Sprintf("r-%d", i), fmt.Sprintf("h-%d.example. 60 IN A 192.0.2.1", i), deadline, false)); out.Rcode != dns.RcodeSuccess {
			t.Fatal(i, out)
		}
	}
	before := snapshot(t, f.s)
	refused := f.r.Apply(ctx, f.signed(l, "r-4", "h-4.example. 60 IN A 192.0.2.1", deadline, false))
	if refused.Rcode != dns.RcodeServerFailure || refused.Committed || !errors.Is(refused.Err, pr.ErrCapacity) || !strings.Contains(fmt.Sprint(refused.Err), "4 updates of this zone are still within their signature window") {
		t.Fatal("full ledger", refused)
	}
	if !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("a refusal changed durable state")
	}
	clock.advance(time.Hour + zonestate.ReceiptClockMargin + time.Second)
	if out := f.r.Apply(ctx, f.signed(l, "r-4", "h-4.example. 60 IN A 192.0.2.1", time.Now().Add(time.Minute), false)); out.Rcode != dns.RcodeSuccess {
		t.Fatal("ledger stayed full after its receipts expired", out)
	}
	if n := len(f.receipts(l)); n != 1 {
		t.Fatal("expired receipts were kept", n)
	}
}

// The adapter decides eviction on its own clock: a runtime whose clock runs
// ahead cannot make a commit drop a receipt that is still live, and no
// receipt can be added without the request's own signature deadline.
func TestUpdateLedgerAdapterKeepsLiveReceipts(t *testing.T) {
	runtimeClock := &ledgerClock{}
	f := setupAt(t, filepath.Join(t.TempDir(), "state.db"), fixtureOptions{RuntimeNow: runtimeClock.now, Limits: zonestate.Limits{MaxReceipts: 8}})
	ctx := f.ctx()
	l := f.active("primary", "example.")
	if out := f.r.Apply(ctx, f.signed(l, "first", "first.example. 60 IN A 192.0.2.1", time.Now().Add(time.Hour), false)); out.Rcode != dns.RcodeSuccess {
		t.Fatal(out)
	}
	runtimeClock.advance(2 * time.Hour)
	before := snapshot(t, f.s)
	if out := f.r.Apply(ctx, f.signed(l, "second", "second.example. 60 IN A 192.0.2.2", time.Now().Add(time.Hour), false)); out.Rcode != dns.RcodeServerFailure || out.Committed || !errors.Is(out.Err, pr.ErrInvalid) {
		t.Fatal("a commit dropped a live receipt", out)
	}
	if !bytes.Equal(before, snapshot(t, f.s)) {
		t.Fatal("refused eviction changed durable state")
	}
	if n := len(f.receipts(l)); n != 1 {
		t.Fatal("live receipt lost", n)
	}
}

// waitUntil blocks until the wall clock reaches at.
func waitUntil(t *testing.T, at time.Time) {
	t.Helper()
	if d := time.Until(at); d > 0 {
		select {
		case <-time.After(d):
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
}
