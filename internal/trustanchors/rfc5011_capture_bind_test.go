package trustanchors_test

import (
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

// generateBINDRollover makes the five rollover stages with the BIND tools
// (dnssec-keygen, dnssec-revoke, dnssec-signzone with explicit signature
// windows) and captures each stage's reply from named on loopback. Without
// named or the tools it skips, or fails under KUDURRU_REQUIRE_BIND.
func generateBINDRollover(t *testing.T) rolloverCapture {
	t.Helper()
	bin := kudurrutest.Named(t)
	dir := t.TempDir()
	var steps []string
	run := func(name string, args ...string) string {
		t.Helper()
		steps = append(steps, name+" "+strings.Join(args, " "))
		cmd := exec.CommandContext(t.Context(), kudurrutest.BINDTool(t, name), args...) //nolint:gosec // fixed test tool and temporary files
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	version, err := exec.CommandContext(t.Context(), bin, "-v").CombinedOutput() //nolint:gosec // the located named binary
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Truncate(time.Second)
	inception, expiration := t0.Add(-24*time.Hour).Format("20060102150405"), t0.Add(90*24*time.Hour).Format("20060102150405")
	k1 := run("dnssec-keygen", "-q", "-a", "ECDSAP256SHA256", "-f", "KSK", ".")
	k2 := run("dnssec-keygen", "-q", "-a", "ECDSAP256SHA256", "-f", "KSK", ".")
	zsk := run("dnssec-keygen", "-q", "-a", "ECDSAP256SHA256", ".")
	k1r := run("dnssec-revoke", k1+".key")
	k1r = strings.TrimSuffix(filepath.Base(k1r), ".key")
	seed := run("dnssec-dsfromkey", "-2", k1+".key")
	stages := map[string][2][]string{ // published, signers
		"s1": {{k1, zsk}, {k1, zsk}},
		"s2": {{k1, k2, zsk}, {k1, zsk}},
		"s3": {{k1, k2, zsk}, {k2, zsk}},
		"s4": {{k2, zsk}, {k2, zsk}},
		"s5": {{k1r, k2, zsk}, {k1r, k2, zsk}},
	}
	c := rolloverCapture{wires: map[string][]byte{}}
	for i, stage := range rolloverStages {
		var b strings.Builder
		fmt.Fprintf(&b, "$ORIGIN .\n$TTL %d\n@ IN SOA a.root-servers.lab. hostmaster.lab. %d 1800 900 604800 86400\n@ IN NS a.root-servers.lab.\na.root-servers.lab. IN A 192.0.2.53\n", rolloverTTL, i+1)
		for _, key := range stages[stage][0] {
			fmt.Fprintf(&b, "$INCLUDE %s.key\n", key)
		}
		if err := os.WriteFile(filepath.Join(dir, stage+".zone"), []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		run("dnssec-signzone", append([]string{"-x", "-s", inception, "-e", expiration, "-o", ".", "-f", stage + ".signed", stage + ".zone"}, stages[stage][1]...)...)
		c.wires[stage] = captureNamed(t, bin, dir, stage+".signed")
	}
	c.provenance = rolloverProvenance{
		Signer: "BIND dnssec-signzone", Authority: strings.TrimSpace(string(version)),
		T0: t0, Captured: time.Now().UTC().Truncate(time.Second), SeedDS: seed,
		Steps: append(steps, fmt.Sprintf("named serves each stage on loopback; the reply to the managed acquisition request (RD clear, EDNS 1232 with DO) over TCP is kept as <stage>.wire; DNSKEY TTL %d; stages: %s", rolloverTTL, stageSummary)),
	}
	return c
}

// stageSummary names what each stage publishes and who signs it.
const stageSummary = "s1 {K1,ZSK} by K1; s2 {K1,K2,ZSK} by K1; s3 {K1,K2,ZSK} by K2; s4 {K2,ZSK} by K2; s5 {K1+REVOKE,K2,ZSK} by K1+REVOKE and K2"

// captureNamed starts named for one zone file, captures the reply and stops.
// named logs "running on ..." before it listens, so readiness is the first
// authoritative NOERROR reply to the capture request itself. BIND 9.18.50
// (seen with the Amazon Linux 2023 package) can abort at startup in
// dns_cache_create; like the interop fixture, named is started up to three
// times.
func captureNamed(t *testing.T, bin, dir, file string) []byte {
	t.Helper()
	for attempt := 1; ; attempt++ {
		raw, log, ok := tryCaptureNamed(t, bin, dir, file)
		if ok {
			return raw
		}
		if attempt == 3 {
			t.Fatalf("named did not answer for %s:\n%s", file, log)
		}
		t.Logf("named exited or did not answer at startup (attempt %d); starting it again", attempt)
	}
}

// tryCaptureNamed is one named start: it returns the captured reply, or
// named's log and false when named exited or did not answer in time.
func tryCaptureNamed(t *testing.T, bin, dir, file string) ([]byte, string, bool) {
	t.Helper()
	port := kudurrutest.FreePort(t)
	conf := filepath.Join(dir, "named.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf(`options {
	directory "%[1]s";
	pid-file "%[1]s/named.pid";
	session-keyfile "%[1]s/session.key";
	managed-keys-directory "%[1]s";
	listen-on port %[2]d { 127.0.0.1; };
	listen-on-v6 { none; };
	recursion no;
	dnssec-validation no;
	notify no;
};
controls { };
zone "." { type primary; file %[3]q; };
`, dir, port, file)), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var log strings.Builder
	cmd := exec.Command(bin, "-g", "-4", "-n", "1", "-c", conf) //nolint:gosec // the located named binary
	w := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return log.Write(p) })
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var raw []byte
	ok := kudurrutest.Eventually(15*time.Second, 50*time.Millisecond, func() bool {
		select {
		case <-done:
			return true
		default:
		}
		got, err := tryCaptureTCP(addr)
		if err != nil {
			return false
		}
		m := new(dns.Msg)
		if m.Unpack(got) != nil || m.Rcode != dns.RcodeSuccess || !m.Authoritative {
			return false
		}
		raw = got
		return true
	})
	mu.Lock()
	logged := log.String()
	mu.Unlock()
	return raw, logged, ok && raw != nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
