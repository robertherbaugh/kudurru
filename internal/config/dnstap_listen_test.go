package config

import (
	"strings"
	"testing"
)

// TestDnstapListenerValidation pins the bounds of listeners.dnstap (D372):
// an address of one of the three schemes on an IP address, a peer list for
// tcp:, a certificate for tls:, TLS settings only on tls: and no peer list
// on a Unix socket.
func TestDnstapListenerValidation(t *testing.T) {
	valid := func(l ...DnstapListener) *Config {
		cfg, err := Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Listeners.Dnstap = l
		return cfg
	}
	for name, l := range map[string]DnstapListener{
		"unix":            {Address: "unix:/run/kudurru/dnstap.sock"},
		"tcp":             {Address: "tcp:127.0.0.1:6000", AllowFrom: []string{"127.0.0.1", "10.0.0.0/24"}, Identities: []string{"edge-1"}, IgnoreClients: []string{"::1"}, MaxConnections: MaxDnstapListenConnections},
		"tls with CA":     {Address: "tls:0.0.0.0:6000", TLS: TLSFiles{CertFile: "/c", KeyFile: "/k"}, ClientCAFile: "/ca"},
		"tls with peers":  {Address: "tls:[::]:6000", TLS: TLSFiles{CertFile: "/c", KeyFile: "/k"}, AllowFrom: []string{"192.0.2.0/24"}},
		"tcp ipv6 listen": {Address: "tcp:[::1]:6000", AllowFrom: []string{"::1"}},
		"unforwarded":     {Address: "unix:/run/d.sock", Identities: []string{"edge-1", "relay-1"}, UnforwardedIdentities: []string{"edge-1"}},
	} {
		if err := valid(l).Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	tooMany := make([]DnstapListener, MaxDnstapListeners+1)
	for i := range tooMany {
		tooMany[i] = DnstapListener{Address: "unix:/run/kudurru/dnstap-" + string(rune('a'+i)) + ".sock"}
	}
	for name, tc := range map[string]struct {
		listeners []DnstapListener
		want      string
	}{
		"scheme":             {[]DnstapListener{{Address: "udp:127.0.0.1:6000"}}, "listeners.dnstap[0].address is not unix:/path"},
		"host name":          {[]DnstapListener{{Address: "tcp:localhost:6000", AllowFrom: []string{"127.0.0.1"}}}, "listen on an IP address"},
		"tcp open":           {[]DnstapListener{{Address: "tcp:127.0.0.1:6000"}}, "listeners.dnstap[0].allowFrom: a tcp: listener needs"},
		"tcp with tls":       {[]DnstapListener{{Address: "tcp:127.0.0.1:6000", AllowFrom: []string{"127.0.0.1"}, ClientCAFile: "/ca"}}, "apply to a tls: listener only"},
		"tls no cert":        {[]DnstapListener{{Address: "tls:127.0.0.1:6000", ClientCAFile: "/ca"}}, "needs certFile and keyFile"},
		"tls open":           {[]DnstapListener{{Address: "tls:127.0.0.1:6000", TLS: TLSFiles{CertFile: "/c", KeyFile: "/k"}}}, "needs clientCAFile or allowFrom"},
		"unix peers":         {[]DnstapListener{{Address: "unix:/run/d.sock", AllowFrom: []string{"127.0.0.1"}}}, "guarded by its file permissions"},
		"bad peer":           {[]DnstapListener{{Address: "tcp:127.0.0.1:6000", AllowFrom: []string{"edge"}}}, `allowFrom: "edge" is neither an address nor a CIDR`},
		"bad ignored":        {[]DnstapListener{{Address: "unix:/run/d.sock", IgnoreClients: []string{"10.0.0.0/33"}}}, "ignoreClients:"},
		"empty identity":     {[]DnstapListener{{Address: "unix:/run/d.sock", Identities: []string{""}}}, "identities: use 1 to 255"},
		"empty unforwarded":  {[]DnstapListener{{Address: "unix:/run/d.sock", UnforwardedIdentities: []string{""}}}, "unforwardedIdentities: use 1 to 255"},
		"unforwarded hidden": {[]DnstapListener{{Address: "unix:/run/d.sock", Identities: []string{"edge-1"}, UnforwardedIdentities: []string{"edge-2"}}}, `unforwardedIdentities: "edge-2" is not in identities`},
		"connections":        {[]DnstapListener{{Address: "unix:/run/d.sock", MaxConnections: MaxDnstapListenConnections + 1}}, "maxConnections: use 0"},
		"twice":              {[]DnstapListener{{Address: "unix:/run/d.sock"}, {Address: "unix:/run/d.sock"}}, "listeners.dnstap[1].address: /run/d.sock is listed twice"},
		"too many":           {tooMany, "at most 8 listeners"},
	} {
		if err := valid(tc.listeners...).Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
