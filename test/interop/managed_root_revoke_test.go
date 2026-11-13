package interop

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
// updater's wall clock (see TestBINDManagedRootRevocation).
const refreshWindow = 45 * time.Second

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
// in the quarantine case (see below).
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
			spec := managedRootNode(t, dir, "managed", root.ap, seeds)
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
			if boot.Owner.NextRefresh == nil || time.Until(*boot.Owner.NextRefresh) > time.Hour {
				t.Fatal("no committed refresh within the one-hour minimum", boot.Owner.NextRefresh)
			}

			// Stage 2: the old KSK is revoked; the revoked key and the new KSK
			// sign the set.
			revoked := s.revoke(old)
			s.sign(2, []string{revoked, next, zsk}, []string{revoked, next, zsk}, "now-3600", "now+2592000")
			root.reload(t, 2)
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
			node = start(t, bin, spec, kudurrutest.Options{Env: []string{"KUDURRU_TEST_ANCHOR_UPDATER_AHEAD=" + ahead.String()}})
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
			node.Stop(15 * time.Second)
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
			if rescheduled := after.Owner.NextRefresh != nil && after.Owner.NextRefresh.After(*boot.Owner.NextRefresh); rescheduled != tc.bothSeeded {
				t.Fatalf("refresh after the revocation %v (before %v)", after.Owner.NextRefresh, boot.Owner.NextRefresh)
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
