package interop

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// F28-RFC-1 (D398): a real authority (BIND 9 here, Knot DNS in
// managed_root_knot_test.go) serves a signed root zone and a signed child,
// and a kudurru node with anchorMode managed bootstraps its RFC 5011 state
// from it over the production TCP acquisition, then answers validated
// recursion through it. No stand-in authority is involved: every signature,
// key and response comes from the real server.

// managedRootIP is a local non-loopback IPv4 address for the authority. The
// native resolver refuses loopback authorities, also with custom root hints,
// and these tests do not relax that. Custom hints admit internal and
// link-local addresses (a private tree), so a sandbox with only those works.
func managedRootIP(t *testing.T) netip.Addr {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var fallback netip.Addr
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err != nil {
			continue
		}
		ip := prefix.Addr().Unmap()
		if !ip.Is4() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		if ip.IsGlobalUnicast() {
			return ip
		}
		if !fallback.IsValid() {
			fallback = ip
		}
	}
	if !fallback.IsValid() {
		t.Fatal("managed root interoperability requires a local non-loopback IPv4 interface")
	}
	return fallback
}

// managedRootTTL is the DNSKEY TTL of the process tests: one hour makes the
// RFC 5011 refresh interval the one-hour minimum (section 2.3), which the
// revocation test needs. The add hold-down stays 30 days.
const managedRootTTL = 3600

// childZone is the signed delegation below the root that validated recursion
// resolves. It must not be a name the resolver answers itself (RFC 6761).
const (
	childZone = "example."
	childName = "www.example."
	childAddr = "192.0.2.80"
)

// rootSigner keeps the keys and zone files of one signed root, made with the
// BIND tools (dnssec-keygen, dnssec-revoke, dnssec-signzone, dnssec-dsfromkey).
type rootSigner struct {
	t       *testing.T
	dir     string
	ip      netip.Addr
	ttl     int
	childDS string
}

func (s *rootSigner) run(name string, args ...string) string {
	s.t.Helper()
	cmd := exec.CommandContext(s.t.Context(), kudurrutest.BINDTool(s.t, name), args...) //nolint:gosec // fixed test tool and temporary files
	cmd.Dir = s.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRootSigner writes and signs the child zone (one ECDSA P-256 key, valid
// from an hour ago for 30 days) and keeps its DS for the root zone.
func newRootSigner(t *testing.T, dir string, ip netip.Addr) *rootSigner {
	t.Helper()
	s := &rootSigner{t: t, dir: dir, ip: ip, ttl: managedRootTTL}
	child := s.run("dnssec-keygen", "-q", "-a", "ECDSAP256SHA256", "-f", "KSK", childZone)
	writeFile(t, filepath.Join(dir, "child.zone"), fmt.Sprintf(`$ORIGIN %[1]s
$TTL 300
@ IN SOA ns.%[1]s hostmaster.%[1]s 1 1800 900 604800 300
@ IN NS ns
ns IN A %[2]s
www IN A %[3]s
$INCLUDE %[4]s.key
`, childZone, ip, childAddr, child))
	s.run("dnssec-signzone", "-z", "-s", "now-3600", "-e", "now+2592000", "-o", childZone, "-f", "child.zone.signed", "child.zone", child)
	s.childDS = s.run("dnssec-dsfromkey", "-2", child+".key")
	return s
}

// key generates a root key ("K.+013+12345"); bits is 0 for curves.
func (s *rootSigner) key(algorithm string, bits int, ksk bool) string {
	s.t.Helper()
	args := []string{"-q", "-a", algorithm}
	if bits > 0 {
		args = append(args, "-b", strconv.Itoa(bits))
	}
	if ksk {
		args = append(args, "-f", "KSK")
	}
	return s.run("dnssec-keygen", append(args, ".")...)
}

// revoke sets the REVOKE bit on key (RFC 5011 section 2.1) and returns the
// new key file name; the key tag changes with the flag.
func (s *rootSigner) revoke(key string) string {
	s.t.Helper()
	return strings.TrimSuffix(filepath.Base(s.run("dnssec-revoke", key+".key")), ".key")
}

// ds returns key's SHA-256 DS in presentation format, as an operator gets it.
func (s *rootSigner) ds(key string) string {
	s.t.Helper()
	return s.run("dnssec-dsfromkey", "-2", key+".key")
}

// keyTag is the tag in a key file name.
func keyTag(t *testing.T, key string) uint16 {
	t.Helper()
	tag, err := strconv.ParseUint(key[strings.LastIndex(key, "+")+1:], 10, 16)
	if err != nil {
		t.Fatal("key file name", key)
	}
	return uint16(tag)
}

// sign writes the root zone with serial, the published keys and the
// delegation of childZone, and signs it with signers only: with a
// zone-signing key among them, -x signs the DNSKEY RRset with the
// key-signing keys only, as the real root does.
// inception and expiration are dnssec-signzone times (now-3600, 20261201...).
func (s *rootSigner) sign(serial int, publish, signers []string, inception, expiration string) {
	s.t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, `$ORIGIN .
$TTL %[1]d
@ IN SOA a.root-servers.lab. hostmaster.lab. %[2]d 1800 900 604800 86400
@ IN NS a.root-servers.lab.
a.root-servers.lab. IN A %[3]s
%[4]s IN NS ns.%[4]s
ns.%[4]s IN A %[3]s
%[5]s
`, s.ttl, serial, s.ip, childZone, s.childDS)
	for _, key := range publish {
		fmt.Fprintf(&b, "$INCLUDE %s.key\n", key)
	}
	writeFile(s.t, filepath.Join(s.dir, "root.zone"), b.String())
	// Without a zone-signing key among the signers, -z lets the key-signing
	// keys sign every RRset instead.
	// -P: dnssec-signzone's own verification refuses expired signatures,
	// which ExpiredSignature makes on purpose; the tests check the result.
	mode := "-z"
	for _, key := range signers {
		data, err := os.ReadFile(filepath.Join(s.dir, key+".key")) //nolint:gosec // generated key under t.TempDir
		if err != nil {
			s.t.Fatal(err)
		}
		if strings.Contains(string(data), "DNSKEY 256 ") {
			mode = "-x"
		}
	}
	s.run("dnssec-signzone", append([]string{mode, "-P", "-s", inception, "-e", expiration, "-o", ".", "-f", "root.zone.next", "root.zone"}, signers...)...)
	if err := os.Rename(filepath.Join(s.dir, "root.zone.next"), filepath.Join(s.dir, "root.zone.signed")); err != nil {
		s.t.Fatal(err)
	}
}

// bindRoot is named serving the signed root and child zones on a
// non-loopback address, with its query log on: every query it answers is a
// log line that names the transport and the header and EDNS flags.
type bindRoot struct {
	*named
	ap netip.AddrPort
}

func startBINDRoot(t *testing.T, s *rootSigner, options string) *bindRoot {
	t.Helper()
	bin := namedBinary(t)
	port := kudurrutest.FreePort(t)
	ap := netip.AddrPortFrom(s.ip, uint16(port)) //nolint:gosec // FreePort returns a valid port
	conf := fmt.Sprintf(`options {
	directory "%[1]s";
	pid-file "%[1]s/named.pid";
	session-keyfile "%[1]s/session.key";
	managed-keys-directory "%[1]s";
	dump-file "%[1]s/named_dump.db";
	statistics-file "%[1]s/named.stats";
	listen-on port %[2]d { %[3]s; };
	listen-on-v6 { none; };
	recursion no;
	dnssec-validation no;
	allow-query { any; };
	notify no;
	querylog yes;
	%[4]s
};
controls { };
logging {
	channel stderr_all { stderr; severity info; print-category yes; print-severity yes; };
	category default { stderr_all; };
	category queries { stderr_all; };
	category lame-servers { null; };
};
zone "." { type primary; file "root.zone.signed"; };
zone %[5]q { type primary; file "child.zone.signed"; };
`, s.dir, port, s.ip, options, childZone)
	// startNamed only sees named's first log lines. Wait until it answers
	// on the address. BIND 9.18.50 (seen with the Amazon Linux 2023
	// package) can abort at startup in dns_cache_create; start it again.
	for attempt := 1; ; attempt++ {
		n := startNamed(t, bin, s.dir, conf, port)
		n.addr = ap.String()
		if kudurrutest.Eventually(15*time.Second, 50*time.Millisecond, func() bool {
			select {
			case <-n.done:
				return true
			default:
			}
			_, rcode := soaSerialTCP(n.addr, ".")
			return rcode == dns.RcodeSuccess
		}) {
			select {
			case <-n.done:
			default:
				return &bindRoot{named: n, ap: ap}
			}
		}
		if attempt == 3 {
			t.Fatalf("named did not answer on %s:\n%s", ap, n.log.String())
		}
		t.Logf("named exited or did not answer at startup (attempt %d); starting it again", attempt)
	}
}

// managedAcquisition is the query log line of one managed root anchor
// acquisition: the root DNSKEY set over TCP ("T") with EDNS version 0, DO
// ("D"), RD clear ("-"), no CD and no cookie, exactly as
// cmd/kudurru/serving_anchor_acquire.go sends it. The resolver's own DNSKEY
// lookups go over UDP with a server cookie and never match.
var managedAcquisition = regexp.MustCompile(`\(\.\): query: \. IN DNSKEY -E\(0\)TD \(`)

// acquisitions counts the managed acquisitions named has answered.
func (r *bindRoot) acquisitions() int {
	return len(managedAcquisition.FindAllString(r.log.String(), -1))
}

// reload makes named load the re-signed root zone (SIGHUP) and waits until
// it serves serial.
func (r *bindRoot) reload(t *testing.T, serial uint32) {
	t.Helper()
	if err := r.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if !kudurrutest.Eventually(15*time.Second, 50*time.Millisecond, func() bool {
		got, rcode := soaSerialTCP(r.ap.String(), ".")
		return rcode == dns.RcodeSuccess && got == serial
	}) {
		t.Fatalf("named did not load root serial %d:\n%s", serial, r.log.String())
	}
}

func soaSerialTCP(addr, zone string) (uint32, int) {
	m := new(dns.Msg)
	m.SetQuestion(zone, dns.TypeSOA)
	m.RecursionDesired = false
	c := &dns.Client{Net: "tcp", Timeout: 3 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _, err := c.ExchangeContext(ctx, m, addr)
	if err != nil {
		return 0, -1
	}
	for _, rr := range r.Answer {
		if soa, ok := rr.(*dns.SOA); ok {
			return soa.Serial, r.Rcode
		}
	}
	return 0, r.Rcode
}

// probeAnchorWire sends the managed acquisition request (RD clear, EDNS 1232
// with DO, TCP) to an authority and checks the strict wire assumptions of
// the bootstrap parser: an authoritative NOERROR answer, no authority
// records and at most the OPT record in the additional section
// (cmd/kudurru/serving_anchor_operation.go, unpackAnchorBootstrap).
func probeAnchorWire(t *testing.T, addr string) *dns.Msg {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	q.RecursionDesired = false
	q.SetEdns0(1232, true)
	c := &dns.Client{Net: "tcp", Timeout: 3 * time.Second}
	m, _, err := c.ExchangeContext(t.Context(), q, addr)
	if err != nil {
		t.Fatal("root DNSKEY over TCP", err)
	}
	if m.Rcode != dns.RcodeSuccess || !m.Authoritative || m.Truncated || len(m.Ns) != 0 || len(m.Extra) != 1 || m.IsEdns0() == nil || !m.IsEdns0().Do() {
		t.Fatalf("authority breaks the managed acquisition wire assumptions (NSCOUNT %d, ARCOUNT %d): %s", len(m.Ns), len(m.Extra), m)
	}
	t.Logf("root DNSKEY over TCP: %d answer records, NSCOUNT 0, ARCOUNT 1 (OPT, DO), %d bytes", len(m.Answer), m.Len())
	return m
}

// managedRootNode is a standalone SQLite node with managed root anchors,
// direct topology, the authority as its only root hint and no cache, so that
// every validated answer is resolved from the authority.
func managedRootNode(t *testing.T, dir, name string, root netip.AddrPort, seeds []string) kudurrutest.NodeSpec {
	t.Helper()
	spec, cfg := kudurrutest.NewNodeSpec(t, dir, name)
	trust := make([]any, len(seeds))
	for i, seed := range seeds {
		trust[i] = seed
	}
	cfg["upstreams"] = map[string]any{
		"groups": map[string]any{"default": map[string]any{"mode": "recursive", "strategy": "parallel_best"}},
		"recursion": map[string]any{"anchorMode": "managed", "managedTopology": "direct", "dnssec": true,
			"trustAnchors": trust, "rootHints": []any{root.String()}, "ipv6": "off"},
	}
	cfg["cache"] = map[string]any{"maxEntries": 0}
	kudurrutest.WriteConfig(t, spec.Config, cfg)
	return spec
}

// validatedWhy asks node, over UDP and TCP, for the child's address and the
// root DNSKEY set and returns "" when every answer is NOERROR with the AD
// bit and the signed data, or else what the first other answer was.
func validatedWhy(node string) string {
	for _, network := range []string{"udp", "tcp"} {
		for _, question := range []dns.Question{{Name: childName, Qtype: dns.TypeA, Qclass: dns.ClassINET}, {Name: ".", Qtype: dns.TypeDNSKEY, Qclass: dns.ClassINET}} {
			q := new(dns.Msg)
			q.SetQuestion(question.Name, question.Qtype)
			q.SetEdns0(1232, true)
			c := &dns.Client{Net: network, Timeout: 3 * time.Second}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			m, _, err := c.ExchangeContext(ctx, q, node)
			cancel()
			if err != nil {
				return network + " " + question.Name + ": " + err.Error()
			}
			signed := false
			for _, rr := range m.Answer {
				if sig, ok := rr.(*dns.RRSIG); ok && sig.TypeCovered == question.Qtype {
					signed = true
				}
			}
			if m.Rcode != dns.RcodeSuccess || !m.AuthenticatedData || !signed || (question.Qtype == dns.TypeA && !hasA(m.Answer, childName, childAddr)) {
				return fmt.Sprintf("%s %s: %s ad=%v answers=%d", network, question.Name, dns.RcodeToString[m.Rcode], m.AuthenticatedData, len(m.Answer))
			}
		}
	}
	return ""
}

// servfailWhy returns "" when node answers SERVFAIL without data or the AD
// bit to the same questions over UDP and TCP.
func servfailWhy(node string) string {
	for _, network := range []string{"udp", "tcp"} {
		for _, question := range []dns.Question{{Name: childName, Qtype: dns.TypeA, Qclass: dns.ClassINET}, {Name: ".", Qtype: dns.TypeDNSKEY, Qclass: dns.ClassINET}} {
			q := new(dns.Msg)
			q.SetQuestion(question.Name, question.Qtype)
			q.SetEdns0(1232, true)
			c := &dns.Client{Net: network, Timeout: 3 * time.Second}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			m, _, err := c.ExchangeContext(ctx, q, node)
			cancel()
			if err != nil {
				return network + " " + question.Name + ": " + err.Error()
			}
			if m.Rcode != dns.RcodeServerFailure || m.AuthenticatedData || len(m.Answer) != 0 {
				return fmt.Sprintf("%s %s: %s ad=%v answers=%d", network, question.Name, dns.RcodeToString[m.Rcode], m.AuthenticatedData, len(m.Answer))
			}
		}
	}
	return ""
}

func requireValidated(t *testing.T, node *kudurrutest.Node, why string) {
	t.Helper()
	if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool { return validatedWhy(node.Ports.DNS) == "" }) {
		t.Fatalf("%s: no validated recursion: %s", why, validatedWhy(node.Ports.DNS))
	}
}

func requireServfail(t *testing.T, node *kudurrutest.Node, why string) {
	t.Helper()
	if !kudurrutest.Eventually(20*time.Second, 100*time.Millisecond, func() bool { return servfailWhy(node.Ports.DNS) == "" }) {
		t.Fatalf("%s: expected SERVFAIL: %s", why, servfailWhy(node.Ports.DNS))
	}
}

// offlineAnchors is the part of `kudurru anchors status --offline` these
// tests read.
type offlineAnchors struct {
	Owner struct {
		Phase       string `json:"phase"`
		Quarantined bool   `json:"quarantined"`
		Keys        []struct {
			KeyTag    uint16 `json:"keyTag"`
			Algorithm uint8  `json:"algorithm"`
			Status    string `json:"status"`
		} `json:"keys"`
		NextRefresh *time.Time `json:"nextRefresh"`
	} `json:"owner"`
}

// keys renders the owner's keys as sorted "tag/algorithm/status" entries.
func (a offlineAnchors) keys() string {
	var out []string
	for _, k := range a.Owner.Keys {
		out = append(out, fmt.Sprintf("%d/%d/%s", k.KeyTag, k.Algorithm, k.Status))
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

// offlineStatus reads a stopped node's managed state.
func offlineStatus(t *testing.T, bin string, spec kudurrutest.NodeSpec) offlineAnchors {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "anchors", "status", "--offline", "--database", filepath.Join(spec.DataDir, "state.db"), "--timeout", "10s").Output() //nolint:gosec // the test binary and its own data directory
	if err != nil {
		t.Fatalf("anchors status --offline: %v: %s", err, out)
	}
	var status offlineAnchors
	if err := json.Unmarshal(out, &status); err != nil {
		t.Fatalf("anchors status --offline output: %v: %s", err, out)
	}
	return status
}

func namedVersion(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), namedBinary(t), "-v").CombinedOutput() //nolint:gosec // the located named binary
	if err != nil {
		t.Fatal("named -v", err)
	}
	return strings.TrimSpace(string(out))
}

// managedRootAlgorithms are the root key algorithms the managed parser
// accepts that these tests sign with: RSA/SHA-256 (the real root's), ECDSA
// P-256 and Ed25519.
var managedRootAlgorithms = []struct {
	name      string
	algorithm uint8
	bits      int
}{
	{"RSASHA256", dns.RSASHA256, 2048},
	{"ECDSAP256SHA256", dns.ECDSAP256SHA256, 0},
	{"ED25519", dns.ED25519, 0},
}

// TestBINDManagedRootBootstrap bootstraps managed root anchors from a
// BIND-signed root for each algorithm, over exactly one production TCP
// acquisition, answers AD-validated recursion through the BIND-signed child,
// and makes no new acquisition across a restart (the next refresh is an
// hour away).
func TestBINDManagedRootBootstrap(t *testing.T) {
	kudurrutest.RequireE2E(t)
	t.Log("authority: " + namedVersion(t))
	ip := managedRootIP(t)
	for _, alg := range managedRootAlgorithms {
		t.Run(alg.name, func(t *testing.T) {
			dir := t.TempDir()
			s := newRootSigner(t, dir, ip)
			ksk := s.key(alg.name, alg.bits, true)
			zsk := s.key(alg.name, alg.bits, false)
			s.sign(1, []string{ksk, zsk}, []string{ksk, zsk}, "now-3600", "now+2592000")
			root := startBINDRoot(t, s, "")
			probe := probeAnchorWire(t, root.addr)
			if len(probe.Answer) != 3 {
				t.Fatal("expected KSK, ZSK and one KSK signature", probe)
			}
			// The probe sends the same request; named logs it after the reply.
			awaitAcquisitions(t, root, 1, "probe")
			base := 1
			spec := managedRootNode(t, dir, "managed", root.ap, []string{s.ds(ksk)})
			node := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
			requireValidated(t, node, "after bootstrap")
			if n := root.acquisitions() - base; n != 1 {
				t.Fatal("bootstrap made other than one managed acquisition", n)
			}
			node.Stop(15 * time.Second)
			node = kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
			requireValidated(t, node, "after restart")
			if n := root.acquisitions() - base; n != 1 {
				t.Fatal("a restart before the refresh is due acquired again", n)
			}
			node.Stop(15 * time.Second)
			status := offlineStatus(t, kudurruBin, spec)
			// Only the seeded KSK is tracked: the ZSK has no SEP bit and is
			// never an anchor candidate.
			want := fmt.Sprintf("%d/%d/Valid", keyTag(t, ksk), alg.algorithm)
			if status.Owner.Phase != "managed" || status.Owner.Quarantined || status.keys() != want {
				t.Fatalf("bootstrapped state: phase %s quarantined %v keys %s, want %s", status.Owner.Phase, status.Owner.Quarantined, status.keys(), want)
			}
			t.Logf("bootstrap from %s: one TCP acquisition, keys %s, validated over UDP and TCP across a restart", alg.name, status.keys())
		})
	}
	t.Run("MinimalResponsesNo", func(t *testing.T) {
		// BIND 9.18 already answers the apex DNSKEY query without authority
		// data by default (minimal-responses no-auth-recursive). The full
		// setting does not add any either, so no BIND configuration parks the
		// bootstrap on a complete but unexpected reply.
		s := newRootSigner(t, t.TempDir(), ip)
		ksk := s.key("ECDSAP256SHA256", 0, true)
		s.sign(1, []string{ksk}, []string{ksk}, "now-3600", "now+2592000")
		root := startBINDRoot(t, s, "minimal-responses no;")
		probeAnchorWire(t, root.addr)
	})
}

// awaitAcquisitions waits until named has answered want managed
// acquisitions in total, then checks that no further one follows for a
// second.
func awaitAcquisitions(t *testing.T, root *bindRoot, want int, why string) {
	t.Helper()
	if !kudurrutest.Eventually(20*time.Second, 50*time.Millisecond, func() bool { return root.acquisitions() >= want }) {
		t.Fatalf("%s: %d managed acquisitions, want %d", why, root.acquisitions(), want)
	}
	time.Sleep(time.Second)
	if got := root.acquisitions(); got != want {
		t.Fatalf("%s: %d managed acquisitions, want exactly %d", why, got, want)
	}
}

// TestBINDManagedRootRefusals runs the bootstrap against BIND-made inputs it
// must refuse, and one it must accept while ignoring a key:
//   - WrongDS: the seed matches no published key.
//   - RevokedSeed: the seeded key is published with REVOKE set, signed by
//     itself (dnssec-revoke), beside a second key that signs the set.
//   - ExpiredSignature: the only signature over the DNSKEY set has expired.
//   - UnsupportedSeedAlgorithm: an Ed448 seed (algorithm 16) is refused by
//     configuration before any acquisition.
//   - UnsupportedKeyIgnored: an Ed448 KSK signs beside the seeded ECDSA key;
//     bootstrap succeeds and the Ed448 key never becomes a candidate.
//
// A refused bootstrap answers SERVFAIL, keeps the node unready, and its
// one complete reply ends the writer term's updater: no second acquisition.
func TestBINDManagedRootRefusals(t *testing.T) {
	kudurrutest.RequireE2E(t)
	t.Log("authority: " + namedVersion(t))
	ip := managedRootIP(t)
	refused := func(t *testing.T, s *rootSigner, seeds []string) {
		t.Helper()
		root := startBINDRoot(t, s, "")
		spec := managedRootNode(t, s.dir, "managed", root.ap, seeds)
		node := kudurrutest.Start(t, kudurruBin, spec, kudurrutest.Options{})
		awaitAcquisitions(t, root, 1, "refused bootstrap")
		requireServfail(t, node, "refused bootstrap")
		if !kudurrutest.Eventually(10*time.Second, 50*time.Millisecond, func() bool {
			return strings.Contains(node.Log(), `msg="root anchor updater stopped for this writer term" err="trustanchors: unauthenticated root observation"`)
		}) {
			t.Fatal("refused bootstrap was not reported")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if code, _, err := kudurrutest.Get(ctx, "http://"+spec.Ports.Ops+"/healthz/ready"); err != nil || code != 503 {
			t.Fatal("node without accepted trust reports ready", code, err)
		}
		awaitAcquisitions(t, root, 1, "terminal refusal")
		select {
		case <-node.Done():
			t.Fatal("refused bootstrap stopped the process")
		default:
		}
		// A restart is a new writer incarnation: it tries once more, and is
		// refused the same way.
		node.Stop(15 * time.Second)
		node = kudurrutest.Start(t, kudurruBin, spec, kudurrutest.Options{})
		awaitAcquisitions(t, root, 2, "restart after a refused bootstrap")
		requireServfail(t, node, "restart after a refused bootstrap")
	}
	t.Run("WrongDS", func(t *testing.T) {
		s := newRootSigner(t, t.TempDir(), ip)
		ksk := s.key("ECDSAP256SHA256", 0, true)
		other := s.key("ECDSAP256SHA256", 0, true) // never published
		s.sign(1, []string{ksk}, []string{ksk}, "now-3600", "now+2592000")
		refused(t, s, []string{s.ds(other)})
	})
	t.Run("RevokedSeed", func(t *testing.T) {
		s := newRootSigner(t, t.TempDir(), ip)
		seed := s.key("ECDSAP256SHA256", 0, true)
		next := s.key("ECDSAP256SHA256", 0, true)
		revoked := s.revoke(seed)
		s.sign(1, []string{revoked, next}, []string{revoked, next}, "now-3600", "now+2592000")
		refused(t, s, []string{s.ds(seed)})
	})
	t.Run("ExpiredSignature", func(t *testing.T) {
		s := newRootSigner(t, t.TempDir(), ip)
		ksk := s.key("ECDSAP256SHA256", 0, true)
		s.sign(1, []string{ksk}, []string{ksk}, "now-2592000", "now-86400")
		refused(t, s, []string{s.ds(ksk)})
	})
	t.Run("UnsupportedSeedAlgorithm", func(t *testing.T) {
		s := newRootSigner(t, t.TempDir(), ip)
		ksk := s.key("ED448", 0, true)
		s.sign(1, []string{ksk}, []string{ksk}, "now-3600", "now+2592000")
		root := startBINDRoot(t, s, "")
		spec := managedRootNode(t, s.dir, "managed", root.ap, []string{s.ds(ksk)})
		node := kudurrutest.Start(t, kudurruBin, spec, kudurrutest.Options{})
		code, err := node.Wait(30 * time.Second)
		if err != nil || code == 0 || !strings.Contains(node.Log(), "unsupported managed DS algorithm") {
			t.Fatalf("Ed448 seed was not refused by configuration: exit %d %v\n%s", code, err, node.Log())
		}
		if n := root.acquisitions(); n != 0 {
			t.Fatal("refused configuration acquired", n)
		}
	})
	t.Run("UnsupportedKeyIgnored", func(t *testing.T) {
		s := newRootSigner(t, t.TempDir(), ip)
		ksk := s.key("ECDSAP256SHA256", 0, true)
		ed448 := s.key("ED448", 0, true)
		s.sign(1, []string{ksk, ed448}, []string{ksk, ed448}, "now-3600", "now+2592000")
		root := startBINDRoot(t, s, "")
		spec := managedRootNode(t, s.dir, "managed", root.ap, []string{s.ds(ksk)})
		node := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
		requireValidated(t, node, "bootstrap beside an Ed448 key")
		awaitAcquisitions(t, root, 1, "bootstrap beside an Ed448 key")
		node.Stop(15 * time.Second)
		status := offlineStatus(t, kudurruBin, spec)
		if want := fmt.Sprintf("%d/13/Valid", keyTag(t, ksk)); status.keys() != want || status.Owner.Quarantined {
			t.Fatalf("Ed448 key entered the managed state: %s, want %s", status.keys(), want)
		}
	})
}
