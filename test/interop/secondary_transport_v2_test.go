package interop

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

func secondaryTransportRecords(rrs []dns.RR) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		out = append(out, rr.String())
	}
	slices.Sort(out)
	return out
}

func secondaryTransportUpdate(t *testing.T, address, origin, key, secret, value string) {
	t.Helper()
	m := new(dns.Msg)
	m.SetUpdate(origin)
	deleteRR, err := dns.NewRR("www." + origin + " 117 IN A 192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	m.RemoveRRset([]dns.RR{deleteRR})
	addRR, err := dns.NewRR("www." + origin + " 119 IN A " + value)
	if err != nil {
		t.Fatal(err)
	}
	m.Insert([]dns.RR{addRR})
	m.SetTsig(dns.Fqdn(key), dns.HmacSHA256, 300, time.Now().Unix())
	response, _, err := (&dns.Client{Net: "tcp", Timeout: 3 * time.Second, TsigSecret: map[string]string{dns.Fqdn(key): secret}}).ExchangeContext(t.Context(), m, address)
	if err != nil || response == nil || response.Rcode != dns.RcodeSuccess {
		t.Fatalf("BIND dynamic update: %v, %v", response, err)
	}
}

// TestBINDSecondaryIXFR verifies actual BIND journal deltas, not simulated
// IXFR replies. Separate signed dynamic updates retain multiple transitions
// across uint32 serial wrap; a static zone exercises RFC 1995 AXFR fallback.
func TestBINDSecondaryIXFR(t *testing.T) {
	kudurrutest.RequireE2E(t)
	const origin = "secondary-ixfr.test."
	const staticOrigin = "secondary-ixfr-static.test."
	const key = "secondary-ixfr-key.test."
	secret := newSecret(t)
	ring := secondaryKeyring(t, key, "hmac-sha256", secret)
	dir := t.TempDir()
	port := kudurrutest.FreePort(t)
	data := func(origin string) string {
		var b strings.Builder
		fmt.Fprintf(&b, "$ORIGIN %s\n$TTL 300\n@ 537 IN SOA ns.%s hostmaster.%s 4294967294 61 13 301 47\n@ IN NS ns.%s\nns IN A 127.0.0.1\nwww 117 IN A 192.0.2.1\nopaque IN TYPE65400 \\# 4 01020304\n", origin, origin, origin, origin)
		for i := range 200 {
			fmt.Fprintf(&b, "bulk%d IN TXT %q\n", i, strings.Repeat("x", 180))
		}
		return b.String()
	}
	writeFile(t, filepath.Join(dir, "dynamic.db"), data(origin))
	writeFile(t, filepath.Join(dir, "static.db"), data(staticOrigin))
	conf := namedOptions(dir, port) + fmt.Sprintf(`
key %q { algorithm hmac-sha256; secret %q; };
zone %q {
 type primary; file "dynamic.db";
 allow-query { key %q; }; allow-transfer { key %q; }; allow-update { key %q; };
 serial-update-method increment;
 max-ixfr-ratio unlimited;
};
zone %q {
 type primary; file "static.db";
 allow-query { key %q; }; allow-transfer { key %q; };
};
`, key, secret, origin, key, key, key, staticOrigin, key, key)
	primary := startNamed(t, namedBinary(t), dir, conf, port)
	client := &secondary.TransferClient{Keyring: ring, Timeout: 3 * time.Second}
	dz := secondaryObject(origin, key)
	base, err := client.AXFR(t.Context(), dz, primary.addr)
	if err != nil {
		t.Fatal(err)
	}
	initial := secondaryTransportRecords(base)
	for _, value := range []string{"192.0.2.2", "192.0.2.3"} {
		secondaryTransportUpdate(t, primary.addr, origin, key, secret, value)
	}
	current, err := client.SOA(t.Context(), dz, primary.addr)
	if err != nil || current.Serial != 1 {
		t.Fatalf("BIND serial did not wrap after two updates: %v, %v", current, err)
	}
	got, err := client.IXFR(t.Context(), dz, primary.addr, base)
	if err != nil {
		t.Fatal(err)
	}
	want, err := client.AXFR(t.Context(), dz, primary.addr)
	if err != nil || !reflect.DeepEqual(secondaryTransportRecords(got), secondaryTransportRecords(want)) {
		t.Fatalf("BIND IXFR differs from fresh AXFR: %v", err)
	}
	if !reflect.DeepEqual(initial, secondaryTransportRecords(base)) {
		t.Fatal("IXFR mutated the old snapshot")
	}
	if !strings.Contains(primary.log.String(), "IXFR started") {
		t.Fatalf("BIND did not report an incremental transfer: %s", primary.log.String())
	}
	t.Logf("real BIND journal IXFR applied two delete/add transitions across 4294967294 -> 4294967295 -> 1 (BIND skips zero); %d records match fresh AXFR", len(got))
	confirmed, err := client.IXFR(t.Context(), dz, primary.addr, got)
	if err != nil || !reflect.DeepEqual(secondaryTransportRecords(got), secondaryTransportRecords(confirmed)) {
		t.Fatalf("BIND same-serial IXFR confirmation: %v", err)
	}
	staticZone := secondaryObject(staticOrigin, key)
	static, err := client.AXFR(t.Context(), staticZone, primary.addr)
	if err != nil {
		t.Fatal(err)
	}
	old := make([]dns.RR, 0, len(static)+1)
	for _, rr := range static {
		old = append(old, dns.Copy(rr))
	}
	old[0].(*dns.SOA).Serial--
	fake, err := dns.NewRR("obsolete." + staticOrigin + " 123 IN A 192.0.2.111")
	if err != nil {
		t.Fatal(err)
	}
	old = append(old, fake)
	fallback, err := client.IXFR(t.Context(), staticZone, primary.addr, old)
	if err != nil || !reflect.DeepEqual(secondaryTransportRecords(fallback), secondaryTransportRecords(static)) {
		t.Fatalf("BIND history-gap AXFR fallback did not replace old records: %v", err)
	}
	t.Log("real BIND without journal history returned AXFR fallback; obsolete local record removed")
	wrong := secondaryKeyring(t, key, "hmac-sha256", newSecret(t))
	invalid, err := (&secondary.TransferClient{Keyring: wrong, Timeout: time.Second}).IXFR(t.Context(), dz, primary.addr, base)
	if err == nil || invalid != nil {
		t.Fatalf("BIND IXFR accepted wrong TSIG secret: %v", err)
	}
	t.Log("real BIND IXFR rejected the wrong TSIG secret without partial data")
}

func secondaryTransportCertificate(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "tls-cert.pem"), filepath.Join(dir, "tls-key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return certPath, keyPath, pool
}

func TestBINDSecondaryVerifiedTLS(t *testing.T) {
	kudurrutest.RequireE2E(t)
	const origin = "secondary-tls.test."
	const key = "secondary-tls-key.test."
	secret := newSecret(t)
	ring := secondaryKeyring(t, key, "hmac-sha256", secret)
	dir := t.TempDir()
	port, tlsPort := kudurrutest.FreePort(t), kudurrutest.FreePort(t)
	certPath, keyPath, pool := secondaryTransportCertificate(t, dir)
	// A dynamic zone with DNSSEC records requires BIND's private signing keys.
	// Raw signed-record preservation is tested separately; keep this fixture
	// unsigned so BIND can create a genuine journal delta without local signing.
	var data strings.Builder
	for line := range strings.SplitSeq(secondaryPrimaryData(origin), "\n") {
		if strings.Contains(line, " IN DNSKEY ") || strings.Contains(line, " IN RRSIG ") || strings.Contains(line, " IN NSEC ") {
			continue
		}
		data.WriteString(line + "\n")
	}
	writeFile(t, filepath.Join(dir, "primary.db"), data.String())
	conf := strings.Replace(namedOptions(dir, port), "listen-on-v6 { none; };", fmt.Sprintf("listen-on-v6 { none; };\nlisten-on port %d tls xfr-tls { 127.0.0.1; };", tlsPort), 1) + fmt.Sprintf(`
tls xfr-tls { cert-file %q; key-file %q; };
key %q { algorithm hmac-sha256; secret %q; };
zone %q {
 type primary; file "primary.db";
 allow-query { key %q; }; allow-transfer { key %q; }; allow-update { key %q; };
 serial-update-method increment; max-ixfr-ratio unlimited;
};
`, certPath, keyPath, key, secret, origin, key, key, key)
	primary := startNamed(t, namedBinary(t), dir, conf, port)
	plainZone := secondaryObject(origin, key)
	plain := &secondary.TransferClient{Keyring: ring, Timeout: 3 * time.Second}
	base, err := plain.AXFR(t.Context(), plainZone, primary.addr)
	if err != nil {
		t.Fatal(err)
	}
	dz := secondaryObject(origin, key)
	dz.Spec.Secondary.TransferProtocol = "tls"
	address := fmt.Sprintf("localhost:%d", tlsPort)
	client := &secondary.TransferClient{Keyring: ring, RootCAs: pool, Timeout: 3 * time.Second}
	soa, err := client.SOA(t.Context(), dz, address)
	if err != nil || !dns.IsDuplicate(soa, base[0]) {
		t.Fatalf("verified BIND TLS SOA: %v, %v", soa, err)
	}
	got, err := client.AXFR(t.Context(), dz, address)
	if err != nil || !reflect.DeepEqual(secondaryTransportRecords(got), secondaryTransportRecords(base)) {
		t.Fatalf("verified BIND TLS AXFR: %v", err)
	}
	confirmed, err := client.IXFR(t.Context(), dz, address, base)
	if err != nil || !reflect.DeepEqual(secondaryTransportRecords(confirmed), secondaryTransportRecords(base)) {
		t.Fatalf("verified BIND TLS IXFR: %v", err)
	}
	t.Logf("native BIND TLS: verified localhost SAN and explicit private CA, TSIG SOA/AXFR/IXFR, %d raw records", len(got))
	secondaryTransportUpdate(t, primary.addr, origin, key, secret, "192.0.2.99")
	incremental, err := client.IXFR(t.Context(), dz, address, base)
	if err != nil {
		t.Fatalf("BIND journal IXFR over TLS: %v", err)
	}
	updated, err := plain.AXFR(t.Context(), plainZone, primary.addr)
	if err != nil || !reflect.DeepEqual(secondaryTransportRecords(incremental), secondaryTransportRecords(updated)) {
		t.Fatalf("BIND TLS IXFR differs from fresh AXFR: %v", err)
	}
	if !strings.Contains(primary.log.String(), "IXFR started") {
		t.Fatal("BIND did not report a journal-driven IXFR over TLS")
	}
	t.Log("native BIND journal IXFR over verified TLS applied delete/add and matches fresh AXFR")
	untrusted, err := plain.AXFR(context.Background(), dz, address)
	if err == nil || untrusted != nil {
		t.Fatalf("system trust accepted private test CA: %v", err)
	}
	t.Log("default system trust rejected the untrusted private BIND certificate")
	wrong := secondaryKeyring(t, key, "hmac-sha256", newSecret(t))
	invalid, err := (&secondary.TransferClient{Keyring: wrong, RootCAs: pool, Timeout: time.Second}).AXFR(t.Context(), dz, address)
	if err == nil || invalid != nil {
		t.Fatalf("BIND TLS bypassed TSIG: %v", err)
	}
}
