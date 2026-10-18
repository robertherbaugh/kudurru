package importers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestNameservers(t *testing.T) {
	got, err := Nameservers([]string{" NS1.Example.COM. ", "ns2.example.com", "ns1.example.com"})
	if err != nil || !reflect.DeepEqual(got, []string{"ns1.example.com.", "ns2.example.com."}) {
		t.Fatalf("normalize: %v %v", got, err)
	}
	for _, names := range [][]string{{""}, {"192.0.2.1"}, {"2001:db8::1"}, {"bad_name.test"}, {"https://ns.test"}, make([]string, 17)} {
		if _, err := Nameservers(names); err == nil {
			t.Fatalf("accepted %v", names)
		}
	}
}
func TestMigrationImportAuthorityOption(t *testing.T) {
	for _, source := range []string{"technitium", "pihole"} {
		for _, replace := range []bool{false, true} {
			t.Run(source+"/"+map[bool]string{true: "replace", false: "preserve"}[replace], func(t *testing.T) {
				im, err := zone.ImportBIND(strings.NewReader("@ 300 IN SOA old.test. hostmaster.test. 1 3600 600 86400 300\n@ 300 IN NS old.test.\nweb 300 IN A 192.0.2.10\n"), "home.test")
				if err != nil {
					t.Fatal(err)
				}
				p := &Plan{}
				if replace {
					p.nameservers = []string{"dns.kudurru.test.", "dns2.kudurru.test."}
				}
				before := *im.Spec.DeepCopy()
				if err := p.zone(source, "default", "home.test", im); err != nil {
					t.Fatal(err)
				}
				var dz v1alpha1.DNSZone
				b, _ := json.Marshal(p.Objects[0])
				if err := json.Unmarshal(b, &dz); err != nil {
					t.Fatal(err)
				}
				if len(p.Zones) != 1 || !reflect.DeepEqual(p.Zones[0].Nameservers, before.Nameservers) || p.Zones[0].Primary != before.SOA.MName {
					t.Fatal("lost original authority")
				}
				want := before
				if replace {
					want.Nameservers = p.nameservers
					want.SOA.MName = p.nameservers[0]
				}
				if !reflect.DeepEqual(dz.Spec, want) {
					t.Fatalf("authority conversion: %+v", dz.Spec)
				}
				if len(p.Objects) != 2 || !IsImportedZone(dz.Name, dz.Spec.ZoneName) {
					t.Fatal("records or previous-import recognition missing")
				}
			})
		}
	}
}

func TestAddressAuthority(t *testing.T) {
	names, records, err := Authority("home.test.", nil, []string{"192.0.2.53/24", "2001:db8::53", "192.0.2.53", "::ffff:192.0.2.53"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ns-c0000235.home.test.", "ns-20010db8000000000000000000000053.home.test."}
	if !reflect.DeepEqual(names, want) || len(records) != 2 || records[0].RecordType != "A" || records[0].Targets[0] != "192.0.2.53" || records[1].RecordType != "AAAA" {
		t.Fatalf("authority: %v %+v", names, records)
	}
	for _, bad := range []string{"0.0.0.0", "::", "::ffff:0.0.0.0", "::ffff:224.0.0.1", "224.0.0.1", "ff02::1", "fe80::1%eth0", "dns.test", "192.0.2.1:53"} {
		if _, err := NameserverAddresses([]string{bad}); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, _, err := Authority("home.test.", []string{"dns.test"}, []string{"192.0.2.53"}); err == nil {
		t.Fatal("accepted both target modes")
	}
	longZone := strings.Repeat("a.", 120) + "test."
	if _, _, err := Authority(longZone, nil, []string{"2001:db8::53"}); err == nil {
		t.Fatal("generated overlong name")
	}
}

func TestMigrationAddressImport(t *testing.T) {
	for _, source := range []string{"technitium", "pihole"} {
		im, err := zone.ImportBIND(strings.NewReader("@ 300 IN SOA old.test. hostmaster.test. 1 3600 600 86400 300\n@ 300 IN NS old.test.\nweb 300 IN A 192.0.2.10\n"), "home.test")
		if err != nil {
			t.Fatal(err)
		}
		p := &Plan{addresses: []string{"192.0.2.53", "2001:db8::53"}}
		if err := p.zone(source, "dns", "home.test", im); err != nil {
			t.Fatal(err)
		}
		if im.Spec.Nameservers[0] != "ns-c0000235.home.test." || im.Spec.SOA.MName != im.Spec.Nameservers[0] || len(im.Endpoints) != 3 || im.Endpoints[0].DNSName != "web.home.test" || p.Zones[0].Primary != "old.test." {
			t.Fatalf("import: %+v", im)
		}
		if err := p.zone(source, "dns", "home.test", im); err == nil {
			t.Fatal("overwrote existing nameserver records")
		}
	}
}
