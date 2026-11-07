package controller

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRecordCacheProductionBuildReplacementAndStatusEcho(t *testing.T) {
	c := newCache()
	z := zoneObj("example", "example.com.")
	c.upsert(z)
	a := rrsetObj("apps", "a", "example", 0, ep("www.example.com.", "A", "192.0.2.1"))
	a.UID = "a"
	a.ResourceVersion = "one"
	b := rrsetObj("apps", "b", "example", time.Second, ep("www.example.com.", "A", "192.0.2.2"))
	b.UID = "b"
	b.ResourceVersion = "two"
	c.upsert(a)
	c.upsert(b)
	first, _ := c.build("example")
	if first.Hash == "" {
		t.Fatal("production build failed")
	}
	echo := a.DeepCopy()
	echo.ResourceVersion = "echo"
	echo.Status.ObservedGeneration = 1
	c.upsert(echo)
	same, _ := c.build("example")
	if same != first {
		t.Fatal("status echo invalidated whole-zone build")
	}
	// Same generation does not make a replacement object's ordering identical.
	replacement := a.DeepCopy()
	replacement.UID = "replacement"
	replacement.ResourceVersion = "new"
	replacement.CreationTimestamp = metav1.NewTime(a.CreationTimestamp.Add(2 * time.Second))
	c.upsert(replacement)
	changed, _ := c.build("example")
	want := buildZone(z, []*public.DNSRecordSet{b, replacement})
	if changed == first || changed.Hash == first.Hash || changed.Hash != want.Hash {
		t.Fatal("replacement identity/order was hidden by whole-zone hit")
	}
	// A producer that reused generation still cannot hide changed TTL/spec.
	next := b.DeepCopy()
	next.ResourceVersion = "changed-ttl"
	next.Spec.Endpoints[0].RecordTTL = 60
	c.upsert(next)
	changedAgain, _ := c.build("example")
	want = buildZone(z, []*public.DNSRecordSet{next, replacement})
	if changedAgain.Hash != want.Hash || changedAgain.Hash == changed.Hash {
		t.Fatal("record TTL edit reused stale whole-zone/record parse")
	}
}

// BenchmarkRecordSetReconciliation changes exactly one source on every build.
// WholeZoneHit is measured separately and never reported as record-cache work.
func BenchmarkRecordSetReconciliation(b *testing.B) {
	for _, n := range []int{100, 1000} {
		for _, mode := range []string{"ChangedUncached", "ChangedRecordCache", "WholeZoneHit"} {
			b.Run(fmt.Sprintf("%d/%s", n, mode), func(b *testing.B) {
				c := newCache()
				if mode == "ChangedUncached" {
					c.records = nil
				}
				z := zoneObj("example", "example.com.")
				c.upsert(z)
				var changed *public.DNSRecordSet
				for i := range n {
					r := rrsetObj("apps", fmt.Sprintf("r%d", i), "example", time.Duration(i)*time.Second, ep(fmt.Sprintf("h%d.example.com.", i), "TXT", "one string", "another string"))
					r.ResourceVersion = "initial"
					c.upsert(r)
					if i == 0 {
						changed = r
					}
				}
				before, _ := c.build("example")
				hash := before.Hash
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if mode != "WholeZoneHit" {
						next := changed.DeepCopy()
						next.ResourceVersion = strconv.Itoa(i + 2)
						next.Generation = int64(i + 2)
						next.Spec.Endpoints[0].Targets[0] = fmt.Sprint(i % 2)
						c.upsert(next)
					}
					result, _ := c.build("example")
					if result.ZoneErr != nil || result.Hash == "" {
						b.Fatal("build failed")
					}
					if mode != "WholeZoneHit" && result.Hash == hash {
						b.Fatal("benchmark did not change served data")
					}
					hash = result.Hash
				}
			})
		}
	}
}
