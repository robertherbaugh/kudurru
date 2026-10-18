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
				p.zone(source, "default", "home.test", im)
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
