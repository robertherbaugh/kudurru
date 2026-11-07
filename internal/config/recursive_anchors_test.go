package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestManagedRootPolicyRequiresExplicitBoundedSeeds(t *testing.T) {
	seed := "12345 15 2 " + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, mode string
		dnssec     bool
		seeds      []string
		valid      bool
	}{
		{"static-default", "static", true, nil, true},
		{"explicit", "managed", true, []string{seed}, true},
		{"implicit-seed", "managed", true, nil, false},
		{"disabled", "managed", false, []string{seed}, false},
		{"unknown", "automatic", true, []string{seed}, false},
		{"short-digest", "managed", true, []string{"12345 15 2 AB"}, false},
		{"unsupported", "managed", true, []string{"12345 253 2 " + strings.Repeat("AB", 32)}, false},
		{"duplicate", "managed", true, []string{seed, ". 3600 IN DS " + seed}, false},
		{"blank", "managed", true, []string{seed, " "}, false},
		{"oversized", "managed", true, []string{strings.Repeat("x", 2049)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flat := Defaults()
			flat["upstreams.recursion.anchorMode"] = tc.mode
			flat["upstreams.recursion.dnssec"] = tc.dnssec
			flat["upstreams.recursion.trustAnchors"] = tc.seeds
			cfg, err := FromFlat(flat)
			if (err == nil) != tc.valid {
				t.Fatalf("managed policy validity: accepted=%t", err == nil)
			}
			if err != nil {
				return
			}
			policy, err := cfg.Upstreams.Recursion.RootAnchorPolicy()
			if err != nil {
				t.Fatal(err)
			}
			if tc.mode == "static" && (policy.PolicyID != "" || len(policy.Seeds) != 0) {
				t.Fatal("static settings fabricated managed seeds")
			}
			restored, err := FromFlat(Flatten(cfg))
			if err != nil || !reflect.DeepEqual(cfg.Upstreams.Recursion, restored.Upstreams.Recursion) {
				t.Fatal("mode/seed restore changed policy")
			}
		})
	}
}

func TestManagedRootPolicyCanonicalIdentityAndDetachedInput(t *testing.T) {
	a := "12345 15 2 " + strings.Repeat("ab", 32)
	b := "12346 15 2 " + strings.Repeat("cd", 32)
	r := Recursion{DNSSEC: true, AnchorMode: RootAnchorsManaged, TrustAnchors: []string{b, a}}
	first, err := r.RootAnchorPolicy()
	if err != nil {
		t.Fatal(err)
	}
	r.TrustAnchors = []string{". 30 IN DS " + strings.ToUpper(a), ". 60 IN DS " + strings.ToUpper(b)}
	second, err := r.RootAnchorPolicy()
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("presentation changes replaced immutable seed policy")
	}
	first.Seeds[0] = "mutated"
	third, err := r.RootAnchorPolicy()
	if err != nil || !reflect.DeepEqual(second, third) {
		t.Fatal("caller changed retained seed configuration")
	}
	r.TrustAnchors = []string{a}
	different, err := r.RootAnchorPolicy()
	if err != nil || different.PolicyID == second.PolicyID || different.SeedDigest == second.SeedDigest {
		t.Fatal("seed change reused policy identity")
	}
}

func TestRootHintsOnlyNumericOptionalPorts(t *testing.T) {
	for _, tc := range []struct {
		text  string
		valid bool
	}{
		{"192.0.2.53", true}, {"192.0.2.53:5353", true}, {"2001:db8::53", true}, {"[2001:db8::53]:5353", true},
		{"dns.example:53", false}, {"192.0.2.53:0", false}, {"[fe80::1%eth0]:53", false}, {":53", false},
	} {
		flat := Defaults()
		flat["upstreams.recursion.rootHints"] = []string{tc.text}
		_, err := FromFlat(flat)
		if (err == nil) != tc.valid {
			t.Errorf("numeric endpoint %q: accepted=%t", tc.text, err == nil)
		}
	}
}
