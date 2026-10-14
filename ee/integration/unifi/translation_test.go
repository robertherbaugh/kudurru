package unifi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const siteID = "11111111-1111-4111-8111-111111111111"
const networkID = "22222222-2222-4222-8222-222222222222"
const clientID = "33333333-3333-4333-8333-333333333333"

func translationFixture() TranslationInput {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	return TranslationInput{Owner: host.InstanceID{ClusterID: "cluster", Namespace: "dns", Name: "unifi-home", UID: "original"}, Config: public.UniFiIntegration{Endpoint: "https://controller.example", Inventory: &public.UniFiInventory{ZoneRef: public.ObjectRef{Name: "lan"}, TTL: 120}}, Zone: &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "lan", UID: "zone-uid", ResourceVersion: "captured"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "lan.example."}}, Observation: Observation{SiteID: siteID, ObservedAt: now, Complete: true, Networks: []Network{{ID: networkID, Name: "LAN", VLAN: 10, Enabled: true, Prefixes: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/64")}}}, Clients: []Device{{ID: siteID + "/02:00:00:00:00:01", MAC: "02:00:00:00:00:01", Name: "Laptop", IPs: []netip.Addr{netip.MustParseAddr("192.0.2.3"), netip.MustParseAddr("2001:db8::3")}, ObservedAt: now, ExpiresAt: now.Add(3 * time.Minute)}}}}
}

func TestTranslationDualStackReverseGroupsAndImmutableInputs(t *testing.T) {
	in := translationFixture()
	in.Config.Groups = public.UniFiGroups{ByNetwork: map[string]string{"LAN": "home"}, ByNote: "kudurru:group="}
	in.Observation.Clients[0].Note = "custom note kudurru:group=kids; unrelated=admin"
	in.Allocations = map[string]string{in.Observation.Clients[0].ID: "laptop"}
	before, _ := json.Marshal(in)
	out, diag, err := Translate(in)
	if err != nil || !out.Complete || diag.AmbiguousIPs != 0 || diag.UnassignedNetworks != 0 || diag.NoMACGroups != 0 || diag.HistoricalWithoutTime != 0 || len(diag.LegacyUnavailable) != 0 {
		t.Fatal(out, diag, err)
	}
	if len(out.Clients) != 1 || len(out.Desired.Zones) != 2 || len(out.Desired.RecordSets) != 3 || len(out.Desired.ClientGroups) != 2 {
		t.Fatal("incomplete translation", out)
	}
	types := map[string]int{}
	for _, rr := range out.Desired.RecordSets {
		for _, e := range rr.Spec.Endpoints {
			types[e.RecordType]++
			if e.RecordTTL != 120 || !e.Kudurru.ExpiresAt.Time.Equal(in.Observation.Clients[0].ExpiresAt) {
				t.Fatal("expiry/TTL lost", e)
			}
			if e.RecordType == "PTR" {
				if e.Targets[0] != "laptop.lan.example." {
					t.Fatal("wrong reverse target", e)
				}
			} else if e.DNSName != "laptop.lan.example." {
				t.Fatal(e)
			}
		}
	}
	if types["A"] != 1 || types["AAAA"] != 1 || types["PTR"] != 2 {
		t.Fatal(types)
	}
	for _, z := range out.Desired.Zones {
		if z.Namespace != "" || z.Spec.Policy.QueryAccess != public.AccessAllowOnlyPrivate || z.Spec.Policy.ZoneTransfer != public.AccessDeny || z.Spec.Policy.Update != public.AccessDeny {
			t.Fatal("reverse zone expanded authorization", z)
		}
	}
	for _, g := range out.Desired.ClientGroups {
		if g.Spec.Sources.IntegrationRef.Name != in.Owner.Name || len(g.Spec.Clients) != 1 || g.Spec.Clients[0].MAC != in.Observation.Clients[0].MAC || g.Spec.Clients[0].IP != "" || g.Spec.Clients[0].CIDR != "" {
			t.Fatal("non-MAC source membership", g)
		}
	}
	if slices.Contains(out.Clients[0].Tags, "dns/home") || slices.Contains(out.Clients[0].Tags, "kids") {
		t.Fatal("group names leaked into tags")
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("translation mutated input")
	}
	out.Clients[0].IPs[0] = netip.Addr{}
	out.Desired.RecordSets[0].Spec.Endpoints[0].Targets[0] = "changed"
	after, _ = json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("output aliases input")
	}
}

func TestStableExplicitCollisionAllocationsAcrossOmission(t *testing.T) {
	in := translationFixture()
	other := in.Observation.Clients[0]
	other.ID, other.MAC = siteID+"/02:00:00:00:00:02", "02:00:00:00:00:02"
	other.IPs = []netip.Addr{netip.MustParseAddr("192.0.2.4")}
	in.Observation.Clients = append(in.Observation.Clients, other)
	if _, _, err := Translate(in); !errors.Is(err, ErrAllocation) {
		t.Fatal("invented a fresh collision allocation", err)
	}
	requests, err := NameRequests(in)
	if err != nil || len(requests) != 2 || requests[0].Base != "laptop" || requests[1].Base != "laptop" {
		t.Fatal(requests, err)
	}
	in.Allocations = map[string]string{requests[0].ID: "laptop", requests[1].ID: "laptop-2"}
	out, _, err := Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, r := range out.Desired.RecordSets {
		for _, e := range r.Spec.Endpoints {
			if e.RecordType != "PTR" {
				names[e.DNSName] = true
			}
		}
	}
	if !names["laptop.lan.example."] || !names["laptop-2.lan.example."] {
		t.Fatal(names)
	}
	in.Observation.Clients = in.Observation.Clients[1:]
	out, _, err = Translate(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out.Desired.RecordSets {
		for _, e := range r.Spec.Endpoints {
			if e.RecordType == "A" && e.DNSName != "laptop-2.lan.example." {
				t.Fatal("omission renumbered durable allocation", e)
			}
		}
	}
	in.Allocations[requests[1].ID] = "external.other."
	if _, _, err := Translate(in); !errors.Is(err, ErrAllocation) {
		t.Fatal("allocation escaped apex", err)
	}
}

func TestAssignmentWinnerExpiryAndAmbiguityNeverFallBack(t *testing.T) {
	for _, mode := range []string{"newer", "tie", "expired_newest"} {
		t.Run(mode, func(t *testing.T) {
			in := translationFixture()
			in.Config.Inventory = nil
			old := in.Observation.Clients[0]
			old.IPs = old.IPs[:1]
			old.ObservedAt = old.ObservedAt.Add(-time.Minute)
			newest := old
			newest.ID = "new-device"
			newest.MAC = "02:00:00:00:00:02"
			newest.ObservedAt = in.Observation.ObservedAt
			if mode == "tie" {
				newest.ObservedAt = old.ObservedAt
			}
			if mode == "expired_newest" {
				newest.ObservedAt = in.Observation.ObservedAt.Add(-time.Second)
				newest.ExpiresAt = in.Observation.ObservedAt
			}
			in.Observation.Clients = []Device{old, newest}
			out, diag, err := Translate(in)
			if err != nil {
				t.Fatal(err)
			}
			h := inventory.New(func() time.Time { return in.Observation.ObservedAt })
			if err = h.Replace(out.Clients[0].Source, 1, out.Clients); err != nil {
				t.Fatal(err)
			}
			got := h.Snapshot().Lookup(old.IPs[0], in.Observation.ObservedAt)
			if mode == "newer" {
				if got == nil || got.ID != newest.ID {
					t.Fatal("older claim won", got)
				}
			} else if got != nil {
				t.Fatal("expired/ambiguous winner fell back", got)
			}
			if mode == "tie" && diag.AmbiguousIPs != 1 {
				t.Fatal(diag)
			}
		})
	}
}

func TestNetworkSelectionAndGroupCapability(t *testing.T) {
	in := translationFixture()
	in.Config.Groups.ByNetwork = map[string]string{"LAN": "home"}
	vpn := in.Observation.Clients[0]
	vpn.ID = "vpn-device"
	vpn.MAC = ""
	vpn.IPs = []netip.Addr{netip.MustParseAddr("192.0.2.5")}
	vpn.Name = "VPN"
	in.Observation.Clients = append(in.Observation.Clients, vpn)
	out, diag, err := Translate(in)
	if err != nil || diag.NoMACGroups != 1 || len(out.Desired.ClientGroups[0].Spec.Clients) != 1 {
		t.Fatal(out, diag, err)
	}
	other := in.Observation.Networks[0]
	other.ID = "44444444-4444-4444-8444-444444444444"
	other.Name = "Other"
	in.Observation.Networks = append(in.Observation.Networks, other)
	in.Config.Inventory.IncludeNetworks = []string{"LAN"}
	out, diag, err = Translate(in)
	if err != nil || len(out.Desired.RecordSets) != 0 || diag.UnassignedNetworks != 2 {
		t.Fatal("ambiguous prefix assigned a group/record", out, diag, err)
	}
	other.Prefixes = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/25")}
	in.Observation.Networks[1] = other
	if network, _ := networkForIP(netip.MustParseAddr("192.0.2.3"), in.Observation.Networks); network == nil || network.ID != other.ID {
		t.Fatal("longest prefix not selected", network)
	}
}

func TestReversePrefixBoundaries(t *testing.T) {
	cases := map[string]int{"192.0.2.0/23": 2, "192.0.2.128/25": 1, "2001:db8::/63": 2, "2001:db8::/65": 8}
	for raw, count := range cases {
		prefix := netip.MustParsePrefix(raw)
		apices, err := reverseApices(prefix)
		if err != nil || len(apices) != count {
			t.Fatal(raw, apices, err)
		}
		for _, apex := range apices {
			if _, ok := dns.IsDomainName(apex); !ok {
				t.Fatal(apex)
			}
		}
	}
	apices, _ := reverseApices(netip.MustParsePrefix("192.0.2.128/25"))
	if !slices.Equal(apices, []string{"2.0.192.in-addr.arpa."}) {
		t.Fatal(apices)
	}
	in := translationFixture()
	in.Config.Inventory.ReverseZones = "disabled"
	out, _, err := Translate(in)
	if err != nil || len(out.Desired.Zones) != 0 || len(out.Desired.RecordSets) != 1 {
		t.Fatal(out, err)
	}
}

func TestTranslationRejectsMalformedAndUnsupportedInputs(t *testing.T) {
	for _, mode := range []string{"incomplete", "wrong_zone", "foreign_zone_namespace", "root", "missing_uid", "future", "expired_lifetime", "invalid_ip", "duplicate_id", "bad_hostname", "unknown_group_network", "bad_note_group", "escaped_template", "template_function", "unsupported_push"} {
		t.Run(mode, func(t *testing.T) {
			in := translationFixture()
			switch mode {
			case "incomplete":
				in.Observation.Complete = false
			case "wrong_zone":
				in.Zone.Name = "other"
			case "foreign_zone_namespace":
				in.Zone.Namespace = "foreign"
			case "root":
				in.Zone.Spec.ZoneName = "."
			case "missing_uid":
				in.Zone.UID = ""
			case "future":
				in.Observation.Clients[0].ObservedAt = in.Observation.ObservedAt.Add(time.Second)
			case "expired_lifetime":
				in.Observation.Clients[0].ExpiresAt = in.Observation.ObservedAt.Add(25 * time.Hour)
			case "invalid_ip":
				in.Observation.Clients[0].IPs = []netip.Addr{{}}
			case "duplicate_id":
				in.Observation.Clients = append(in.Observation.Clients, in.Observation.Clients[0])
			case "bad_hostname":
				in.Observation.Clients[0].Hostname = "bad name"
			case "unknown_group_network":
				in.Config.Groups.ByNetwork = map[string]string{"missing": "admin"}
			case "bad_note_group":
				in.Config.Groups.ByNote = "group="
				in.Observation.Clients[0].Note = "group=../admin"
			case "escaped_template":
				in.Config.Inventory.NameTemplate = "other.external."
			case "template_function":
				in.Config.Inventory.NameTemplate = "{{ .Name | readFile }}"
			case "unsupported_push":
				in.Config.Push.DHCPDNSServers = true
			}
			out, _, err := Translate(in)
			if err == nil || out.Complete {
				t.Fatal("unsafe input became executable desired", out, err)
			}
		})
	}
	for _, raw := range []string{"{{ .Alias | default .Hostname | slug }}", "{{ .Name | slug }}"} {
		in := translationFixture()
		in.Config.Inventory.NameTemplate = raw
		in.Observation.Clients[0].Alias = "My Laptop ♥"
		requests, err := NameRequests(in)
		if err != nil || len(requests) != 1 || !label(requests[0].Base) {
			t.Fatal(requests, err)
		}
	}
	long := strings.Repeat("a", 63)
	if !allocatedLabel(long, strings.Repeat("a", 61)+"-2") {
		t.Fatal("long stable suffix was not bounded")
	}
}

func TestReverseCoverageFollowsNetworksAndPTRStaysInsideActualPrefix(t *testing.T) {
	in := translationFixture()
	in.Observation.Networks[0].Prefixes = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/23")}
	in.Observation.Clients[0].IPs = []netip.Addr{netip.MustParseAddr("192.0.2.3")}
	out, _, err := Translate(in)
	if err != nil || len(out.Desired.Zones) != 2 {
		t.Fatal("empty reverse child not preserved", out, err)
	}
	in.Observation.Clients = nil
	out, _, err = Translate(in)
	if err != nil || len(out.Desired.Zones) != 2 || len(out.Desired.RecordSets) != 0 {
		t.Fatal("omission changed authenticated coverage", out, err)
	}
	in.Observation.Clients = translationFixture().Observation.Clients
	in.Observation.Networks[0].Prefixes = []netip.Prefix{netip.MustParsePrefix("192.0.2.128/25")}
	out, _, err = Translate(in)
	if err != nil || len(out.Desired.Zones) != 1 {
		t.Fatal(out, err)
	}
	for _, rr := range out.Desired.RecordSets {
		for _, e := range rr.Spec.Endpoints {
			if e.RecordType == "PTR" {
				t.Fatal("covering child granted PTR outside actual prefix", e)
			}
		}
	}
	in.Config.Inventory.ReverseZones = "disabled"
	out, _, err = Translate(in)
	if err != nil || len(out.Desired.Zones) != 0 {
		t.Fatal(out, err)
	}
}

func TestTranslationCapacityAndFinalNameBounds(t *testing.T) {
	t.Run("fqdn", func(t *testing.T) {
		in := translationFixture()
		in.Zone.Spec.ZoneName = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 50) + "."
		in.Observation.Clients[0].Name = strings.Repeat("e", 63)
		if out, _, err := Translate(in); !errors.Is(err, ErrInvalid) || out.Complete {
			t.Fatal("overlength FQDN published", out, err)
		}
	})
	t.Run("groups", func(t *testing.T) {
		in := translationFixture()
		in.Config.Inventory = nil
		in.Config.Groups.ByNetwork = map[string]string{"LAN": "home"}
		d := in.Observation.Clients[0]
		in.Observation.Clients = nil
		for i := 0; i < 1001; i++ {
			c := d
			c.ID = fmt.Sprintf("device-%d", i)
			c.MAC = fmt.Sprintf("02:00:00:%02x:%02x:%02x", i>>16, (i>>8)&255, i&255)
			c.IPs = []netip.Addr{netip.AddrFrom4([4]byte{192, 0, byte(i / 256), byte(i % 256)})}
			in.Observation.Clients = append(in.Observation.Clients, c)
		}
		in.Observation.Networks[0].Prefixes = []netip.Prefix{netip.MustParsePrefix("192.0.0.0/16")}
		if out, _, err := Translate(in); !errors.Is(err, ErrCapacity) || out.Complete {
			t.Fatal("membership silently truncated", out, err)
		}
	})
	t.Run("reserved_labels", func(t *testing.T) {
		in := translationFixture()
		in.Allocations = map[string]string{"removed-owner": "laptop"}
		if _, _, err := Translate(in); !errors.Is(err, ErrAllocation) {
			t.Fatal("removed reservation stolen", err)
		}
	})
	t.Run("reverse", func(t *testing.T) {
		in := translationFixture()
		in.Observation.Networks[0].Prefixes = nil
		for i := 0; i < 33; i++ {
			in.Observation.Networks[0].Prefixes = append(in.Observation.Networks[0].Prefixes, netip.MustParsePrefix(fmt.Sprintf("2001:db8:%x::/65", i)))
		}
		if out, _, err := Translate(in); !errors.Is(err, ErrCapacity) || out.Complete {
			t.Fatal("reverse coverage silently truncated", out, err)
		}
	})
}
