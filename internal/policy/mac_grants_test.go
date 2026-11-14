package policy

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPrivateMACGrantsKeepOriginalSourceUIDAndExclusiveExpiry(t *testing.T) {
	at := time.Unix(1000, 0)
	group := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "devices", Namespace: "default", UID: "original-group"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{MAC: "02:00:00:00:00:09"}}}}
	grants := []MACGrant{{Group: "default/devices", GroupUID: "original-group", MAC: "02:00:00:00:00:01", Source: "original-source", ExpiresAt: at.Add(time.Minute)}}
	ix := Build(Input{ClientGroups: []*public.ClientGroup{group}, MACGrants: grants}, Options{Inventory: true})
	grants[0].ExpiresAt = at.Add(time.Hour)
	client := pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:53"), IdentityCaptured: true, InventorySource: "original-source", MACs: []string{"02:00:00:00:00:01"}}
	matches := func(ix *Index, c pipeline.Client, at time.Time) bool {
		return slices.Contains(ix.IdentifyAt(c, at).GroupIDs, "default/devices")
	}
	if !matches(ix, client, at.Add(time.Minute-time.Nanosecond)) || matches(ix, client, at.Add(time.Minute)) {
		t.Fatal("private grant was renewed or included its expiry")
	}
	client.InventorySource = "another-source"
	if matches(ix, client, at) {
		t.Fatal("other source inherited private contribution")
	}
	client.InventorySource, client.IdentityCaptured = "original-source", false
	if matches(ix, client, at) {
		t.Fatal("uncaptured source identity granted membership")
	}
	client.MACs = []string{"02:00:00:00:00:09"}
	if !matches(ix, client, at.Add(24*time.Hour)) {
		t.Fatal("private expiry removed manual membership")
	}
	group.UID = "recreated-group"
	client.MACs, client.IdentityCaptured = []string{"02:00:00:00:00:01"}, true
	fresh := Build(Input{ClientGroups: []*public.ClientGroup{group}, MACGrants: grants}, Options{Inventory: true})
	if matches(fresh, client, at) {
		t.Fatal("recreated group adopted another UID's contribution")
	}
}
