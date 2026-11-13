package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// anchorFenceWindowFixture is the managed DNS-only fixture with a native
// recursive group validating from the accepted managed owner, against the
// signed private root on a real non-loopback interface.
func anchorFenceWindowFixture(t *testing.T) *managedDNSOnlyFixture {
	t.Helper()
	endpoint, seeds, _, _ := anchorRecursiveServer(t)
	return newManagedDNSOnlyFixture(t, endpoint, seeds, func(cfg *config.Config) {
		cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Mode: "recursive", Strategy: "parallel_best"}}
		cfg.Upstreams.Recursion.DNSSEC = true
		cfg.Cache.MaxEntries = 0 // every sample is a native validating lookup
		cfg.DNSSEC.Enabled = false
		cfg.Blocking.Enabled = false
		// Cluster settings come from the canonical image unless the host
		// locks them, as the fixture already does for the anchor settings.
		cfg.EnvLocked = append(cfg.EnvLocked, "upstreams.groups", "upstreams.recursion.dnssec", "cache.maxEntries", "dnssec.enabled", "blocking.enabled")
	})
}

// validated asks for the root DNSKEY set with recursion over UDP or TCP and
// reports why the answer is not AD-validated, or "" when it is.
func (f *managedDNSOnlyFixture) validated(tcp bool) string {
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	q.RecursionDesired = true
	q.SetEdns0(1232, true)
	m, err := f.net.exchange(q, tcp)
	if err != nil {
		return fmt.Sprintf("tcp=%v: %v", tcp, err)
	}
	if m.Rcode != dns.RcodeSuccess || !m.AuthenticatedData || len(m.Answer) != 2 {
		return fmt.Sprintf("tcp=%v: %s ad=%v answers=%v", tcp, dns.RcodeToString[m.Rcode], m.AuthenticatedData, m.Answer)
	}
	return ""
}

// holdWriteLock takes SQLite's write lock on the fixture's database from a
// second connection until release, so an authority write waits for it.
func holdWriteLock(t *testing.T, path string) (release func()) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?"+url.Values{"_pragma": {"busy_timeout(10000)"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(t.Context())
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
			_ = conn.Close()
			_ = db.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// observeHeld starts the fencing observation of a higher term while the
// database write lock is held and waits until the store has blocked writes.
func (f *managedDNSOnlyFixture) observeHeld(t *testing.T) <-chan error {
	t.Helper()
	f.writerLoss.Store(true)
	term := f.s.opts.Store.Authority().Term
	term.Epoch++
	term.PrimaryID, term.PromotionID = "other", "held-fence"
	done := make(chan error, 1)
	go func() {
		_, err := f.s.opts.Store.ObserveAuthority(t.Context(), term)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, blocked := f.s.opts.Store.ServingAuthority(); blocked {
			return done
		}
		if time.Now().After(deadline) {
			t.Fatal("the fence observation never blocked writes")
		}
		time.Sleep(time.Millisecond)
	}
}

// A managed member that observes a fencing term blocks its writers, then
// persists the new authority, then serves a DNS-only publication of the same
// last-good data and accepted owner (D360, D369). Until this package the write
// block alone retired its managed resolver input, so every validated recursive
// answer, and every authoritative one, failed for as long as that write took:
// the SERVFAIL the enrolled process samplers recorded at conflict detection.
// The write is held here for 1.5 s. Every sample over UDP and TCP from before
// the observation until the DNS-only publication is served, and after it,
// must be validated, and the zone must keep answering while the write is
// held. Writers and protected HTTP stay closed throughout.
func TestRootAnchorManagedRecursionAcrossAPersistedFence(t *testing.T) {
	f := anchorFenceWindowFixture(t)
	if !awaitTrue(20*time.Second, func() bool { return f.validated(false) == "" && f.validated(true) == "" }) {
		t.Fatal("writable managed fixture never validated native recursion", f.validated(false), f.validated(true))
	}
	release := holdWriteLock(t, f.s.opts.Store.Path())
	done := f.observeHeld(t)
	samples := 0
	sample := func(authoritative bool) {
		t.Helper()
		for _, tcp := range []bool{false, true} {
			if why := f.validated(tcp); why != "" {
				t.Fatalf("validated recursion failed during the fence (sample %d, mode %s): %s", samples, f.s.opts.Store.Authority().Mode, why)
			}
			if authoritative {
				if ok, rcode := f.authoritative(t, tcp); !ok {
					t.Fatalf("authoritative data failed while the fence was persisted (sample %d): %s", samples, dns.RcodeToString[rcode])
				}
			}
		}
		samples++
	}
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if mode := f.s.opts.Store.Authority().Mode; mode == replication.Fenced {
			t.Fatal("the fence was published while its write was held")
		}
		if _, err := f.s.publisher.CaptureHTTP(t.Context()); err == nil {
			t.Fatal("protected HTTP stayed open while writes were blocked")
		}
		sample(true)
	}
	held := samples
	release()
	if err := <-done; err != nil {
		t.Fatal("the held fence did not persist", err)
	}
	if a := f.s.opts.Store.Authority(); a.Mode != replication.Fenced {
		t.Fatal("member was not fenced", a.Mode)
	}
	deadline := time.Now().Add(15 * time.Second)
	for f.s.publisher.Status().Reason != "dns_only" {
		if time.Now().After(deadline) {
			t.Fatal("DNS-only publication never arrived", f.s.publisher.Status().Reason)
		}
		sample(false)
	}
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		sample(true)
	}
	t.Logf("%d validated samples on UDP and TCP, %d while the fence write was held", samples, held)
	if held < 10 {
		t.Fatal("too few samples inside the held window", held)
	}
}

// The other side of the same rule: when the fence cannot be persisted (the
// write lock outlasts the store's five-second busy timeout), the store stays
// blocked and the member cannot prove which authority it holds, so it answers
// no validated recursion and no zone data, as before this package.
func TestRootAnchorManagedRecursionStopsAfterAFailedFence(t *testing.T) {
	f := anchorFenceWindowFixture(t)
	if !awaitTrue(20*time.Second, func() bool { return f.validated(false) == "" && f.validated(true) == "" }) {
		t.Fatal("writable managed fixture never validated native recursion", f.validated(false), f.validated(true))
	}
	release := holdWriteLock(t, f.s.opts.Store.Path())
	done := f.observeHeld(t)
	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the fence observation outlived the busy timeout")
	}
	if err == nil {
		t.Fatal("the fence persisted while another connection held the write lock")
	}
	release()
	for _, tcp := range []bool{false, true} {
		q := new(dns.Msg)
		q.SetQuestion(".", dns.TypeDNSKEY)
		q.RecursionDesired = true
		q.SetEdns0(1232, true)
		m, err := f.net.exchange(q, tcp)
		if err == nil && (m.Rcode != dns.RcodeServerFailure || m.AuthenticatedData || len(m.Answer) != 0) {
			t.Fatalf("a member whose fence failed answered recursion: tcp=%v %s ad=%v answers=%d", tcp, dns.RcodeToString[m.Rcode], m.AuthenticatedData, len(m.Answer))
		}
		q = new(dns.Msg)
		q.SetQuestion("example.", dns.TypeSOA)
		m, err = f.net.exchange(q, tcp)
		if err == nil && (m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0) {
			t.Fatalf("a member whose fence failed answered its zone: tcp=%v %s answers=%d", tcp, dns.RcodeToString[m.Rcode], len(m.Answer))
		}
	}
}

func awaitTrue(within time.Duration, ok func() bool) bool {
	for end := time.Now().Add(within); ; time.Sleep(50 * time.Millisecond) {
		if ok() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}
