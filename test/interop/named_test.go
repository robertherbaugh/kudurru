package interop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// named is a BIND 9 process run as the current (non-root) user with a
// configuration written by the test.
type named struct {
	dir  string
	addr string
	cmd  *exec.Cmd
	log  *lockedBuffer
	done chan struct{}
}

// namedBinary returns the named executable or skips the test.
func namedBinary(t *testing.T) string {
	t.Helper()
	for _, p := range []string{os.Getenv("KUDURRU_NAMED"), "named", "/usr/sbin/named", "/usr/local/sbin/named"} {
		if p == "" {
			continue
		}
		if path, err := exec.LookPath(p); err == nil {
			return path
		}
	}
	t.Skip("named (BIND 9) is not installed: dnf install bind / apt-get install bind9")
	return ""
}

// namedOptions is the options block every test instance shares: one
// loopback port, no recursion, no rndc, no IPv6, everything under dir.
func namedOptions(dir string, port int) string {
	return fmt.Sprintf(`options {
	directory "%[1]s";
	pid-file "%[1]s/named.pid";
	session-keyfile "%[1]s/session.key";
	managed-keys-directory "%[1]s";
	dump-file "%[1]s/named_dump.db";
	statistics-file "%[1]s/named.stats";
	listen-on port %[2]d { 127.0.0.1; };
	listen-on-v6 { none; };
	recursion no;
	dnssec-validation no;
	allow-query { any; };
	notify no;
	transfer-source 127.0.0.1;
	notify-source 127.0.0.1;
};
controls { };
logging {
	channel stderr_all { stderr; severity info; print-category yes; print-severity yes; };
	category default { stderr_all; };
	category xfer-in { stderr_all; };
	category notify { stderr_all; };
	category lame-servers { null; };
};
`, dir, port)
}

// startNamed checks conf with named-checkconf (when present), starts named
// in the foreground and waits until it answers on its port.
func startNamed(t *testing.T, bin, dir, conf string, port int) *named {
	t.Helper()
	path := filepath.Join(dir, "named.conf")
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if check, err := exec.LookPath(filepath.Join(filepath.Dir(bin), "named-checkconf")); err == nil {
		if out, err := exec.Command(check, path).CombinedOutput(); err != nil { //nolint:gosec // test binary
			t.Fatalf("named-checkconf: %v\n%s\n%s", err, out, conf)
		}
	}
	n := &named{dir: dir, addr: fmt.Sprintf("127.0.0.1:%d", port), log: &lockedBuffer{}, done: make(chan struct{})}
	// -g: foreground, log to stderr; -n 1: one worker; -4: IPv4 only.
	n.cmd = exec.Command(bin, "-g", "-4", "-n", "1", "-c", path) //nolint:gosec // test binary
	n.cmd.Stdout, n.cmd.Stderr = n.log, n.log
	if err := n.cmd.Start(); err != nil {
		t.Fatalf("start named: %v", err)
	}
	go func() { _ = n.cmd.Wait(); close(n.done) }()
	t.Cleanup(func() {
		_ = n.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-n.done:
		case <-time.After(10 * time.Second):
			_ = n.cmd.Process.Kill()
			<-n.done
		}
		if t.Failed() {
			t.Logf("named log:\n%s", n.log.String())
		}
	})
	ok := kudurrutest.Eventually(15*time.Second, 100*time.Millisecond, func() bool {
		select {
		case <-n.done:
			return true
		default:
		}
		return strings.Contains(n.log.String(), "running")
	})
	select {
	case <-n.done:
		t.Fatalf("named exited:\n%s", n.log.String())
	default:
	}
	if !ok {
		t.Fatalf("named did not start:\n%s", n.log.String())
	}
	return n
}

// query sends one question over UDP (TCP when tcp is set).
func query(t *testing.T, addr, name string, qtype uint16, tcp bool) (*dns.Msg, error) {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.RecursionDesired = true
	c := &dns.Client{Timeout: 3 * time.Second}
	if tcp {
		c.Net = "tcp"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _, err := c.ExchangeContext(ctx, m, addr)
	return r, err
}

// soaSerial returns the SOA serial addr answers for zone, or 0.
func soaSerial(t *testing.T, addr, zone string) (uint32, int) {
	t.Helper()
	r, err := query(t, addr, zone, dns.TypeSOA, false)
	if err != nil || r == nil {
		return 0, -1
	}
	for _, rr := range r.Answer {
		if soa, ok := rr.(*dns.SOA); ok {
			return soa.Serial, r.Rcode
		}
	}
	return 0, r.Rcode
}

// axfr runs a zone transfer, signed with key/secret when key is non-empty,
// and returns the records or the error (a refusal is an error).
func axfr(addr, zone, key, algorithm, secret string) ([]dns.RR, error) {
	m := new(dns.Msg)
	m.SetAxfr(dns.Fqdn(zone))
	tr := &dns.Transfer{DialTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second}
	if key != "" {
		m.SetTsig(dns.Fqdn(key), algorithm, 300, time.Now().Unix())
		tr.TsigSecret = map[string]string{dns.Fqdn(key): secret}
	}
	ch, err := tr.In(m, addr)
	if err != nil {
		return nil, err
	}
	var rrs []dns.RR
	for env := range ch {
		if env.Error != nil {
			return rrs, env.Error
		}
		rrs = append(rrs, env.RR...)
	}
	return rrs, nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
