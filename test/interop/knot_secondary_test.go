package interop

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// The inbound secondary interop of TestBINDInboundSecondaryTransfer,
// TestBINDSecondaryIXFR, TestBINDSecondaryVerifiedTLS and
// TestBINDSecondaryThroughListeners, with actual Knot DNS 3.6.2 as the
// primary instead of BIND (README.md, V2-ZONE: "BIND/Knot interop").
const (
	knotPrimaryKey     = "knot-primary-xfr.test."
	knotIXFROrigin     = "knot-ixfr.test."
	knotStaticOrigin   = "knot-static.test."
	knotServingOrigin  = "knot-serving.test."
	knotStartSerial    = uint32(4294967294)
	knotPrimaryDNSPort = "1053"
	knotPrimaryTLSPort = "1853"
)

// knotPrimaryData is a primary zone with timers, TTLs, an unknown RR type
// and enough data for a multi-message transfer. knot-serving.test. carries
// the exact SOA and A record that assertSecondaryAnswer expects.
func knotPrimaryData(origin string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `$ORIGIN %[1]s
$TTL 300
@ 537 IN SOA ns1.%[1]s hostmaster.%[1]s %[2]d 61 13 120 47
@ 721 IN NS ns1.%[1]s
ns1 87 IN A 127.0.0.1
www 117 IN A 192.0.2.80
`, origin, knotStartSerial)
	if origin == knotServingOrigin {
		return b.String()
	}
	b.WriteString("txt 214 IN TXT \"Hello From Knot\"\nopaque 89 IN TYPE65400 \\# 4 01020304\n")
	for i := range 200 {
		fmt.Fprintf(&b, "bulk%04d 93 IN TXT %q\n", i, strings.Repeat("x", 180))
	}
	return b.String()
}

type knotPrimary struct {
	*knotFixture
	addr, tlsAddr string
	pool          *x509.CertPool
	// good holds the configured key; every other client is a negative case.
	good *secondary.TransferClient
}

// newKnotPrimary starts one actual Knot primary on a fresh internal network
// of the private daemon. The container waits for a gate file, so its
// certificate can name the address Docker assigned before knotd starts.
func newKnotPrimary(t *testing.T) *knotPrimary {
	t.Helper()
	f := newKnotDocker(t)
	suffix := strings.ToLower(rand.Text()[:12])
	f.knot, f.netID = "kudurru-knot-xfr-primary-"+suffix, "kudurru-knot-xfr-"+suffix
	t.Log("actual primary: " + knotVersion + "; " + knotImage)
	// Install cleanup before creating any resources; only this test's named
	// container and network are removed.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		b, _ := f.dockerOutput(ctx, "logs", "--tail", "2000", f.knot)
		f.privateLog(f.knot, b)
		_, _ = f.dockerOutput(ctx, "rm", "-f", f.knot)
		_, _ = f.dockerOutput(ctx, "network", "rm", f.netID)
	})
	f.mustDocker("network", "create", "--internal", f.netID)
	for _, path := range []string{"zones", "knot-data"} {
		if err := os.MkdirAll(filepath.Join(f.dir, path), 0o750); err != nil {
			t.Fatal("create private fixture directory")
		}
	}
	f.mustDocker("run", "-d", "--name", f.knot, "--network", f.netID, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
		"-v", f.dir+":/fixture", "--entrypoint", "/bin/sh", knotImage, "-c",
		"while [ ! -f /fixture/knot.start ]; do sleep 0.1; done; exec knotd -c /fixture/knot.conf")
	f.knotIP = strings.TrimSpace(string(f.mustDocker("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", f.knot)))
	ip := net.ParseIP(f.knotIP)
	if ip == nil {
		t.Fatal("private Knot primary container has no IP address")
	}
	_, _, pool := secondaryTransportCertificate(t, f.dir, ip)
	for _, origin := range []string{knotIXFROrigin, knotStaticOrigin, knotServingOrigin} {
		writeFile(t, filepath.Join(f.dir, "zones", origin+"zone"), knotPrimaryData(origin))
	}
	// Transfers need the key; ordinary queries are open, as Knot has no
	// query ACL. The zone files are never written back (zonefile-sync: -1),
	// so a change made through knotc exists only in the journal and the
	// in-memory zone. knot-static.test. keeps no journal at all.
	writeFile(t, filepath.Join(f.dir, "knot.conf"), fmt.Sprintf(`server:
  listen: 0.0.0.0@%[1]s
  listen-tls: 0.0.0.0@%[2]s
  cert-file: /fixture/tls-cert.pem
  key-file: /fixture/tls-key.pem
  rundir: /fixture/knot-data
  udp-workers: 1
  tcp-workers: 1
  background-workers: 1
database:
  storage: /fixture/knot-data
log:
  - target: stdout
    any: info
key:
  - id: %[3]s
    algorithm: hmac-sha256
    secret: %[4]s
acl:
  - id: transfer-key
    key: %[3]s
    action: transfer
template:
  - id: default
    storage: /fixture/zones
    zonefile-sync: -1
    acl: transfer-key
zone:
  - domain: %[5]s
    journal-content: changes
  - domain: %[6]s
    journal-content: none
  - domain: %[7]s
`, knotPrimaryDNSPort, knotPrimaryTLSPort, knotPrimaryKey, f.secret, knotIXFROrigin, knotStaticOrigin, knotServingOrigin))
	writeFile(t, filepath.Join(f.dir, "knot.start"), "start\n")
	p := &knotPrimary{knotFixture: f, pool: pool,
		addr:    net.JoinHostPort(f.knotIP, knotPrimaryDNSPort),
		tlsAddr: net.JoinHostPort(f.knotIP, knotPrimaryTLSPort),
		good:    &secondary.TransferClient{Keyring: secondaryKeyring(t, knotPrimaryKey, "hmac-sha256", f.secret), Timeout: 3 * time.Second},
	}
	// Readiness is a read-only signed SOA query per zone, never a transfer.
	for _, origin := range []string{knotIXFROrigin, knotStaticOrigin, knotServingOrigin} {
		if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			soa, err := p.good.SOA(t.Context(), secondaryObject(origin, knotPrimaryKey), p.addr)
			return err == nil && soa.Serial == knotStartSerial
		}) {
			t.Fatal("actual Knot primary did not load " + origin)
		}
	}
	return p
}

// change commits one delete/add of www's A record in a single knotc
// transaction. Knot increments the serial (serial-policy: increment) and
// records the difference in its journal.
func (p *knotPrimary) change(origin, address string) {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.t.Context(), 10*time.Second)
	defer cancel()
	knotc := "knotc -c /fixture/knot.conf "
	script := knotc + "zone-begin " + origin + " && " + knotc + "zone-unset " + origin + " www A && " +
		knotc + "zone-set " + origin + " www 119 A " + address + " && " + knotc + "zone-commit " + origin
	if _, err := p.dockerOutput(ctx, "exec", p.knot, "/bin/sh", "-c", script); err != nil {
		p.t.Fatal("knotc zone transaction failed; process output withheld")
	}
}

// log returns Knot's own transfer log lines (stdout of the container).
func (p *knotPrimary) log() string {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(p.t.Context(), 10*time.Second)
	defer cancel()
	b, err := p.dockerOutput(ctx, "logs", p.knot)
	if err != nil {
		p.t.Fatal("read private Knot log failed")
	}
	var lines []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.Contains(line, "XFR, outgoing") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func TestKnotInboundSecondaryTransfer(t *testing.T) {
	p := newKnotPrimary(t)
	good := p.good

	t.Run("AXFR", func(t *testing.T) {
		for _, origin := range []string{knotIXFROrigin, knotStaticOrigin} {
			zone := secondaryObject(origin, knotPrimaryKey)
			want := secondaryFixtureRecords(t, origin, knotPrimaryData(origin))
			soa, err := good.SOA(t.Context(), zone, p.addr)
			if err != nil || soa.String() != want[0].String() {
				t.Fatalf("signed SOA from Knot: %v, %v", soa, err)
			}
			start := time.Now()
			got, err := good.AXFR(t.Context(), zone, p.addr)
			if err != nil {
				t.Fatalf("AXFR from Knot: %v", err)
			}
			if len(got) != len(want) || secondaryRecordSet(got) != secondaryRecordSet(want) {
				t.Fatalf("transferred zone differs from the Knot zone file: got %d records, want %d", len(got), len(want))
			}
			soas := 0
			for _, rr := range got {
				if rr.Header().Rrtype == dns.TypeSOA {
					soas++
				}
			}
			if _, ok := got[0].(*dns.SOA); !ok || soas != 1 {
				t.Fatalf("AXFR returned %d SOAs or did not start with the SOA", soas)
			}
			t.Logf("Knot -> TransferClient %s: %d records in %s; TTLs, SOA timers, TXT case and the unknown RR type preserved", origin, len(got), time.Since(start).Round(time.Millisecond))
		}
		if !strings.Contains(p.log(), "["+knotIXFROrigin+"] AXFR, outgoing") {
			t.Fatal("Knot did not report the outgoing AXFR")
		}
	})

	t.Run("TSIGRejection", func(t *testing.T) {
		for _, test := range []struct {
			name, key string
			client    *secondary.TransferClient
		}{
			{"wrong secret", knotPrimaryKey, &secondary.TransferClient{Keyring: secondaryKeyring(t, knotPrimaryKey, "hmac-sha256", newSecret(t)), Timeout: 3 * time.Second}},
			{"unknown key", "knot-unknown.test.", &secondary.TransferClient{Keyring: secondaryKeyring(t, "knot-unknown.test.", "hmac-sha256", p.secret), Timeout: 3 * time.Second}},
			{"wrong algorithm", knotPrimaryKey, &secondary.TransferClient{Keyring: secondaryKeyring(t, knotPrimaryKey, "hmac-sha512", p.secret), Timeout: 3 * time.Second}},
		} {
			t.Run(test.name, func(t *testing.T) {
				zone := secondaryObject(knotIXFROrigin, test.key)
				soa, err := test.client.SOA(t.Context(), zone, p.addr)
				if err == nil || soa != nil {
					t.Fatalf("SOA under a bad TSIG key accepted: %v", err)
				}
				rrs, err := test.client.AXFR(t.Context(), zone, p.addr)
				if err == nil || rrs != nil {
					t.Fatalf("AXFR under a bad TSIG key accepted or returned partial data: %v", err)
				}
				t.Logf("Knot rejected SOA and AXFR (%s): %v", test.name, err)
			})
		}
		t.Run("unsigned", func(t *testing.T) {
			rrs, err := (&secondary.TransferClient{Timeout: 3 * time.Second}).AXFR(t.Context(), secondaryObject(knotIXFROrigin, ""), p.addr)
			if err == nil || rrs != nil {
				t.Fatalf("unsigned AXFR outside the transfer ACL accepted: %v", err)
			}
			t.Logf("Knot refused the unsigned AXFR: %v", err)
		})
		t.Run("required reply signature", func(t *testing.T) {
			// Knot answers an unsigned SOA query (it has no query ACL). The
			// relay strips the request's TSIG to obtain that real unsigned
			// answer; the receiver still requires the configured signature.
			relay := secondaryUnsignedRelay(t, p.addr)
			soa, err := good.SOA(t.Context(), secondaryObject(knotIXFROrigin, knotPrimaryKey), relay)
			if soa != nil || !errors.Is(err, dns.ErrNoSig) {
				t.Fatalf("unsigned Knot SOA reply accepted: %v", err)
			}
			rrs, err := good.AXFR(t.Context(), secondaryObject(knotIXFROrigin, knotPrimaryKey), relay)
			if rrs != nil || err == nil {
				t.Fatalf("unsigned Knot AXFR reply accepted: %v", err)
			}
			t.Logf("real unsigned Knot replies rejected: SOA %v; AXFR %v", dns.ErrNoSig, err)
		})
	})

	var current []dns.RR
	t.Run("IXFR", func(t *testing.T) {
		zone := secondaryObject(knotIXFROrigin, knotPrimaryKey)
		base, err := good.AXFR(t.Context(), zone, p.addr)
		if err != nil {
			t.Fatal(err)
		}
		initial := secondaryTransportRecords(base)
		// Two separate commits: 4294967294 -> 4294967295 -> 0. Unlike BIND,
		// Knot's increment policy steps onto zero across the wrap.
		p.change(knotIXFROrigin, "192.0.2.2")
		p.change(knotIXFROrigin, "192.0.2.3")
		soa, err := good.SOA(t.Context(), zone, p.addr)
		if err != nil || soa.Serial != 0 {
			t.Fatalf("Knot serial did not wrap to 0 after two commits: %v, %v", soa, err)
		}
		got, err := good.IXFR(t.Context(), zone, p.addr, base)
		if err != nil {
			t.Fatal(err)
		}
		want, err := good.AXFR(t.Context(), zone, p.addr)
		if err != nil || !reflect.DeepEqual(secondaryTransportRecords(got), secondaryTransportRecords(want)) {
			t.Fatalf("Knot IXFR differs from a fresh AXFR: %v", err)
		}
		if !reflect.DeepEqual(initial, secondaryTransportRecords(base)) {
			t.Fatal("IXFR mutated the old snapshot")
		}
		if !hasA(got, "www."+knotIXFROrigin, "192.0.2.3") || hasA(got, "www."+knotIXFROrigin, "192.0.2.80") {
			t.Fatal("IXFR did not apply Knot's delete/add transitions")
		}
		if !strings.Contains(p.log(), "["+knotIXFROrigin+"] IXFR, outgoing") || !strings.Contains(p.log(), "started, serial 4294967294 -> 0") {
			t.Fatal("Knot did not report a journal IXFR 4294967294 -> 0")
		}
		t.Logf("real Knot journal IXFR applied two delete/add transitions across 4294967294 -> 4294967295 -> 0; %d records match a fresh AXFR", len(got))
		current = got
		// Knot 3.6.2 answers an IXFR at the current serial with one signed SOA
		// but without the AA bit (BIND sets it). The receiver requires AA on
		// every transfer message and refuses that reply without data. A
		// refresh against a single consistent primary does not ask for it: at
		// an unchanged SOA serial the Manager keeps its stored copy and sends
		// no transfer (manager.go, refresh). It can still be reached when the
		// SOA poll and the IXFR land on different instances behind one
		// address, or the primary moves back to the stored serial between the
		// two requests; that refresh then fails and is not confirmed, and no
		// data is lost (followup: accept a signed single-SOA reply at the
		// client's serial without AA?).
		confirmed, err := good.IXFR(t.Context(), zone, p.addr, got)
		if err == nil || confirmed != nil || !strings.Contains(err.Error(), "non-authoritative response") {
			t.Fatalf("Knot same-serial IXFR reply was not the known AA-less refusal: %v", err)
		}
		t.Logf("Knot same-serial IXFR: one SOA without AA; refused without data: %v", err)
		wrong := &secondary.TransferClient{Keyring: secondaryKeyring(t, knotPrimaryKey, "hmac-sha256", newSecret(t)), Timeout: time.Second}
		if invalid, err := wrong.IXFR(t.Context(), zone, p.addr, base); err == nil || invalid != nil {
			t.Fatalf("Knot IXFR accepted the wrong TSIG secret: %v", err)
		}
	})

	t.Run("JournalFallback", func(t *testing.T) {
		zone := secondaryObject(knotStaticOrigin, knotPrimaryKey)
		static, err := good.AXFR(t.Context(), zone, p.addr)
		if err != nil {
			t.Fatal(err)
		}
		old := make([]dns.RR, 0, len(static)+1)
		for _, rr := range static {
			old = append(old, dns.Copy(rr))
		}
		old[0].(*dns.SOA).Serial--
		fake, err := dns.NewRR("obsolete." + knotStaticOrigin + " 123 IN A 192.0.2.111")
		if err != nil {
			t.Fatal(err)
		}
		old = append(old, fake)
		fallback, err := good.IXFR(t.Context(), zone, p.addr, old)
		if err != nil || !reflect.DeepEqual(secondaryTransportRecords(fallback), secondaryTransportRecords(static)) {
			t.Fatalf("Knot history-gap AXFR fallback did not replace the old records: %v", err)
		}
		if !strings.Contains(p.log(), "["+knotStaticOrigin+"] IXFR, outgoing") || !strings.Contains(p.log(), "incomplete history, serial 4294967293, fallback to AXFR") {
			t.Fatal("Knot did not report the journal-less AXFR fallback")
		}
		t.Log("real Knot without journal (journal-content: none) answered IXFR with AXFR; obsolete local record removed")
	})

	t.Run("VerifiedTLS", func(t *testing.T) {
		if current == nil {
			t.Skip("IXFR subtest failed; no current snapshot")
		}
		zone := secondaryObject(knotIXFROrigin, knotPrimaryKey)
		zone.Spec.Secondary.TransferProtocol = "tls"
		client := &secondary.TransferClient{Keyring: good.Keyring, RootCAs: p.pool, Timeout: 3 * time.Second}
		soa, err := client.SOA(t.Context(), zone, p.tlsAddr)
		if err != nil || !dns.IsDuplicate(soa, current[0]) {
			t.Fatalf("verified Knot TLS SOA: %v, %v", soa, err)
		}
		got, err := client.AXFR(t.Context(), zone, p.tlsAddr)
		if err != nil || !reflect.DeepEqual(secondaryTransportRecords(got), secondaryTransportRecords(current)) {
			t.Fatalf("verified Knot TLS AXFR: %v", err)
		}
		p.change(knotIXFROrigin, "192.0.2.4")
		incremental, err := client.IXFR(t.Context(), zone, p.tlsAddr, current)
		if err != nil {
			t.Fatalf("Knot journal IXFR over TLS: %v", err)
		}
		fresh, err := good.AXFR(t.Context(), secondaryObject(knotIXFROrigin, knotPrimaryKey), p.addr)
		if err != nil || !reflect.DeepEqual(secondaryTransportRecords(incremental), secondaryTransportRecords(fresh)) || !hasA(incremental, "www."+knotIXFROrigin, "192.0.2.4") {
			t.Fatalf("Knot TLS IXFR differs from a fresh AXFR: %v", err)
		}
		if !strings.Contains(p.log(), "started, serial 0 -> 1") {
			t.Fatal("Knot did not report the journal IXFR 0 -> 1 over TLS")
		}
		t.Logf("native Knot XoT: certificate verified for %s against an explicit private CA; TSIG SOA/AXFR and journal IXFR 0 -> 1 match a fresh AXFR", p.knotIP)
		untrusted, err := good.AXFR(t.Context(), zone, p.tlsAddr)
		if err == nil || untrusted != nil {
			t.Fatalf("system trust accepted the private test CA: %v", err)
		}
		if plain, err := client.AXFR(t.Context(), zone, p.addr); err == nil || plain != nil {
			t.Fatalf("TLS downgraded to the plaintext port: %v", err)
		}
		wrong := &secondary.TransferClient{Keyring: secondaryKeyring(t, knotPrimaryKey, "hmac-sha256", newSecret(t)), RootCAs: p.pool, Timeout: time.Second}
		if invalid, err := wrong.AXFR(t.Context(), zone, p.tlsAddr); err == nil || invalid != nil {
			t.Fatalf("Knot TLS bypassed TSIG: %v", err)
		}
		t.Log("untrusted certificate, plaintext port under TLS and a wrong TSIG secret over TLS all rejected without data")
	})

	// Last: it stops the primary.
	t.Run("ExpiryAfterPrimaryStops", func(t *testing.T) {
		knotSecondaryExpiry(t, p)
	})
}

// knotSecondaryExpiry runs the production receiver chain of
// TestBINDSecondaryThroughListeners against Knot: TransferClient, Manager,
// durable local.db, zoneview.Source and real UDP/TCP listeners. It stops the
// Knot container, restarts the receiver offline and expires the zone.
func knotSecondaryExpiry(t *testing.T, p *knotPrimary) {
	root, forwarded := secondaryRootForwarder(t, knotServingOrigin)
	config := memory.New()
	t.Cleanup(func() { _ = config.Close() })
	dz := secondaryObject(knotServingOrigin, knotPrimaryKey)
	dz.ObjectMeta = metav1.ObjectMeta{Name: "knot-secondary-serving", UID: "stable-knot-secondary-serving"}
	dz.Spec.Secondary.Primaries = []string{p.addr}
	for _, object := range []store.Object{dz, &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "knot-secondary-test-root"},
		Spec:       v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypeForward, ZoneName: "."},
	}} {
		if _, err := config.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.Get(t.Context(), store.Key{Kind: "DNSZone", Name: dz.Name}, dz); err != nil {
		t.Fatal(err)
	}
	clock := new(secondaryServingClock)
	clock.set(time.Now().UTC())
	dbPath := filepath.Join(t.TempDir(), localdb.FileName)
	first := startSecondaryServingRuntime(t, config, dbPath, secondaryKeyring(t, knotPrimaryKey, "hmac-sha256", p.secret), clock, root)
	assertSecondaryUnavailable(t, first, knotServingOrigin)
	first.startManager()
	initial := awaitSecondaryServingStatus(t, first.manager, dz, func(st secondary.Status) bool { return st.Available })
	if initial.Serial != knotStartSerial || !initial.ExpiresAt.Equal(initial.ConfirmedAt.Add(120*time.Second)) {
		t.Fatalf("wrong primary serial or expiry: %+v", initial)
	}
	assertSecondaryAnswer(t, first, knotServingOrigin, knotStartSerial)
	t.Logf("Knot -> TransferClient -> Manager -> zoneview.Source -> ingress/authoritative -> UDP/TCP: serial=%d, A=192.0.2.80 TTL=117", initial.Serial)

	first.stop()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := p.dockerOutput(ctx, "stop", "--time", "5", p.knot); err != nil {
		t.Fatal("stop the Knot primary failed; process output withheld")
	}
	if _, err := p.good.SOA(t.Context(), secondaryObject(knotServingOrigin, knotPrimaryKey), p.addr); err == nil {
		t.Fatal("stopped Knot primary still answered")
	}
	clock.set(initial.ConfirmedAt.Add(30 * time.Second))
	second := startSecondaryServingRuntime(t, config, dbPath, secondaryKeyring(t, knotPrimaryKey, "hmac-sha256", p.secret), clock, root)
	second.startManager()
	restored := awaitSecondaryServingStatus(t, second.manager, dz, func(st secondary.Status) bool {
		return st.Available && st.Error != "" // The real offline SOA poll failed.
	})
	if restored.Serial != knotStartSerial || !restored.ConfirmedAt.Equal(initial.ConfirmedAt) || !restored.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatalf("offline restart changed serial or renewed expiry: initial=%+v restored=%+v", initial, restored)
	}
	assertSecondaryAnswer(t, second, knotServingOrigin, knotStartSerial)
	t.Log("Knot stopped; offline restart reopened local.db and served the unexpired zone without renewing confirmation or expiry")

	clock.set(initial.ExpiresAt)
	assertSecondaryUnavailable(t, second, knotServingOrigin)
	assertSecondaryRootWorks(t, second)
	if n := forwarded.Load(); n != 0 {
		t.Fatalf("secondary queries leaked %d times to the root forwarder", n)
	}
	expired, ok := second.manager.StatusFor(dz)
	if !ok || expired.Available || !expired.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatalf("expired diagnostics disagree with listener behavior: %+v", expired)
	}
	t.Log("at SOA expire (120 s) the Knot-fed zone returned SERVFAIL on UDP/TCP with zero root forwarding; outside names still resolved")
}
