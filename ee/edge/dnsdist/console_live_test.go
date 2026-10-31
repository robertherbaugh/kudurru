package dnsdist

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
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

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
)

const consoleLabImage = "sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e"

func certificateForTest(t *testing.T, serial int64) (cert, key []byte) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})
}

type labBackend struct {
	endpoint        cluster.Endpoint
	wrong           atomic.Bool
	checks, proxied atomic.Int64
}

func startLabBackend(t *testing.T, name string, answer byte, secret []byte) *labBackend {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	policy, err := proxyproto.NewPolicy([]string{"127.0.0.1"}, secret)
	if err != nil {
		t.Fatal(err)
	}
	backend := &labBackend{endpoint: cluster.Endpoint{Name: name, Address: tcp.Addr().String(), Ready: true, Serving: true}}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		if len(q.Question) == 1 {
			question := q.Question[0]
			switch question.Qtype {
			case dns.TypeTXT:
				backend.checks.Add(1)
				node := name
				if backend.wrong.Load() {
					node = "wrong-node"
				}
				m.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{node + " 1"}}}
			case dns.TypeA:
				if _, ok := w.RemoteAddr().(*proxyproto.Addr); ok {
					backend.proxied.Add(1)
				}
				m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(203, 0, 113, answer)}}
			}
		}
		_ = w.WriteMsg(m)
	})
	tcpServer := &dns.Server{Listener: &proxyproto.Listener{Listener: tcp, Policy: policy}, Handler: handler}
	udpServer := &dns.Server{PacketConn: &proxyproto.PacketConn{PacketConn: udp, Policy: policy}, Handler: handler}
	go func() { _ = tcpServer.ActivateAndServe() }()
	go func() { _ = udpServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = tcpServer.Shutdown(); _ = udpServer.Shutdown() })
	return backend
}

func labWait(t *testing.T, label string, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for !predicate() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s (private child diagnostics retained only inside owned fixture)", label)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func labPID(t *testing.T, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "--host", "unix:///tmp/kudurru-lab-docker.sock", "inspect", "--format", "{{.State.Pid}}", name).Output() //nolint:gosec // fixed Docker endpoint/command, generated owned resource name
	if err != nil {
		return ""
	}
	pid := strings.TrimSpace(string(out))
	if pid == "0" {
		return ""
	}
	return pid
}

func labDNS(t *testing.T, addr string, conn *dns.Conn, want byte) {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(fmt.Sprintf("%d.lab.invalid.", time.Now().UnixNano()), dns.TypeA)
	var reply *dns.Msg
	var err error
	if conn == nil {
		client := &dns.Client{Timeout: 2 * time.Second}
		reply, _, err = client.Exchange(q, addr)
	} else {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		err = conn.WriteMsg(q)
		if err == nil {
			reply, err = conn.ReadMsg()
		}
	}
	if err != nil || reply == nil || reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 1 {
		t.Fatal("DNS acceptance failed (transport or response)")
	}
	a, ok := reply.Answer[0].(*dns.A)
	if !ok || !a.A.Equal(net.IPv4(203, 0, 113, want)) {
		t.Fatal("DNS backend attribution mismatch")
	}
}

func labTLS(t *testing.T, addr string, serial int64) *dns.Conn {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}) //nolint:gosec // disposable fixture, exact leaf serial asserted below
	if err != nil {
		t.Fatal("TLS fixture handshake failed")
	}
	if conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64() != serial {
		_ = conn.Close()
		t.Fatal("TLS leaf mismatch")
	}
	return &dns.Conn{Conn: conn}
}

// TestConsoleLiveSupervisor opts into the actual pinned external executable.
// No broad repository build, shared daemon state or upstream code is required.
func TestConsoleLiveSupervisor(t *testing.T) {
	if os.Getenv("KUDURRU_F43_LIVE") != "1" {
		t.Skip("set KUDURRU_F43_LIVE=1 for owned dnsdist Docker acceptance")
	}
	parent := os.Getenv("KUDURRU_F43_WORKDIR")
	if parent == "" {
		t.Fatal("KUDURRU_F43_WORKDIR must name an owned scratch parent")
	}
	dir, err := os.MkdirTemp(parent, "supervisor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			if data, err := os.ReadFile(filepath.Join(dir, "console-evidence.jsonl")); err == nil { //nolint:gosec // fresh owned fixture; structured sanitized evidence only
				t.Logf("Sanitized console evidence: %s", data)
			}
		}
		_ = os.RemoveAll(dir) //nolint:gosec // remove only the fresh owned fixture directory
	})
	t.Setenv("TMPDIR", dir)
	name := "kudurru-f43-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	fixture, _ := json.Marshal(map[string]string{"name": name, "image": consoleLabImage})
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), fixture, 0o600); err != nil { //nolint:gosec // fresh directory under explicitly configured test scratch parent
		t.Fatal(err)
	}
	wrapper, err := os.ReadFile("testdata/console-lab/client.py")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "dnsdist-client")
	if err := os.WriteFile(binary, wrapper, 0o700); err != nil { //nolint:gosec // private fixture executable must be executable
		t.Fatal(err)
	}
	// Cleanup only this fixture's named resource, including an interrupted child.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "--host", "unix:///tmp/kudurru-lab-docker.sock", "rm", "--force", name).Run() //nolint:gosec // generated owned container only, fixed endpoint and command
	})
	credentials, err := edge.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(credentials.Password)
	a := startLabBackend(t, "node-a", 1, secret)
	b := startLabBackend(t, "node-b", 2, secret)
	wrong := startLabBackend(t, "node-c", 3, secret)
	wrong.wrong.Store(true)
	src := &chanSource{initial: []cluster.Endpoint{a.endpoint}, ch: make(chan []cluster.Endpoint, 4)}
	source := filepath.Join(dir, "source")
	if err := os.Mkdir(source, 0o700); err != nil { //nolint:gosec // fresh owned fixture directory
		t.Fatal(err)
	}
	writePair := func(serial int64) {
		cert, key := certificateForTest(t, serial)
		if err := edge.WriteFileAtomic(filepath.Join(source, "tls.crt"), cert, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := edge.WriteFileAtomic(filepath.Join(source, "tls.key"), key, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writePair(1)
	ports := map[edge.Protocol]int{}
	for _, protocol := range edge.AllProtocols {
		_, port, _ := net.SplitHostPort(freePort(t))
		ports[protocol], _ = strconv.Atoi(port)
	}
	var renderer Renderer = YAMLRenderer{}
	if os.Getenv("KUDURRU_F43_RENDERER") == "lua" {
		renderer = LuaRenderer{}
	}
	sup, err := New(Config{Renderer: renderer, Options: edge.Options{ServerID: "f43-live", PoolResolver: "firstAvailable", Listen: []string{"127.0.0.1"}, Ports: ports, API: freePort(t), Console: freePort(t), CertFile: filepath.Join(source, "tls.crt"), KeyFile: filepath.Join(source, "tls.key"), ProxyProtocolSecret: string(secret)}, Source: src, ConfigDir: filepath.Join(dir, "config"), Binary: binary, ChildEnv: []string{"KUDURRU_F43_LAB=" + dir}, TLSDir: source, Debounce: 100 * time.Millisecond, StatusPoll: 100 * time.Millisecond, HealthTimeout: 5 * time.Second, StopTimeout: 10 * time.Second, ConsoleTimeout: 3 * time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), NewCredentials: func() (edge.Credentials, error) { return credentials, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("supervisor: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("supervisor shutdown timed out")
		}
	})
	ready := func() bool { ok, _ := sup.Ready(); return ok }
	labWait(t, "initial automatic health and readiness", ready)
	pid := labPID(t, name)
	if pid == "" {
		t.Fatal("missing child PID")
	}
	dnsAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[edge.Do53]))
	tlsAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[edge.DoT]))
	labDNS(t, dnsAddr, nil, 1)
	if err := sup.console(t.Context(), `getServer(`+luaString(edge.BackendID(a.endpoint.Name))+`):setHealthCheckResponseValidator(function(dr) return false end)`); err == nil {
		t.Fatal("pinned runtime validator capability unexpectedly changed")
	}
	t.Log("PASS pinned runtime validator setter refuses mutation; named staging preserves original YAML validator")
	held := labTLS(t, tlsAddr, 1)
	defer func() { _ = held.Close() }()
	labDNS(t, "", held, 1)
	if a.proxied.Load() == 0 || a.checks.Load() == 0 {
		t.Fatal("original PROXY and health contracts not observed")
	}
	src.ch <- []cluster.Endpoint{a.endpoint, b.endpoint}
	labWait(t, "named validator staging and positive health", func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return len(sup.endpoints) == 2 && sup.status != nil && sup.status.UpCount() == 2 && b.checks.Load() >= 2
	})
	if labPID(t, name) == pid {
		t.Fatal("named addition must install validator in a staged restart")
	}
	pid = labPID(t, name)
	// Removal of a qualified named member uses only supported console methods.
	src.ch <- []cluster.Endpoint{b.endpoint}
	labWait(t, "console membership convergence", func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return len(sup.endpoints) == 1 && sup.endpoints[0].Name == "node-b" && sup.status != nil && sup.status.UpCount() == 1 && len(sup.status.Servers) == 1
	})
	labDNS(t, dnsAddr, nil, 2)
	fresh := labTLS(t, tlsAddr, 1)
	labDNS(t, "", fresh, 2)
	_ = fresh.Close()
	if labPID(t, name) != pid {
		t.Fatal("successful membership update restarted child")
	}
	if b.checks.Load() < 2 || b.proxied.Load() == 0 {
		t.Fatal("new member skipped automatic health or PROXY")
	}
	t.Log("PASS generated membership: named addition staged restart, original automatic node health/PROXYv2, removal fresh UDP/DoT with same PID")
	// Retain this connection on the accepted backend through certificate changes.
	heldCert := labTLS(t, tlsAddr, 1)
	defer func() { _ = heldCert.Close() }()
	labDNS(t, "", heldCert, 2)
	writePair(2)
	labWait(t, "all frontend certificate reload", func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return sup.updateError == "" && !sup.restarting && sup.status != nil && sup.status.UpCount() == 1 && func() bool {
			p, _ := tls.DialWithDialer(&net.Dialer{Timeout: 100 * time.Millisecond}, "tcp", tlsAddr, &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}) //nolint:gosec // disposable fixture observes leaf serial, no application data
			if p == nil {
				return false
			}
			defer func() { _ = p.Close() }()
			return p.ConnectionState().PeerCertificates[0].SerialNumber.Int64() == 2
		}()
	}) //nolint:gosec // fixture observes leaf serial only
	labDNS(t, "", heldCert, 2)
	if labPID(t, name) != pid {
		t.Fatal("successful certificate reload restarted child")
	}
	t.Log("PASS generated certificate reload: all enabled TLS frontends verified, held DoT preserved, same PID")
	mismatchCert, _ := certificateForTest(t, 3)
	if err := edge.WriteFileAtomic(filepath.Join(source, "tls.crt"), mismatchCert, 0o600); err != nil {
		t.Fatal(err)
	}
	labWait(t, "half-rotated mismatched pair rejected", func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return strings.Contains(sup.updateError, "certificate update rejected")
	})
	fresh = labTLS(t, tlsAddr, 2)
	labDNS(t, "", fresh, 2)
	_ = fresh.Close()
	labDNS(t, "", heldCert, 2)
	if labPID(t, name) != pid {
		t.Fatal("mismatched source disturbed child")
	}
	t.Log("PASS half-rotated cert/key mismatch retained accepted leaf and held connection")
	// Mismatched/invalid external source is rejected before any console mutation;
	// accepted private material remains the restart input.
	if err := edge.WriteFileAtomic(filepath.Join(source, "tls.key"), []byte("invalid source"), 0o600); err != nil {
		t.Fatal(err)
	}
	labWait(t, "invalid certificate visible", func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return strings.Contains(sup.updateError, "certificate update rejected")
	})
	fresh = labTLS(t, tlsAddr, 2)
	labDNS(t, "", fresh, 2)
	_ = fresh.Close()
	_ = sup.Signal(syscall.SIGUSR1)
	labWait(t, "restart with accepted certificate", func() bool { next := labPID(t, name); return next != "" && next != pid && ready() })
	pid = labPID(t, name)
	fresh = labTLS(t, tlsAddr, 2)
	labDNS(t, "", fresh, 2)
	_ = fresh.Close()
	t.Log("PASS invalid mounted certificate retained accepted context and child restart material")
	// Wrong-node health is rejected; old restart target survives staging.
	src.ch <- []cluster.Endpoint{wrong.endpoint}
	labWait(t, "wrong-node health rejected", func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return strings.Contains(sup.updateError, "membership console update failed") && sup.status != nil && sup.status.UpCount() == 1
	})
	if wrong.checks.Load() < 2 {
		t.Fatal("wrong-node validator was not exercised")
	}
	labDNS(t, dnsAddr, nil, 2)
	config, _ := os.ReadFile(sup.MainPath())
	if strings.Contains(string(config), "name: node-c") {
		t.Fatal("unqualified member became restart input")
	}
	// Coalesce the rejected candidate away, then inject loss after actual rmServer.
	src.ch <- []cluster.Endpoint{b.endpoint}
	labWait(t, "accepted membership stable", ready)
	if err := os.WriteFile(filepath.Join(dir, "lose-ack-remove"), nil, 0o600); err != nil { //nolint:gosec // owned fault-injection file
		t.Fatal(err)
	}
	beforeRecovery := counterForSupervisor(t, sup, "membership")
	src.ch <- []cluster.Endpoint{a.endpoint}
	labWait(t, "partial mutation restart convergence", func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return !sup.restarting && counterForSupervisor(t, sup, "membership") > beforeRecovery && len(sup.endpoints) == 1 && sup.endpoints[0].Name == "node-a" && sup.status != nil && len(sup.status.Servers) == 1 && sup.status.UpCount() == 1
	})
	labDNS(t, dnsAddr, nil, 1)
	t.Log("PASS wrong-node rejection and lost-ack-after-removal recovery to validated desired configuration")
	pid = labPID(t, name)
	_ = sup.Signal(syscall.SIGUSR1)
	labWait(t, "persisted desired child restart", func() bool { next := labPID(t, name); return next != "" && next != pid && ready() })
	labDNS(t, dnsAddr, nil, 1)
	pid = labPID(t, name)
	anonymous := b.endpoint
	anonymous.Name = ""
	src.ch <- []cluster.Endpoint{a.endpoint, anonymous}
	labWait(t, "anonymous original-contract console addition", func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return len(sup.endpoints) == 2 && sup.status != nil && len(sup.status.Servers) == 2 && sup.status.UpCount() == 2
	})
	if labPID(t, name) != pid {
		t.Fatal("anonymous original-contract addition restarted child")
	}
	src.ch <- []cluster.Endpoint{anonymous}
	labWait(t, "anonymous console replacement", func() bool {
		sup.mu.Lock()
		defer sup.mu.Unlock()
		return len(sup.endpoints) == 1 && sup.endpoints[0].Name == "" && sup.status != nil && len(sup.status.Servers) == 1 && sup.status.UpCount() == 1
	})
	labDNS(t, dnsAddr, nil, 2)
	if labPID(t, name) != pid {
		t.Fatal("supported console replacement restarted child")
	}
	t.Log("PASS original anonymous static health contract: newServer, automatic health, promotion/down/purge/rmServer, same PID")
}

func counterForSupervisor(t *testing.T, s *Supervisor, reason string) float64 {
	t.Helper()
	metrics, err := s.gatherer.Gather()
	if err != nil {
		t.Fatal("cannot gather supervisor evidence")
	}
	for _, family := range metrics {
		if family.GetName() == "kudurru_edge_dnsdist_restarts_total" {
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetValue() == reason {
						return metric.GetCounter().GetValue()
					}
				}
			}
		}
	}
	return 0
}
