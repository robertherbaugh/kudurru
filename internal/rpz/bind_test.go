package rpz

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

type bindLog struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *bindLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}
func (b *bindLog) contains(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(b.data.String(), s)
}

func (b *bindLog) diagnostic(secret string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ReplaceAll(b.data.String(), secret, "[redacted]")
}

type rpzBIND struct {
	address, tlsAddress, key, secret string
	ring                             *dnsserver.StaticKeyring
	roots                            *x509.CertPool
	log                              *bindLog
	restart                          func()
	stop                             func()
}

func bindWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}
func bindCertificate(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
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
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	bindWrite(t, certFile, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	bindWrite(t, keyFile, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return certFile, keyFile, roots
}

func startRPZBIND(t *testing.T, reference bool) *rpzBIND {
	t.Helper()
	kudurrutest.RequireE2E(t)
	bin, err := exec.LookPath("named")
	if err != nil {
		bin, err = exec.LookPath("/usr/sbin/named")
	}
	if err != nil {
		t.Skip("BIND named not installed")
	}
	dir := t.TempDir()
	port, tlsPort := kudurrutest.FreePort(t), kudurrutest.FreePort(t)
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	key := "rpz-transfer.test."
	ring, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: key, Algorithm: dns.HmacSHA256, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, roots := bindCertificate(t, dir)
	b := &rpzBIND{address: fmt.Sprintf("127.0.0.1:%d", port), tlsAddress: fmt.Sprintf("localhost:%d", tlsPort), key: key, secret: base64.StdEncoding.EncodeToString(secret), ring: ring, roots: roots, log: &bindLog{}}
	// First-party fixture data only. Native BIND journal updates exercise the
	// acquisition seam; no feed or third-party implementation is imported.
	bindWrite(t, filepath.Join(dir, "policy.db"), `$ORIGIN policy.test.
$TTL 123
@ IN SOA ns hostmaster ( 1 10 3 30 5 )
@ IN NS ns.primary.test.
bad.answers.test CNAME .
nodata.answers.test CNAME *.
pass.answers.test CNAME rpz-passthru.
local.answers.test A 192.0.2.90
alias.answers.test CNAME garden.answers.test.
*.tree.answers.test CNAME .
leaf.branch.tree.answers.test CNAME rpz-passthru.
24.0.2.0.192.rpz-ip CNAME *.
128.17.zz.db8.2001.rpz-ip CNAME *.
32.2.0.0.127.rpz-client-ip CNAME .
*.cname.answers.test CNAME *.garden.answers.test.
drop.answers.test CNAME rpz-drop.
tcp.answers.test CNAME rpz-tcp-only.
`)
	bindWrite(t, filepath.Join(dir, "static.db"), `$ORIGIN static.test.
$TTL 123
@ IN SOA ns hostmaster ( 7 10 3 30 5 )
@ IN NS ns.primary.test.
bad.answers.test CNAME .
`)
	bindWrite(t, filepath.Join(dir, "answers.db"), `$ORIGIN answers.test.
$TTL 123
@ IN SOA ns hostmaster ( 1 10 3 30 5 )
@ IN NS ns.primary.test.
* A 198.51.100.1
garden A 198.51.100.9
*.garden A 198.51.100.9
address A 192.0.2.17
address6 AAAA 2001:db8::17
chain CNAME bad.answers.test.
`)
	policy := ""
	if reference {
		policy = `response-policy { zone "policy.test."; } recursive-only no break-dnssec yes qname-wait-recurse no;`
	}
	conf := fmt.Sprintf(`options {
 directory %q; pid-file %q; session-keyfile %q; managed-keys-directory %q;
 listen-on port %d { 127.0.0.1; }; listen-on port %d tls rpz-tls { 127.0.0.1; }; listen-on-v6 { none; };
 recursion yes; allow-recursion { 127.0.0.1; }; dnssec-validation no; notify no;
 %s
};
controls {};
tls rpz-tls { cert-file %q; key-file %q; };
key %q { algorithm hmac-sha256; secret %q; };
zone "policy.test." { type primary; file "policy.db"; allow-transfer { key %q; }; allow-update { key %q; }; serial-update-method increment; max-ixfr-ratio unlimited; };
zone "static.test." { type primary; file "static.db"; allow-transfer { key %q; }; };
zone "answers.test." { type primary; file "answers.db"; };
`, dir, filepath.Join(dir, "named.pid"), filepath.Join(dir, "session.key"), dir, port, tlsPort, policy, certFile, keyFile, key, b.secret, key, key, key)
	confFile := filepath.Join(dir, "named.conf")
	bindWrite(t, confFile, conf)
	// Config can contain secrets. Only report command errors, never its text.
	if check, err := exec.LookPath(filepath.Join(filepath.Dir(bin), "named-checkconf")); err == nil {
		if err := exec.Command(check, confFile).Run(); err != nil { //nolint:gosec // test executable
			t.Fatalf("BIND configuration rejected: %v", err)
		} //nolint:gosec // test executable
	}
	b.restart = func() {
		cmd := exec.Command(bin, "-g", "-4", "-n", "1", "-c", confFile) //nolint:gosec // test executable
		cmd.Stdout, cmd.Stderr = b.log, b.log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		var once sync.Once
		b.stop = func() {
			once.Do(func() {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					_ = cmd.Process.Kill()
					<-done
				}
			})
		}
		eventually(t, func() bool {
			select {
			case <-done:
				t.Fatal("BIND exited; inspect fixture without exposing TSIG configuration")
			default:
			}
			question := new(dns.Msg)
			question.SetQuestion("answers.test.", dns.TypeSOA)
			reply, _, err := (&dns.Client{Net: "tcp", Timeout: 100 * time.Millisecond}).ExchangeContext(t.Context(), question, b.address)
			return err == nil && reply != nil && reply.Rcode == dns.RcodeSuccess && len(reply.Answer) != 0
		})
	}
	b.restart()
	t.Cleanup(func() { b.stop() })
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(b.log.diagnostic(b.secret))
		}
	})
	return b
}

func (b *rpzBIND) update(t *testing.T, target string) {
	t.Helper()
	m := new(dns.Msg)
	m.SetUpdate("policy.test.")
	old := &dns.CNAME{Hdr: dns.RR_Header{Name: "bad.answers.test.policy.test.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 123}, Target: "."}
	m.RemoveRRset([]dns.RR{old})
	old.Target = target
	m.Insert([]dns.RR{old})
	m.SetTsig(b.key, dns.HmacSHA256, 300, time.Now().Unix())
	reply, _, err := (&dns.Client{Net: "tcp", Timeout: 3 * time.Second, TsigSecret: map[string]string{b.key: b.secret}}).ExchangeContext(t.Context(), m, b.address)
	if err != nil || reply == nil || reply.Rcode != dns.RcodeSuccess {
		t.Fatalf("BIND dynamic update failed: %v", err)
	}
}

type bindReceiver struct {
	*secondary.TransferClient
	axfr, ixfr atomic.Int64
}

func (r *bindReceiver) AXFR(ctx context.Context, dz *v1alpha1.DNSZone, p string) ([]dns.RR, error) {
	r.axfr.Add(1)
	return r.TransferClient.AXFR(ctx, dz, p)
}
func (r *bindReceiver) IXFR(ctx context.Context, dz *v1alpha1.DNSZone, p string, base []dns.RR) ([]dns.RR, error) {
	r.ixfr.Add(1)
	return r.TransferClient.IXFR(ctx, dz, p, base)
}

func TestBINDRPZRuntimeTransfersRefreshRestart(t *testing.T) {
	for _, protocol := range []string{"tcp", "tls"} {
		t.Run(protocol, func(t *testing.T) {
			b := startRPZBIND(t, false)
			list := runtimeList()
			list.Spec.Sources[0].RPZ.Primaries = []string{b.address}
			list.Spec.Sources[0].RPZ.TSIGKeyRef = b.key
			list.Spec.Sources[0].RPZ.TransferProtocol = protocol
			if protocol == "tls" {
				list.Spec.Sources[0].RPZ.Primaries = []string{b.tlsAddress}
			}
			rx := &bindReceiver{TransferClient: &secondary.TransferClient{Keyring: b.ring, RootCAs: b.roots, Timeout: time.Second}}
			fx := newRuntime(t, rx, runtimeDB(t), list)
			fx.m.opts.Keyring = b.ring
			stop := startRuntime(t, fx.m)
			first := awaitStatus(t, fx, func(s Status) bool { return s.Available })
			if first.Serial != 1 {
				t.Fatal("external serial not preserved")
			}
			selection := fx.m.Snapshot().Select([]*v1alpha1.BlockList{fx.list}, true)
			if d := selection.Evaluate(Context{Name: "bad.answers.test.", Type: dns.TypeA}, fx.clock.now()); d.Action != ActionNXDOMAIN {
				t.Fatalf("initial transferred policy = %+v", d)
			}
			fx.clock.add(10 * time.Second)
			fx.m.Wake()
			renewed := awaitStatus(t, fx, func(s Status) bool { return s.ConfirmedAt.After(first.ConfirmedAt) })
			if rx.axfr.Load() != 1 || rx.ixfr.Load() != 0 {
				t.Fatal("same-serial BIND SOA performed a transfer")
			}
			b.update(t, "*.")
			fx.clock.add(10 * time.Second)
			fx.m.Wake()
			changed := awaitStatus(t, fx, func(s Status) bool { return s.Serial == 2 })
			if !changed.ConfirmedAt.After(renewed.ConfirmedAt) || rx.ixfr.Load() != 1 || !b.log.contains("IXFR started") {
				t.Fatal("journal IXFR did not update feed")
			}
			fresh, err := rx.TransferClient.AXFR(t.Context(), runtimeJob(t, fx).dz, list.Spec.Sources[0].RPZ.Primaries[0])
			if err != nil {
				t.Fatal(err)
			}
			actual := fx.m.Snapshot().Lookup(SourceIdentity(fx.list, 0)).Records()
			toStrings := func(records []dns.RR) []string {
				out := make([]string, 0, len(records))
				for _, rr := range records {
					out = append(out, rr.String())
				}
				slices.Sort(out)
				return out
			}
			if !slices.Equal(toStrings(fresh), toStrings(actual)) {
				t.Fatal("incremental result differs from full raw transfer")
			}
			if d := selection.Evaluate(Context{Name: "bad.answers.test."}, fx.clock.now()); d.Action != ActionNXDOMAIN {
				t.Fatal("new transfer mutated pinned selection")
			}
			stop()
			b.stop()
			restarted, err := New(fx.m.opts)
			if err != nil {
				t.Fatal(err)
			}
			fx.m = restarted
			startRuntime(t, restarted)
			restored := awaitStatus(t, fx, func(s Status) bool { return s.Serial == 2 && s.Error != "" })
			if !restored.Available || !restored.ExpiresAt.Equal(changed.ExpiresAt) {
				t.Fatal("offline restart did not retain exact durable lifetime")
			}
			fx.clock.add(30 * time.Second)
			if d := restarted.Snapshot().Select([]*v1alpha1.BlockList{fx.list}, true).Evaluate(Context{Name: "unrelated.answers.test."}, fx.clock.now()); !d.Unavailable || d.Rcode() != dns.RcodeServerFailure {
				t.Fatal("expired feed failed open")
			}
			if s, _ := restarted.StatusFor(fx.list, 0); s.Available {
				t.Fatal("expiry diagnostics claimed available")
			}
			before := rx.axfr.Load()
			b.restart()
			fx.clock.add(3 * time.Second) // Observe the SOA retry timer after the offline poll.
			restarted.Wake()
			recovered := awaitStatus(t, fx, func(s Status) bool { return s.Available && s.Serial == 2 })
			if !recovered.ConfirmedAt.After(changed.ConfirmedAt) || rx.axfr.Load() != before+1 || rx.ixfr.Load() != 1 {
				t.Fatal("expired BIND copy did not recover through fresh AXFR")
			}
		})
	}
}

func TestBINDRPZFallbackAndAuthentication(t *testing.T) {
	b := startRPZBIND(t, false)
	list := runtimeList()
	list.Spec.Sources[0].RPZ.ZoneName = "static.test."
	list.Spec.Sources[0].RPZ.TSIGKeyRef = b.key
	_, dz, err := sourceConfig(list, 0)
	if err != nil {
		t.Fatal(err)
	}
	client := &secondary.TransferClient{Keyring: b.ring, Timeout: time.Second}
	fresh, err := client.AXFR(t.Context(), dz, b.address)
	if err != nil {
		t.Fatal(err)
	}
	base := make([]dns.RR, len(fresh))
	for i, rr := range fresh {
		base[i] = dns.Copy(rr)
	}
	base[0].(*dns.SOA).Serial = 1
	got, err := client.IXFR(t.Context(), dz, b.address, base)
	if err != nil || got[0].(*dns.SOA).Serial != 7 {
		t.Fatalf("no-journal BIND AXFR fallback: %v", err)
	}
	if base[0].(*dns.SOA).Serial != 1 {
		t.Fatal("fallback mutated baseline")
	}
	wrong, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: b.key, Algorithm: dns.HmacSHA256, Secret: []byte("wrong")})
	if err != nil {
		t.Fatal(err)
	}
	if records, err := (&secondary.TransferClient{Keyring: wrong, Timeout: time.Second}).AXFR(t.Context(), dz, b.address); err == nil || records != nil {
		t.Fatal("wrong TSIG accepted feed")
	}
	dz.Spec.Secondary.TransferProtocol = "tls"
	if records, err := client.AXFR(t.Context(), dz, b.tlsAddress); err == nil || records != nil {
		t.Fatal("untrusted TLS certificate accepted feed")
	}
	// A bounded runtime failover skips an unreachable first primary and uses
	// the authenticated second source; transport is reused without a fork.
	list.Spec.Sources[0].RPZ.Primaries = []string{"127.0.0.1:1", b.address}
	fx := newRuntime(t, client, runtimeDB(t), list)
	fx.m.opts.Keyring = b.ring
	r := fx.m.refresh(t.Context(), runtimeJob(t, fx))
	if r.err != nil || r.data.Primary != b.address || r.z.SOA().Serial != 7 {
		t.Fatalf("runtime primary failover: %v", r.err)
	}
}

func TestBINDRPZPolicyReference(t *testing.T) {
	b := startRPZBIND(t, true)
	list := runtimeList()
	list.Spec.Sources[0].RPZ.TSIGKeyRef = b.key
	spec, dz, err := sourceConfig(list, 0)
	if err != nil {
		t.Fatal(err)
	}
	records, err := (&secondary.TransferClient{Keyring: b.ring, Timeout: time.Second}).AXFR(t.Context(), dz, b.address)
	if err != nil {
		t.Fatal(err)
	}
	spec.ExpiresAt = testNow.Add(time.Minute)
	z, err := Compile(spec, records)
	if err != nil {
		t.Fatal(err)
	}
	selection := NewSelection(z)
	for _, name := range []string{"bad.answers.test.", "nodata.answers.test.", "pass.answers.test.", "local.answers.test.", "alias.answers.test.", "tree.answers.test.", "twig.tree.answers.test.", "deep.twig.tree.answers.test.", "branch.tree.answers.test.", "twig.branch.tree.answers.test.", "leaf.branch.tree.answers.test.", "address.answers.test.", "query.cname.answers.test.", "chain.answers.test."} {
		t.Run(name, func(t *testing.T) {
			m := new(dns.Msg)
			m.SetQuestion(name, dns.TypeA)
			reply, _, err := (&dns.Client{Timeout: time.Second}).ExchangeContext(t.Context(), m, b.address)
			if err != nil || reply == nil {
				t.Fatalf("native BIND policy query: %v", err)
			}
			inspection := Context{Name: name, Type: dns.TypeA, Class: dns.ClassINET, ClientIP: netip.MustParseAddr("127.0.0.1")}
			if name == "address.answers.test." {
				inspection.Addresses = []netip.Addr{netip.MustParseAddr("192.0.2.17")}
			}
			if name == "chain.answers.test." {
				inspection.Names = []string{"bad.answers.test."}
			}
			d := selection.Evaluate(inspection, testNow)
			if d.Rcode() != reply.Rcode {
				t.Fatalf("compiler %s rcode=%d; BIND=%d", d.Action, d.Rcode(), reply.Rcode)
			}
			switch d.Action {
			case ActionNODATA:
				if len(reply.Answer) != 0 {
					t.Fatal("BIND did not agree with NODATA")
				}
			case ActionLocalData:
				local, err := d.Records(name, dns.TypeA, 0)
				if err != nil {
					t.Fatal(err)
				}
				if len(local) != 1 || len(reply.Answer) == 0 || !dns.IsDuplicate(local[0], reply.Answer[0]) {
					t.Fatalf("BIND local-data disagrees: action=%s", d.Action)
				}
			case ActionNone, ActionPassthru:
				if len(reply.Answer) == 0 {
					t.Fatalf("compiler passed; BIND denied: %s", d.Action)
				}
			}
		})
	}
	for _, tc := range []struct {
		name   string
		typ    uint16
		client string
		want   Action
	}{
		{"bad.answers.test.", dns.TypeA, "127.0.0.2", ActionNXDOMAIN},
		{"address6.answers.test.", dns.TypeAAAA, "127.0.0.1", ActionNODATA},
		{"drop.answers.test.", dns.TypeA, "127.0.0.1", ActionDrop},
		{"tcp.answers.test.", dns.TypeA, "127.0.0.1", ActionTCPOnly},
	} {
		t.Run(string(tc.want)+tc.client, func(t *testing.T) {
			inspection := Context{Name: tc.name, Type: tc.typ, Class: dns.ClassINET, ClientIP: netip.MustParseAddr(tc.client)}
			if tc.typ == dns.TypeAAAA {
				inspection.Addresses = []netip.Addr{netip.MustParseAddr("2001:db8::17")}
			}
			d := selection.Evaluate(inspection, testNow)
			if d.Action != tc.want {
				t.Fatalf("compiler action=%s, want %s", d.Action, tc.want)
			}
			m := new(dns.Msg)
			m.SetQuestion(tc.name, tc.typ)
			client := &dns.Client{Timeout: 200 * time.Millisecond, Dialer: &net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP(tc.client)}}}
			reply, _, err := client.ExchangeContext(t.Context(), m, b.address)
			if tc.want == ActionDrop {
				if err == nil || reply != nil {
					t.Fatal("BIND did not drop")
				}
				return
			}
			if err != nil || reply == nil {
				t.Fatalf("native policy query: %v", err)
			}
			if tc.want == ActionTCPOnly {
				if !reply.Truncated {
					t.Fatal("BIND TCP-only did not truncate UDP")
				}
				reply, _, err = (&dns.Client{Net: "tcp", Timeout: time.Second}).ExchangeContext(t.Context(), m, b.address)
				if err != nil || reply == nil || reply.Truncated || len(reply.Answer) == 0 {
					t.Fatal("BIND TCP-only did not pass stream request")
				}
			} else if reply.Rcode != d.Rcode() || tc.want == ActionNODATA && len(reply.Answer) != 0 {
				t.Fatal("BIND client/address trigger disagrees")
			}
		})
	}

}
