package interop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// BIND generates the signatures/proofs; the shipped server validates them with
// an explicit local trust anchor through its actual listeners and raw cache.
func TestBINDDNSSECValidation(t *testing.T) {
	kudurrutest.RequireE2E(t)
	bind := namedBinary(t)
	for _, nsec3 := range []bool{false, true} {
		t.Run(fmt.Sprintf("NSEC3=%t", nsec3), func(t *testing.T) {
			dir := t.TempDir()
			const origin = "signed-validation.test."
			anchor := signedValidationZone(t, dir, origin, nsec3)
			port := kudurrutest.FreePort(t)
			primary := startNamed(t, bind, dir, namedOptions(dir, port)+fmt.Sprintf("zone %q { type primary; file \"zone.signed\"; };\n", origin), port)
			spec, cfg := kudurrutest.NewNodeSpec(t, dir, "validator")
			cfg["upstreams"].(map[string]any)["groups"] = map[string]any{"default": map[string]any{"servers": []string{primary.addr}, "timeout": "2s"}}
			cfg["dnssec"] = map[string]any{"enabled": true, "useBuiltinAnchors": false, "trustAnchors": []string{anchor}}
			kudurrutest.WriteConfig(t, spec.Config, cfg)
			kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
			for _, tc := range []struct {
				name  string
				typ   uint16
				rcode int
			}{
				{"www.", dns.TypeA, dns.RcodeSuccess},
				{"alias.", dns.TypeA, dns.RcodeSuccess},
				{"child.wild.", dns.TypeA, dns.RcodeSuccess},
				{"www.", dns.TypeAAAA, dns.RcodeSuccess},
				{"missing.", dns.TypeA, dns.RcodeNameError},
			} {
				for _, tcp := range []bool{false, true} {
					for attempt := range 2 { // First answer and raw-cache hit.
						m := validationQuery(t, spec.Ports.DNS, tc.name+origin, tc.typ, tcp, true, false, false)
						if m.Rcode != tc.rcode || !m.AuthenticatedData || !hasSignature(m) {
							t.Fatalf("%s type=%d tcp=%t attempt=%d: expected secure response: %s", tc.name, tc.typ, tcp, attempt, m)
						}
					}
				}
			}
			// AD interest without DO must retain local validation but strip proof RRs.
			m := validationQuery(t, spec.Ports.DNS, "www."+origin, dns.TypeA, false, false, true, false)
			if !m.AuthenticatedData || hasSignature(m) {
				t.Fatalf("AD/DO representation: %s", m)
			}
			m = validationQuery(t, spec.Ports.DNS, "www."+origin, dns.TypeA, false, true, false, true)
			if m.AuthenticatedData || !m.CheckingDisabled || !hasSignature(m) {
				t.Fatalf("CD bypass: %s", m)
			}
			// The signed zone deliberately has one altered A RR after signing.
			m = validationQuery(t, spec.Ports.DNS, "bogus."+origin, dns.TypeA, true, true, false, false)
			if m.Rcode != dns.RcodeServerFailure || m.AuthenticatedData || !hasValidationEDE(m) {
				t.Fatalf("tampered signed data accepted: %s", m)
			}
			m = validationQuery(t, spec.Ports.DNS, "bogus."+origin, dns.TypeA, true, true, false, true)
			if m.Rcode != dns.RcodeSuccess || m.AuthenticatedData || !hasA(m.Answer, "bogus."+origin, "192.0.2.99") {
				t.Fatalf("CD must return unchecked data: %s", m)
			}
		})
	}
}

func validationQuery(t *testing.T, addr, name string, typ uint16, tcp, do, ad, cd bool) *dns.Msg {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(name, typ)
	q.SetEdns0(1232, do)
	q.AuthenticatedData, q.CheckingDisabled = ad, cd
	client := &dns.Client{Timeout: 3 * time.Second}
	if tcp {
		client.Net = "tcp"
	}
	m, _, err := client.ExchangeContext(t.Context(), q, addr)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func hasSignature(m *dns.Msg) bool {
	for _, section := range [][]dns.RR{m.Answer, m.Ns} {
		for _, rr := range section {
			if rr.Header().Rrtype == dns.TypeRRSIG {
				return true
			}
		}
	}
	return false
}
func hasValidationEDE(m *dns.Msg) bool {
	if opt := m.IsEdns0(); opt != nil {
		for _, option := range opt.Option {
			if _, ok := option.(*dns.EDNS0_EDE); ok {
				return true
			}
		}
	}
	return false
}

func signedValidationZone(t *testing.T, dir, origin string, nsec3 bool) string {
	t.Helper()
	run := func(name string, args ...string) string {
		t.Helper()
		bin, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s required for signed BIND interoperability", name)
		}
		cmd := exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // fixed test tool and temporary files
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	key := run("dnssec-keygen", "-q", "-a", "ECDSAP256SHA256", "-f", "KSK", origin)
	writeFile(t, filepath.Join(dir, "zone"), fmt.Sprintf(`$ORIGIN %s
$TTL 300
@ IN SOA ns.%s hostmaster.%s 7 60 10 3600 60
@ IN NS ns
ns IN A 127.0.0.1
www IN A 192.0.2.80
alias IN CNAME www
*.wild IN A 192.0.2.81
bogus IN A 192.0.2.98
`, origin, origin, origin))
	args := []string{"-S", "-z", "-n", "1", "-o", origin, "-f", "zone.signed"}
	if nsec3 {
		args = append(args, "-3", "-", "-H", "0")
	}
	run("dnssec-signzone", append(args, "zone")...)
	data, err := os.ReadFile(filepath.Join(dir, "zone.signed")) //nolint:gosec // generated fixture under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "192.0.2.98") {
		t.Fatal("signed fixture missing tamper target")
	}
	writeFile(t, filepath.Join(dir, "zone.signed"), strings.ReplaceAll(string(data), "192.0.2.98", "192.0.2.99"))
	return run("dnssec-dsfromkey", "-2", key+".key")
}
