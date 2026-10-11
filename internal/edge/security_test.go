package edge

import (
	"strings"
	"testing"
)

// TestValidateRefusesUnsafeRenderedStrings is the regression test of the
// M5 finding "backend addresses, bind addresses, certificate paths and DoH
// paths reach dnsdist.yml/dnsdist.lua unrestricted": a newline in a
// backend address ended the "-- backend" comment of dnsdist.lua and the
// rest of the line ran as Lua. Every rendered string is now limited to the
// characters its field needs.
func TestValidateRefusesUnsafeRenderedStrings(t *testing.T) {
	for name, tc := range map[string]struct {
		opts Options
		be   []Backend
	}{
		"backend address newline": {be: []Backend{{Name: "n1", Address: "10.0.0.1\nos.execute('id')--:5353"}}},
		"backend address quote":   {be: []Backend{{Name: "n1", Address: `10.0.0.1"x:5353`}}},
		"backend address space":   {be: []Backend{{Name: "n1", Address: "a b:5353"}}},
		"internal address":        {be: []Backend{{Name: "n1", Address: "10.0.0.1:5353", InternalAddress: "x\ny:8082"}}},
		"bind address":            {opts: Options{Listen: []string{"0.0.0.0\n--"}}},
		"cert file newline":       {opts: Options{Protocols: []Protocol{DoT}, CertFile: "/c\nos.exit()", KeyFile: "/k"}},
		"key file quote":          {opts: Options{Protocols: []Protocol{DoT}, CertFile: "/c", KeyFile: `/k"`}},
	} {
		t.Run(name, func(t *testing.T) {
			tc.opts.ServerID = "e"
			if _, err := Build(tc.opts, tc.be, testCreds); err == nil {
				t.Fatal("Build accepted an unsafe string")
			}
		})
	}
	// Ordinary values stay valid: IPv4, IPv6, host names, paths.
	top, err := Build(Options{ServerID: "e", Protocols: []Protocol{Do53, DoH}, CertFile: "/etc/kudurru/tls/tls.crt", KeyFile: "/etc/kudurru/tls/tls.key"},
		[]Backend{{Name: "n1", Address: "10.0.0.1:5353"}, {Name: "n2", Address: "[fd00::2]:5353"}, {Name: "n3", Address: "kudurru-2.kudurru-headless.dns.svc:5353", InternalAddress: "kudurru-2.kudurru-headless.dns.svc:8082"}},
		testCreds)
	if err != nil {
		t.Fatalf("valid topology refused: %v", err)
	}
	if len(top.Backends) != 3 || !strings.Contains(top.Backends[2].Address, "svc") {
		t.Fatalf("backends %+v", top.Backends)
	}
	for i, b := range top.Binds {
		if b.Protocol != DoH {
			continue
		}
		top.Binds[i].DoHPaths = []string{"/dns-query", "/resolve"}
		if err := top.Validate(); err != nil {
			t.Fatalf("valid DoH paths refused: %v", err)
		}
		top.Binds[i].DoHPaths = []string{"/dns-query\"\nos.exit()"}
		if err := top.Validate(); err == nil {
			t.Fatal("Validate accepted an unsafe DoH path")
		}
	}
}
