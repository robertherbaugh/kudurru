package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDHCPExtendedConfigurationRoundTrip(t *testing.T) {
	d := testDHCP()
	d.ConflictDetection, d.ProbeTimeout = "disabled", Duration(150*time.Millisecond)
	d.NTPServers, d.DomainSearch = []string{"192.168.1.1"}, []string{"home.arpa", "lab.home.arpa"}
	d.ClasslessRoutes = []DHCPClasslessRoute{{Destination: "0.0.0.0/0", Router: "192.168.1.1"}, {Destination: "198.51.100.0/24", Router: "192.168.1.3"}}
	d.BootServer, d.BootServerName, d.BootFile = "192.168.1.9", "boot.home.arpa", "boot.ipxe"
	d.BootProfiles = []DHCPBootProfile{{Architecture: 7, BootFile: "http://192.168.1.9/efi.ipxe"}}
	d.TrustedRelays = []DHCPTrustedRelay{{SourceIP: "192.168.1.1", GatewayIP: "192.168.1.1", SourcePort: 1067, LinkSelection: "192.168.1.1", ServerIdentifier: "192.168.1.4"}}
	c, err := d.Config()
	if err != nil {
		t.Fatal(err)
	}
	if c.NTPServers[0] != netip.MustParseAddr(d.NTPServers[0]) || c.TrustedRelays[0].SourcePort != 1067 || c.BootProfiles[0].Architecture != 7 || c.ProbeTimeout != 150*time.Millisecond {
		t.Fatalf("conversion lost values: %+v", c)
	}
	file := DHCPScopeFromConfig(c)
	next, err := (DHCP{Scopes: []DHCPScope{file}}).Config()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, next) {
		t.Fatalf("round trip changed runtime options: %+v vs %+v", c, next)
	}
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DHCP = d
	round, err := FromFlat(Flatten(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round.DHCP, d) {
		t.Fatalf("flat conversion changed extended settings: %+v", round.DHCP)
	}
}

func TestDHCPFailoverValidation(t *testing.T) {
	base := DHCPFailover{Enabled: true, ClusterID: "home", NodeID: "a", ListenAddress: "127.0.0.1:9167", Bootstrap: true,
		Peers: []DHCPFailoverPeer{{ID: "b", Address: "127.0.0.2:9167"}, {ID: "a", Address: "127.0.0.1:9167"}, {ID: "c", Address: "127.0.0.3:9167"}}}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*DHCPFailover)
	}{
		{"two voters", func(f *DHCPFailover) { f.Peers = f.Peers[:2] }},
		{"duplicate address", func(f *DHCPFailover) { f.Peers[1].Address = f.Peers[0].Address }},
		{"unknown node", func(f *DHCPFailover) { f.NodeID = "absent" }},
		{"wrong bootstrap", func(f *DHCPFailover) { f.NodeID = "b" }},
		{"wildcard peer", func(f *DHCPFailover) { f.Peers[0].Address = "0.0.0.0:9167" }},
		{"zero port", func(f *DHCPFailover) { f.ListenAddress = "127.0.0.1:0" }},
		{"partial TLS", func(f *DHCPFailover) { f.CertFile = "certificate.pem" }},
		{"unsafe URL", func(f *DHCPFailover) { f.Peers[0].APIURL = "javascript:alert(1)" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base.Clone()
			tc.edit(&candidate)
			if candidate.Validate() == nil {
				t.Fatal("accepted invalid failover")
			}
		})
	}
	scope := testDHCP()
	scope.FailoverServers = []DHCPScopeServer{{NodeID: "a", Address: scope.ServerIP}, {NodeID: "b", Address: "192.168.1.3"}, {NodeID: "c", Address: "192.168.1.4"}}
	configs, err := scope.Configs()
	if err != nil {
		t.Fatal(err)
	}
	if err := base.ValidateScopes(configs); err != nil {
		t.Fatal(err)
	}
	configs[0].ServerIP = netip.MustParseAddr("192.168.1.5")
	if base.ValidateScopes(configs) == nil {
		t.Fatal("accepted wrong local server address")
	}
}

func TestDHCPExtendedEnvironmentOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `dhcp:
  enabled: true
  management: api
  interface: fixture0
  serverIP: 192.0.2.2
  subnet: 192.0.2.0/24
  rangeStart: 192.0.2.100
  rangeEnd: 192.0.2.110
  router: 192.0.2.1
  dnsServers: [192.0.2.2]
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, []string{"KUDURRU_DHCP__BOOTFILE=boot.ipxe"})
	if err == nil || !strings.Contains(err.Error(), "environment overrides are incompatible") {
		t.Fatalf("mutable environment override: %v", err)
	}
}
