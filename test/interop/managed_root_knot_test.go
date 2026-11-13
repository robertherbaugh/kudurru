package interop

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

// countingRelay forwards DNS over UDP and TCP, byte for byte, to an
// authority that keeps no query log (knotd), and counts the managed
// acquisitions it carries: the root DNSKEY question over TCP with RD clear
// and EDNS DO, as cmd/kudurru/serving_anchor_acquire.go sends it.
type countingRelay struct {
	ap           netip.AddrPort
	acquisitions atomic.Int32
}

func managedAcquisitionRequest(raw []byte) bool {
	q := new(dns.Msg)
	if q.Unpack(raw) != nil || len(q.Question) != 1 || q.RecursionDesired || q.CheckingDisabled {
		return false
	}
	opt := q.IsEdns0()
	return q.Question[0].Name == "." && q.Question[0].Qtype == dns.TypeDNSKEY && opt != nil && opt.Do() && opt.UDPSize() == 1232
}

func startCountingRelay(t *testing.T, ip netip.Addr, upstream string) *countingRelay {
	t.Helper()
	listener, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	packet, err := net.ListenPacket("udp4", listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	r := &countingRelay{ap: netip.MustParseAddrPort(listener.Addr().String())}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = listener.Close()
		_ = packet.Close()
		wg.Wait()
	})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = conn.Close() }()
				up, err := net.DialTimeout("tcp", upstream, 3*time.Second)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				for {
					_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
					_ = up.SetDeadline(time.Now().Add(10 * time.Second))
					frame, err := readFrame(conn)
					if err != nil {
						return
					}
					if managedAcquisitionRequest(frame[2:]) {
						r.acquisitions.Add(1)
					}
					if _, err := up.Write(frame); err != nil {
						return
					}
					reply, err := readFrame(up)
					if err != nil {
						return
					}
					if _, err := conn.Write(reply); err != nil {
						return
					}
				}
			}()
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			n, from, err := packet.ReadFrom(buf)
			if err != nil {
				return
			}
			query := append([]byte(nil), buf[:n]...)
			wg.Add(1)
			go func() {
				defer wg.Done()
				up, err := net.DialTimeout("udp", upstream, 3*time.Second)
				if err != nil {
					return
				}
				defer func() { _ = up.Close() }()
				_ = up.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := up.Write(query); err != nil {
					return
				}
				reply := make([]byte, 65535)
				m, err := up.Read(reply)
				if err != nil {
					return
				}
				_, _ = packet.WriteTo(reply[:m], from)
			}()
		}
	}()
	return r
}

// readFrame reads one length-prefixed DNS message, prefix included.
func readFrame(conn net.Conn) ([]byte, error) {
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, err
	}
	frame := make([]byte, 2+int(binary.BigEndian.Uint16(size[:])))
	copy(frame, size[:])
	if _, err := io.ReadFull(conn, frame[2:]); err != nil {
		return nil, err
	}
	return frame, nil
}

// knotRoot is knotd (the pinned image of knot_catalog_test.go) on the host
// network, signing the root and childZone online with keys made by keymgr.
type knotRoot struct {
	t              *testing.T
	docker, socket string
	dir, suffix    string
	server         string
	addr           string
	version        string
	rootDS         string
}

func (k *knotRoot) dockerRun(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(k.t.Context(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, k.docker, append([]string{"-H", k.socket}, args...)...).CombinedOutput() //nolint:gosec // explicit private test daemon and fixed executable
	return strings.TrimSpace(string(out)), err
}

func (k *knotRoot) keymgr(zone string, args ...string) string {
	k.t.Helper()
	out, err := k.dockerRun(append([]string{"run", "--rm", "--network", "none", "--name", "kudurru-f28-rfc-keymgr-" + k.suffix, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "-v", k.dir + ":/fixture", "--entrypoint", "keymgr", knotImage, "-c", "/fixture/knot.conf", zone}, args...)...)
	if err != nil {
		k.t.Fatalf("keymgr %s %s: %v: %s", zone, strings.Join(args, " "), err, out)
	}
	return out
}

// sha256DS is the SHA-256 DS line of keymgr's ds output, in the
// presentation form the managed seed setting takes.
func (k *knotRoot) sha256DS(zone string) string {
	k.t.Helper()
	for _, line := range strings.Split(k.keymgr(zone, "ds"), "\n") {
		if f := strings.Fields(line); len(f) == 6 && f[1] == "DS" && f[4] == "2" {
			return f[0] + " IN DS " + strings.Join(f[2:], " ")
		}
	}
	k.t.Fatal("keymgr printed no SHA-256 DS for", zone)
	return ""
}

func startKnotRoot(t *testing.T, ip netip.Addr) *knotRoot {
	t.Helper()
	kudurrutest.RequireE2E(t)
	if os.Getenv("KUDURRU_KNOT") != "1" {
		t.Skip("set KUDURRU_KNOT=1 and KUDURRU_KNOT_DOCKER_HOST for actual Knot DNS qualification")
	}
	socket := os.Getenv("KUDURRU_KNOT_DOCKER_HOST")
	if !strings.HasPrefix(socket, "unix:///") || socket == "unix:///var/run/docker.sock" {
		t.Fatal("KUDURRU_KNOT_DOCKER_HOST must explicitly select a private Unix Docker socket")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal("Knot qualification requires the Docker CLI")
	}
	k := &knotRoot{t: t, docker: docker, socket: socket, dir: t.TempDir(), suffix: strings.ToLower(rand.Text()[:10])}
	k.server = "kudurru-f28-rfc-knot-" + k.suffix
	t.Cleanup(func() { _, _ = k.dockerRun("rm", "-f", k.server) })
	if k.version, err = k.dockerRun("run", "--rm", "--network", "none", "--entrypoint", "knotd", knotImage, "-V"); err != nil || k.version != knotVersion {
		t.Fatalf("pinned Knot executable: %q %v", k.version, err)
	}
	for _, sub := range []string{"db", "run"} {
		if err := os.MkdirAll(filepath.Join(k.dir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	port := kudurrutest.FreePort(t)
	k.addr = netip.AddrPortFrom(ip, uint16(port)).String() //nolint:gosec // FreePort returns a valid port
	writeFile(t, filepath.Join(k.dir, "knot.conf"), fmt.Sprintf(`server:
  listen: %[1]s@%[2]d
  rundir: /fixture/run
  udp-workers: 1
  tcp-workers: 1
  background-workers: 1
database:
  storage: /fixture/db
log:
  - target: stdout
    any: info
policy:
  - id: manual
    manual: on
    algorithm: ecdsap256sha256
    rrsig-lifetime: 30d
    rrsig-refresh: 7d
    dnskey-ttl: %[3]d
    zone-max-ttl: 86400
template:
  - id: default
    storage: /fixture
    dnssec-signing: on
    dnssec-policy: manual
zone:
  - domain: .
    file: root.zone
  - domain: %[4]s
    file: child.zone
`, ip, port, managedRootTTL, childZone))
	// Keys first, so that the root can carry the child's DS before knotd
	// signs either zone.
	for _, zone := range []string{".", childZone} {
		k.keymgr(zone, "generate", "algorithm=13", "ksk=yes", "zsk=no")
		k.keymgr(zone, "generate", "algorithm=13", "ksk=no", "zsk=yes")
	}
	writeFile(t, filepath.Join(k.dir, "child.zone"), fmt.Sprintf("$ORIGIN %[1]s\n$TTL 300\n@ IN SOA ns.%[1]s hostmaster.%[1]s 1 1800 900 604800 300\n@ IN NS ns\nns IN A %[2]s\nwww IN A %[3]s\n", childZone, ip, childAddr))
	writeFile(t, filepath.Join(k.dir, "root.zone"), fmt.Sprintf("$ORIGIN .\n$TTL %[1]d\n@ IN SOA a.root-servers.lab. hostmaster.lab. 1 1800 900 604800 86400\n@ IN NS a.root-servers.lab.\na.root-servers.lab. IN A %[2]s\n%[3]s IN NS ns.%[3]s\nns.%[3]s IN A %[2]s\n%[4]s\n", managedRootTTL, ip, childZone, k.sha256DS(childZone)))
	// keymgr cannot open the key database while knotd runs; read the
	// operator's root DS now.
	k.rootDS = k.sha256DS(".")
	if out, err := k.dockerRun("run", "-d", "--network", "host", "--name", k.server, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "128", "-v", k.dir+":/fixture", "--entrypoint", "knotd", knotImage, "-c", "/fixture/knot.conf"); err != nil {
		t.Fatalf("start knotd: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := k.dockerRun("logs", k.server)
			t.Logf("knotd log:\n%s", logs)
		}
	})
	if !kudurrutest.Eventually(30*time.Second, 100*time.Millisecond, func() bool {
		serial, rcode := soaSerialTCP(k.addr, childZone)
		return rcode == dns.RcodeSuccess && serial > 0
	}) {
		logs, _ := k.dockerRun("logs", k.server)
		t.Fatalf("knotd did not answer on %s:\n%s", k.addr, logs)
	}
	return k
}

// TestKnotManagedRootBootstrap bootstraps managed root anchors from a root
// that Knot DNS signs online (keys made with keymgr) over exactly one
// production TCP acquisition, answers AD-validated recursion through the
// Knot-signed child, and makes no new acquisition across a restart. A
// byte-for-byte relay in front of knotd counts the acquisitions.
func TestKnotManagedRootBootstrap(t *testing.T) {
	ip := managedRootIP(t)
	k := startKnotRoot(t, ip)
	t.Log("authority: " + k.version + "; " + knotImage)
	probe := probeAnchorWire(t, k.addr)
	if len(probe.Answer) != 3 {
		t.Fatal("expected KSK, ZSK and one KSK signature", probe)
	}
	relay := startCountingRelay(t, ip, k.addr)
	seed := k.rootDS
	dir := t.TempDir()
	spec := managedRootNode(t, dir, "managed", relay.ap, []string{seed})
	node := kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
	requireValidated(t, node, "after bootstrap")
	if n := relay.acquisitions.Load(); n != 1 {
		t.Fatal("bootstrap made other than one managed acquisition", n)
	}
	node.Stop(15 * time.Second)
	node = kudurrutest.StartReady(t, kudurruBin, spec, kudurrutest.Options{})
	requireValidated(t, node, "after restart")
	time.Sleep(time.Second)
	if n := relay.acquisitions.Load(); n != 1 {
		t.Fatal("a restart before the refresh is due acquired again", n)
	}
	node.Stop(15 * time.Second)
	status := offlineStatus(t, kudurruBin, spec)
	rr, err := dns.NewRR(seed)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d/13/Valid", rr.(*dns.DS).KeyTag); status.Owner.Phase != "managed" || status.Owner.Quarantined || status.keys() != want {
		t.Fatalf("bootstrapped state: phase %s quarantined %v keys %s, want %s", status.Owner.Phase, status.Owner.Quarantined, status.keys(), want)
	}
	t.Logf("bootstrap from Knot: one TCP acquisition, keys %s, validated over UDP and TCP across a restart", status.keys())
}
