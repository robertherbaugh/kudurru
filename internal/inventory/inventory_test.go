package inventory

import (
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

func observation(id, ip string, at time.Time) Client {
	return Client{ID: id, IPs: []netip.Addr{netip.MustParseAddr(ip)}, MACs: []string{"02-00-00-00-00-01"}, Name: "Laptop", Hostname: "LAPTOP.LAN.", Tags: []string{"kids"}, ObservedAt: at, ExpiresAt: at.Add(time.Minute)}
}

func TestReplacementPinsIdentityAndNeverResurrectsAnOldIP(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	h := New(func() time.Time { return now })
	ip := netip.MustParseAddr("192.0.2.1")
	c := observation("old", ip.String(), now)
	if err := h.Replace("unifi", 1, []Client{c}); err != nil {
		t.Fatal(err)
	}
	first := h.Snapshot()
	c.MACs[0], c.Tags[0], c.IPs[0] = "corrupted", "corrupted", netip.Addr{}
	got := first.Lookup(ip, now)
	if got == nil || got.MACs[0] != "02:00:00:00:00:01" || !slices.Equal(got.Names(), []string{"laptop", "laptop.lan"}) || got.Tags[0] != "kids" {
		t.Fatalf("input alias or normalization: %+v", got)
	}
	now = now.Add(time.Second)
	if err := h.Replace("dhcp", 1, []Client{observation("new", ip.String(), now)}); err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(ip, now).ID != "new" || first.Lookup(ip, now).ID != "old" {
		t.Fatal("reassignment mutated a captured snapshot")
	}
	now = now.Add(time.Second)
	if err := h.Replace("dhcp", 2, nil); err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(ip, now) != nil {
		t.Fatal("removal resurrected an older claim")
	}
	if err := h.Replace("unifi", 2, []Client{observation("old", ip.String(), now.Add(-2*time.Second))}); err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(ip, now) != nil {
		t.Fatal("repeated stale poll reclaimed IP")
	}
	now = now.Add(time.Second)
	if err := h.Replace("unifi", 3, []Client{observation("old", ip.String(), now)}); err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(ip, now) == nil {
		t.Fatal("fresh actual observation cannot reclaim IP")
	}
	if h.Snapshot().Lookup(ip, now.Add(time.Minute)) != nil {
		t.Fatal("expired inventory still enriches")
	}
}

func TestAmbiguousClaimsAndAtomicInvalidBatch(t *testing.T) {
	now := time.Now()
	h := New(func() time.Time { return now })
	ip := netip.MustParseAddr("192.0.2.4")
	for _, source := range []string{"a", "b"} {
		if err := h.Replace(source, 1, []Client{observation(source, ip.String(), now)}); err != nil {
			t.Fatal(err)
		}
	}
	if h.Snapshot().Lookup(ip, now) != nil {
		t.Fatal("equal claims must be ambiguous")
	}
	before := h.Snapshot()
	bad := observation("bad", ip.String(), now)
	bad.ExpiresAt = now.Add(25 * time.Hour)
	if err := h.Replace("a", 2, []Client{observation("good", "192.0.2.6", now), bad}); err == nil || h.Snapshot() != before {
		t.Fatal("invalid batch partially published")
	}
	if err := h.Replace("a", 1, nil); err == nil || h.Snapshot() != before {
		t.Fatal("old producer overwrote new inventory")
	}
	rows := before.Clients()
	rows[0].IPs[0] = netip.Addr{}
	if !before.Clients()[0].IPs[0].IsValid() {
		t.Fatal("API diagnostic copies alias shared snapshot")
	}
}

func TestExpiredNewestNeverFallsBackAndConcurrentReads(t *testing.T) {
	now := time.Now()
	h := New(func() time.Time { return now })
	ip := netip.MustParseAddr("2001:db8::4")
	old := observation("old", ip.String(), now.Add(-time.Minute))
	old.ExpiresAt = now.Add(time.Hour)
	if err := h.Replace("old", 1, []Client{old}); err != nil {
		t.Fatal(err)
	}
	c := observation("new", ip.String(), now)
	c.ExpiresAt = now.Add(time.Second)
	if err := h.Replace("new", 1, []Client{c}); err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().Lookup(ip, now.Add(time.Second)) != nil {
		t.Fatal("expired newest fell back to old owner")
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 1000 {
			_ = h.Snapshot().Lookup(ip, now)
		}
	})
	for i := uint64(2); i < 20; i++ {
		if err := h.Replace("new", i, []Client{c}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
