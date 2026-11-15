package policy

import (
	"slices"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestPacketCacheClientIndependentMembership(t *testing.T) {
	if (*Index)(nil).ClientIndependent() {
		t.Fatal("missing original policy qualified")
	}
	for _, tc := range []struct {
		name   string
		groups []*public.ClientGroup
		want   bool
	}{
		{"empty", nil, true},
		{"bootstrap", []*public.ClientGroup{group(ns, "default", public.ClientMatcher{CIDR: "0.0.0.0/0"}, public.ClientMatcher{CIDR: "::/0"})}, true},
		{"different-families", []*public.ClientGroup{group(ns, "default", public.ClientMatcher{CIDR: "0.0.0.0/0"}), group(ns, "other", public.ClientMatcher{CIDR: "::/0"})}, false},
		{"nondefault-catchall", []*public.ClientGroup{group(ns, "other", public.ClientMatcher{CIDR: "0.0.0.0/0"}, public.ClientMatcher{CIDR: "::/0"})}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ix := Build(Input{ClientGroups: tc.groups}, Options{})
			if ix.ClientIndependent() != tc.want {
				t.Fatal("incorrect shared cache proof")
			}
		})
	}
	for _, matcher := range []public.ClientMatcher{
		{IP: "192.0.2.1"}, {CIDR: "192.0.2.0/24"}, {MAC: "02:00:00:00:00:01"},
		{Hostname: "device"}, {NameGlob: "device*"}, {ClientID: "device"},
	} {
		ix := Build(Input{ClientGroups: []*public.ClientGroup{group(ns, "scoped", matcher)}}, Options{Inventory: true})
		if ix.ClientIndependent() {
			t.Fatalf("client selector qualified: %+v", matcher)
		}
	}
	// Equal wire questions actually select different original groups. The
	// cache proof must reject even before any policies differ or are added.
	g := group(ns, "scoped", public.ClientMatcher{IP: "192.0.2.1"})
	ix := Build(Input{ClientGroups: []*public.ClientGroup{g}}, Options{})
	if slices.Equal(ix.Identify(client("192.0.2.1")).GroupIDs, ix.Identify(client("192.0.2.2")).GroupIDs) || ix.ClientIndependent() {
		t.Fatal("client distinction not fenced")
	}
	g.Annotations = map[string]string{public.EnabledAnnotation: "false"}
	if !Build(Input{ClientGroups: []*public.ClientGroup{g}}, Options{}).ClientIndependent() {
		t.Fatal("disabled selector changed compiled membership")
	}
	g = group(ns, "default")
	g.UID = "original-group"
	ix = Build(Input{ClientGroups: []*public.ClientGroup{g}, MACGrants: []MACGrant{{Group: "default/default", GroupUID: "original-group", MAC: "02:00:00:00:00:01", Source: "original-source", ExpiresAt: time.Now().Add(time.Minute)}}}, Options{Inventory: true})
	if ix.ClientIndependent() {
		t.Fatal("private MAC grant qualified")
	}
}
