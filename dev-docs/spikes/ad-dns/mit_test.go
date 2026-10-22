package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/types"
)

// TestMITInterop uses an independent Kerberos implementation and DNS client.
// It creates an ephemeral loopback-only realm; no existing realm is modified.
// This is not Windows qualification. See the report for the remaining gates.
func TestMITInterop(t *testing.T) {
	if os.Getenv("KUDURRU_AD_MIT_TEST") != "1" {
		t.Skip("set KUDURRU_AD_MIT_TEST=1; requires MIT KDC/tools and nsupdate")
	}
	for _, binary := range []string{"kdb5_util", "kadmin.local", "krb5kdc", "kinit", "nsupdate"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatalf("required tool %s: %v", binary, err)
		}
	}
	dir := t.TempDir()
	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	kdcAddr := portListener.Addr().String()
	port := portListener.Addr().(*net.TCPAddr).Port
	_ = portListener.Close()
	const realm = "ADPROBE.TEST"
	config := fmt.Sprintf(`[libdefaults]
 default_realm = %s
 dns_lookup_kdc = false
 dns_lookup_realm = false
 rdns = false
 dns_canonicalize_hostname = false
 udp_preference_limit = 1
[realms]
 %s = {
  kdc = %s
 }
[domain_realm]
 localhost = %s
`, realm, realm, kdcAddr, realm)
	kdcConfig := fmt.Sprintf(`[kdcdefaults]
 kdc_ports = %d
 kdc_tcp_ports = %d
[realms]
 %s = {
  database_name = %s/principal
  key_stash_file = %s/stash
  acl_file = %s/kadm5.acl
  supported_enctypes = aes256-cts-hmac-sha1-96:normal aes128-cts-hmac-sha1-96:normal
 }
`, port, port, realm, dir, dir, dir)
	for name, content := range map[string]string{"krb5.conf": config, "kdc.conf": kdcConfig, "kadm5.acl": ""} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KRB5_CONFIG", filepath.Join(dir, "krb5.conf"))
	t.Setenv("KRB5_KDC_PROFILE", filepath.Join(dir, "kdc.conf"))
	t.Setenv("KRB5CCNAME", "FILE:"+filepath.Join(dir, "ccache"))
	t.Setenv("KRB5_TRACE", "/dev/null")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(name string, args ...string) {
		t.Helper()
		//nolint:gosec // G204: commands and arguments are local test fixtures, never network input.
		if output, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", name, err, output)
		}
	}
	var master [32]byte
	if _, err := rand.Read(master[:]); err != nil {
		t.Fatal(err)
	}
	run("kdb5_util", "create", "-s", "-r", realm, "-P", hex.EncodeToString(master[:]))
	for _, principal := range []string{"DNS/localhost", "member"} {
		run("kadmin.local", "-r", realm, "-q", "addprinc -randkey "+principal)
		name := "member"
		if strings.HasPrefix(principal, "DNS/") {
			name = "dns"
		}
		run("kadmin.local", "-r", realm, "-q", "ktadd -k "+filepath.Join(dir, name+".keytab")+" "+principal)
	}
	kdc := exec.CommandContext(ctx, "krb5kdc", "-n", "-r", realm)
	// Discard KDC output: no Kerberos tokens or test keys in CI logs.
	kdc.Stdout, kdc.Stderr = io.Discard, io.Discard
	if err := kdc.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = kdc.Process.Kill(); _ = kdc.Wait() }()
	for {
		conn, err := net.DialTimeout("tcp", kdcAddr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("ephemeral KDC did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	run("kinit", "-k", "-t", filepath.Join(dir, "member.keytab"), "member@"+realm)
	kt, err := keytab.Load(filepath.Join(dir, "dns.keytab"))
	if err != nil {
		t.Fatal("could not load test DNS keytab")
	}
	principal, _ := types.ParseSPNString("DNS/localhost")
	verified := make(chan string, 4)
	p := &probe{acceptor: &acceptor{keytab: kt, principal: principal, realm: realm},
		logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		zone:   "probe.example.", hostname: "localhost.",
		allowed: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, contexts: make(map[string]*securityContext),
		onVerified: func(principal string) { verified <- principal }}
	ready, done := make(chan string, 1), make(chan error, 1)
	probeCtx, stopProbe := context.WithCancel(ctx)
	go func() { done <- runServers(probeCtx, "127.0.0.1:0", p, func(addr string) { ready <- addr }) }()
	defer func() {
		stopProbe()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	var address string
	select {
	case address = <-ready:
	case <-ctx.Done():
		t.Fatal("probe did not start")
	}
	host, probePort, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"TCP", "UDP"} {
		t.Run(transport, func(t *testing.T) {
			args := []string{"-g", "-t", "5"}
			if transport == "TCP" {
				args = append(args, "-v")
			}
			cmd := exec.CommandContext(ctx, "nsupdate", args...)
			cmd.Stdin = strings.NewReader(fmt.Sprintf("server %s %s\nzone probe.example.\nupdate add member.probe.example. 60 A 192.0.2.10\nsend\nquit\n", host, probePort))
			output, err := cmd.CombinedOutput()
			if err == nil || !bytes.Contains(output, []byte("REFUSED")) || bytes.Contains(output, []byte("tsig verify failure")) {
				t.Fatalf("expected a verified signed REFUSED reply: %v\n%s", err, output)
			}
			select {
			case got := <-verified:
				if got != "member@"+realm {
					t.Fatalf("wrong authenticated principal %q", got)
				}
			case <-time.After(time.Second):
				t.Fatal("no authenticated update reached the probe")
			}
		})
	}
}
