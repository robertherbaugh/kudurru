package license

import (
	"testing"
	"time"
)

func TestBrandingProjectedStatusRequiresEnterpriseAndEffectiveState(t *testing.T) {
	for _, plan := range []Plan{"", PlanPersonal, PlanBusiness, PlanTrial, PlanEnterprise} {
		for _, state := range []State{"", StateCommunity, StateInvalid, StateExpired, StateActive, StateGrace} {
			status := Status{Plan: plan, State: state, Features: []Feature{FeatureBranding, FeatureRequests}}
			want := plan == PlanEnterprise && (state == StateActive || state == StateGrace)
			if status.Allows(FeatureBranding) != want {
				t.Fatalf("projected %s/%s branding bypassed plan or state", plan, state)
			}
			if !status.Allows(FeatureRequests) {
				t.Fatal("branding defense changed unrelated projected features")
			}
			status.Features = nil
			if status.Allows(FeatureBranding) {
				t.Fatal("Enterprise status ignored restricted effective feature list")
			}
		}
	}
}

func TestBrandingRequiresEnterpriseEvenExplicit(t *testing.T) {
	f := newFixture(t)
	for _, plan := range []Plan{PlanPersonal, PlanBusiness, PlanTrial, PlanEnterprise} {
		for _, list := range []struct {
			name     string
			features []Feature
			granted  bool
		}{
			{"all", nil, true},
			{"explicit", []Feature{FeatureBranding}, true},
			{"restricted", []Feature{FeatureOIDC}, false},
		} {
			t.Run(string(plan)+"/"+list.name, func(t *testing.T) {
				claims := Claims{Plan: plan, Features: list.features}
				want := plan == PlanEnterprise && list.granted
				if claims.Allows(FeatureBranding) != want {
					t.Fatal("claims bypassed plan or explicit feature restriction")
				}
				m := f.manager(t, t0)
				if err := m.Set(f.key(t, func(c *Claims) { c.Plan, c.Features = plan, list.features }), "test"); err != nil {
					t.Fatal(err)
				}
				if m.Allows(FeatureBranding) != want || m.Capture().Status().Allows(FeatureBranding) != want {
					t.Fatal("verified manager/capture disagrees with claims entitlement")
				}
			})
		}
	}
}

func TestBrandingOriginalValidityAndGrace(t *testing.T) {
	f := newFixture(t)
	now := t0
	m, err := New(Options{Keyring: f.ring, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	exp := t0.Add(time.Hour)
	raw := f.key(t, func(c *Claims) {
		c.Plan, c.Features = PlanEnterprise, []Feature{FeatureBranding}
		c.NotBefore, c.ExpiresAt = t0.Unix(), exp.Unix()
	})
	if err = m.Set(raw, "test"); err != nil {
		t.Fatal(err)
	}
	cut := m.Capture()
	from, until := cut.Validity()
	if !cut.Status().Allows(FeatureBranding) || !from.Equal(t0) || !until.Equal(exp.Add(Grace)) {
		t.Fatal("active cut lacks original finite branding window")
	}
	for _, at := range []time.Time{exp, exp.Add(Grace).Add(-time.Nanosecond)} {
		now = at
		if !cut.Current() || !m.Allows(FeatureBranding) || m.Status().State != StateGrace {
			t.Fatal("Enterprise branding did not retain normal grace semantics")
		}
	}
	now = until
	if cut.Current() || m.Allows(FeatureBranding) || m.Status().State != StateExpired {
		t.Fatal("exact grace deadline retained branding")
	}
	now = t0.Add(-time.Nanosecond)
	if cut.Current() || m.Allows(FeatureBranding) {
		t.Fatal("clock before signed/captured validity retained branding")
	}
	// Replacing the installation fences current commands, but cannot mutate the
	// original detached policy/window used by already held reads.
	now = t0
	if err = m.Set("", ""); err != nil {
		t.Fatal(err)
	}
	if cut.Current() || !cut.Status().Allows(FeatureBranding) || m.Capture().Status().Allows(FeatureBranding) {
		t.Fatal("replacement changed held policy or retained current entitlement")
	}
	gotFrom, gotUntil := cut.Validity()
	if !gotFrom.Equal(from) || !gotUntil.Equal(until) {
		t.Fatal("replacement renewed original absolute validity")
	}
}
