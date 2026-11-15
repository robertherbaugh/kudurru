package primaryzone

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCompilerRecordCacheChangedSourceExpiryAndDetachedOutputs(t *testing.T) {
	z := desiredZone()
	a := desiredSet("a", endpoint("a", "192.0.2.1"))
	b := desiredSet("b", endpoint("b", "192.0.2.2"))
	expires := metav1.NewTime(fixedNow.Add(time.Minute))
	a.Spec.Endpoints[0].Kudurru = &public.EndpointExt{ExpiresAt: &expires}
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{a, b}, Complete: true}
	c := NewCompiler(CompilerOptions{})
	uncached := NewCompiler(CompilerOptions{})
	uncached.records = nil
	original, _ := json.Marshal(in)
	v := compile(t, c, blank(z), in, fixedNow)
	again := compile(t, c, v, in, fixedNow.Add(time.Second))
	if again.Head.Spec.RootDigest != v.Head.Spec.RootDigest || again.Head.Spec.Serial != v.Head.Spec.Serial {
		t.Fatal("warm cache caused serial/content churn")
	}
	// An independent next source edit retains the held old root and expiry.
	next := b.DeepCopy()
	next.Spec.Endpoints[0].Targets[0] = "192.0.2.3"
	next.ResourceVersion = "changed"
	in.RecordSets = []*public.DNSRecordSet{a, next}
	for _, now := range []time.Time{fixedNow.Add(2 * time.Second), expires.Time, expires.Add(time.Second)} {
		got, err := c.Project(context.Background(), v, in, now)
		if err != nil {
			t.Fatal(err)
		}
		want, err := uncached.Project(context.Background(), v, in, now)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("cached current compilation differs from uncached result")
		}
		if !now.Before(expires.Time) {
			expired := prepareView(t, got.Aggregate, v.Head)
			if built(t, expired).Lookup("a.example.", dns.TypeA).Kind != zone.NXDomain || len(got.Aggregate.Reservations) != 1 {
				t.Fatal("expiry lost reservation or revived data")
			}
		}
		// Caller mutation of a produced raw record cannot poison later parses.
		if len(got.Aggregate.Records) > 0 {
			got.Aggregate.Records[0].Wire[0] ^= 1
		}
	}
	if built(t, v).Lookup("b.example.", dns.TypeA).Answer[0].(*dns.A).A.String() != "192.0.2.2" || !v.Aggregate.Expiry.Equal(expires.Time) {
		t.Fatal("changed reconciliation modified held committed root/deadline")
	}
	in.RecordSets = []*public.DNSRecordSet{a, b}
	after, _ := json.Marshal(in)
	if string(after) != string(original) {
		t.Fatal("compiler mutated original desired sources")
	}
}

func TestCompilerRecordCacheConcurrentProjections(t *testing.T) {
	c := NewCompiler(CompilerOptions{})
	var wg sync.WaitGroup
	for i := range uint32(8) {
		wg.Go(func() {
			z := desiredZone()
			z.Spec.DefaultTTL = 60 + i
			rs := desiredSet("a", endpoint("a", fmt.Sprintf("192.0.2.%d", i+1)))
			rs.ResourceVersion = fmt.Sprint(i)
			in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{rs}, Complete: true}
			uncached := NewCompiler(CompilerOptions{})
			uncached.records = nil
			want, err := uncached.Project(context.Background(), blank(z), in, fixedNow)
			if err != nil {
				t.Error(err)
				return
			}
			for range 5 {
				got, err := c.Project(context.Background(), blank(z), in, fixedNow)
				if err != nil {
					t.Error(err)
					return
				}
				if !reflect.DeepEqual(got, want) {
					t.Error("concurrent input/TTL contaminated current compilation")
					return
				}
			}
		})
	}
	wg.Wait()
}
