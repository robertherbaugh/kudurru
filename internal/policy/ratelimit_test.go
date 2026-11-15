package policy

import (
	"net/netip"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestRateLimitExemptionUsesOnlyWinningMembership(t *testing.T) {
	makeGroup := func(name string, exempt bool, matcher public.ClientMatcher) *public.ClientGroup {
		g := group("default", name, matcher)
		g.Spec.RateLimitExempt = exempt
		return g
	}
	def := makeGroup("default", true, public.ClientMatcher{CIDR: "0.0.0.0/0"})
	broad := makeGroup("broad", true, public.ClientMatcher{CIDR: "192.0.2.0/24"})
	exact := makeGroup("exact", false, public.ClientMatcher{IP: "192.0.2.1"})
	tied := makeGroup("tied", true, public.ClientMatcher{IP: "192.0.2.1"})
	off := disable(makeGroup("disabled", true, public.ClientMatcher{IP: "192.0.2.2"}), "false")
	for _, tc := range []struct {
		name   string
		groups []*public.ClientGroup
		ip     string
		want   bool
	}{
		{"exact overrides exempt CIDR and default", []*public.ClientGroup{def, broad, exact}, "192.0.2.1", false},
		{"any tied true wins", []*public.ClientGroup{def, broad, exact, tied}, "192.0.2.1", true},
		{"winning CIDR", []*public.ClientGroup{def, broad}, "192.0.2.2", true},
		{"disabled never contributes", []*public.ClientGroup{off}, "192.0.2.2", false},
		{"default opt in", []*public.ClientGroup{def}, "198.51.100.1", true},
		{"unmatched", []*public.ClientGroup{exact}, "198.51.100.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ix := Build(Input{ClientGroups: tc.groups}, Options{})
			c := client(tc.ip)
			c.GroupIDs = []string{"default/tied"} // Caller-supplied group labels never grant exemption.
			if got := ix.IdentifyAt(c, time.Now()).RateLimitExempt(); got != tc.want {
				t.Fatal("wrong exemption", got)
			}
		})
	}
	// Build copies the flag; editing an input or building a successor cannot
	// change an earlier publication. Dynamic hostname joins need the same rule.
	h1 := makeGroup("h1", false, public.ClientMatcher{Hostname: "phone.lan"})
	h2 := makeGroup("h2", true, public.ClientMatcher{Hostname: "phone.lan"})
	ix := Build(Input{ClientGroups: []*public.ClientGroup{h1, h2}}, Options{Inventory: true})
	h2.Spec.RateLimitExempt = false
	c := pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53"), IdentityCaptured: true, Names: []string{"phone.lan"}}
	if !ix.IdentifyAt(c, time.Now()).RateLimitExempt() {
		t.Fatal("dynamic joined membership lost immutable flag")
	}
	fresh := Build(Input{ClientGroups: []*public.ClientGroup{h1, h2}}, Options{Inventory: true})
	if fresh.IdentifyAt(c, time.Now()).RateLimitExempt() {
		t.Fatal("successor adopted old exemption")
	}
}

func TestRateLimitPrivateGrantRetainsSourceUIDAndOriginalExpiry(t *testing.T) {
	at := time.Unix(1000, 0)
	g := group("default", "device")
	g.UID, g.Spec.RateLimitExempt = "original", true
	grant := MACGrant{Group: "default/device", GroupUID: "original", MAC: "02:00:00:00:00:01", Source: "trusted", ExpiresAt: at.Add(time.Minute)}
	ix := Build(Input{ClientGroups: []*public.ClientGroup{g}, MACGrants: []MACGrant{grant}}, Options{Inventory: true})
	c := pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53"), IdentityCaptured: true, InventorySource: "trusted", MACs: []string{grant.MAC}}
	if !ix.IdentifyAt(c, at).RateLimitExempt() || ix.IdentifyAt(c, grant.ExpiresAt).RateLimitExempt() {
		t.Fatal("source grant deadline changed")
	}
	c.InventorySource = "other"
	if ix.IdentifyAt(c, at).RateLimitExempt() {
		t.Fatal("different source adopted exemption")
	}
	c.InventorySource, c.IdentityCaptured = "trusted", false
	if ix.IdentifyAt(c, at).RateLimitExempt() {
		t.Fatal("uncaptured identity granted exemption")
	}
	c.IdentityCaptured, g.UID = true, "replacement"
	fresh := Build(Input{ClientGroups: []*public.ClientGroup{g}, MACGrants: []MACGrant{grant}}, Options{Inventory: true})
	if fresh.IdentifyAt(c, at).RateLimitExempt() {
		t.Fatal("recreated group adopted old source contribution")
	}
}
