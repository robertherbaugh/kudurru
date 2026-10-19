package zoneview

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type stubClock struct{ ns atomic.Int64 }

func (c *stubClock) now() time.Time  { return time.Unix(0, c.ns.Load()).UTC() }
func (c *stubClock) set(t time.Time) { c.ns.Store(t.UnixNano()) }

// stubRoot is a positive-control terminal. Any covered name reaching it is a
// routing bug, independent of how a resolver would answer that leaked query.
type stubRoot struct {
	calls   atomic.Int32
	covered atomic.Int32
}

func (*stubRoot) Name() string { return "upstream" }
func (r *stubRoot) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	r.calls.Add(1)
	q := req.Msg.Question[0]
	if dns.IsSubDomain("stub.test.", q.Name) {
		r.covered.Add(1)
	}
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(203, 0, 113, 250)}}
	return &pipeline.Response{Msg: m, Origin: pipeline.OriginForwarded}, nil
}

type stubRuntime struct {
	manager      *stub.Manager
	source       Source
	server       *dnsserver.Server
	stop         func()
	startManager func()
}

func startStubRuntime(t *testing.T, st *memory.Store, path string, c *stubClock, root *stubRoot) *stubRuntime {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	disk, err := localdb.OpenSnapshotStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := zonecache.New(st, zonecache.Options{Debounce: time.Millisecond, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Register(Builder(logger)); err != nil {
		t.Fatal(err)
	}
	client := &stub.Client{Timeout: 200 * time.Millisecond, QueryTimeout: 200 * time.Millisecond, Now: c.now}
	manager, err := stub.New(stub.Options{Cache: cache, Blobs: disk, ClusterID: "stub-interop", Discoverer: client, Now: c.now, PollInterval: 10 * time.Millisecond, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	source := Source{Cache: cache, Stub: manager}
	stage := authoritative.New(source)
	stage.Now = c.now
	stage.StubQuery = client.Exchange
	server := dnsserver.New("127.0.0.1:0", []string{"udp", "tcp"}, pipeline.MustNew(&ingress.Stage{}, stage, root), logger)
	var jobs []<-chan error
	launch := func(f func(context.Context) error) {
		done := make(chan error, 1)
		jobs = append(jobs, done)
		go func() { done <- f(ctx) }()
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			for _, done := range jobs {
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					t.Error("stub runtime did not stop")
				}
			}
			if err := disk.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	launch(cache.Start)
	launch(server.Start)
	eventuallyStub(t, func() bool { return source.Zones().Gate() == zone.GateOpen && len(server.Addrs()) == 2 })
	var startOnce sync.Once
	return &stubRuntime{manager: manager, source: source, server: server, stop: stop, startManager: func() { startOnce.Do(func() { launch(manager.Run) }) }}
}
func eventuallyStub(t *testing.T, f func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stub readiness/state timeout")
}
func stubQuery(t *testing.T, r *stubRuntime, proto, name string, typ uint16, rd bool) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, typ)
	m.RecursionDesired = rd
	out, _, err := (&dns.Client{Net: proto, Timeout: time.Second}).ExchangeContext(t.Context(), m, r.server.Addrs()[proto])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Native BIND interop lives in this owned package so the accepted stub slice
// can exercise the full production path without changing shared harness files.
func TestBINDStubThroughListenersAndDurableOfflineReferral(t *testing.T) {
	for _, transport := range []string{"udp", "tcp"} {
		t.Run(transport, func(t *testing.T) {
			addr, stopPrimary := bindStubPrimary(t)
			st := memory.New()
			t.Cleanup(func() { _ = st.Close() })
			dz := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "stub", UID: "stable-stub"}, Spec: v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypeStub, ZoneName: "stub.test.", Stub: &v1alpha1.StubSpec{Nameservers: []string{transport + "://" + addr}}}}
			for _, obj := range []store.Object{dz, &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypeForward, ZoneName: "."}}} {
				if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Get(t.Context(), store.Key{Kind: "DNSZone", Name: dz.Name}, dz); err != nil {
				t.Fatal(err)
			}
			c := new(stubClock)
			c.set(time.Now())
			root := new(stubRoot)
			path := filepath.Join(t.TempDir(), "local.db")
			first := startStubRuntime(t, st, path, c, root)
			for _, proto := range []string{"udp", "tcp"} {
				control := stubQuery(t, first, proto, "outside.test.", dns.TypeA, true)
				if control.Rcode != dns.RcodeSuccess || len(control.Answer) != 1 {
					t.Fatal("root positive control")
				}
				unavailable := stubQuery(t, first, proto, "www.stub.test.", dns.TypeA, true)
				if unavailable.Rcode != dns.RcodeServerFailure {
					t.Fatal("unavailable stub forwarded")
				}
			}
			first.startManager()
			var initial stub.Status
			eventuallyStub(t, func() bool { initial, _ = first.manager.StatusFor(dz); return initial.Available })
			if initial.Serial != 4294967295 {
				t.Fatalf("external serial: %+v", initial)
			}
			captured := first.source.Zones()
			for _, proto := range []string{"udp", "tcp"} {
				answer := stubQuery(t, first, proto, "www.stub.test.", dns.TypeA, true)
				if answer.Rcode != dns.RcodeSuccess || answer.Authoritative || answer.AuthenticatedData || len(answer.Answer) != 1 || answer.Answer[0].(*dns.A).A.String() != "192.0.2.80" || answer.Answer[0].Header().Ttl != 117 {
					t.Fatalf("forwarded answer %s: %s", proto, answer)
				}
				referral := stubQuery(t, first, proto, "www.stub.test.", dns.TypeA, false)
				if referral.Authoritative || len(referral.Answer) != 0 || len(referral.Ns) != 1 || len(referral.Extra) != 1 {
					t.Fatalf("local RD0 referral: %s", referral)
				}
				child := stubQuery(t, first, proto, "www.deleg.stub.test.", dns.TypeA, true)
				if child.Rcode != dns.RcodeSuccess || len(child.Answer) != 0 || len(child.Ns) != 1 || len(child.Extra) != 1 || child.Ns[0].(*dns.NS).Hdr.Name != "deleg.stub.test." {
					t.Fatalf("child referral was chased/lost: %s", child)
				}
			}
			if root.covered.Load() != 0 {
				t.Fatal("stub escaped to root forwarder")
			}
			first.stop()
			stopPrimary()
			c.set(initial.ConfirmedAt.Add(5 * time.Second))
			second := startStubRuntime(t, st, path, c, root)
			second.startManager()
			var restored stub.Status
			eventuallyStub(t, func() bool {
				restored, _ = second.manager.StatusFor(dz)
				return restored.Available && restored.Error != ""
			})
			if restored.ConfirmedAt != initial.ConfirmedAt || restored.ExpiresAt != initial.ExpiresAt {
				t.Fatal("offline restart renewed validity")
			}
			for _, proto := range []string{"udp", "tcp"} {
				referral := stubQuery(t, second, proto, "www.stub.test.", dns.TypeA, false)
				if referral.Rcode != dns.RcodeSuccess || len(referral.Ns) != 1 || referral.Ns[0].Header().Ttl != 15 {
					t.Fatalf("durable aged referral: %s", referral)
				}
				unavailable := stubQuery(t, second, proto, "www.stub.test.", dns.TypeA, true)
				if unavailable.Rcode != dns.RcodeServerFailure {
					t.Fatal("offline RD1 leaked")
				}
			}
			c.set(initial.ExpiresAt)
			for _, proto := range []string{"udp", "tcp"} {
				for _, rd := range []bool{false, true} {
					expired := stubQuery(t, second, proto, "www.stub.test.", dns.TypeA, rd)
					if expired.Rcode != dns.RcodeServerFailure {
						t.Fatal("expired route served")
					}
				}
			}
			if root.covered.Load() != 0 {
				t.Fatal("expired/offline stub escaped to root")
			}
			if captured.Match("www.stub.test.").StubServers()[0].Address.String() != addr {
				t.Fatal("captured view mutated")
			}
			// A changed spec immediately installs an unavailable placeholder; old
			// runtime data may not revive the replacement configuration.
			dz.Spec.Stub.Nameservers = []string{"tls://" + addr}
			if _, err := st.Apply(t.Context(), dz, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				t.Fatal(err)
			}
			eventuallyStub(t, func() bool {
				return second.source.Zones().Match("www.stub.test.").StubIdentity() == zone.StubIdentity(dz)
			})
			if second.source.Zones().Match("www.stub.test.").RoutingAvailableAt(c.now()) {
				t.Fatal("old route revived new config")
			}
			t.Logf("BIND %s discovery -> durable Manager -> zoneview.Source -> ingress/authoritative -> UDP/TCP: answer, RD0 referral, unchanged child referral, offline restore, expiry and zero root fallthrough", transport)
		})
	}
}

func bindStubPrimary(t *testing.T) (string, func()) {
	t.Helper()
	bin, err := exec.LookPath("named")
	if err != nil {
		t.Skip("BIND named unavailable")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	dir := t.TempDir()
	conf := fmt.Sprintf(`options { directory %q; listen-on port %s { 127.0.0.1; }; listen-on-v6 { none; }; recursion no; dnssec-validation no; notify no; pid-file "named.pid"; session-keyfile "session.key"; }; controls {}; zone "stub.test" { type primary; file "primary.db"; };`, dir, port)
	zoneFile := `$ORIGIN stub.test.
$TTL 300
@ 300 IN SOA ns.stub.test. admin.stub.test. 4294967295 10 3 30 10
@ 25 IN NS ns.stub.test.
ns 20 IN A 127.0.0.1
www 117 IN A 192.0.2.80
deleg 60 IN NS ns.deleg.stub.test.
ns.deleg 60 IN A 192.0.2.44
`
	for name, content := range map[string]string{"named.conf": conf, "primary.db": zoneFile} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	log, err := os.Create(filepath.Join(dir, "named.log")) //nolint:gosec // G304: private test directory.
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-g", "-4", "-n", "1", "-c", filepath.Join(dir, "named.conf")) //nolint:gosec // Native test executable and private generated configuration.
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("named shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				if err := <-done; err == nil {
					t.Error("killed named exited successfully")
				}
			}
			_ = log.Close()
			if t.Failed() {
				b, _ := os.ReadFile(filepath.Join(dir, "named.log")) //nolint:gosec // G304: private test directory.
				t.Log(string(b))
			}
		})
	}
	t.Cleanup(stop)
	eventuallyStub(t, func() bool {
		m := new(dns.Msg)
		m.SetQuestion("stub.test.", dns.TypeSOA)
		r, _, err := (&dns.Client{Timeout: 100 * time.Millisecond}).ExchangeContext(t.Context(), m, addr)
		return err == nil && r.Rcode == dns.RcodeSuccess && r.Authoritative
	})
	return addr, stop
}
