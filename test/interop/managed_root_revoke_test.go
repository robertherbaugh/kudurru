package interop

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// licensedInterop is the -tags licensetest build of this checkout, made once
// per run, or the prebuilt KUDURRU_E2E_LICENSED_BINARY. The revocation test
// needs its KUDURRU_TEST_ANCHOR_UPDATER_AHEAD seam
// (cmd/kudurru/anchor_updater_wall_licensetest.go); a release binary has none.
var licensedInterop struct {
	once     sync.Once
	dir, bin string
	err      error
}

func licensedInteropBinary(t *testing.T) string {
	t.Helper()
	b := &licensedInterop
	b.once.Do(func() {
		if b.dir, b.err = os.MkdirTemp("", "kudurru-interop-licensed-"); b.err != nil {
			return
		}
		b.bin, b.err = kudurrutest.BuildLicensed(b.dir)
	})
	if b.err != nil {
		t.Fatalf("licensed interop binary: %v", b.err)
	}
	id, err := kudurrutest.Describe(b.bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("licensed binary " + id)
	return b.bin
}

// refreshWindow is how long the boot must precede the restart that moves the
// updater's wall clock (see TestBINDManagedRootRevocation). After that
// restart the rescheduled refresh comes due again refreshWindow minus 10 s
// later, so startup, readiness, the acquisition wait, one validated answer
// and Stop share about 110 s, several times what the recorded runs need.
const refreshWindow = 120 * time.Second

// TestBINDManagedRootRevocation observes a BIND-made RFC 5011 revocation
// (dnssec-revoke: the REVOKE bit set and the set signed by the revoked key
// itself) at the updater's committed refresh:
//   - OnlyTrustedKey: the seed is the only trusted key and the new KSK is
//     still in its 30-day add hold-down. The revocation is immediate, the
//     pending key loses its only supporter and is dropped, and the node holds
//     no trusted key: it answers SERVFAIL, also after a restart.
//   - SecondSeedContinues: both KSKs are seeds (as the shipped IANA anchors
//     are). The revoked key becomes a permanent tombstone and validated
//     recursion continues through the other key, also after a restart.
//
// The refresh is due an hour after the bootstrap (TTL 3600, RFC 5011
// section 2.3). The node is restarted from a -tags licensetest build with
// KUDURRU_TEST_ANCHOR_UPDATER_AHEAD just past that instant, which moves
// only the updater's due comparison: the acquisition, its receipt sample,
// the authentication and the committed state are production behavior. The
// last restart uses no seam. Exactly two acquisitions are made, and a third
// in the quarantine case (see below). Both refresh instants are checked
// against the RFC 5011 interval computed from the TTL and signature
// expiration BIND publishes, and the node is required to stop before its
// moved clock reaches the rescheduled one. A binary without the seam fails
// at requireMovedUpdaterClock: its refresh is, correctly, an hour away
// (the hosted failures of November 13, 2026, D398).
func TestBINDManagedRootRevocation(t *testing.T) {
	kudurrutest.RequireE2E(t)
	t.Log("authority: " + namedVersion(t))
	ip := managedRootIP(t)
	for _, tc := range []struct {
		name       string
		bothSeeded bool
	}{{"OnlyTrustedKey", false}, {"SecondSeedContinues", true}} {
		t.Run(tc.name, func(t *testing.T) {
			bin := licensedInteropBinary(t)
			dir := t.TempDir()
			s := newRootSigner(t, dir, ip)
			old := s.key("ECDSAP256SHA256", 0, true)
			next := s.key("ECDSAP256SHA256", 0, true)
			zsk := s.key("ECDSAP256SHA256", 0, false)
			// Stage 1: the new KSK is published but does not sign yet.
			s.sign(1, []string{old, next, zsk}, []string{old, zsk}, "now-3600", "now+2592000")
			root := startBINDRoot(t, s, "")
			seeds := []string{s.ds(old)}
			if tc.bothSeeded {
				seeds = append(seeds, s.ds(next))
			}
			published := publishedTimers(t, root.addr)
			spec := managedRootNode(t, dir, "managed", root.ap, seeds)
			started := time.Now()
			node := kudurrutest.StartReady(t, bin, spec, kudurrutest.Options{})
			booted := time.Now()
			requireValidated(t, node, "after bootstrap")
			awaitAcquisitions(t, root, 1, "bootstrap")
			node.Stop(15 * time.Second)
			boot := offlineStatus(t, bin, spec)
			pending := "AddPend"
			if tc.bothSeeded {
				pending = "Valid"
			}
			if want := sortedKeys(t, old, "Valid", next, pending); boot.keys() != want {
				t.Fatalf("bootstrap keys %s, want %s", boot.keys(), want)
			}
			// The committed refresh is the RFC 5011 section 2.3 interval from
			// the timers this BIND publishes, counted from the bootstrap
			// acquisition (made between started and booted).
			if boot.Owner.NextRefresh == nil {
				t.Fatal("no committed refresh after the bootstrap")
			}
			earliest, latest := started.Add(published.refresh(started)), booted.Add(published.refresh(booted))
			if nr := *boot.Owner.NextRefresh; nr.Before(earliest.Add(-time.Second)) || nr.After(latest.Add(time.Second)) {
				t.Fatalf("committed refresh %v, want between %v and %v (%s)", nr, earliest, latest, published)
			}
			t.Logf("published %s: refresh %v after the bootstrap", published, published.refresh(booted))

			// Stage 2: the old KSK is revoked; the revoked key and the new KSK
			// sign the set.
			revoked := s.revoke(old)
			s.sign(2, []string{revoked, next, zsk}, []string{revoked, next, zsk}, "now-3600", "now+2592000")
			root.reload(t, 2)
			republished := publishedTimers(t, root.addr)
			if n := root.acquisitions(); n != 1 {
				t.Fatal("acquired before the refresh was due", n)
			}
			// The updater's moved clock is due again once the real time passes
			// the new refresh minus the shift, which is (restart - boot) minus
			// the 10 s margin after the restart. Keep that window well above
			// the checks below so that exactly one refresh is made.
			if wait := refreshWindow - time.Since(booted); wait > 0 {
				time.Sleep(wait)
			}
			ahead := time.Until(*boot.Owner.NextRefresh) + 10*time.Second
			// A node that loses every trusted key during startup never becomes
			// ready, so only the continuing case waits for readiness.
			start := kudurrutest.Start
			if tc.bothSeeded {
				start = kudurrutest.StartReady
			}
			restarting := time.Now()
			node = start(t, bin, spec, kudurrutest.Options{Env: []string{"KUDURRU_TEST_ANCHOR_UPDATER_AHEAD=" + ahead.String()}})
			requireMovedUpdaterClock(t, node)
			// The quarantining observation is authenticated only by the
			// revoked key itself, so it commits the tombstone without a new
			// refresh schedule (trustanchors.Advance reschedules only
			// authenticated observations). The committed refresh stays due
			// and the updater polls once more at once; with no trusted key
			// left that poll is unauthenticated and stops the updater for the
			// term. This also happens without the clock seam (the refresh was
			// due), and is recorded in D398.
			want := 2
			if !tc.bothSeeded {
				want = 3
			}
			awaitAcquisitions(t, root, want, "committed refresh")
			if tc.bothSeeded {
				requireValidated(t, node, "after the revocation")
			} else {
				requireServfail(t, node, "after losing every trusted key")
				if !kudurrutest.Eventually(10*time.Second, 50*time.Millisecond, func() bool {
					return strings.Contains(node.Log(), `msg="root anchor updater stopped for this writer term" err="trustanchors: unauthenticated root observation"`)
				}) {
					t.Fatal("the poll after the quarantine did not stop the updater")
				}
			}
			stopping := time.Now()
			node.Stop(15 * time.Second)
			stopped := time.Now()
			if n := root.acquisitions(); n != want {
				t.Fatal("unexpected refreshes", n, want)
			}
			after := offlineStatus(t, bin, spec)
			keys := sortedKeys(t, old, "Revoked")
			if tc.bothSeeded {
				keys = sortedKeys(t, old, "Revoked", next, "Valid")
			}
			if after.keys() != keys || after.Owner.Quarantined == tc.bothSeeded {
				t.Fatalf("after the revocation: keys %s quarantined %v, want %s", after.keys(), after.Owner.Quarantined, keys)
			}
			if after.Owner.NextRefresh == nil {
				t.Fatal("no committed refresh after the revocation")
			}
			if tc.bothSeeded {
				// The authenticated refresh is rescheduled by the published
				// timers. The moved clock reaches that instant at real time
				// NextRefresh - ahead; the node must have stopped before, or
				// a third acquisition was due and "exactly 2" would not hold.
				nr := *after.Owner.NextRefresh
				if lo, hi := restarting.Add(republished.refresh(restarting)), stopping.Add(republished.refresh(stopping)); nr.Before(lo) || nr.After(hi.Add(time.Second)) {
					t.Fatalf("refresh after the revocation %v, want between %v and %v (%s)", nr, lo, hi, republished)
				}
				if dueAgain := nr.Add(-ahead); !stopped.Before(dueAgain) {
					t.Fatalf("the node ran until %v, past %v when its moved clock reached the next refresh: raise refreshWindow", stopped, dueAgain)
				}
			} else if !after.Owner.NextRefresh.Equal(*boot.Owner.NextRefresh) {
				// The quarantining observation is not authenticated by a
				// trusted key and never reschedules.
				t.Fatalf("refresh after the quarantine %v, want the bootstrap's %v", after.Owner.NextRefresh, boot.Owner.NextRefresh)
			}

			// A production restart keeps the outcome and does not acquire: the
			// revocation is permanent and the next refresh is an hour away.
			node = kudurrutest.Start(t, bin, spec, kudurrutest.Options{})
			if tc.bothSeeded {
				requireValidated(t, node, "after a restart")
			} else {
				requireServfail(t, node, "quarantined after a restart")
			}
			node.Stop(15 * time.Second)
			if n := root.acquisitions(); n != want {
				t.Fatal("a restart acquired", n)
			}
			t.Logf("revocation of key %d: keys %s, quarantined %v, %d acquisitions", keyTag(t, old), after.keys(), after.Owner.Quarantined, want)
		})
	}
}

// rootTimers are the RFC 5011 section 2.3 inputs the authority publishes for
// the root DNSKEY set: its original TTL and the expiration of the
// signatures over it.
type rootTimers struct {
	ttl        uint32
	expiration time.Time
}

func (p rootTimers) String() string {
	return fmt.Sprintf("DNSKEY TTL %d s, signatures until %s", p.ttl, p.expiration.UTC().Format(time.RFC3339))
}

// refresh is the active refresh interval for an observation at: the larger
// of one hour and the smallest of 15 days, half the original TTL and half
// the remaining signature validity. It is written out here from the RFC,
// not taken from internal/trustanchors, so that the node's schedule is
// checked against the specification.
func (p rootTimers) refresh(at time.Time) time.Duration {
	return max(time.Hour, min(15*24*time.Hour, time.Duration(p.ttl)*time.Second/2, p.expiration.Sub(at)/2))
}

// publishedTimers reads the root DNSKEY set over UDP (which the
// managedAcquisition count never matches) and returns its timers. Every
// signature over the set must expire at the same second, so the interval
// does not depend on which key authenticated the observation.
func publishedTimers(t *testing.T, addr string) rootTimers {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	q.RecursionDesired = false
	q.SetEdns0(4096, true)
	c := &dns.Client{Net: "udp", Timeout: 3 * time.Second, UDPSize: 4096}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	m, _, err := c.ExchangeContext(ctx, q, addr)
	if err != nil || m.Rcode != dns.RcodeSuccess || m.Truncated {
		t.Fatalf("root DNSKEY over UDP: %v %v", err, m)
	}
	var p rootTimers
	for _, rr := range m.Answer {
		sig, ok := rr.(*dns.RRSIG)
		if !ok || sig.TypeCovered != dns.TypeDNSKEY {
			continue
		}
		until := time.Unix(int64(sig.Expiration), 0)
		if p.ttl != 0 && (sig.OrigTtl != p.ttl || !until.Equal(p.expiration)) {
			t.Fatalf("signatures over the root DNSKEY set disagree: %s", m)
		}
		p.ttl, p.expiration = sig.OrigTtl, until
	}
	if p.ttl == 0 {
		t.Fatalf("no signature over the root DNSKEY set: %s", m)
	}
	return p
}

// requireMovedUpdaterClock fails unless the node's writer updater logs that
// it runs on the moved clock. A binary without the licensetest seam (a
// prebuilt one, or a checkout without
// cmd/kudurru/anchor_updater_wall_licensetest.go) ignores
// KUDURRU_TEST_ANCHOR_UPDATER_AHEAD, and its refresh is correctly not due
// for an hour: that must fail here, not as a missing acquisition.
func requireMovedUpdaterClock(t *testing.T, node *kudurrutest.Node) {
	t.Helper()
	if !kudurrutest.Eventually(20*time.Second, 50*time.Millisecond, func() bool {
		return strings.Contains(node.Log(), `msg="root anchor updater runs its due checks on a moved test clock"`)
	}) {
		t.Fatal("the restarted node's updater does not run on the moved clock: the binary lacks the KUDURRU_TEST_ANCHOR_UPDATER_AHEAD seam (cmd/kudurru/anchor_updater_wall_licensetest.go)")
	}
}

// sortedKeys renders ECDSA P-256 key file/status pairs as
// offlineAnchors.keys does.
func sortedKeys(t *testing.T, pairs ...string) string {
	t.Helper()
	var out []string
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, fmt.Sprintf("%d/13/%s", keyTag(t, pairs[i]), pairs[i+1]))
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

// removeLicensedInterop removes what licensedInteropBinary built.
func removeLicensedInterop() {
	if licensedInterop.dir != "" {
		_ = os.RemoveAll(licensedInterop.dir)
	}
}
