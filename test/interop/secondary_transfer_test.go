package interop

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

const secondaryBulkRecords = 600

// secondaryPrimaryData exceeds one maximum-size DNS message. BIND's small
// transfer-message-size also exercises TSIG chains and unsigned intermediate
// messages over a long stream. The records are zone data, not endpoint objects.
func secondaryPrimaryData(origin string) string {
	data := fmt.Sprintf(`$ORIGIN %[1]s
$TTL 300
@ 537 IN SOA ns1.%[1]s hostmaster.%[1]s 2026101401 61 13 301 47
@ 721 IN NS ns1.%[1]s
ns1 87 IN A 127.0.0.1
www 301 IN A 192.0.2.53
alias 287 IN CNAME www.%[1]s
txt 214 IN TXT "Hello From BIND"
opaque 89 IN TYPE65400 \# 4 01020304
@ 90 IN DNSKEY 256 3 15 %[2]s
@ 91 IN RRSIG DNSKEY 15 2 90 20300101000000 20200101000000 42 %[1]s %[3]s
@ 92 IN NSEC alias.%[1]s NS SOA RRSIG NSEC DNSKEY
`, origin, base64.StdEncoding.EncodeToString(make([]byte, 32)), base64.StdEncoding.EncodeToString(make([]byte, 64)))
	var b strings.Builder
	b.WriteString(data)
	for i := range secondaryBulkRecords {
		fmt.Fprintf(&b, "bulk%04d 93 IN TXT %q\n", i, strings.Repeat("x", 180))
	}
	return b.String()
}

func secondaryObject(origin, key string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{Spec: v1alpha1.DNSZoneSpec{
		ZoneName: origin, Type: v1alpha1.ZoneTypeSecondary,
		Secondary: &v1alpha1.SecondarySpec{TSIGKeyRef: key, TransferProtocol: "tcp"},
	}}
}

func secondaryKeyring(t *testing.T, key, algorithm, secret string) *dnsserver.StaticKeyring {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: key, Algorithm: algorithm, Secret: decoded})
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func secondaryFixtureRecords(t *testing.T, origin, data string) []dns.RR {
	t.Helper()
	p := dns.NewZoneParser(strings.NewReader(data), origin, "secondary.db")
	var rrs []dns.RR
	for rr, ok := p.Next(); ok; rr, ok = p.Next() {
		rrs = append(rrs, rr)
	}
	if err := p.Err(); err != nil {
		t.Fatal(err)
	}
	return rrs
}

func secondaryRecordSet(rrs []dns.RR) string {
	lines := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		lines = append(lines, rr.String())
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func TestBINDInboundSecondaryTransfer(t *testing.T) {
	kudurrutest.RequireE2E(t)
	bin := namedBinary(t)
	if version, err := exec.Command(bin, "-v").CombinedOutput(); err == nil { //nolint:gosec // test binary
		t.Logf("primary: %s", strings.TrimSpace(string(version)))
	}
	dir := t.TempDir()
	port := kudurrutest.FreePort(t)
	secret256, secret512 := newSecret(t), newSecret(t)
	const key256 = "secondary-xfr256.test."
	const key512 = "secondary-xfr512.test."
	ring256 := secondaryKeyring(t, key256, "hmac-sha256", secret256)
	ring512 := secondaryKeyring(t, key512, "hmac-sha512", secret512)
	fixtures := []struct {
		origin, key string
		ring        dnsserver.TSIGKeyring
	}{
		{"secondary-unsigned.test.", "", nil},
		{"secondary-signed.test.", key256, ring256},
		{"secondary-sha512.test.", key512, ring512},
	}
	options := strings.Replace(namedOptions(dir, port), "recursion no;", "recursion no;\n\ttransfer-message-size 1024;", 1)
	conf := options + fmt.Sprintf(`
key %q { algorithm hmac-sha256; secret %q; };
key %q { algorithm hmac-sha512; secret %q; };
`, key256, secret256, key512, secret512)
	for _, fixture := range fixtures {
		filename := strings.TrimSuffix(fixture.origin, ".") + ".db"
		writeFile(t, filepath.Join(dir, filename), secondaryPrimaryData(fixture.origin))
		access := "any;"
		if fixture.key != "" {
			access = fmt.Sprintf("key %q;", fixture.key)
		}
		conf += fmt.Sprintf(`zone %q {
	type primary;
	file %q;
	allow-query { %s };
	allow-transfer { %s };
};
`, fixture.origin, filename, access, access)
	}
	primary := startNamed(t, bin, dir, conf, port)
	for _, fixture := range fixtures {
		t.Run(fixture.origin, func(t *testing.T) {
			client := &secondary.TransferClient{Keyring: fixture.ring, Timeout: 5 * time.Second}
			zone := secondaryObject(fixture.origin, fixture.key)
			want := secondaryFixtureRecords(t, fixture.origin, secondaryPrimaryData(fixture.origin))
			soa, err := client.SOA(context.Background(), zone, primary.addr)
			if err != nil {
				t.Fatalf("SOA from BIND: %v", err)
			}
			if soa.String() != want[0].String() {
				t.Fatalf("SOA changed: got %s, want %s", soa, want[0])
			}
			start := time.Now()
			got, err := client.AXFR(context.Background(), zone, primary.addr)
			if err != nil {
				t.Fatalf("AXFR from BIND: %v", err)
			}
			if len(got) != len(want) || secondaryRecordSet(got) != secondaryRecordSet(want) {
				t.Fatalf("transferred zone differs from BIND fixture: got %d records, want %d", len(got), len(want))
			}
			if _, ok := got[0].(*dns.SOA); !ok {
				t.Fatal("AXFR did not return the initial SOA first")
			}
			soas := 0
			for _, rr := range got {
				if rr.Header().Rrtype == dns.TypeSOA {
					soas++
				}
			}
			if soas != 1 {
				t.Fatalf("AXFR returned %d SOAs, want one", soas)
			}
			var expanded int
			for _, rr := range got {
				expanded += dns.Len(rr)
			}
			t.Logf("BIND -> TransferClient: signed=%v records=%d expanded-record-bytes=%d elapsed=%s; TTLs, SOA timers, TXT case, unknown RR and DNSSEC RRs preserved",
				fixture.key != "", len(got), expanded, time.Since(start).Round(time.Millisecond))
		})
	}
	for _, line := range strings.Split(primary.log.String(), "\n") {
		if strings.Contains(line, "AXFR ended:") {
			t.Logf("BIND primary evidence: %s", line)
		}
	}

	t.Run("TSIGRejection", func(t *testing.T) {
		for _, test := range []struct {
			name, key string
			ring      dnsserver.TSIGKeyring
		}{
			{"unsigned", "", nil},
			{"wrong secret", key256, secondaryKeyring(t, key256, "hmac-sha256", newSecret(t))},
			{"unknown key", "unknown-key.test.", secondaryKeyring(t, "unknown-key.test.", "hmac-sha256", secret256)},
			{"wrong algorithm", key256, secondaryKeyring(t, key256, "hmac-sha512", secret256)},
		} {
			t.Run(test.name, func(t *testing.T) {
				client := &secondary.TransferClient{Keyring: test.ring, Timeout: 3 * time.Second}
				zone := secondaryObject("secondary-signed.test.", test.key)
				soa, err := client.SOA(context.Background(), zone, primary.addr)
				if err == nil || soa != nil {
					t.Fatalf("unauthenticated SOA accepted: %v", err)
				}
				t.Logf("BIND SOA rejected: %v", err)
				rrs, err := client.AXFR(context.Background(), zone, primary.addr)
				if err == nil || rrs != nil {
					t.Fatalf("unauthenticated AXFR accepted or returned partial data: %v", err)
				}
				t.Logf("BIND AXFR rejected: %v", err)
			})
		}
	})

	t.Run("RequiredReplySignature", func(t *testing.T) {
		// Obtain a real unsigned BIND answer through a request-stripping
		// relay. The receiver still requires its configured TSIG signature.
		relay := secondaryUnsignedRelay(t, primary.addr)
		client := &secondary.TransferClient{Keyring: ring256, Timeout: 3 * time.Second}
		zone := secondaryObject("secondary-unsigned.test.", key256)
		soa, err := client.SOA(context.Background(), zone, relay)
		if soa != nil || !errors.Is(err, dns.ErrNoSig) {
			t.Fatalf("unsigned BIND SOA reply accepted: %v", err)
		}
		rrs, err := client.AXFR(context.Background(), zone, relay)
		if rrs != nil || !errors.Is(err, dns.ErrNoSig) {
			t.Fatalf("unsigned BIND AXFR reply accepted: %v", err)
		}
		t.Log("real unsigned BIND SOA/AXFR replies rejected with dns.ErrNoSig")
	})

	t.Run("Bounds", func(t *testing.T) {
		for _, client := range []*secondary.TransferClient{
			{Keyring: ring256, MaxRecords: secondaryBulkRecords},
			{Keyring: ring256, MaxBytes: 2048},
		} {
			rrs, err := client.AXFR(context.Background(), secondaryObject("secondary-signed.test.", key256), primary.addr)
			if err == nil || rrs != nil {
				t.Fatalf("BIND AXFR bypassed bounds: %v", err)
			}
			t.Logf("bounded BIND transfer rejected without partial data: %v", err)
		}
	})

	t.Run("TLSCannotUsePlaintextPrimary", func(t *testing.T) {
		zone := secondaryObject("secondary-signed.test.", key256)
		zone.Spec.Secondary.TransferProtocol = "tls"
		_, err := (&secondary.TransferClient{Keyring: ring256, Timeout: time.Second}).AXFR(context.Background(), zone, primary.addr)
		if err == nil {
			t.Fatalf("TLS must not downgrade to plaintext: %v", err)
		}
	})
}

// secondaryUnsignedRelay handles one client at a time and relays one real BIND
// reply. It strips the outgoing query's TSIG solely to test missing required
// response signatures. Deadlines and cleanup bound its one worker goroutine.
func secondaryUnsignedRelay(t *testing.T, upstream string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				client := &dns.Conn{Conn: conn}
				wire, err := client.ReadMsgHeader(nil)
				if err != nil {
					return
				}
				q := new(dns.Msg)
				if err := q.Unpack(wire); err != nil {
					return
				}
				q.Extra = nil
				primary, err := dns.DialTimeout("tcp", upstream, time.Second)
				if err != nil {
					return
				}
				defer func() { _ = primary.Close() }()
				_ = primary.SetDeadline(time.Now().Add(3 * time.Second))
				if err := primary.WriteMsg(q); err != nil {
					return
				}
				m, err := primary.ReadMsg()
				if err == nil {
					_ = client.WriteMsg(m)
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	return ln.Addr().String()
}
