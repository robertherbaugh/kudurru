package interop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// These are executable fixtures, never linked dependencies. This qualifies an
// ordinary primary serving RFC 9432 records to Knot's catalog interpreter, not
// kudurru's deferred native catalog/secondaryCatalog configuration types.
const (
	knotImage   = "cznic/knot@sha256:174b0fcb24c6c3bc5065c9d1ce46b588eec607a5f32bc40ff0cda60150f00bd9"
	knotBase    = "alpine@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a"
	knotVersion = "knotd, Knot DNS 3.6.2"
)

type knotFixture struct {
	t                    *testing.T
	docker, socket, dir  string
	primary, knot, netID string
	primaryIP, knotIP    string
	secret               string
	client               *http.Client
	token                string
}

// dockerOutput never includes an exec error or its arguments in test output.
// Daemon and process output can include credentials and stays private.
func (f *knotFixture) dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, f.docker, append([]string{"-H", f.socket}, args...)...) //nolint:gosec // explicit private test daemon and fixed executable
	return cmd.CombinedOutput()
}

func (f *knotFixture) privateLog(name string, b []byte) {
	if logDir := os.Getenv("KUDURRU_KNOT_LOG_DIR"); logDir != "" {
		if err := os.MkdirAll(logDir, 0o700); err == nil { //nolint:gosec // explicitly selected private test log directory
			_ = os.WriteFile(filepath.Join(logDir, name+".raw.log"), b, 0o600) //nolint:gosec // fixture-owned output, never printed
		}
	}
}

func (f *knotFixture) mustDocker(args ...string) []byte {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 20*time.Second)
	defer cancel()
	b, err := f.dockerOutput(ctx, args...)
	if err != nil {
		f.t.Fatal("private Docker fixture operation failed; process output withheld")
	}
	return b
}

func newKnotFixture(t *testing.T) *knotFixture {
	t.Helper()
	kudurrutest.RequireE2E(t)
	if os.Getenv("KUDURRU_KNOT") != "1" {
		t.Skip("set KUDURRU_KNOT=1 or run hack/test-knot-catalog.sh for actual Knot qualification")
	}
	if os.Getenv(kudurrutest.EnvBinary) != "" {
		t.Fatal("Knot qualification requires the temporary binary built from this checkout")
	}
	socket := os.Getenv("KUDURRU_KNOT_DOCKER_HOST")
	if !strings.HasPrefix(socket, "unix:///") || socket == "unix:///var/run/docker.sock" {
		t.Fatal("KUDURRU_KNOT_DOCKER_HOST must explicitly select a private Unix Docker socket")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("Knot qualification requires the Docker CLI")
	}
	suffix := strings.ToLower(rand.Text()[:12])
	f := &knotFixture{t: t, docker: docker, socket: socket, dir: t.TempDir(), secret: newSecret(t), client: &http.Client{Timeout: 2 * time.Second}}
	f.primary, f.knot, f.netID = "kudurru-knot-primary-"+suffix, "kudurru-knot-consumer-"+suffix, "kudurru-knot-"+suffix
	for _, image := range []string{knotImage, knotBase} {
		f.mustDocker("image", "inspect", image, "--format", "{{.Id}}")
	}
	version := strings.TrimSpace(string(f.mustDocker("run", "--rm", "--network", "none", "--entrypoint", "knotd", knotImage, "-V")))
	if version != knotVersion {
		t.Fatal("pinned Knot executable version did not match the fixture")
	}
	t.Log("actual consumer: " + version + "; " + knotImage)
	// Install cleanup before creating any resources. Only this test's named
	// containers/network are removed. No daemon or shared cache is touched.
	t.Cleanup(func() {
		f.client.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, name := range []string{f.knot, f.primary} {
			b, _ := f.dockerOutput(ctx, "logs", "--tail", "2000", name)
			f.privateLog(name, b)
			_, _ = f.dockerOutput(ctx, "rm", "-f", name)
		}
		_, _ = f.dockerOutput(ctx, "network", "rm", f.netID)
	})
	f.mustDocker("network", "create", "--internal", f.netID)
	for _, path := range []string{"objects.d", "data", "knot-data"} {
		if err := os.MkdirAll(filepath.Join(f.dir, path), 0o750); err != nil {
			t.Fatal("create private fixture directory")
		}
	}
	// A hard link preserves the temporary MainBuild binary without rebuilding
	// or replacing a shared binary. Both locations live on the sandbox disk.
	if err := os.Link(kudurruBin, filepath.Join(f.dir, "kudurru")); err != nil {
		t.Fatal("link temporary checkout binary into private container fixture")
	}
	uid := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	start := func(name, image, gate, command string) string {
		t.Helper()
		f.mustDocker("run", "-d", "--name", name, "--network", f.netID, "--user", uid,
			"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128",
			"-v", f.dir+":/fixture", "--entrypoint", "/bin/sh", image, "-c",
			"while [ ! -f /fixture/"+gate+" ]; do sleep 0.1; done; exec "+command)
		ip := strings.TrimSpace(string(f.mustDocker("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)))
		if net.ParseIP(ip) == nil {
			t.Fatal("private fixture container has no IP address")
		}
		return ip
	}
	f.primaryIP = start(f.primary, knotBase, "primary.start", "/fixture/kudurru serve --config /fixture/primary.yaml")
	f.knotIP = start(f.knot, knotImage, "knot.start", "knotd -c /fixture/knot.conf")
	writeFile(t, filepath.Join(f.dir, "good.secret"), f.secret)
	for _, key := range []struct{ file, name, secret string }{
		{"good.key", keyName, f.secret}, {"wrong.key", keyName, newSecret(t)}, {"unknown.key", "unknown-key", f.secret},
	} {
		writeFile(t, filepath.Join(f.dir, key.file), fmt.Sprintf("hmac-sha256:%s:%s\n", key.name, key.secret))
	}
	ports := kudurrutest.Ports{DNS: "0.0.0.0:1053", API: "0.0.0.0:18080", Ops: "0.0.0.0:18081", Metrics: "0.0.0.0:19090", Internal: "0.0.0.0:18443"}
	cfg := kudurrutest.BaseConfig("knot-primary", "/fixture/data", ports)
	cfg["objectsDir"] = "/fixture/objects.d"
	cfg["tsigKeys"] = []any{map[string]any{"name": keyName, "algorithm": "hmac-sha256", "secretFile": "/fixture/good.secret"}} //nolint:gosec // file path; credential bytes are generated randomly
	cfg["notify"] = map[string]any{"source": f.primaryIP}
	kudurrutest.WriteConfig(t, filepath.Join(f.dir, "primary.yaml"), cfg)
	writeFile(t, filepath.Join(f.dir, "objects.d", "zones.yaml"), knotZones(f.knotIP))
	writeFile(t, filepath.Join(f.dir, "objects.d", "catalog.yaml"), knotCatalog("alpha"))
	writeFile(t, filepath.Join(f.dir, "knot.conf"), fmt.Sprintf(`server:
  listen: 0.0.0.0@15353
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
  - id: %s
    algorithm: hmac-sha256
    secret: %s
remote:
  - id: primary
    address: %s@1053
    key: %s
acl:
  - id: notify-primary
    address: %s
    key: %s
    action: notify
template:
  - id: catalog-members
    storage: /fixture/knot-data
    master: primary
    acl: notify-primary
zone:
  - domain: catalog.test.
    storage: /fixture/knot-data
    master: primary
    acl: notify-primary
    catalog-role: interpret
    catalog-template: catalog-members
`, keyName, f.secret, f.primaryIP, keyName, f.primaryIP, keyName))
	writeFile(t, filepath.Join(f.dir, "primary.start"), "start\n")
	if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, f.ready) {
		t.Fatal("current primary serving publication did not become ready")
	}
	b, err := exec.CommandContext(t.Context(), kudurruBin, "admin", "create-token", "--socket", filepath.Join(f.dir, "data", "admin.sock")).Output() //nolint:gosec // temporary checkout binary and private admin socket
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		t.Fatal("create private fixture readiness credential")
	}
	f.token = strings.TrimSpace(string(b))
	return f
}

func knotZones(knotIP string) string {
	var b strings.Builder
	for i, name := range []string{"catalog", "alpha", "beta", "denied"} {
		policy := fmt.Sprintf("zoneTransfer: useNetworkACL\n    transferACL: [%q]\n    transferTSIGKeyRefs: [%s]", knotIP+"/32", keyName)
		if name == "denied" {
			policy = "zoneTransfer: deny\n    transferTSIGKeyRefs: [" + keyName + "]"
		}
		fmt.Fprintf(&b, `---
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: knot-%[1]s }
spec:
  zoneName: %[1]s.test.
  type: primary
  soa: { mname: ns.%[1]s.test., rname: hostmaster.%[1]s.test., refresh: 3600, retry: 600 }
  nameservers: [ns.%[1]s.test.]
  policy:
    %[2]s
    notify: specified
    notifyTargets: ["%[3]s:15353"]
---
apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: { name: knot-%[1]s, namespace: default }
spec:
  zoneRef: { name: knot-%[1]s }
  endpoints:
    - { dnsName: ns.%[1]s.test, recordType: A, targets: ["192.0.2.1"] }
    - { dnsName: www.%[1]s.test, recordType: A, targets: ["192.0.2.%[4]d"] }
`, name, policy, knotIP, i+10)
	}
	return b.String()
}

func knotCatalog(member string) string {
	return fmt.Sprintf(`apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: { name: knot-catalog-members, namespace: default }
spec:
  zoneRef: { name: knot-catalog }
  endpoints:
    - { dnsName: version.catalog.test, recordType: TXT, targets: ['"2"'] }
    - { dnsName: %[1]s.zones.catalog.test, recordType: PTR, targets: [%[1]s.test.] }
    - { dnsName: denied.zones.catalog.test, recordType: PTR, targets: [denied.test.] }
`, member)
}

func (f *knotFixture) ready() bool {
	r, err := f.client.Get("http://" + f.primaryIP + ":18081/healthz/ready")
	if err != nil {
		return false
	}
	defer func() { _ = r.Body.Close() }()
	return r.StatusCode == http.StatusOK
}

// settled requires the status projection AND current committed serving view,
// and a matching native SOA. It never probes/retries a transfer for readiness.
func (f *knotFixture) settled(name string) (uint32, bool) {
	f.t.Helper()
	req, err := http.NewRequestWithContext(f.t.Context(), http.MethodGet, "http://"+f.primaryIP+":18080/api/v1/zones/knot-"+name, nil)
	if err != nil {
		f.t.Fatal("construct primary readiness read")
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	r, err := f.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode == http.StatusServiceUnavailable || r.StatusCode == http.StatusNotFound {
		return 0, false
	}
	if r.StatusCode != http.StatusOK {
		f.t.Fatalf("primary readiness read refused: HTTP %d", r.StatusCode)
	}
	var detail struct {
		Ready  bool           `json:"ready"`
		Serial uint32         `json:"serial"`
		Object public.DNSZone `json:"object"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&detail) != nil {
		f.t.Fatal("decode bounded primary readiness view")
	}
	z := &detail.Object
	condition := meta.FindStatusCondition(z.Status.Conditions, public.ConditionReady)
	if z.Name != "knot-"+name || condition == nil || condition.Status != metav1.ConditionTrue ||
		condition.ObservedGeneration != z.Generation || z.Status.ObservedGeneration != z.Generation ||
		!detail.Ready || detail.Serial == 0 || z.Status.Serial != detail.Serial {
		return 0, false
	}
	serial, code := soaSerial(f.t, f.primaryIP+":1053", name+".test.")
	return serial, code == dns.RcodeSuccess && serial == detail.Serial
}

func (f *knotFixture) waitSettled(name string) uint32 {
	f.t.Helper()
	var serial uint32
	if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool {
		var ok bool
		serial, ok = f.settled(name)
		return ok && f.ready()
	}) {
		f.t.Fatal("primary status/current publication/native SOA did not settle for " + name)
	}
	return serial
}

// Knot deliberately refuses ordinary queries to an interpreted catalog. Read
// its installed serial via the private local control socket without granting a
// public query ACL merely to satisfy this fixture's readiness check.
func (f *knotFixture) catalogSerial() (uint32, bool) {
	ctx, cancel := context.WithTimeout(f.t.Context(), 3*time.Second)
	defer cancel()
	b, err := f.dockerOutput(ctx, "exec", f.knot, "knotc", "-c", "/fixture/knot.conf", "zone-status", "catalog.test.", "+serial")
	if err != nil {
		return 0, false
	}
	_, tail, found := strings.Cut(string(b), "serial: ")
	if !found {
		return 0, false
	}
	fields := strings.Fields(tail)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(fields[0], 10, 32)
	return uint32(n), err == nil && n != 0
}

func (f *knotFixture) transfer(zone, keyFile string) ([]dns.RR, string, bool) {
	f.t.Helper()
	args := []string{"exec", f.knot, "kdig", "@" + f.primaryIP, "-p", "1053", "+tcp", "+retry=0", "+timeout=3", "+noall", "+answer", "+header"}
	if keyFile != "" {
		args = append(args, "-k", "/fixture/"+keyFile)
	}
	args = append(args, zone, "AXFR")
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	b, err := f.dockerOutput(ctx, args...)
	f.privateLog(f.knot+"-"+zone+"-"+keyFile, b)
	var records []dns.RR
	status := ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "status: ") {
			_, tail, _ := strings.Cut(line, "status: ")
			fields := strings.Fields(tail)
			if len(fields) > 0 {
				status = strings.TrimRight(fields[0], ";,")
			}
		}
		// kdig reports AXFR refusal in its error comment rather than a normal
		// response header. Require an explicit DNS refusal, never a timeout or
		// generic nonzero process exit. No request is retried.
		if strings.HasPrefix(strings.TrimSpace(line), ";;") {
			for _, code := range []string{"REFUSED", "NOTAUTH", "BADSIG", "BADKEY"} {
				if strings.Contains(line, code) {
					status = code
				}
			}
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";;") {
			continue
		}
		rr, parseErr := dns.NewRR(line)
		if parseErr == nil {
			records = append(records, rr)
		}
	}
	return records, status, err == nil
}

func TestKnotCatalogConsumer(t *testing.T) {
	f := newKnotFixture(t)
	serials := map[string]uint32{}
	for _, zone := range []string{"catalog", "alpha", "beta", "denied"} {
		serials[zone] = f.waitSettled(zone)
	}
	writeFile(t, filepath.Join(f.dir, "knot.start"), "start\n")
	start := time.Now()
	consumer := f.knotIP + ":15353"
	if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
		catalog, c := f.catalogSerial()
		alpha, a := soaSerial(t, consumer, "alpha.test.")
		return c && a == dns.RcodeSuccess && catalog == serials["catalog"] && alpha == serials["alpha"]
	}) {
		t.Fatal("actual Knot did not consume the catalog and automatically transfer alpha")
	}
	t.Logf("initial catalog and automatic member transfer: %s", time.Since(start).Round(time.Millisecond))
	assertA := func(zone, address string) {
		t.Helper()
		r, err := query(t, consumer, "www."+zone+".test.", dns.TypeA, false)
		if err != nil || r.Rcode != dns.RcodeSuccess || !r.Authoritative || !hasA(r.Answer, "www."+zone+".test.", address) {
			t.Fatal("Knot member did not serve its expected authoritative record: " + zone)
		}
	}
	assertA("alpha", "192.0.2.11")
	t.Run("CatalogQueriesRefused", func(t *testing.T) {
		r, err := query(t, consumer, "catalog.test.", dns.TypeSOA, false)
		if err != nil || r.Rcode != dns.RcodeNotAuth || r.Authoritative || len(r.Answer) != 0 {
			t.Fatal("Knot catalog unexpectedly permitted public DNS queries")
		}
	})
	t.Run("SignedAXFR", func(t *testing.T) {
		rrs, status, ok := f.transfer("catalog.test.", "good.key")
		if !ok || len(rrs) < 4 || rrs[0].Header().Rrtype != dns.TypeSOA || rrs[len(rrs)-1].Header().Rrtype != dns.TypeSOA {
			t.Fatalf("one-shot signed catalog AXFR did not complete: command_success=%t, records=%d, status_present=%t", ok, len(rrs), status != "")
		}
		version, member := false, false
		for _, rr := range rrs {
			switch rr := rr.(type) {
			case *dns.TXT:
				version = version || (rr.Hdr.Name == "version.catalog.test." && len(rr.Txt) == 1 && rr.Txt[0] == "2")
			case *dns.PTR:
				member = member || (rr.Hdr.Name == "alpha.zones.catalog.test." && rr.Ptr == "alpha.test.")
			}
		}
		if !version || !member {
			t.Fatal("catalog transfer did not preserve RFC 9432 version and membership data")
		}
	})
	for _, mode := range []struct{ name, zone, key, want string }{
		{"UnsignedRefused", "catalog.test.", "", "REFUSED"},
		{"WrongKeyRefused", "catalog.test.", "wrong.key", "BADSIG"},
		{"UnknownKeyRefused", "catalog.test.", "unknown.key", "BADKEY"},
		{"PolicyDenyRefused", "denied.test.", "good.key", "REFUSED"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			rrs, status, _ := f.transfer(mode.zone, mode.key)
			if len(rrs) != 0 || status != mode.want {
				t.Fatalf("one-shot disallowed AXFR was not explicitly refused without record data: records=%d, status_present=%t", len(rrs), status != "")
			}
		})
	}
	t.Run("SourceACLRefused", func(t *testing.T) {
		// The host's bridge address is outside the exact Knot /32 ACL. Read
		// the single denial frame so a timeout cannot masquerade as refusal.
		m := new(dns.Msg)
		m.SetAxfr("catalog.test.")
		m.SetTsig(dns.Fqdn(keyName), keyAlg, 30, time.Now().Unix())
		c := &dns.Client{Net: "tcp", Timeout: 3 * time.Second, TsigSecret: map[string]string{dns.Fqdn(keyName): f.secret}}
		r, _, err := c.ExchangeContext(t.Context(), m, f.primaryIP+":1053")
		if err != nil || r == nil || r.Rcode != dns.RcodeRefused || len(r.Answer) != 0 || r.IsTsig() == nil {
			t.Fatal("valid credential from outside the source ACL did not receive an explicit signed refusal")
		}
	})
	t.Run("MountedDoctor", func(t *testing.T) {
		b, err := f.dockerOutput(t.Context(), "exec", f.primary, "/fixture/kudurru", "--config", "/fixture/primary.yaml", "doctor", "--skip-api", "--skip-ports", "--notify", "--notify-target", f.primaryIP+":1053", "--notify-key", keyName)
		if err != nil || !bytes.Contains(b, []byte("notify: OK")) || !bytes.Contains(b, []byte("observed egress "+f.primaryIP)) {
			t.Fatal("production doctor mount did not attest to the selected non-loopback source")
		}
	})
	// All direct transfers above are one-shot. A refusal is a failure, never a
	// request to retry. Only readonly readiness and consumer SOA queries poll.
	if t.Failed() {
		return
	}
	before := serials["catalog"]
	changed := time.Now()
	writeFile(t, filepath.Join(f.dir, "objects.d", "catalog.yaml"), knotCatalog("beta"))
	after := f.waitSettled("catalog")
	if after == before {
		// The source watcher has a bounded interval; wait for its new committed
		// status rather than treating the still-current old publication as new.
		if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool {
			var ok bool
			after, ok = f.settled("catalog")
			return ok && after != before
		}) {
			t.Fatal("catalog file replacement did not produce a new admitted serial")
		}
	}
	if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
		catalog, c := f.catalogSerial()
		beta, b := soaSerial(t, consumer, "beta.test.")
		old, err := query(t, consumer, "www.alpha.test.", dns.TypeA, false)
		removed := err == nil && old.Rcode == dns.RcodeRefused && !old.Authoritative && len(old.Answer) == 0
		return c && b == dns.RcodeSuccess && catalog == after && beta == serials["beta"] && removed
	}) {
		t.Fatal("Knot did not consume notified catalog replacement and add/remove member zones within 30s")
	}
	assertA("beta", "192.0.2.12")
	r, err := query(t, consumer, "www.denied.test.", dns.TypeA, false)
	if err != nil || len(r.Answer) != 0 || r.Authoritative || (r.Rcode != dns.RcodeRefused && r.Rcode != dns.RcodeServerFailure) {
		t.Fatal("Knot served a catalog member whose transfer policy denies acquisition")
	}
	if time.Since(changed) >= time.Minute {
		t.Fatal("catalog propagation was too slow to distinguish NOTIFY from refresh")
	}
	t.Logf("catalog serial %d -> %d; Knot added beta and removed alpha in %s (SOA refresh 3600s); denied member remained unavailable", before, after, time.Since(changed).Round(time.Millisecond))
}
