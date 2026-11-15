package recursor

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/testport"
)

type countedAuthority struct {
	native   netExchanger
	requests atomic.Int64
}

func (e *countedAuthority) Exchange(ctx context.Context, q *dns.Msg, server netip.AddrPort, tcp bool) (*dns.Msg, error) {
	e.requests.Add(1)
	return e.native.Exchange(ctx, q, server, tcp)
}

// Opt-in external interoperability needs installed BIND tools, never a download
// or a shared application build. Both the signer and serving peer are BIND.
func TestAggressiveBIND(t *testing.T) {
	if os.Getenv("KUDURRU_RECURSOR_BIND") != "1" {
		t.Skip("set KUDURRU_RECURSOR_BIND=1 for actual BIND interoperability")
	}
	for _, n3 := range []bool{false, true} {
		t.Run(fmt.Sprintf("nsec3=%t", n3), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
			defer cancel()
			dir := t.TempDir()
			childDS := bindSign(ctx, t, dir, "child", "example.", `$ORIGIN example.
$TTL 300
@ IN SOA ns.example. hostmaster.example. 1 60 10 3600 60
@ IN NS ns
ns IN A 127.0.0.1
www IN A 192.0.2.80
*.wild IN A 192.0.2.81
`, n3)
			rootDS := bindSign(ctx, t, dir, "root", ".", `$ORIGIN .
$TTL 300
@ IN SOA ns.root. hostmaster.root. 1 60 10 3600 60
@ IN NS ns.root.
ns.root. IN A 127.0.0.1
example. IN NS ns.example.
ns.example. IN A 127.0.0.1
`+childDS+"\n", false)
			addr := bindDenialPeer(ctx, t, dir)
			for _, tc := range []struct {
				name, first, second   string
				firstType, secondType uint16
				code                  int
				address               string
			}{
				{"NXDOMAIN", "one.absent.example.", "two.absent.example.", dns.TypeA, dns.TypeAAAA, dns.RcodeNameError, ""},
				{"NODATA", "www.example.", "www.example.", dns.TypeTXT, dns.TypeAAAA, dns.RcodeSuccess, ""},
				{"wildcard", "one.branch.wild.example.", "two.branch.wild.example.", dns.TypeA, dns.TypeA, dns.RcodeSuccess, "192.0.2.81"},
				{"wildcard NODATA", "one.branch.wild.example.", "two.branch.wild.example.", dns.TypeTXT, dns.TypeMX, dns.RcodeSuccess, ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ex := &countedAuthority{native: netExchanger{cookies: newCookieJar(time.Now)}}
					cfg := DefaultConfig()
					cfg.IPv6, cfg.QNAMEMinimization = IPv6Off, false
					cfg.RootHints, cfg.TrustAnchors = []string{addr}, []string{rootDS}
					r, err := New(cfg, Options{AllowLoopback: true, Exchanger: ex})
					if err != nil {
						t.Fatal(err)
					}
					m, sec := ask(t, r, tc.first, tc.firstType)
					want(t, m, sec, tc.code, SecSecure)
					before := ex.requests.Load()
					m, sec = ask(t, r, tc.second, tc.secondType)
					want(t, m, sec, tc.code, SecSecure)
					if before == 0 || ex.requests.Load() != before {
						t.Fatal("new covered question contacted BIND")
					}
					if tc.address != "" {
						want(t, m, sec, tc.code, SecSecure, tc.address)
					}
					verifyBINDProof(t, addr, m)
					_, _ = ask(t, r, tc.second, tc.secondType, cd)
					if ex.requests.Load() <= before {
						t.Fatal("CD query did not contact BIND")
					}
				})
			}
		})
	}
}

func verifyBINDProof(t *testing.T, addr string, m *dns.Msg) {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion("example.", dns.TypeDNSKEY)
	q.SetEdns0(1232, true)
	keys, _, err := (&dns.Client{Net: "tcp", Timeout: time.Second}).ExchangeContext(t.Context(), q, addr)
	if err != nil {
		t.Fatal("independent proof verifier could not read BIND keys")
	}
	checked := 0
	for _, section := range [][]dns.RR{m.Answer, m.Ns} {
		for _, rr := range section {
			sig, ok := rr.(*dns.RRSIG)
			if !ok {
				continue
			}
			set := rrsetOf(section, sig.Hdr.Name, sig.TypeCovered)
			verified := false
			for _, key := range keys.Answer {
				if k, ok := key.(*dns.DNSKEY); ok && sig.Verify(k, set) == nil {
					verified = true
					break
				}
			}
			if !verified {
				t.Fatal("synthesized proof failed independent signature verification")
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("synthesized response omitted cryptographic proof")
	}
}

func bindTool(t *testing.T, name string) string {
	t.Helper()
	for _, candidate := range []string{name, filepath.Join("/usr/sbin", name), filepath.Join("/usr/bin", name)} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Fatalf("explicit BIND interoperability requires installed %s", name)
	return ""
}

func bindRun(ctx context.Context, t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, bindTool(t, name), args...) //nolint:gosec // fixed BIND test tools and owned temporary files
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("BIND fixture tool %s failed: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

func bindSign(ctx context.Context, t *testing.T, dir, file, origin, data string, n3 bool) string {
	t.Helper()
	key := bindRun(ctx, t, dir, "dnssec-keygen", "-q", "-a", "ECDSAP256SHA256", "-f", "KSK", origin)
	if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-S", "-z", "-n", "1", "-o", origin, "-f", file + ".signed"}
	if n3 {
		args = append(args, "-3", "-", "-H", "0")
	}
	bindRun(ctx, t, dir, "dnssec-signzone", append(args, file)...)
	return bindRun(ctx, t, dir, "dnssec-dsfromkey", "-2", key+".key")
}

func bindDenialPeer(ctx context.Context, t *testing.T, dir string) string {
	t.Helper()
	port := testport.Reserve(t)
	addr := net.JoinHostPort(testport.Host, strconv.Itoa(port))
	conf := fmt.Sprintf(`options {
 directory %q;
 pid-file "named.pid";
 session-keyfile "session.key";
 managed-keys-directory ".";
 listen-on port %d { 127.0.0.1; };
 listen-on-v6 { none; };
 recursion no;
 dnssec-validation no;
 notify no;
};
controls {};
zone "." { type primary; file "root.signed"; };
zone "example." { type primary; file "child.signed"; };
`, dir, port)
	path := filepath.Join(dir, "named.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(dir, "named.log"), os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // fixed log name inside the test-owned temporary directory
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.CommandContext(ctx, bindTool(t, "named"), "-g", "-4", "-n", "1", "-c", path) //nolint:gosec // installed BIND, explicit private loopback configuration
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	ready, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	client := &dns.Client{Net: "tcp", Timeout: 100 * time.Millisecond}
	q := new(dns.Msg)
	q.SetQuestion("example.", dns.TypeSOA)
	for {
		m, _, err := client.ExchangeContext(ready, q, addr)
		if err == nil && m.Rcode == dns.RcodeSuccess && len(m.Answer) > 0 {
			return addr
		}
		select {
		case <-done:
			t.Fatal("owned BIND process exited before readiness")
		case <-ready.Done():
			t.Fatal("owned BIND process did not become ready")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
