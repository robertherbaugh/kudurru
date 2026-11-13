package inventory

import (
	"net/netip"
	"testing"
	"time"
)

// D397: the management view lists every device, flags staleness with the
// Lookup rule, and explains each address: assigned, expired, ambiguous tie,
// superseded by another device, or withdrawn by its own source.
func TestDevicesExplainStalenessAndAddressOwnership(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	h := New(func() time.Time { return now })
	if h.Snapshot().Devices(now) != nil {
		t.Fatal("empty inventory listed devices")
	}
	ip := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	dev := func(id string, at time.Time, life time.Duration, ips ...string) Client {
		c := Client{ID: id, ObservedAt: at, ExpiresAt: at.Add(life)}
		for _, s := range ips {
			c.IPs = append(c.IPs, ip(s))
		}
		return c
	}
	if err := h.Replace("a", 1, []Client{dev("quiet", now, time.Hour, "192.0.2.1"), dev("tie", now, time.Hour, "192.0.2.2"), dev("short", now, time.Minute, "192.0.2.3")}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := h.Replace("b", 1, []Client{dev("tie", now.Add(-time.Second), time.Hour, "192.0.2.2"), dev("newer", now, time.Hour, "192.0.2.1", "192.0.2.4")}); err != nil {
		t.Fatal(err)
	}
	read := func(at time.Time) map[string]Device {
		out := map[string]Device{}
		for _, d := range h.Snapshot().Devices(at) {
			out[d.Source+"/"+d.ID] = d
		}
		return out
	}
	got := read(now)
	if len(got) != 5 {
		t.Fatalf("devices: %d", len(got))
	}
	want := func(key string, stale bool, states ...AddressState) {
		t.Helper()
		d := got[key]
		if d.Stale != stale || len(d.Addresses) != len(states) {
			t.Fatalf("%s: %+v", key, d)
		}
		for i, s := range states {
			if d.Addresses[i].State != s {
				t.Fatalf("%s address %d: %+v", key, i, d.Addresses[i])
			}
		}
	}
	want("a/quiet", false, AddressSuperseded)
	if a := got["a/quiet"].Addresses[0]; a.OwnerSource != "b" || a.OwnerID != "newer" {
		t.Fatalf("superseded owner: %+v", a)
	}
	want("b/newer", false, AddressAssigned, AddressAssigned)
	want("a/tie", false, AddressAmbiguous)
	want("b/tie", false, AddressAmbiguous)
	want("a/short", false, AddressAssigned)
	got = read(now.Add(time.Minute))
	want("a/short", true, AddressExpired)
	// The source removes the address from its device: the old claim is not
	// restored, and the copy cannot alias the snapshot.
	now = now.Add(time.Second)
	if err := h.Replace("b", 2, []Client{dev("tie", now.Add(-2*time.Second), time.Hour, "192.0.2.2"), dev("newer", now.Add(-time.Second), time.Hour, "192.0.2.1", "192.0.2.4")}); err != nil {
		t.Fatal(err)
	}
	got = read(now)
	want("b/newer", false, AddressAssigned, AddressAssigned)
	if err := h.Replace("b", 3, []Client{dev("newer", now.Add(-2*time.Second), time.Hour, "192.0.2.4")}); err != nil {
		t.Fatal(err)
	}
	got = read(now)
	want("b/newer", false, AddressWithdrawn)
	got["b/newer"].IPs[0] = netip.Addr{}
	if read(now)["b/newer"].IPs[0] != ip("192.0.2.4") {
		t.Fatal("device copy aliases the snapshot")
	}
}
