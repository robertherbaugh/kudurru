// Package interop runs kudurru against BIND 9 (named) on loopback:
// named as a secondary of a kudurru primary zone (AXFR, NOTIFY, TSIG,
// transfer policy) and named as a primary behind a kudurru conditional
// upstream group. The tests need KUDURRU_E2E=1 (make interop) and skip
// when named is not installed.
package interop

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

var kudurruBin string

func TestMain(m *testing.M) { os.Exit(kudurrutest.MainBuild(m, &kudurruBin)) }

const (
	keyName = "xfr-key"
	keyAlg  = dns.HmacSHA256
)

func newSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// zonesYAML is the objectsDir file with the two primary zones: interop.test
// (transfers from 127.0.0.0/8 signed with xfr-key, NOTIFY to named) and
// denied.test (zoneTransfer: deny).
func zonesYAML(notifyTarget string) string {
	return fmt.Sprintf(`apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: interop-test }
spec:
  zoneName: interop.test.
  type: primary
  soa: { mname: ns1.interop.test., rname: hostmaster.interop.test., refresh: 3600, retry: 600 }
  nameservers: [ns1.interop.test.]
  policy:
    zoneTransfer: useNetworkACL
    transferACL: ["127.0.0.0/8"]
    transferTSIGKeyRefs: [%[2]s]
    notify: specified
    notifyTargets: ["%[1]s"]
---
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: denied-test }
spec:
  zoneName: denied.test.
  type: primary
  soa: { mname: ns1.denied.test., rname: hostmaster.denied.test. }
  nameservers: [ns1.denied.test.]
  policy:
    zoneTransfer: deny
    transferTSIGKeyRefs: [%[2]s]
---
apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: { name: denied, namespace: default }
spec:
  zoneRef: { name: denied-test }
  endpoints:
    - { dnsName: ns1.denied.test, recordType: A, targets: ["127.0.0.1"] }
    - { dnsName: secret.denied.test, recordType: A, targets: ["192.0.2.99"] }
`, notifyTarget, keyName)
}

// recordsYAML is the interop.test record set; extra adds endpoints.
func recordsYAML(extra ...string) string {
	var b strings.Builder
	b.WriteString(`apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: { name: interop, namespace: default }
spec:
  zoneRef: { name: interop-test }
  endpoints:
    - { dnsName: ns1.interop.test, recordType: A, targets: ["127.0.0.1"] }
    - { dnsName: www.interop.test, recordType: A, targets: ["192.0.2.10", "192.0.2.11"] }
    - { dnsName: txt.interop.test, recordType: TXT, targets: ["hello from kudurru"] }
`)
	for _, e := range extra {
		b.WriteString("    - " + e + "\n")
	}
	return b.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil { //nolint:gosec // test-owned temporary fixture paths
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func hasA(rrs []dns.RR, name, ip string) bool {
	for _, rr := range rrs {
		if a, ok := rr.(*dns.A); ok && strings.EqualFold(a.Hdr.Name, dns.Fqdn(name)) && a.A.String() == ip {
			return true
		}
	}
	return false
}

// TestBINDSecondary makes named a TSIG-authenticated secondary of a
// kudurru primary zone.
func TestBINDSecondary(t *testing.T) {
	kudurrutest.RequireE2E(t)
	namedBin := namedBinary(t)
	dir := t.TempDir()
	secret := newSecret(t)
	namedPort := kudurrutest.FreePort(t)

	// kudurru: the primary.
	spec, cfg := kudurrutest.NewNodeSpec(t, dir, "primary")
	objDir := filepath.Join(dir, "objects.d")
	if err := os.MkdirAll(objDir, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(objDir, "zones.yaml"), zonesYAML(fmt.Sprintf("127.0.0.1:%d", namedPort)))
	writeFile(t, filepath.Join(objDir, "records.yaml"), recordsYAML())
	secretFile := filepath.Join(dir, "xfr.secret")
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg["objectsDir"] = objDir
	cfg["tsigKeys"] = []any{map[string]any{"name": keyName, "algorithm": "hmac-sha256", "secretFile": secretFile}}
	cfg["notify"] = map[string]any{"source": "127.0.0.1"}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	start := time.Now()
	kd := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
	t.Logf("timing: kudurru ready in %s", time.Since(start).Round(time.Millisecond))

	var primarySerial uint32
	if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool {
		primarySerial, _ = soaSerial(t, spec.Ports.DNS, "interop.test.")
		return primarySerial != 0
	}) {
		t.Fatal("kudurru does not serve interop.test SOA")
	}

	// named: the secondary.
	ndir := filepath.Join(dir, "named")
	if err := os.MkdirAll(ndir, 0o750); err != nil {
		t.Fatal(err)
	}
	_, kport, _ := strings.Cut(spec.Ports.DNS, ":")
	conf := namedOptions(ndir, namedPort) + fmt.Sprintf(`
key "%[1]s" { algorithm hmac-sha256; secret "%[2]s"; };
zone "interop.test" {
	type secondary;
	file "interop.test.db";
	primaries port %[3]s { 127.0.0.1 key "%[1]s"; };
};
zone "denied.test" {
	type secondary;
	file "denied.test.db";
	primaries port %[3]s { 127.0.0.1 key "%[1]s"; };
};
`, keyName, secret, kport)
	start = time.Now()
	nd := startNamed(t, namedBin, ndir, conf, namedPort)

	t.Run("AXFR", func(t *testing.T) {
		if !kudurrutest.Eventually(30*time.Second, 200*time.Millisecond, func() bool {
			s, rcode := soaSerial(t, nd.addr, "interop.test.")
			return rcode == dns.RcodeSuccess && s == primarySerial
		}) {
			t.Fatalf("named did not load interop.test (serial %d) from kudurru", primarySerial)
		}
		t.Logf("timing: named transferred interop.test in %s", time.Since(start).Round(time.Millisecond))
		r, err := query(t, nd.addr, "www.interop.test.", dns.TypeA, false)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Authoritative || !hasA(r.Answer, "www.interop.test.", "192.0.2.10") || !hasA(r.Answer, "www.interop.test.", "192.0.2.11") {
			t.Fatalf("named answer for www.interop.test: %v", r)
		}
		if dig, err := exec.LookPath("dig"); err == nil {
			host, port, _ := strings.Cut(nd.addr, ":")
			out, err := exec.Command(dig, "@"+host, "-p", port, "+short", "www.interop.test", "A").CombinedOutput() //nolint:gosec // test binary
			if err != nil || !strings.Contains(string(out), "192.0.2.10") || !strings.Contains(string(out), "192.0.2.11") {
				t.Errorf("dig @named www.interop.test: %v\n%s", err, out)
			}
		}
		r, err = query(t, nd.addr, "txt.interop.test.", dns.TypeTXT, false)
		if err != nil || len(r.Answer) != 1 || !strings.Contains(r.Answer[0].String(), "hello from kudurru") {
			t.Fatalf("named TXT answer: %v %v", r, err)
		}
		// The transfer named received is exactly the zone kudurru serves.
		mine, err := axfr(spec.Ports.DNS, "interop.test.", keyName, keyAlg, secret)
		if err != nil {
			t.Fatalf("signed AXFR from kudurru: %v", err)
		}
		theirs, err := axfr(nd.addr, "interop.test.", "", "", "")
		if err != nil {
			t.Fatalf("AXFR from named (no transfer ACL on the secondary): %v", err)
		}
		if a, b := canonicalSet(mine), canonicalSet(theirs); a != b {
			t.Errorf("zone differs between kudurru and named:\nkudurru:\n%s\nnamed:\n%s", a, b)
		}
		if !strings.Contains(nd.log.String(), "Transfer status: success") {
			t.Errorf("named log has no successful transfer")
		}
	})

	t.Run("NOTIFY", func(t *testing.T) {
		before, _ := soaSerial(t, spec.Ports.DNS, "interop.test.")
		changed := time.Now()
		writeFile(t, filepath.Join(objDir, "records.yaml"), recordsYAML(`{ dnsName: new.interop.test, recordType: A, targets: ["192.0.2.77"] }`))
		var after uint32
		if !kudurrutest.Eventually(20*time.Second, 50*time.Millisecond, func() bool {
			var rcode int
			after, rcode = soaSerial(t, spec.Ports.DNS, "interop.test.")
			return rcode == dns.RcodeSuccess && after != 0 && after != before
		}) {
			t.Fatalf("kudurru serial did not move after the record change (still %d)", before)
		}
		bumped := time.Since(changed)
		// named's refresh timer is clamped to min-refresh-time (300 s):
		// seeing the new serial within 30 s means NOTIFY triggered it.
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			s, _ := soaSerial(t, nd.addr, "interop.test.")
			return s == after
		}) {
			s, _ := soaSerial(t, nd.addr, "interop.test.")
			t.Fatalf("named still at serial %d, kudurru at %d: NOTIFY did not trigger a refresh", s, after)
		}
		t.Logf("timing: file change -> kudurru serial %d->%d in %s, -> named in %s", before, after, bumped.Round(time.Millisecond), time.Since(changed).Round(time.Millisecond))
		r, err := query(t, nd.addr, "new.interop.test.", dns.TypeA, false)
		if err != nil || !hasA(r.Answer, "new.interop.test.", "192.0.2.77") {
			t.Fatalf("named does not serve the new record: %v %v", r, err)
		}
		if !strings.Contains(nd.log.String(), "notify from 127.0.0.1") {
			t.Errorf("named log shows no NOTIFY from kudurru")
		}
	})

	t.Run("TSIGRefused", func(t *testing.T) {
		cases := []struct {
			name, key, secret string
		}{
			{"unsigned", "", ""},
			{"wrong secret", keyName, newSecret(t)},
			{"unknown key", "other-key", secret},
		}
		for _, c := range cases {
			rrs, err := axfr(spec.Ports.DNS, "interop.test.", c.key, keyAlg, c.secret)
			if err == nil {
				t.Errorf("%s: AXFR succeeded with %d records", c.name, len(rrs))
				continue
			}
			for _, rr := range rrs {
				if rr.Header().Rrtype != dns.TypeSOA {
					t.Errorf("%s: transfer leaked %s", c.name, rr)
				}
			}
			t.Logf("%s: refused: %v", c.name, err)
		}
	})

	t.Run("ZoneTransferDeny", func(t *testing.T) {
		if s, rcode := soaSerial(t, spec.Ports.DNS, "denied.test."); rcode != dns.RcodeSuccess || s == 0 {
			t.Fatalf("kudurru does not serve denied.test (rcode %d)", rcode)
		}
		rrs, err := axfr(spec.Ports.DNS, "denied.test.", keyName, keyAlg, secret)
		if err == nil {
			t.Fatalf("AXFR of a zoneTransfer: deny zone succeeded with %d records", len(rrs))
		}
		t.Logf("denied.test: %v", err)
		// named tried the same transfer at startup and cannot serve it.
		_, rcode := soaSerial(t, nd.addr, "denied.test.")
		if rcode != dns.RcodeServerFailure {
			t.Errorf("named answers denied.test with rcode %s, want SERVFAIL (zone never loaded)", dns.RcodeToString[rcode])
		}
		r, err := query(t, nd.addr, "secret.denied.test.", dns.TypeA, false)
		if err == nil && hasA(r.Answer, "secret.denied.test.", "192.0.2.99") {
			t.Errorf("named serves data of a denied zone")
		}
	})

	_ = kd
}

// canonicalSet renders RRs sorted, without the trailing duplicate SOA.
func canonicalSet(rrs []dns.RR) string {
	seen := map[string]bool{}
	var lines []string
	for _, rr := range rrs {
		if rr.Header().Rrtype == dns.TypeTSIG {
			continue
		}
		s := strings.ToLower(rr.String())
		if !seen[s] {
			seen[s] = true
			lines = append(lines, s)
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}
