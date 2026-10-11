package config

import (
	"net/netip"
	"strings"
	"testing"
)

func TestBlockPageDefaults(t *testing.T) {
	cfg, err := Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.BlockPage
	if b.Enabled || b.IPv4 != "" || !b.ShowDetails || b.Title == "" || b.Message == "" || b.RateLimit != 120 {
		t.Errorf("blockPage defaults: %+v", b)
	}
	if l := cfg.Listeners.BlockPage; l.HTTP != ":80" || l.HTTPS != "" {
		t.Errorf("listeners.blockPage defaults: %+v", l)
	}
}

func TestBlockPageAddrs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		doc    string
		v4, v6 string
	}{
		{"configured", "blockPage: {enabled: true, ipv4: 192.0.2.10, ipv6: '2001:db8::10'}\n", "192.0.2.10", "2001:db8::10"},
		{"configured v6 only", "blockPage: {enabled: true, ipv6: '2001:db8::10'}\nlisteners: {blockPage: {http: '192.0.2.99:80'}}\n", "", "2001:db8::10"},
		{"block page listener", "blockPage: {enabled: true}\nlisteners: {blockPage: {http: '192.0.2.20:80', https: '[2001:db8::20]:443'}}\n", "192.0.2.20", "2001:db8::20"},
		{"dns listener", "blockPage: {enabled: true}\nlisteners: {dns: [{address: '0.0.0.0:53', protocols: [udp]}, {address: '192.0.2.30:53', protocols: [udp]}]}\n", "192.0.2.30", ""},
		{"loopback dns", "blockPage: {enabled: true}\nlisteners: {dns: [{address: '127.0.0.1:5353', protocols: [udp]}]}\n", "127.0.0.1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadYAML(t, tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			v4, v6 := cfg.BlockPageAddrs()
			if got := addrString(v4); got != tc.v4 {
				t.Errorf("v4 = %q, want %q", got, tc.v4)
			}
			if got := addrString(v6); got != tc.v6 {
				t.Errorf("v6 = %q, want %q", got, tc.v6)
			}
		})
	}
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func TestBlockPageValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{"no address", "blockPage: {enabled: true}\n", "blockPage.enabled: no address to send blocked clients to"},
		{"loopback behind an edge", "blockPage: {enabled: true}\nlisteners: {dns: [{address: '127.0.0.1:5353', protocols: [udp]}], proxyProtocolFrom: [127.0.0.1/32]}\n", "no address to send blocked clients to"},
		{"default response without the page", "blocking: {defaultResponse: blockPage}\n", "blocking.defaultResponse blockPage needs blockPage.enabled"},
		{"bad addresses", "blockPage: {ipv4: '2001:db8::1', ipv6: 192.0.2.1}\n", `blockPage.ipv4: "2001:db8::1" is not an IPv4 address`},
		{"bad v6", "blockPage: {ipv6: 192.0.2.1}\n", `blockPage.ipv6: "192.0.2.1" is not an IPv6 address`},
		{"no listener", "blockPage: {enabled: true, ipv4: 192.0.2.1}\nlisteners: {blockPage: {http: ''}}\n", "needs listeners.blockPage.http or listeners.blockPage.https"},
		{"bad listener", "listeners: {blockPage: {http: '80'}}\n", `listeners.blockPage.http: "80" is not host:port`},
		{"same listener", "listeners: {blockPage: {http: ':8080', https: ':8080'}}\n", "http and https bind the same address"},
		{"half a key pair", "listeners: {blockPage: {tls: {certFile: /x.crt}}}\n", "listeners.blockPage.tls: certFile and keyFile must be set together"},
		{"contact", "blockPage: {contact: 'javascript:alert(1)'}\n", "blockPage.contact"},
		{"contact query", "blockPage: {contact: 'a@b.example?subject=x'}\n", "blockPage.contact"},
		{"dashboard", "blockPage: {dashboardURL: 'ftp://x'}\n", "blockPage.dashboardURL"},
		{"title", "blockPage: {title: '" + strings.Repeat("x", 201) + "'}\n", "blockPage.title: longer than 200"},
		{"rate", "blockPage: {rateLimit: -1}\n", "blockPage.rateLimit: -1 is negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.doc)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	cfg, err := loadYAML(t, "blockPage: {enabled: true, ipv4: 192.0.2.1, contact: admin@example.net}\nblocking: {defaultResponse: blockPage}\n")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Blocking.DefaultResponse != "blockPage" {
		t.Errorf("defaultResponse = %q", cfg.Blocking.DefaultResponse)
	}
}

func TestBlockPageContactURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                             "",
		"admin@example.net":            "mailto:admin@example.net",
		"mailto:admin@example.net":     "mailto:admin@example.net",
		"https://help.example/blocked": "https://help.example/blocked",
		"http://intranet/it":           "http://intranet/it",
	} {
		got, err := BlockPageContactURL(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "Admin <a@b.example>", "data:text/html,x", "https://", "call 555"} {
		if _, err := BlockPageContactURL(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}
