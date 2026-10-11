package fuzz

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/internal/edge"
	"github.com/robertherbaugh/kudurru/internal/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/edge/keepalived"
)

// unsafeChars can break out of a quoted string or a line in keepalived.conf,
// dnsdist.conf (Lua) and dnsdist.yml.
const unsafeChars = "\n\r\"\\{}#!;"

// checkNoRawInjection fails when an input value holding a character that
// could end a token, string or line appears verbatim in a rendered file:
// such a value must be refused or escaped.
func checkNoRawInjection(t *testing.T, file string, data []byte, inputs map[string]string) {
	t.Helper()
	for field, v := range inputs {
		if v == "" || !strings.ContainsAny(v, unsafeChars) {
			continue
		}
		if strings.Contains(string(data), v) {
			t.Fatalf("%s: %s %q accepted and rendered verbatim:\n%s", file, field, v, data)
		}
	}
}

// FuzzKeepalivedRender renders keepalived.conf from fuzzed flag values
// (`kudurru vip run --vip --node --unicast-src --peers --hmac-key-file`).
// Whatever Validate accepts must render without injecting configuration:
// balanced braces and no unsafe value copied verbatim.
func FuzzKeepalivedRender(f *testing.F) {
	f.Add("dns=10.0.0.53/24@eth0#53", "node-a", "10.0.0.11", "10.0.0.12", "/run/secrets/vrrp_hmac_{vrid}", "health.kudurru.internal.", "/run/kudurru")
	f.Add("v6=2001:db8::53/64@eth0#54", "node-b", "2001:db8::11", "2001:db8::12", "", "", "")
	f.Add("dns=10.0.0.53/24@eth0#53", "node-a", "fe80::1%eth0", "fe80::2%eth0 }", "", "", "")
	f.Add("x=10.0.0.53/32@lo:1#255", "a.b_c-d", "10.0.0.1", "10.0.0.2", "/k{vrid}", "q.", "/r")
	f.Fuzz(func(t *testing.T, vipSpec, router, src, peer, hmac, qname, runDir string) {
		v, err := keepalived.ParseVIP(vipSpec)
		if err != nil {
			return
		}
		// E2E-6: Validate's safePath admits '{' and '}' (for the {vrid}
		// placeholder) in every path-like field, so a value such as "/x}"
		// closes a keepalived.conf block early.
		if skipKnownBug("E2E-6") {
			for _, v := range []string{strings.ReplaceAll(hmac, "{vrid}", ""), qname, runDir} {
				if strings.ContainsAny(v, "{}") {
					return
				}
			}
		}
		c := keepalived.Config{
			RouterID: router, VIPs: []keepalived.VIP{v}, UnicastSrc: src, Peers: []string{peer},
			HMACKeyFile: hmac, RunDir: runDir, Probe: keepalived.Probe{QName: qname},
		}
		out, err := keepalived.Render(c)
		if err != nil {
			return
		}
		if o, c := strings.Count(string(out), "{"), strings.Count(string(out), "}"); o != c {
			t.Fatalf("unbalanced braces (%d/%d):\n%s", o, c, out)
		}
		checkNoRawInjection(t, "keepalived.conf", out, map[string]string{
			"vip": vipSpec, "router_id": router, "unicast_src": src, "peer": peer,
			"hmac": hmac, "qname": qname, "runDir": runDir,
		})
	})
}

// FuzzKeepalivedFIFOLine feeds lines of the keepalived notify FIFO to the
// `kudurru vip fifo` parser.
func FuzzKeepalivedFIFOLine(f *testing.F) {
	f.Add(`INSTANCE "VI_dns" MASTER 200`)
	f.Add(`INSTANCE "VI_dns" BACKUP 190`)
	f.Add(`GROUP "G" FAULT 0`)
	f.Add(`INSTANCE "VI_\"x" STOP 0`)
	f.Add("")
	f.Fuzz(func(t *testing.T, line string) {
		n, err := keepalived.ParseLine(line)
		if err != nil {
			return
		}
		if strings.ContainsAny(n.Name, "\n\r") {
			t.Fatalf("instance name with a line break: %+v", n)
		}
	})
}

// FuzzDNSDistRender builds an edge Topology from fuzzed `kudurru edge
// run` inputs and renders it with both renderers. Whatever edge.Build
// accepts must render to YAML that parses and keeps server_id, and to Lua
// with no unsafe value copied verbatim.
func FuzzDNSDistRender(f *testing.F) {
	f.Add("edge", "0.0.0.0", "10.0.0.0/8", "example.com.", "node-a", "10.0.0.11:5353", "10.0.0.0/24", "0123456789abcdef0123456789abcdef")
	f.Add("node-a", "::", "::/0", ".", "", "[2001:db8::1]:5353", "2001:db8::/64", "")
	f.Add("x\"y", "0.0.0.0", "0.0.0.0/0", "a\nb.", "n\"); os.exit(1) --", "127.0.0.1:5353", "", "s\nx")
	f.Fuzz(func(t *testing.T, serverID, listen, acl, zone, backendName, backendAddr, xfrSource, secret string) {
		o := edge.Options{
			ServerID: serverID, Listen: []string{listen}, ACL: []string{acl},
			AuthoritativeZones: []string{zone}, TransferSources: []string{xfrSource},
			ProxyProtocolSecret: secret,
		}
		creds := edge.Credentials{ConsoleKey: "a3VkdXJydS1mdXp6LWNvbnNvbGUta2V5LTMyYnl0ZXM=", APIKey: "fuzz-api-key", Password: "fuzz-password"} //nolint:gosec // fixed test credentials
		topo, err := edge.Build(o, []edge.Backend{{Name: backendName, Address: backendAddr}}, creds)
		if err != nil {
			return
		}
		inputs := map[string]string{
			"server_id": serverID, "listen": listen, "acl": acl, "zone": zone,
			"backend name": backendName, "backend address": backendAddr, "transfer source": xfrSource, "secret": secret,
		}
		for _, name := range []string{"yaml", "lua"} {
			r, err := dnsdist.NewRenderer(name)
			if err != nil {
				t.Fatal(err)
			}
			files, err := r.Render(topo)
			if err != nil {
				continue
			}
			for _, file := range files {
				if strings.HasSuffix(file.Name, ".yml") {
					var doc struct {
						General struct {
							ServerID string `json:"server_id"`
						} `json:"general"`
					}
					if err := yaml.Unmarshal(file.Data, &doc); err != nil {
						t.Fatalf("%s does not parse: %v\n%s", file.Name, err, file.Data)
					}
					if doc.General.ServerID != topo.ServerID {
						t.Fatalf("%s: server_id %q, want %q", file.Name, doc.General.ServerID, topo.ServerID)
					}
					continue
				}
				checkNoRawInjection(t, file.Name, file.Data, inputs)
			}
		}
	})
}
