package interop

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"github.com/robertherbaugh/kudurru/internal/zoneview"
)

type secondaryServingClock struct{ ns atomic.Int64 }

func (c *secondaryServingClock) now() time.Time    { return time.Unix(0, c.ns.Load()).UTC() }
func (c *secondaryServingClock) set(now time.Time) { c.ns.Store(now.UnixNano()) }

// TestBINDSecondaryThroughListeners exercises the production runtime chain,
// including actual durable data and real UDP/TCP query sockets. Configuration
// uses the memory Store seam; zone contents always come from the BIND process.
func TestBINDSecondaryThroughListeners(t *testing.T) {
	kudurrutest.RequireE2E(t)
	bin := namedBinary(t)
	const origin = "secondary-serving.test."
	const key = "secondary-serving-key.test."
	const serial = uint32(4294967294)
	secret := newSecret(t)
	ring := secondaryKeyring(t, key, "hmac-sha256", secret)
	dir := t.TempDir()
	primaryPort := kudurrutest.FreePort(t)
	writeFile(t, filepath.Join(dir, "primary.db"), fmt.Sprintf(`$ORIGIN %s
$TTL 300
@ 537 IN SOA ns1.%s hostmaster.%s %d 61 13 120 47
@ 721 IN NS ns1.%s
ns1 87 IN A 127.0.0.1
www 117 IN A 192.0.2.80
`, origin, origin, origin, serial, origin))
	conf := namedOptions(dir, primaryPort) + fmt.Sprintf(`
key %q { algorithm hmac-sha256; secret %q; };
zone %q {
	type primary;
	file "primary.db";
	allow-query { key %q; };
	allow-transfer { key %q; };
};
`, key, secret, origin, key, key)
	primary := startNamed(t, bin, dir, conf, primaryPort)
	root, forwarded := secondaryRootForwarder(t, origin)
	config := memory.New()
	t.Cleanup(func() { _ = config.Close() })
	dz := secondaryObject(origin, key)
	dz.ObjectMeta = metav1.ObjectMeta{Name: "secondary-serving", UID: "stable-secondary-serving"}
	dz.Spec.Secondary.Primaries = []string{primary.addr}
	dz.Status.Serial = 17 // Legacy object serial must not become the served serial.
	for _, object := range []store.Object{dz, &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "secondary-test-root"},
		Spec:       v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypeForward, ZoneName: "."},
	}} {
		if _, err := config.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.Get(t.Context(), store.Key{Kind: "DNSZone", Name: dz.Name}, dz); err != nil {
		t.Fatal(err)
	}
	clock := new(secondaryServingClock)
	clock.set(time.Now().UTC())
	dbPath := filepath.Join(t.TempDir(), localdb.FileName)
	first := startSecondaryServingRuntime(t, config, dbPath, ring, clock, root)

	// The cache is live and a working root forwarder exists, but no transfer
	// has run. Secondary coverage must terminate queries with SERVFAIL.
	assertSecondaryRootWorks(t, first)
	assertSecondaryUnavailable(t, first, origin)
	if n := forwarded.Load(); n != 0 {
		t.Fatalf("unavailable secondary leaked %d queries to the root forwarder", n)
	}
	first.startManager()
	initial := awaitSecondaryServingStatus(t, first.manager, dz, func(st secondary.Status) bool { return st.Available })
	if initial.Serial != serial || !initial.ExpiresAt.Equal(initial.ConfirmedAt.Add(120*time.Second)) {
		t.Fatalf("wrong primary serial or expiry: %+v", initial)
	}
	assertSecondaryAnswer(t, first, origin, serial)
	t.Logf("BIND -> TransferClient -> Manager -> zoneview.Source -> ingress/authoritative -> UDP/TCP: external serial=%d, A=192.0.2.80 TTL=117", initial.Serial)

	// Stop the entire receiver runtime and close local.db, then stop BIND.
	// Reopening the same database proves recovery is from durable records,
	// not a reused manager/snapshot or an available primary.
	first.stop()
	if err := primary.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-primary.done:
	case <-time.After(5 * time.Second):
		t.Fatal("BIND did not stop")
	}
	clock.set(initial.ConfirmedAt.Add(30 * time.Second))
	second := startSecondaryServingRuntime(t, config, dbPath, ring, clock, root)
	second.startManager()
	restored := awaitSecondaryServingStatus(t, second.manager, dz, func(st secondary.Status) bool {
		return st.Available && st.Error != "" // The real offline SOA poll failed.
	})
	if restored.Serial != serial || !restored.ConfirmedAt.Equal(initial.ConfirmedAt) || !restored.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatalf("offline restart changed serial or renewed expiry: initial=%+v restored=%+v", initial, restored)
	}
	assertSecondaryAnswer(t, second, origin, serial)
	t.Log("offline restart reopened local.db and served the persisted unexpired zone on UDP/TCP without renewing confirmation or expiry")

	// The serving stage checks expiry on every query, independently of the
	// scheduler. Exactly at expiry, old positive and nonexistent names both
	// produce SERVFAIL; an unrelated root-forwarded name still works.
	clock.set(initial.ExpiresAt)
	assertSecondaryUnavailable(t, second, origin)
	assertSecondaryRootWorks(t, second)
	if n := forwarded.Load(); n != 0 {
		t.Fatalf("secondary queries leaked %d times to the root forwarder", n)
	}
	expired, ok := second.manager.StatusFor(dz)
	if !ok || expired.Available || !expired.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatalf("expired diagnostics disagree with listener behavior: %+v", expired)
	}
	t.Log("unavailable and expired coverage returned SERVFAIL on UDP/TCP with zero root forwarding; outside names still resolved")
}

type secondaryServingRuntime struct {
	manager      *secondary.Manager
	notify       *secondary.NotifyReceiver
	addrs        map[string]string
	startManager func()
	stop         func()
}

func startSecondaryServingRuntime(t *testing.T, config *memory.Store, dbPath string, ring dnsserver.TSIGKeyring, clock *secondaryServingClock, root string, accepted ...func(string)) *secondaryServingRuntime {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	logger := slog.New(slog.DiscardHandler)
	db, err := localdb.OpenSnapshotStore(ctx, dbPath)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var jobs []<-chan error
	launch := func(run func(context.Context) error) {
		done := make(chan error, 1)
		jobs = append(jobs, done)
		go func() { done <- run(ctx) }()
	}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			for _, done := range jobs {
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("secondary runtime shutdown: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("secondary runtime did not stop")
				}
			}
			if err := db.Close(); err != nil {
				t.Errorf("close secondary persistence: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	cache, err := zonecache.New(config, zonecache.Options{Debounce: time.Millisecond, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Register(zoneview.Builder(logger)); err != nil {
		t.Fatal(err)
	}
	manager, err := secondary.New(secondary.Options{
		Cache: cache, Blobs: db, ClusterID: "secondary-serving-interop", Keyring: ring,
		Receiver: &secondary.TransferClient{Keyring: ring, Timeout: time.Second},
		Now:      clock.now, PollInterval: 10 * time.Millisecond, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	stage := authoritative.New(zoneview.Source{Cache: cache, Secondary: manager})
	stage.Now = clock.now
	notify := secondary.NewNotifyReceiver(manager, nil)
	if len(accepted) != 0 {
		notify.Accepted = accepted[0]
	}
	stage.Notify = notify.Handle
	forward, err := upstreamstage.New("default", []string{root}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forward.Set().Close() })
	chain := pipeline.MustNew(&ingress.Stage{NodeID: "secondary-interop", Version: "test"}, stage, forward)
	registry, err := dnsserver.NewRegistry(dnsserver.Options{
		Handler: &dnsserver.Handler{Chain: chain, Logger: logger}, Logger: logger, TSIG: ring, Sockets: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Add(
		dnsserver.Spec{Protocol: dnsserver.ProtoUDP, Address: "127.0.0.1:0"},
		dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"},
	); err != nil {
		t.Fatal(err)
	}
	launch(cache.Start)
	launch(notify.Run)
	launch(registry.Run)
	select {
	case <-cache.Initialized():
	case <-time.After(5 * time.Second):
		t.Fatal("secondary config cache did not initialize")
	}
	// Initialized closes when store watches sync, before the zone builder
	// publishes its view. Wait for the serving source's gate to open too;
	// listening sockets alone can still answer SERVFAIL while unsynced.
	if !kudurrutest.Eventually(5*time.Second, 10*time.Millisecond, stage.Ready) {
		t.Fatal("secondary authoritative source did not open its gate")
	}
	if !kudurrutest.Eventually(5*time.Second, 10*time.Millisecond, func() bool { ready, _ := registry.Ready(); return ready }) {
		t.Fatal("secondary UDP/TCP listeners did not become ready")
	}
	var startOnce sync.Once
	return &secondaryServingRuntime{
		manager: manager, notify: notify, stop: stop,
		addrs:        map[string]string{"udp": registry.Addr(dnsserver.ProtoUDP).String(), "tcp": registry.Addr(dnsserver.ProtoTCP).String()},
		startManager: func() { startOnce.Do(func() { launch(manager.Run) }) },
	}
}

func awaitSecondaryServingStatus(t *testing.T, manager *secondary.Manager, dz *v1alpha1.DNSZone, predicate func(secondary.Status) bool) secondary.Status {
	t.Helper()
	var status secondary.Status
	if !kudurrutest.Eventually(5*time.Second, 10*time.Millisecond, func() bool {
		var ok bool
		status, ok = manager.StatusFor(dz)
		return ok && predicate(status)
	}) {
		t.Fatalf("secondary did not reach expected state: %+v", status)
	}
	return status
}

func assertSecondaryAnswer(t *testing.T, runtime *secondaryServingRuntime, origin string, serial uint32) {
	t.Helper()
	for _, proto := range []string{"udp", "tcp"} {
		m, err := query(t, runtime.addrs[proto], "www."+origin, dns.TypeA, proto == "tcp")
		if err != nil || m == nil || m.Rcode != dns.RcodeSuccess || !m.Authoritative || len(m.Answer) != 1 || !hasA(m.Answer, "www."+origin, "192.0.2.80") || m.Answer[0].Header().Ttl != 117 {
			t.Fatalf("%s did not serve transferred A/TTL: %v %v", proto, m, err)
		}
		m, err = query(t, runtime.addrs[proto], origin, dns.TypeSOA, proto == "tcp")
		if err != nil || m == nil || m.Rcode != dns.RcodeSuccess || !m.Authoritative || len(m.Answer) != 1 {
			t.Fatalf("%s did not serve transferred SOA: %v %v", proto, m, err)
		}
		soa, ok := m.Answer[0].(*dns.SOA)
		if !ok || soa.Serial != serial || soa.Hdr.Ttl != 537 || soa.Refresh != 61 || soa.Retry != 13 || soa.Expire != 120 || soa.Minttl != 47 {
			t.Fatalf("%s overwrote the external SOA: %v", proto, m)
		}
	}
}

func assertSecondaryUnavailable(t *testing.T, runtime *secondaryServingRuntime, origin string) {
	t.Helper()
	for _, proto := range []string{"udp", "tcp"} {
		for _, name := range []string{"www." + origin, "missing." + origin} {
			m, err := query(t, runtime.addrs[proto], name, dns.TypeA, proto == "tcp")
			if err != nil || m == nil || m.Rcode != dns.RcodeServerFailure || len(m.Answer) != 0 {
				t.Fatalf("%s unavailable/expired secondary must SERVFAIL %s: %v %v", proto, name, m, err)
			}
		}
	}
}

func assertSecondaryRootWorks(t *testing.T, runtime *secondaryServingRuntime) {
	t.Helper()
	for _, proto := range []string{"udp", "tcp"} {
		m, err := query(t, runtime.addrs[proto], "outside.secondary-root.test.", dns.TypeA, proto == "tcp")
		if err != nil || m == nil || m.Rcode != dns.RcodeSuccess || !hasA(m.Answer, "outside.secondary-root.test.", "203.0.113.250") {
			t.Fatalf("%s root forwarder positive control failed: %v %v", proto, m, err)
		}
	}
}

func secondaryRootForwarder(t *testing.T, origin string) (string, *atomic.Int64) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	covered := new(atomic.Int64)
	started, done := make(chan struct{}), make(chan error, 1)
	srv := &dns.Server{
		PacketConn: pc, NotifyStartedFunc: func() { close(started) },
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
			if dns.IsSubDomain(origin, q.Question[0].Name) {
				covered.Add(1)
			}
			m := new(dns.Msg)
			m.SetReply(q)
			m.RecursionAvailable = true
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(203, 0, 113, 250)}}
			_ = w.WriteMsg(m)
		}),
	}
	go func() { done <- srv.ActivateAndServe() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.ShutdownContext(ctx); err != nil {
			t.Errorf("root forwarder shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("root forwarder: %v", err)
		}
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("root forwarder did not start")
	}
	return pc.LocalAddr().String(), covered
}
