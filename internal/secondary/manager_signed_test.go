package secondary

import (
	"crypto"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// Exercise the actual acquisition, publication and durable restart paths. The
// signing key is test-only; a secondary preserves signatures without claiming
// to authenticate their chain of trust.
func TestManagerSignedAcquireRenewRejectAndRestart(t *testing.T) {
	clock := newManagerClock()
	records, key := managerSignedRecords(t, clock.now())
	receiver := &managerReceiver{serial: 42, records: records}
	path := filepath.Join(t.TempDir(), "local.db")
	db, err := localdb.OpenSnapshotStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m, _, dz := managerFor(t, clock, receiver, db)
	stop := runManager(t, m)
	initial := awaitManager(t, m, dz, func(st Status) bool { return st.Available })
	lookup := func(manager *Manager, signed bool) zone.Result {
		t.Helper()
		z := manager.Snapshot().Lookup(zone.SecondaryIdentity(dz))
		if z == nil || !z.HasSignatures() {
			t.Fatal("signed copy not published")
		}
		res, err := z.LookupSignedWithin("www.example.", dns.TypeA, 0, nil, signed, clock.now())
		if err != nil {
			t.Fatal(err)
		}
		if res.Kind != zone.Success {
			t.Fatalf("lookup kind %v", res.Kind)
		}
		return res
	}
	verify := func(res zone.Result) {
		t.Helper()
		var answer []dns.RR
		var signature *dns.RRSIG
		for _, rr := range res.Answer {
			if sig, ok := rr.(*dns.RRSIG); ok {
				signature = sig
			} else {
				answer = append(answer, rr)
			}
		}
		if signature == nil || signature.Verify(key, answer) != nil {
			t.Fatal("published answer did not preserve a valid signature")
		}
	}
	verify(lookup(m, true))
	old := m.Snapshot().Lookup(zone.SecondaryIdentity(dz))
	clock.advance(10 * time.Second)
	m.Wake()
	renewed := awaitManager(t, m, dz, func(st Status) bool { return st.ConfirmedAt.After(initial.ConfirmedAt) })
	if receiver.count() != 1 || !old.NextExpiry().Equal(initial.ExpiresAt) {
		t.Fatal("SOA confirmation transferred again or mutated a captured copy")
	}
	verify(lookup(m, true))
	// A newer serial with an incomplete denial chain cannot replace good data.
	receiver.mu.Lock()
	receiver.serial, receiver.records = 43, managerRecords(43)
	receiver.records = append(receiver.records, dns.Copy(records[len(records)-1]))
	receiver.mu.Unlock()
	clock.advance(10 * time.Second)
	m.Wake()
	failed := awaitManager(t, m, dz, func(st Status) bool { return st.Error != "" })
	if failed.Serial != 42 || !failed.ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatalf("bad signed transfer replaced good copy: %+v", failed)
	}
	verify(lookup(m, true))
	stop()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = localdb.OpenSnapshotStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	m.opts.Blobs = db
	receiver.set(42, errors.New("offline"))
	clock.advance(6 * time.Second) // Signatures expire at 25s; zone expires at 40s.
	restarted, err := New(m.opts)
	if err != nil {
		t.Fatal(err)
	}
	runManager(t, restarted)
	restored := awaitManager(t, restarted, dz, func(st Status) bool { return st.Error != "" })
	if !restored.Available || !restored.ExpiresAt.Equal(renewed.ExpiresAt) || !restored.ConfirmedAt.Equal(renewed.ConfirmedAt) {
		t.Fatalf("restart renewed expiry or lost signed copy: %+v", restored)
	}
	lookup(restarted, false)
	z := restarted.Snapshot().Lookup(zone.SecondaryIdentity(dz))
	if _, err := z.LookupSignedWithin("www.example.", dns.TypeA, 0, nil, true, clock.now()); err == nil {
		t.Fatal("restart or SOA confirmation renewed expired signatures")
	}
	raw, err := z.LookupSignedWithin("www.example.", dns.TypeRRSIG, 0, nil, true, clock.now())
	if err != nil || len(raw.Answer) == 0 {
		t.Fatalf("expired diagnostic signatures were lost: %v", err)
	}
	clock.advance(14 * time.Second)
	if z.AvailableAt(clock.now()) {
		t.Fatal("signed copy survived SOA expiry")
	}
}

func managerSignedRecords(t *testing.T, now time.Time) ([]dns.RR, *dns.DNSKEY) {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 300}, Flags: 257, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal(err)
	}
	records := append(managerRecords(42), key)
	type rrkey struct {
		owner string
		typ   uint16
	}
	sets := map[rrkey][]dns.RR{}
	bitmaps := map[string][]uint16{}
	for _, rr := range records {
		k := rrkey{rr.Header().Name, rr.Header().Rrtype}
		sets[k] = append(sets[k], rr)
		bitmaps[k.owner] = append(bitmaps[k.owner], k.typ)
	}
	// Canonical DNS order for this fixture's apex and single-label children.
	names := []string{"example.", "ns.example.", "raw.example.", "www.example."}
	for i, name := range names {
		bits := append(bitmaps[name], dns.TypeRRSIG, dns.TypeNSEC)
		slices.Sort(bits)
		nsec := &dns.NSEC{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: 300}, NextDomain: names[(i+1)%len(names)], TypeBitMap: bits}
		records = append(records, nsec)
		sets[rrkey{name, dns.TypeNSEC}] = []dns.RR{nsec}
	}
	for _, set := range sets {
		sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: set[0].Header().Name, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: 300}, Algorithm: key.Algorithm, KeyTag: key.KeyTag(), SignerName: key.Hdr.Name, Inception: uint32(now.Add(-time.Hour).Unix()), Expiration: uint32(now.Add(25 * time.Second).Unix())} //nolint:gosec // Fixed present-day test fixture in the RFC serial-time range.
		if err := sig.Sign(private.(crypto.Signer), set); err != nil {
			t.Fatal(err)
		}
		records = append(records, sig)
	}
	return records, key
}
