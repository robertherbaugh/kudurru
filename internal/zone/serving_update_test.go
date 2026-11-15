package zone_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/miekg/dns"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// ServingErrors refuses a primary zone exactly when the primary runtime
// cannot compile its update mode: such a zone is never committed, so the
// SQLite node would report it not ready forever (D373). 0.1.0 stored every
// access mode in spec.policy.update, which was a placeholder there.
func TestServingErrorsRefuseUpdateModesThePrimaryRuntimeCannotCompile(t *testing.T) {
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	modes := []v1alpha1.AccessMode{"", v1alpha1.AccessDeny, v1alpha1.AccessAllow, v1alpha1.AccessAllowOnlyPrivate,
		v1alpha1.AccessAllowOnlyZoneNameServers, v1alpha1.AccessUseNetworkACL, v1alpha1.AccessZoneNameServersAndACL}
	for _, mode := range modes {
		dz := &v1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: "upd", UID: "upd-uid", ResourceVersion: "1"},
			Spec: v1alpha1.DNSZoneSpec{ZoneName: "upd.example.", Type: v1alpha1.ZoneTypePrimary, Nameservers: []string{"ns1.upd.example."},
				Policy: v1alpha1.ZonePolicy{Update: mode}},
		}
		errs := zone.ServingErrors(dz)
		_, compileErr := currentguard.CompileUpdatePolicy(context.Background(), keys, dz, 0)
		if (len(errs) == 0) != (compileErr == nil) {
			t.Errorf("update %q: ServingErrors %v, but the primary runtime says %v", mode, errs, compileErr)
		}
		if len(errs) > 0 && errs[0].Field != "spec.policy.update" {
			t.Errorf("update %q: refusal names %v", mode, errs)
		}
		// A forward zone has no update policy and is not judged on it.
		dz.Spec.Type, dz.Spec.Forward = v1alpha1.ZoneTypeForward, &v1alpha1.ForwardSpec{UpstreamGroup: "default"}
		if errs := zone.ServingErrors(dz); len(errs) != 0 {
			t.Errorf("forward zone with update %q: %v", mode, errs)
		}
	}
}

// ServingErrors refuses a primary zone exactly when the primary runtime
// cannot compile its update grants, whatever the update mode: the runtime
// validates every grant, also of a zone that denies updates, and a zone
// whose policy does not compile is never committed (D373). 0.1.0 stored any
// grant. The keyring holds every key the grants name, so the runtime also
// charges the workspace of every grant.
func TestServingErrorsRefuseUpdateGrantsThePrimaryRuntimeCannotCompile(t *testing.T) {
	long := strings.Repeat("k", 63)
	manyNames := make([]v1alpha1.UpdatePolicy, 0, 4097)
	for i := range 4097 {
		manyNames = append(manyNames, v1alpha1.UpdatePolicy{TSIGKeyRef: "key", Name: fmt.Sprintf("h%d.upd.example.", i)})
	}
	bigKeys := make([]v1alpha1.UpdatePolicy, 0, 3500)
	for i := range 3500 {
		key := fmt.Sprintf("%s.%s.%s.k%d.", long, long, long, i)
		bigKeys = append(bigKeys, v1alpha1.UpdatePolicy{TSIGKeyRef: key, Name: "*.upd.example.", Types: []string{"A"}})
	}
	cases := []struct {
		name   string
		grants []v1alpha1.UpdatePolicy
		field  string
		// allowOnly is a refusal of a zone that allows updates only: one
		// that denies them compiles no grant into its policy.
		allowOnly bool
	}{
		{"none", nil, "", false},
		{"owner-at-apex", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "upd.example."}}, "", false},
		{"owner-below", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key.", Name: "Host.UPD.example.", Types: []string{"a", "AAAA", "A", "TYPE65280"}}}, "", false},
		{"wildcard", []v1alpha1.UpdatePolicy{{TSIGKeyRef: " key ", Name: "*.upd.example.", Types: []string{"ANY"}}}, "", false},
		{"owner-outside", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "host.other.example."}}, "spec.policy.updatePolicies[0].name", false},
		{"owner-relative", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "host.upd.example"}}, "spec.policy.updatePolicies[0].name", false},
		{"owner-inner-star", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "a.*.upd.example."}}, "spec.policy.updatePolicies[0].name", false},
		{"owner-empty", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: ""}}, "spec.policy.updatePolicies[0].name", false},
		{"type-unknown", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "upd.example.", Types: []string{"BOGUS"}}}, "spec.policy.updatePolicies[0].types", false},
		{"type-zero", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "upd.example.", Types: []string{"TYPE0"}}}, "spec.policy.updatePolicies[0].types", false},
		{"type-opt", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "upd.example.", Types: []string{"OPT"}}}, "spec.policy.updatePolicies[0].types", false},
		{"type-axfr", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "upd.example.", Types: []string{"AXFR"}}}, "spec.policy.updatePolicies[0].types", false},
		{"type-65535", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "upd.example.", Types: []string{"TYPE65535"}}}, "spec.policy.updatePolicies[0].types", false},
		{"type-too-long", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "upd.example.", Types: []string{"TYPE00000000000000001"}}}, "spec.policy.updatePolicies[0].types", false},
		{"key-empty", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "", Name: "upd.example."}}, "spec.policy.updatePolicies[0].tsigKeyRef", false},
		{"key-root", []v1alpha1.UpdatePolicy{{TSIGKeyRef: ".", Name: "upd.example."}}, "spec.policy.updatePolicies[0].tsigKeyRef", false},
		{"key-not-a-name", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "a..b", Name: "upd.example."}}, "spec.policy.updatePolicies[0].tsigKeyRef", false},
		{"second-entry", []v1alpha1.UpdatePolicy{{TSIGKeyRef: "key", Name: "upd.example."}, {TSIGKeyRef: "key", Name: "x.example."}}, "spec.policy.updatePolicies[1].name", false},
		{"too-many-entries", manyNames, "spec.policy.updatePolicies", false},
		{"over-the-workspace", bigKeys, "spec.policy.updatePolicies", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var keys []dnsserver.TSIGKey
			seen := map[string]bool{}
			for _, g := range tc.grants {
				name := dnsserver.TSIGKeyName(g.TSIGKeyRef)
				if _, ok := dns.IsDomainName(name); ok && name != "." && !seen[name] {
					seen[name] = true
					keys = append(keys, dnsserver.TSIGKey{Name: name, Algorithm: "hmac-sha256", Secret: []byte("0123456789abcdef0123456789abcdef")})
				}
			}
			ring, err := dnsserver.NewStaticKeyring(keys...)
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []v1alpha1.AccessMode{v1alpha1.AccessAllow, v1alpha1.AccessDeny} {
				dz := &v1alpha1.DNSZone{
					ObjectMeta: metav1.ObjectMeta{Name: "upd", UID: "upd-uid", ResourceVersion: "1"},
					Spec: v1alpha1.DNSZoneSpec{ZoneName: "upd.example.", Type: v1alpha1.ZoneTypePrimary, Nameservers: []string{"ns1.upd.example."},
						Policy: v1alpha1.ZonePolicy{Update: mode, UpdatePolicies: tc.grants}},
				}
				errs := zone.ServingErrors(dz)
				_, compileErr := currentguard.CompileUpdatePolicy(context.Background(), ring, dz, 0)
				if (len(errs) == 0) != (compileErr == nil) {
					t.Fatalf("update %q: ServingErrors %v, but the primary runtime says %v", mode, errs, compileErr)
				}
				want := tc.field
				if tc.allowOnly && mode != v1alpha1.AccessAllow {
					want = ""
				}
				if want == "" && len(errs) != 0 {
					t.Fatalf("update %q: %v", mode, errs)
				}
				if want != "" && (len(errs) != 1 || errs[0].Field != want) {
					t.Fatalf("update %q: refusal %v, want one on %s", mode, errs, tc.field)
				}
			}
		})
	}
}
