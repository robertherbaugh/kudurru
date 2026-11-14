package trustanchors_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// knotImage and knotVersion are the digest-pinned Knot DNS image of the
// catalog interop fixture (test/interop/knot_catalog_test.go).
const (
	knotImage   = "cznic/knot@sha256:174b0fcb24c6c3bc5065c9d1ce46b588eec607a5f32bc40ff0cda60150f00bd9"
	knotVersion = "knotd, Knot DNS 3.6.2"
)

// generateKnotRollover makes the rollover stages with Knot DNS's own online
// signing under a manual key policy, driving the key timeline with keymgr
// (generate, set publish/ready/active/revoke/retire/remove), and captures
// each stage's reply from knotd. Knot signs the DNSKEY set with every
// published KSK, K2 included as soon as it is published, so s2 already
// carries signatures by K1 and K2 and the keymgr ready/active step before s3
// changes nothing knotd serves: s3 repeats s2's key set and signatures, and
// Knot yields four distinct stages. Only the BIND capture has "K2 published,
// signed by K1 only" and "K1 still published, signed by K2 only". The
// timeline and its assertions are the same. It needs KUDURRU_KNOT=1 and a
// private Docker daemon in KUDURRU_KNOT_DOCKER_HOST, as the catalog fixture.
func generateKnotRollover(t *testing.T) rolloverCapture {
	t.Helper()
	if os.Getenv("KUDURRU_KNOT") != "1" {
		t.Skip("set KUDURRU_KNOT=1 and KUDURRU_KNOT_DOCKER_HOST to capture a fresh Knot DNS rollover")
	}
	socket := os.Getenv("KUDURRU_KNOT_DOCKER_HOST")
	if !strings.HasPrefix(socket, "unix:///") || socket == "unix:///var/run/docker.sock" {
		t.Fatal("KUDURRU_KNOT_DOCKER_HOST must explicitly select a private Unix Docker socket")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("Knot capture requires the Docker CLI")
	}
	dir := t.TempDir()
	for _, sub := range []string{"db", "run"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	suffix := strings.ToLower(rand.Text()[:10])
	server := "kudurru-f28-rfc-knotd-" + suffix
	uid := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	var steps []string
	dockerRunIn := func(parent context.Context, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(parent, 60*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, docker, append([]string{"-H", socket}, args...)...).CombinedOutput() //nolint:gosec // explicit private test daemon and fixed executable
		return strings.TrimSpace(string(out)), err
	}
	dockerRun := func(args ...string) (string, error) { return dockerRunIn(t.Context(), args...) }
	// Go cancels t.Context() before cleanups run, so the cleanup has its
	// own bounded context and checks that knotd is really gone.
	t.Cleanup(func() {
		_, _ = dockerRunIn(context.Background(), "rm", "-f", server)
		if left, err := dockerRunIn(context.Background(), "ps", "-aq", "--filter", "name=^"+server+"$"); err != nil || left != "" {
			t.Errorf("knotd container %s was not removed: %q %v", server, left, err)
		}
	})
	keymgr := func(args ...string) string {
		t.Helper()
		// keymgr opens the KASP database exclusively; knotd is stopped here.
		steps = append(steps, "keymgr -c knot.conf . "+strings.Join(args, " "))
		out, err := dockerRun(append([]string{"run", "--rm", "--network", "none", "--name", "kudurru-f28-rfc-keymgr-" + suffix, "--user", uid,
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-v", dir + ":/fixture", "--entrypoint", "keymgr", knotImage, "-c", "/fixture/knot.conf", "."}, args...)...)
		if err != nil {
			t.Fatalf("keymgr %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return out
	}
	version, err := dockerRun("run", "--rm", "--network", "none", "--entrypoint", "knotd", knotImage, "-V")
	if err != nil || version != knotVersion {
		t.Fatalf("pinned Knot executable: %q %v", version, err)
	}
	port := kudurrutest.FreePort(t)
	conf := fmt.Sprintf(`server:
  listen: 127.0.0.1@%d
  rundir: /fixture/run
database:
  storage: /fixture/db
log:
  - target: stdout
    any: info
policy:
  - id: manual
    manual: on
    algorithm: ecdsap256sha256
    rrsig-lifetime: 90d
    rrsig-refresh: 30d
    dnskey-ttl: %[2]d
    zone-max-ttl: %[2]d
template:
  - id: default
    storage: /fixture
zone:
  - domain: .
    file: root.zone
    dnssec-signing: on
    dnssec-policy: manual
`, port, rolloverTTL)
	if err := os.WriteFile(filepath.Join(dir, "knot.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	zone := fmt.Sprintf("$ORIGIN .\n$TTL %d\n@ IN SOA a.root-servers.lab. hostmaster.lab. 1 1800 900 604800 86400\n@ IN NS a.root-servers.lab.\na.root-servers.lab. IN A 192.0.2.53\n", rolloverTTL)
	if err := os.WriteFile(filepath.Join(dir, "root.zone"), []byte(zone), 0o600); err != nil {
		t.Fatal(err)
	}
	// capture serves the current key timeline with knotd (host network,
	// loopback only), waits for the signed set with keys DNSKEY records and
	// stops knotd again.
	capture := func(stage string, keys int) []byte {
		t.Helper()
		steps = append(steps, "knotd -c knot.conf; capture "+stage)
		if out, err := dockerRun("run", "-d", "--network", "host", "--name", server, "--user", uid, "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
			"--pids-limit", "128", "-v", dir+":/fixture", "--entrypoint", "knotd", knotImage, "-c", "/fixture/knot.conf"); err != nil {
			t.Fatalf("start knotd: %v: %s", err, out)
		}
		var raw []byte
		ok := kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
			got, err := tryCaptureTCP(fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return false
			}
			m := new(dns.Msg)
			if m.Unpack(got) != nil || m.Rcode != dns.RcodeSuccess {
				return false
			}
			n := 0
			for _, rr := range m.Answer {
				if rr.Header().Rrtype == dns.TypeDNSKEY {
					n++
				}
			}
			raw = got
			return n == keys
		})
		logs, _ := dockerRun("logs", server)
		if _, err := dockerRun("rm", "-f", server); err != nil {
			t.Fatal("remove knotd")
		}
		if !ok {
			t.Fatalf("%s: knotd did not serve %d keys:\n%s", stage, keys, logs)
		}
		return raw
	}
	c := rolloverCapture{wires: map[string][]byte{}}
	k1 := keymgr("generate", "algorithm=13", "ksk=yes", "zsk=no")
	keymgr("generate", "algorithm=13", "ksk=no", "zsk=yes")
	// The operator's DS of K1, read before K1 is removed at the end.
	var seed string
	for _, line := range strings.Split(keymgr("ds", k1), "\n") {
		if f := strings.Fields(line); len(f) == 6 && f[1] == "DS" && f[4] == "2" {
			seed = strings.Join(f, " ")
		}
	}
	if seed == "" {
		t.Fatal("keymgr printed no SHA-256 DS for K1")
	}
	c.wires["s1"] = capture("s1", 2)
	k2 := keymgr("generate", "algorithm=13", "ksk=yes", "zsk=no", "publish=+0", "ready=+365d", "active=+365d")
	c.wires["s2"] = capture("s2", 3)
	keymgr("set", k2, "ready=+0", "active=+0")
	c.wires["s3"] = capture("s3", 3)
	revoke := time.Now().Unix()
	keymgr("set", k1, "revoke="+strconv.FormatInt(revoke, 10))
	c.wires["s5"] = capture("s5", 3)
	// Knot requires revoke < retire < remove; all must be past to remove K1.
	time.Sleep(time.Until(time.Unix(revoke+3, 0)))
	keymgr("set", k1, "retire="+strconv.FormatInt(revoke+1, 10), "remove="+strconv.FormatInt(revoke+2, 10))
	c.wires["s4"] = capture("s4", 2)
	// Knot's inceptions are 90 minutes before each signing; T0 follows the
	// last capture so that every stage's signatures are current from T0 on.
	t0 := time.Now().UTC().Truncate(time.Second).Add(time.Minute)
	c.provenance = rolloverProvenance{
		Signer: "Knot DNS online signing (manual policy, keymgr timeline)", Authority: version + "; " + knotImage,
		T0: t0, Captured: time.Now().UTC().Truncate(time.Second), SeedDS: seed,
		Steps: append(steps, fmt.Sprintf("knotd serves each stage on loopback; the reply to the managed acquisition request (RD clear, EDNS 1232 with DO) over TCP is kept as <stage>.wire; rrsig-lifetime 90d; DNSKEY TTL %d; stages: %s", rolloverTTL, knotStageSummary)),
	}
	return c
}

// knotStageSummary names what each Knot stage publishes and who signs it.
// Knot signs with every published KSK, so s3 repeats s2.
const knotStageSummary = "s1 {K1,ZSK} by K1; s2 {K1,K2,ZSK} by K1 and K2 (Knot signs with K2 once it is published); s3 repeats s2 (the ready/active step changes nothing served); s4 {K2,ZSK} by K2; s5 {K1+REVOKE,K2,ZSK} by K1+REVOKE and K2; four distinct stages"
