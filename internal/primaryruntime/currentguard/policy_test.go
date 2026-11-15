package currentguard

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func testRing(t *testing.T, secret string) *dnsserver.StaticKeyring {
	t.Helper()
	k, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "key.", Algorithm: dns.HmacSHA256, Secret: []byte(secret)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func testSource() *public.DNSZone {
	return &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "primary", UID: types.UID("source-uid"), ResourceVersion: "opaque"}, Spec: public.DNSZoneSpec{ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."}, Policy: public.ZonePolicy{QueryAccess: public.AccessAllow, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "key.", Name: "*.example."}}}}}
}

func TestPolicyCanonicalDetachedAndMissingKeyRevocation(t *testing.T) {
	k := testRing(t, "first")
	o, _ := New(Options{})
	z := testSource()
	z.Spec.Policy.UpdatePolicies = []public.UpdatePolicy{
		{TSIGKeyRef: " KEY ", Name: "*.EXAMPLE.", Types: []string{"AAAA", "a", "A"}},
		{TSIGKeyRef: "key.", Name: "*.example.", Types: []string{"A", "AAAA"}},
		{TSIGKeyRef: "removed.", Name: "example.", Types: []string{"ANY"}},
		{TSIGKeyRef: "key.", Name: "exact.example.", Types: []string{"TYPE65280"}},
	}
	p, err := o.compile(t.Context(), k, z)
	if err != nil || len(p.Credentials) != 1 || len(p.Grants) != 2 || p.AuthEpoch != 1 || !p.Enabled {
		t.Fatal(p, err)
	}
	if p.Grants[0].Owner != "*.example." || !reflect.DeepEqual(p.Grants[0].Types, []uint16{dns.TypeA, dns.TypeAAAA}) || p.Grants[1].Types[0] != 65280 {
		t.Fatal(p)
	}
	z.Spec.Policy.UpdatePolicies[0].Name = "changed.example."
	z.Spec.Policy.UpdatePolicies[0].Types[0] = "TXT"
	if p.Grants[0].Owner != "*.example." || p.Grants[0].Types[1] != dns.TypeAAAA {
		t.Fatal("capture retained desired slices")
	}
	empty, _ := dnsserver.NewStaticKeyring()
	p, err = o.compile(t.Context(), empty, testSource())
	if err != nil || !p.Enabled || len(p.Credentials) != 0 || len(p.Grants) != 0 {
		t.Fatal("missing key invented active version/grant or blocked revocation", p, err)
	}
	for _, mode := range []public.AccessMode{"", public.AccessDeny} {
		z = testSource()
		z.Spec.Policy.Update = mode
		p, err = o.compile(t.Context(), k, z)
		if err != nil || p.Enabled || len(p.Credentials) != 0 || len(p.Grants) != 0 {
			t.Fatal("deny/default enabled credentials", p, err)
		}
	}
}

func TestPolicyUnsupportedMalformedAndCapacityFailClosed(t *testing.T) {
	o, _ := New(Options{})
	k := testRing(t, "first")
	for _, mode := range []public.AccessMode{public.AccessAllowOnlyPrivate, public.AccessAllowOnlyZoneNameServers, public.AccessUseNetworkACL, public.AccessZoneNameServersAndACL, "unknown"} {
		z := testSource()
		z.Spec.Policy.Update = mode
		if _, err := o.compile(t.Context(), k, z); !errors.Is(err, pr.ErrUnavailable) {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
	for _, grant := range []public.UpdatePolicy{
		{TSIGKeyRef: "", Name: "*.example."},
		{TSIGKeyRef: ".", Name: "*.example."},
		{TSIGKeyRef: "key.", Name: "relative"},
		{TSIGKeyRef: "key.", Name: "*.outside."},
		{TSIGKeyRef: "key.", Name: "*.*.example."},
		{TSIGKeyRef: "missing.", Name: "*.example.", Types: []string{"AXFR"}},
		{TSIGKeyRef: "key.", Name: "*.example.", Types: []string{"ANY", "OPT"}},
		{TSIGKeyRef: "key.", Name: "*.example.", Types: []string{"TYPE0"}},
		{TSIGKeyRef: "key.", Name: "*.example.", Types: []string{"TYPE65535"}},
		{TSIGKeyRef: "key.", Name: "*.example.", Types: []string{"TYPE65536"}},
	} {
		z := testSource()
		z.Spec.Policy.UpdatePolicies = []public.UpdatePolicy{grant}
		if _, err := o.compile(t.Context(), k, z); !errors.Is(err, pr.ErrInvalid) {
			t.Fatalf("invalid grant accepted: %v", err)
		}
	}
	z := testSource()
	z.Spec.Policy.UpdatePolicies = make([]public.UpdatePolicy, dynamicupdate.MaxPolicyEntries+1)
	if _, err := o.compile(t.Context(), k, z); !errors.Is(err, pr.ErrCapacity) {
		t.Fatal(err)
	}
	z = testSource()
	z.Spec.Policy.UpdatePolicies[0].Types = make([]string, dynamicupdate.MaxTotalGrantTypes+1)
	if _, err := o.compile(t.Context(), k, z); !errors.Is(err, pr.ErrCapacity) {
		t.Fatal(err)
	}
	z = testSource()
	z.Spec.Policy.UpdatePolicies[0].Name = strings.Repeat("x", dynamicupdate.MaxNameBytes+3)
	if _, err := o.compile(t.Context(), k, z); !errors.Is(err, pr.ErrCapacity) {
		t.Fatal(err)
	}
	small, _ := New(Options{Limits: zonestate.Limits{MaxAggregateBytes: 1}})
	if _, err := small.compile(t.Context(), k, testSource()); !errors.Is(err, pr.ErrCapacity) {
		t.Fatal("workspace budget ignored", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := o.compile(ctx, k, testSource()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
