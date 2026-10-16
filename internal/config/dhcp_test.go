package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

func testDHCP() DHCP {
	return DHCP{
		Management: "file", DNSTTL: Duration(30 * time.Second), Scopes: []DHCPScope{}, TrustedRelays: []DHCPTrustedRelay{},
		Enabled: true, Interface: "eth0", ServerIP: "192.168.1.2", Subnet: "192.168.1.0/24",
		RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", Router: "192.168.1.1",
		DNSServers: []string{"192.168.1.2", "9.9.9.9"}, DomainName: "home.example", LeaseDuration: Duration(12 * time.Hour),
		Reservations: []DHCPReservation{{MAC: "02:00:00:00:00:01", IP: "192.168.1.10", Hostname: "printer"}},
	}
}

func TestDHCPDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := DHCP{Management: "file", DNSTTL: Duration(30 * time.Second), Scopes: []DHCPScope{}, TrustedRelays: []DHCPTrustedRelay{}, LeaseDuration: Duration(12 * time.Hour), DNSServers: []string{}, Reservations: []DHCPReservation{}}
	if !reflect.DeepEqual(cfg.DHCP, want) {
		t.Fatalf("DHCP defaults: %+v, want %+v", cfg.DHCP, want)
	}
	// A Kubernetes node keeps working with DHCP disabled and no network
	// fields. The runtime conversion still requires a usable config.
	cfg.Store.Driver = "kube"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled DHCP: %v", err)
	}
	if _, err := cfg.DHCP.Config(); err == nil {
		t.Fatal("runtime conversion accepted empty network fields")
	}
}

func TestDHCPConfig(t *testing.T) {
	d := testDHCP()
	got, err := d.Config()
	if err != nil {
		t.Fatal(err)
	}
	want := dhcp.Config{
		Name: "default", Management: "file", DNSTTL: 30 * time.Second,
		Interface: "eth0", ServerIP: netip.MustParseAddr("192.168.1.2"), Subnet: netip.MustParsePrefix("192.168.1.0/24"),
		RangeStart: netip.MustParseAddr("192.168.1.100"), RangeEnd: netip.MustParseAddr("192.168.1.200"), Router: netip.MustParseAddr("192.168.1.1"),
		DNSServers: []netip.Addr{netip.MustParseAddr("192.168.1.2"), netip.MustParseAddr("9.9.9.9")},
		DomainName: "home.example", LeaseDuration: 12 * time.Hour,
		Reservations: []dhcp.Reservation{{MAC: "02:00:00:00:00:01", IP: netip.MustParseAddr("192.168.1.10"), Hostname: "printer"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime config: %+v, want %+v", got, want)
	}
	// Conversion owns its slices; a consumer cannot change the settings.
	got.DNSServers[0] = netip.MustParseAddr("1.1.1.1")
	got.Reservations[0].Hostname = "changed"
	if d.DNSServers[0] != "192.168.1.2" || d.Reservations[0].Hostname != "printer" {
		t.Fatal("conversion changed source configuration")
	}
}

func TestDHCPConfigParseErrors(t *testing.T) {
	d := testDHCP()
	d.ServerIP, d.Subnet, d.RangeStart, d.RangeEnd, d.Router = "invalid", "192.168.1.0", "", "invalid", "invalid"
	d.DNSServers = []string{"192.168.1.2", "invalid"}
	d.Reservations[0].IP = "invalid"
	_, err := d.Config()
	if err == nil {
		t.Fatal("invalid network syntax accepted")
	}
	for _, path := range []string{"serverIP", "subnet", "rangeStart", "rangeEnd", "router", "dnsServers[1]", "reservations[0].ip"} {
		if !strings.Contains(err.Error(), "dhcp."+path) {
			t.Errorf("error does not name dhcp.%s: %v", path, err)
		}
	}
}

func TestDHCPValidation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*DHCP)
	}{
		{"missing interface", func(d *DHCP) { d.Interface = "" }},
		{"server outside subnet", func(d *DHCP) { d.ServerIP = "192.168.2.2" }},
		{"IPv6 server", func(d *DHCP) { d.ServerIP = "2001:db8::2" }},
		{"mapped server", func(d *DHCP) { d.ServerIP = "::ffff:192.168.1.2" }},
		{"subnet with host bits", func(d *DHCP) { d.Subnet = "192.168.1.2/24" }},
		{"IPv6 subnet", func(d *DHCP) { d.Subnet = "2001:db8::/64" }},
		{"range reversed", func(d *DHCP) { d.RangeStart, d.RangeEnd = d.RangeEnd, d.RangeStart }},
		{"range outside subnet", func(d *DHCP) { d.RangeEnd = "192.168.2.200" }},
		{"router outside subnet", func(d *DHCP) { d.Router = "192.168.2.1" }},
		{"missing router", func(d *DHCP) { d.Router = "" }},
		{"missing DNS servers", func(d *DHCP) { d.DNSServers = nil }},
		{"IPv6 DNS", func(d *DHCP) { d.DNSServers = []string{"2001:db8::53"} }},
		{"empty DNS entry", func(d *DHCP) { d.DNSServers = []string{""} }},
		{"invalid domain", func(d *DHCP) { d.DomainName = "bad domain" }},
		{"zero lease", func(d *DHCP) { d.LeaseDuration = 0 }},
		{"short lease", func(d *DHCP) { d.LeaseDuration = Duration(59 * time.Second) }},
		{"long lease", func(d *DHCP) { d.LeaseDuration = Duration(30*24*time.Hour + time.Second) }},
		{"fractional lease", func(d *DHCP) { d.LeaseDuration = Duration(time.Minute + time.Nanosecond) }},
		{"invalid MAC", func(d *DHCP) { d.Reservations[0].MAC = "invalid" }},
		{"EUI-64 reservation", func(d *DHCP) { d.Reservations[0].MAC = "02:00:00:00:00:00:00:01" }},
		{"reservation outside subnet", func(d *DHCP) { d.Reservations[0].IP = "192.168.2.10" }},
		{"reservation on server", func(d *DHCP) { d.Reservations[0].IP = d.ServerIP }},
		{"reservation on router", func(d *DHCP) { d.Reservations[0].IP = d.Router }},
		{"reservation on network", func(d *DHCP) { d.Reservations[0].IP = "192.168.1.0" }},
		{"reservation on broadcast", func(d *DHCP) { d.Reservations[0].IP = "192.168.1.255" }},
		{"duplicate MAC", func(d *DHCP) {
			d.Reservations = append(d.Reservations, DHCPReservation{MAC: "02-00-00-00-00-01", IP: "192.168.1.11"})
		}},
		{"duplicate reservation IP", func(d *DHCP) {
			d.Reservations = append(d.Reservations, DHCPReservation{MAC: "02:00:00:00:00:02", IP: "192.168.1.10"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load("", nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.DHCP = testDHCP()
			tc.change(&cfg.DHCP)
			if _, err := cfg.DHCP.Config(); err == nil {
				t.Fatal("runtime conversion accepted invalid DHCP settings")
			}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "dhcp") {
				t.Fatalf("Config.Validate did not report invalid DHCP settings: %v", err)
			}
		})
	}

	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DHCP = testDHCP()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("enabled DHCP with SQLite: %v", err)
	}
	cfg.Store.Driver = "kube"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "store.driver=sqlite") {
		t.Fatalf("enabled DHCP with Kubernetes: %v", err)
	}
}

func TestDHCPOptionalSettingsAndLeaseBounds(t *testing.T) {
	for _, lease := range []time.Duration{time.Minute, 30 * 24 * time.Hour} {
		d := testDHCP()
		d.LeaseDuration = Duration(lease)
		d.DomainName = ""
		d.Reservations = nil
		got, err := d.Config()
		if err != nil {
			t.Fatalf("lease %s and optional fields unset: %v", lease, err)
		}
		if got.DomainName != "" || len(got.Reservations) != 0 || got.LeaseDuration != lease {
			t.Fatalf("optional settings changed: %+v", got)
		}
	}
	// Reservations may sit inside or outside the dynamic range. The core
	// allocator owns excluding them, so conversion must preserve both.
	d := testDHCP()
	d.Reservations = append(d.Reservations, DHCPReservation{MAC: "02:00:00:00:00:02", IP: "192.168.1.150"})
	if got, err := d.Config(); err != nil || len(got.Reservations) != 2 {
		t.Fatalf("reservations inside and outside the range: %+v %v", got, err)
	}
}

func TestDHCPLoadAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kudurru.yaml")
	if err := os.WriteFile(path, []byte(`dhcp:
  enabled: true
  interface: eth0
  serverIP: 192.168.1.2
  subnet: 192.168.1.0/24
  rangeStart: 192.168.1.100
  rangeEnd: 192.168.1.200
  router: 192.168.1.1
  dnsServers: [192.168.1.2, 9.9.9.9]
  domainName: home.example
  leaseDuration: 12h
  reservations:
    - {mac: "02:00:00:00:00:01", ip: 192.168.1.10, hostname: printer}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.DHCP, testDHCP()) {
		t.Fatalf("file settings: %+v", cfg.DHCP)
	}
	nameFile := filepath.Join(dir, "dhcp-interface")
	if err := os.WriteFile(nameFile, []byte("enp1s0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"KUDURRU_DHCP__ENABLED=true",
		"KUDURRU_DHCP__INTERFACE_FILE=" + nameFile,
		"KUDURRU_DHCP__SERVERIP=192.168.2.2",
		"KUDURRU_DHCP__SUBNET=192.168.2.0/24",
		"KUDURRU_DHCP__RANGESTART=192.168.2.100",
		"KUDURRU_DHCP__RANGEEND=192.168.2.150",
		"KUDURRU_DHCP__ROUTER=192.168.2.1",
		"KUDURRU_DHCP__DNSSERVERS=192.168.2.2, 1.1.1.1",
		"KUDURRU_DHCP__DOMAINNAME=lan.example",
		"KUDURRU_DHCP__LEASEDURATION=24h",
	}
	envCfg, err := Load("", env)
	if err != nil {
		t.Fatal(err)
	}
	want := DHCP{
		Management: "file", DNSTTL: Duration(30 * time.Second), Scopes: []DHCPScope{}, TrustedRelays: []DHCPTrustedRelay{},
		Enabled: true, Interface: "enp1s0", ServerIP: "192.168.2.2", Subnet: "192.168.2.0/24",
		RangeStart: "192.168.2.100", RangeEnd: "192.168.2.150", Router: "192.168.2.1",
		DNSServers: []string{"192.168.2.2", "1.1.1.1"}, DomainName: "lan.example", LeaseDuration: Duration(24 * time.Hour),
		Reservations: []DHCPReservation{},
	}
	if !reflect.DeepEqual(envCfg.DHCP, want) {
		t.Fatalf("environment settings: %+v, want %+v", envCfg.DHCP, want)
	}
	for _, path := range []string{"enabled", "interface", "serverIP", "subnet", "rangeStart", "rangeEnd", "router", "dnsServers", "domainName", "leaseDuration"} {
		if !slices.Contains(envCfg.EnvLocked, "dhcp."+path) {
			t.Errorf("dhcp.%s not environment-locked: %v", path, envCfg.EnvLocked)
		}
	}
	// Environment takes precedence over the file, including disabling DHCP
	// when switching to the Kubernetes store driver.
	cfg, err = Load(path, []string{"KUDURRU_DHCP__ENABLED=false", "KUDURRU_STORE__DRIVER=kube", "KUDURRU_DHCP__LEASEDURATION=1h"})
	if err != nil || cfg.DHCP.Enabled || cfg.DHCP.LeaseDuration != Duration(time.Hour) {
		t.Fatalf("environment precedence: %+v %v", cfg, err)
	}
	if _, err := Load("", []string{"KUDURRU_DHCP__RESERVATIONS=[]"}); err == nil || !strings.Contains(err.Error(), "config file") {
		t.Fatalf("reservation list environment override: %v", err)
	}
	if _, err := Load("", []string{"KUDURRU_DHCP__ENABLED=true"}); err == nil || !strings.Contains(err.Error(), "dhcp.serverIP") {
		t.Fatalf("incomplete enabled environment config: %v", err)
	}
}

func TestDHCPLoadRejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		yaml, want string
	}{
		{"dhcp: {enabled: true}", "dhcp.serverIP"},
		{"dhcp: {leaseDuration: tomorrow}", "decode"},
		{"dhcp: {serverIP: '::1'}", "dhcp.serverIP"},
		{"dhcp: {dnsServers: [dns.example]}", "dhcp.dnsServers[0]"},
		{"dhcp: {reservations: [{ip: 192.168.1.10}]}", "dhcp.reservations[0].mac"},
		{"dhcp: {reservations: [{mac: '02:00:00:00:00:01'}]}", "dhcp.reservations[0].ip"},
	} {
		path := filepath.Join(t.TempDir(), "kudurru.yaml")
		if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %s", tc.yaml, err, tc.want)
		}
	}
}

func TestDHCPNodeScope(t *testing.T) {
	cur, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cur.DHCP = testDHCP()
	roundTrip, err := FromFlat(Flatten(cur))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip.DHCP, cur.DHCP) {
		t.Fatalf("flat round trip: %+v, want %+v", roundTrip.DHCP, cur.DHCP)
	}
	fresh, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Cache.MaxEntries++
	next, err := MergeCluster(cur, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.DHCP, cur.DHCP) || next.Cache.MaxEntries != fresh.Cache.MaxEntries {
		t.Fatalf("cluster reload changed DHCP: %+v", next.DHCP)
	}
	for _, k := range Schema {
		if !strings.HasPrefix(k.Path, "dhcp.") {
			continue
		}
		if k.Scope != ScopeNode || !k.HostOnly || !k.RestartRequired || k.Group != GroupDHCP {
			t.Errorf("DHCP metadata permits remote or live changes: %+v", k)
		}
		path := filepath.Join(t.TempDir(), "settings.yaml")
		if err := WriteSettingsFile(path, map[string]any{k.Path: k.Default}); err == nil || !strings.Contains(err.Error(), "node-scope") {
			t.Errorf("cluster settings file accepted %s: %v", k.Path, err)
		}
	}
	path := filepath.Join(t.TempDir(), "settings.yaml")
	if err := os.WriteFile(path, []byte("dhcp: {enabled: false}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("", []string{"KUDURRU_SETTINGS_FILE=" + path}); err == nil || !strings.Contains(err.Error(), "node-scope") {
		t.Fatalf("cluster layer loaded DHCP settings: %v", err)
	}
}
