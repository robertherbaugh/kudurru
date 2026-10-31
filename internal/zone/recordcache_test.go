package zone

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func cachedParser(t testing.TB, c *RecordCache, z *public.DNSZone, rs *public.DNSRecordSet, now time.Time) *ParsedRecordSet {
	t.Helper()
	b, errs := NewBuilder(z, BuildOptions{Now: now, Records: c})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	return b.ParseRecordSet(rs)
}

func TestRecordCacheIdentityAndContextInvalidation(t *testing.T) {
	z := dnsZone("example.")
	rs := recordSet("apps", "a", ep("web.example.", "A", "192.0.2.1"))
	for _, tc := range []struct {
		name   string
		change func(*public.DNSZone, *public.DNSRecordSet)
	}{
		{"uid", func(_ *public.DNSZone, r *public.DNSRecordSet) { r.UID = "replacement" }},
		{"namespace", func(_ *public.DNSZone, r *public.DNSRecordSet) { r.Namespace = "other" }},
		{"name", func(_ *public.DNSZone, r *public.DNSRecordSet) { r.Name = "other" }},
		{"opaque-rv", func(_ *public.DNSZone, r *public.DNSRecordSet) { r.ResourceVersion = "opaque" }},
		{"generation", func(_ *public.DNSZone, r *public.DNSRecordSet) { r.Generation++ }},
		{"creation-order", func(_ *public.DNSZone, r *public.DNSRecordSet) { r.CreationTimestamp = metav1.NewTime(testNow) }},
		{"labels", func(_ *public.DNSZone, r *public.DNSRecordSet) { r.Labels = map[string]string{"select": "no"} }},
		{"owner", func(_ *public.DNSZone, r *public.DNSRecordSet) {
			r.Annotations = map[string]string{"kudurru.io/managed-by": "other"}
		}},
		{"endpoint-same-rv", func(_ *public.DNSZone, r *public.DNSRecordSet) { r.Spec.Endpoints[0].Targets[0] = "192.0.2.2" }},
		{"ttl", func(_ *public.DNSZone, r *public.DNSRecordSet) { r.Spec.Endpoints[0].RecordTTL = 60 }},
		{"expiry", func(_ *public.DNSZone, r *public.DNSRecordSet) {
			r.Spec.Endpoints[0].Kudurru = &public.EndpointExt{ExpiresAt: &metav1.Time{Time: testNow.Add(time.Hour)}}
		}},
		{"disabled", func(_ *public.DNSZone, r *public.DNSRecordSet) {
			r.Spec.Endpoints[0].Kudurru = &public.EndpointExt{Disabled: true}
		}},
		{"view", func(_ *public.DNSZone, r *public.DNSRecordSet) {
			r.Spec.Endpoints[0].Kudurru = &public.EndpointExt{View: "public"}
		}},
		{"zone-uid", func(z *public.DNSZone, _ *public.DNSRecordSet) { z.UID = "replacement" }},
		{"zone-ttl", func(z *public.DNSZone, _ *public.DNSRecordSet) { z.Spec.DefaultTTL = 600 }},
		{"zone-origin", func(z *public.DNSZone, _ *public.DNSRecordSet) { z.Spec.ZoneName = "." }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &RecordCache{}
			before := cachedParser(t, c, z, rs, testNow)
			if cachedParser(t, c, z, rs, testNow).parsed != before.parsed {
				t.Fatal("unchanged object was reparsed")
			}
			nextZ, nextRS := z.DeepCopy(), rs.DeepCopy()
			tc.change(nextZ, nextRS)
			next := cachedParser(t, c, nextZ, nextRS, testNow)
			if next.parsed == before.parsed {
				t.Fatal("changed complete identity/context reused old parse")
			}
			got, gotErr := next.Records(0)
			want, wantErr := ParseEndpoint(recordScope(nextZ).origin, recordScope(nextZ).ttl, &nextRS.Spec.Endpoints[0], recordCachePath(0))
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotErr, wantErr) {
				t.Fatal("cached parse differs from actual new input")
			}
		})
	}
}

func TestRecordCacheDetachedRdataAndChangedSibling(t *testing.T) {
	c := &RecordCache{}
	z := dnsZone("example.")
	rs := recordSet("apps", "a", ep("a.example.", "A", "192.0.2.1"), ep("txt.example.", "TXT", "hello"), ep("svc.example.", "HTTPS", "1 . alpn=h2 ipv4hint=192.0.2.1"))
	original := rs.DeepCopy()
	p := cachedParser(t, c, z, rs, testNow)
	first, _ := p.Records(0)
	first[0].(*dns.A).A[0] = 1
	first[0].Header().Ttl = 1
	txt, _ := p.Records(1)
	txt[0].(*dns.TXT).Txt[0] = "changed"
	svc, _ := p.Records(2)
	svc[0].(*dns.HTTPS).Value[1].(*dns.SVCBIPv4Hint).Hint[0][0] = 1
	// The wrapper also freezes endpoint extensions/targets; caller mutation cannot
	// mix a previously parsed RR with a later expiry or disabled choice.
	rs.Spec.Endpoints[0].Kudurru = &public.EndpointExt{Disabled: true}
	rs.Spec.Endpoints[0].Targets[0] = "192.0.2.99"
	if errs := p.Add("apps/a", 0); len(errs) > 0 {
		t.Fatal(errs)
	}
	if p.builder.Build().Lookup("a.example.", dns.TypeA).Kind != Success {
		t.Fatal("caller changed retained wrapper source")
	}
	again := cachedParser(t, c, z, original, testNow)
	if again.parsed != p.parsed {
		t.Fatal("unchanged original parse was not retained")
	}
	for i := range original.Spec.Endpoints {
		got, _ := again.Records(i)
		want, errs := ParseEndpoint("example.", 300, &original.Spec.Endpoints[i], recordCachePath(i))
		if len(errs) > 0 || !reflect.DeepEqual(got, want) {
			t.Fatal("mutable RR/nested rdata escaped cache")
		}
	}
	other := recordSet("apps", "b", ep("b.example.", "A", "192.0.2.2"))
	cachedParser(t, c, z, other, testNow)
	other.Spec.Endpoints[0].Targets[0] = "192.0.2.3"
	cachedParser(t, c, z, other, testNow)
	if cachedParser(t, c, z, original, testNow).parsed != p.parsed {
		t.Fatal("changed sibling reparsed unchanged object")
	}
}

func TestRecordCacheExactExpiryAndDiagnosticsParity(t *testing.T) {
	c := &RecordCache{}
	z := dnsZone("example.")
	rs := recordSet("apps", "a", ep("a.example.", "A", "192.0.2.1"))
	expiry := testNow.Add(time.Second)
	rs.Spec.Endpoints[0].Kudurru = &public.EndpointExt{ExpiresAt: &metav1.Time{Time: expiry}}
	before := cachedParser(t, c, z, rs, testNow)
	for _, now := range []time.Time{testNow, expiry, expiry.Add(time.Hour)} {
		selected := ServedRecordSetsCached(z, []*public.DNSRecordSet{rs}, c, now)
		got, gotErr := Build(z, selected, BuildOptions{Now: now, Records: c})
		want, wantErr := Build(z, ServedRecordSets(z, []*public.DNSRecordSet{rs}), BuildOptions{Now: now})
		if got.ContentHash() != want.ContentHash() || got.NextExpiry() != want.NextExpiry() || !reflect.DeepEqual(gotErr, wantErr) {
			t.Fatal("expiry build differs from uncached result")
		}
		if !now.Before(expiry) && got.Lookup("a.example.", dns.TypeA).Kind != NXDomain {
			t.Fatal("expired record revived")
		}
	}
	if cachedParser(t, c, z, rs, expiry).parsed == before.parsed {
		t.Fatal("expiry did not invalidate parse entry")
	}
	for _, bad := range []*public.DNSRecordSet{recordSet("apps", "bad", ep("bad.example.", "A", "invalid")), recordSet("apps", "outside", ep("outside.invalid.", "A", "192.0.2.1"))} {
		got, gotErr := Build(z, []*public.DNSRecordSet{bad}, BuildOptions{Now: testNow, Records: c})
		want, wantErr := Build(z, []*public.DNSRecordSet{bad}, BuildOptions{Now: testNow})
		if got.ContentHash() != want.ContentHash() || !reflect.DeepEqual(gotErr, wantErr) {
			t.Fatal("parse diagnostics changed with caching")
		}
	}
}

func TestRecordCachePruningLimitsAndConcurrency(t *testing.T) {
	c := &RecordCache{}
	z := dnsZone("example.")
	rs := recordSet("apps", "a", ep("a.example.", "A", "192.0.2.1"))
	p := cachedParser(t, c, z, rs, testNow)
	c.Prune(z, nil)
	if len(c.entries) != 0 || c.bytes != 0 {
		t.Fatal("complete empty zone did not prune")
	}
	if r, _ := p.Records(0); len(r) != 1 {
		t.Fatal("pruning invalidated held parse")
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 30 {
				r := rs.DeepCopy()
				r.ResourceVersion = fmt.Sprintf("%d-%d", i, j)
				cachedParser(t, c, z, r, testNow)
				c.Prune(z, []*public.DNSRecordSet{rs})
			}
		})
	}
	wg.Wait()
	for i := range recordCacheObjects + 1 {
		r := rs.DeepCopy()
		r.Name = fmt.Sprintf("n%d", i)
		cachedParser(t, c, z, r, testNow)
	}
	if len(c.entries) != recordCacheObjects || c.bytes > recordCacheBytes {
		t.Fatal("object cap was not enforced")
	}
	c.Prune(z, nil)
	for i := range 20 {
		r := rs.DeepCopy()
		r.Name = fmt.Sprint(i)
		r.Annotations = map[string]string{"large": strings.Repeat("x", 1<<20)}
		cachedParser(t, c, z, r, testNow)
	}
	if c.bytes > recordCacheBytes || len(c.entries) >= 20 {
		t.Fatal("estimated byte cap was not enforced")
	}
	huge := rs.DeepCopy()
	huge.Annotations = map[string]string{"large": strings.Repeat("x", recordCacheBytes)}
	count := len(c.entries)
	cachedParser(t, c, z, huge, testNow)
	if len(c.entries) != count {
		t.Fatal("oversize object was retained")
	}
}

func recordCachePath(i int) *field.Path { return field.NewPath("spec", "endpoints").Index(i) }
