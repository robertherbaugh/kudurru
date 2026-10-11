package zone

import (
	"sync"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestSnapshotLongestSuffix(t *testing.T) {
	root := mustBuild(t, dnsZone("."))
	org := mustBuild(t, dnsZone("example.org."))
	sub := mustBuild(t, dnsZone("lab.example.org."))
	fwd, _ := Build(dnsZone("corp.example.", func(dz *v1alpha1.DNSZone) { dz.Spec.Type = v1alpha1.ZoneTypeForward }), nil, BuildOptions{})
	s, err := NewSnapshot(1, true, root, org, sub, fwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]*Zone{
		"example.org.":           org,
		"www.example.org.":       org,
		"WWW.LAB.Example.ORG.":   sub,
		"lab.example.org":        sub,
		"xlab.example.org.":      org,
		"example.net.":           root,
		".":                      root,
		"a.b.corp.example.":      fwd,
		"corp.example.":          fwd,
		`a\.lab.example.org.`:    org, // escaped dot: one label "a.lab"
		"ns1.lab.example.org.x.": root,
	} {
		if got := s.Match(q); got != want {
			t.Errorf("Match(%q) = %v, want %v", q, name(got), name(want))
		}
	}
	if s.Zone("LAB.example.org") != sub || s.Zone("nope.") != nil || s.Len() != 4 || s.Generation() != 1 || !s.Synced() {
		t.Fatal("accessors")
	}
	zs := s.Zones()
	if len(zs) != 4 || zs[0] != root || zs[1] != fwd {
		t.Fatalf("Zones order: %v", zs)
	}

	noRoot := s.Without(2, ".")
	if noRoot.Match("example.net.") != nil || s.Match("example.net.") != root {
		t.Fatal("Without must copy")
	}
	if noRoot.Generation() != 2 {
		t.Fatal("generation")
	}
	org2 := mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Status.Serial = 9 }))
	if got := s.With(3, org2).Match("example.org."); got != org2 || s.Match("example.org.") != org {
		t.Fatal("With must replace in a copy")
	}
	if s.WithSynced(4, false).Synced() || !s.Synced() {
		t.Fatal("WithSynced")
	}
	if _, err := NewSnapshot(1, true, org, org2); err == nil {
		t.Fatal("duplicate zones must be rejected")
	}
	empty, _ := NewSnapshot(0, false)
	if empty.Match("example.org.") != nil {
		t.Fatal("empty snapshot")
	}
}

func name(z *Zone) string {
	if z == nil {
		return "<nil>"
	}
	return z.Name()
}

func TestHolder(t *testing.T) {
	var h Holder
	if h.Zones() != nil || h.Load() != nil {
		t.Fatal("zero holder is empty")
	}
	org := mustBuild(t, dnsZone("example.org."), ep("www.example.org", "A", "192.0.2.1"))
	s, _ := NewSnapshot(1, true, org)
	h.Store(s)
	if h.Zones() == nil || h.Load() != s {
		t.Fatal("store/load")
	}

	// Readers race a writer that keeps swapping snapshots (go test -race).
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				z := h.Zones().Match("www.example.org.")
				if r := z.Lookup("www.example.org.", dns.TypeA); r.Kind != Success {
					t.Error("lookup failed")
					return
				}
			}
		}()
	}
	for i := range uint32(200) {
		next := mustBuild(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Status.Serial = i }), ep("www.example.org", "A", "192.0.2.1"))
		h.Store(h.Load().With(uint64(i), next))
	}
	close(stop)
	wg.Wait()
}

func BenchmarkSnapshotMatch(b *testing.B) {
	var zones []*Zone
	for _, n := range []string{".", "example.org.", "lab.example.org.", "corp.example.", "home.arpa.", "10.in-addr.arpa."} {
		zones = append(zones, mustBuild(b, dnsZone(n)))
	}
	s, _ := NewSnapshot(1, true, zones...)
	b.ReportAllocs()
	for b.Loop() {
		if s.Match("host.dev.lab.example.org.") == nil {
			b.Fatal("no match")
		}
	}
}
