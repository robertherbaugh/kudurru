package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testDHCPScope() DHCPScope {
	d := testDHCP()
	return DHCPScope{Name: "lan", Interface: d.Interface, ServerIP: d.ServerIP, Subnet: d.Subnet, RangeStart: d.RangeStart, RangeEnd: d.RangeEnd, Router: d.Router, DNSServers: d.DNSServers, DomainName: d.DomainName}
}

func TestDHCPNamedScopeDefaultsAndLegacyConversion(t *testing.T) {
	d := DHCP{Enabled: true, Scopes: []DHCPScope{testDHCPScope()}}
	configs, err := d.Configs()
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 || configs[0].Name != "lan" || configs[0].Management != "file" || configs[0].LeaseDuration != 12*time.Hour || configs[0].DNSTTL != 30*time.Second {
		t.Fatalf("scope defaults: %+v", configs)
	}
	one, err := d.Config()
	if err != nil || !reflect.DeepEqual(one, configs[0]) {
		t.Fatalf("single named scope: %+v %v", one, err)
	}
	configs[0].DNSServers[0] = configs[0].Router
	if d.Scopes[0].DNSServers[0] != "192.168.1.2" {
		t.Fatal("conversion aliased source DNS slice")
	}
	relay := testDHCPScope()
	relay.Name = "remote"
	relay.Subnet = "192.168.2.0/24"
	relay.RangeStart = "192.168.2.100"
	relay.RangeEnd = "192.168.2.200"
	relay.Router = "192.168.2.1"
	relay.RelayOnly = true
	relay.Management = "api"
	relay.RegisterDNS = true
	relay.TrustedRelays = []DHCPTrustedRelay{{SourceIP: "192.168.1.1", GatewayIP: "192.168.2.1"}}
	d.Scopes = append(d.Scopes, relay)
	configs, err = d.Configs()
	if err != nil || len(configs) != 2 || !configs[1].RelayOnly || !configs[1].RegisterDNS || len(configs[1].TrustedRelays) != 1 {
		t.Fatalf("direct and relay scopes: %+v %v", configs, err)
	}
	if _, err := d.Config(); err == nil {
		t.Fatal("single conversion accepted multiple scopes")
	}
}

func TestDHCPNamedScopeConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*DHCP)
	}{
		{"mixed topology", func(d *DHCP) { d.Interface = "eth0" }},
		{"mixed API owner", func(d *DHCP) { d.Management = "api" }},
		{"mixed lease option", func(d *DHCP) { d.LeaseDuration = Duration(time.Hour) }},
		{"empty name", func(d *DHCP) { d.Scopes[0].Name = "" }},
		{"unsafe name", func(d *DHCP) { d.Scopes[0].Name = "../other" }},
		{"duplicate name", func(d *DHCP) { d.Scopes = append(d.Scopes, d.Scopes[0]) }},
		{"overlapping subnet", func(d *DHCP) { c := d.Scopes[0]; c.Name = "other"; d.Scopes = append(d.Scopes, c) }},
		{"invalid owner", func(d *DHCP) { d.Scopes[0].Management = "cluster" }},
		{"fractional TTL", func(d *DHCP) { d.Scopes[0].DNSTTL = Duration(time.Second + time.Nanosecond) }},
		{"too long TTL", func(d *DHCP) { d.Scopes[0].DNSTTL = Duration(time.Hour + time.Second) }},
		{"invalid relay source", func(d *DHCP) {
			d.Scopes[0].TrustedRelays = []DHCPTrustedRelay{{SourceIP: "invalid", GatewayIP: "192.168.1.1"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DHCP{Scopes: []DHCPScope{testDHCPScope()}}
			tc.edit(&d)
			if _, err := d.Configs(); err == nil {
				t.Fatal("invalid scopes accepted")
			}
		})
	}
	// The loader's legacy defaults must not count as explicitly mixed fields.
	d := DHCP{Management: "file", LeaseDuration: Duration(12 * time.Hour), DNSTTL: Duration(30 * time.Second), Scopes: []DHCPScope{testDHCPScope()}}
	if _, err := d.Configs(); err != nil {
		t.Fatal(err)
	}
}

func TestDHCPScopeLoadAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kudurru.yaml")
	body := `dhcp:
  enabled: true
  scopes:
    - name: lan
      management: api
      interface: eth0
      serverIP: 192.168.1.2
      subnet: 192.168.1.0/24
      rangeStart: 192.168.1.100
      rangeEnd: 192.168.1.200
      router: 192.168.1.1
      dnsServers: [192.168.1.2]
      domainName: home.arpa
      registerDNS: true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	scopes, err := cfg.DHCP.Configs()
	if err != nil || len(scopes) != 1 || scopes[0].Management != "api" || scopes[0].DNSTTL != 30*time.Second {
		t.Fatalf("loaded scopes: %+v %v", scopes, err)
	}
	round, err := FromFlat(Flatten(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.DHCP, round.DHCP) {
		t.Fatal("named scopes changed in settings round trip")
	}
	if _, err := Load(path, []string{"KUDURRU_DHCP__MANAGEMENT=api"}); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("mixed flat env accepted: %v", err)
	}
	if _, err := Load(path, []string{"KUDURRU_DHCP__SCOPES=[]"}); err == nil || !strings.Contains(err.Error(), "config file") {
		t.Fatalf("scope list env accepted: %v", err)
	}
	for _, field := range []string{"management", "relayOnly", "trustedRelays", "registerDNS", "dnsTTL", "scopes"} {
		k, ok := Lookup("dhcp." + field)
		if !ok || k.Help == "" || k.Group != GroupDHCP || !k.HostOnly || !k.RestartRequired {
			t.Fatalf("scope metadata: %+v", k)
		}
	}
}
