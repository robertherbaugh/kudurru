package notifier

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

const keyName = "xfr-key."

// secondary is a fake secondary on 127.0.0.1: it verifies TSIG with the
// shared key (miekg's own HMAC), records every NOTIFY and answers it,
// except for the first `drop` messages, which it ignores (silence).
type secondary struct {
	addr netip.AddrPort
	mu   sync.Mutex
	got  []received
	drop int
}

type received struct {
	serial uint32
	tsig   bool
	status error
}

func startSecondary(t *testing.T, drop int) *secondary {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &secondary{addr: netip.MustParseAddrPort(pc.LocalAddr().String()), drop: drop}
	srv := &dns.Server{PacketConn: pc, TsigSecret: map[string]string{keyName: base64.StdEncoding.EncodeToString(secret)},
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			rec := received{tsig: r.IsTsig() != nil, status: w.TsigStatus()}
			if len(r.Answer) == 1 {
				if soa, ok := r.Answer[0].(*dns.SOA); ok {
					rec.serial = soa.Serial
				}
			}
			s.mu.Lock()
			s.got = append(s.got, rec)
			silent := len(s.got) <= s.drop
			s.mu.Unlock()
			if silent || r.Opcode != dns.OpcodeNotify {
				return
			}
			m := new(dns.Msg)
			m.SetReply(r)
			if ts := r.IsTsig(); ts != nil && rec.status == nil {
				m.SetTsig(ts.Hdr.Name, ts.Algorithm, 300, time.Now().Unix())
			}
			_ = w.WriteMsg(m)
		})}
	started := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(started) }
	go func() { _ = srv.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = srv.Shutdown() })
	return s
}

func (s *secondary) received() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func testZone(t testing.TB, serial uint32, mutate func(*v1alpha1.DNSZone)) *zone.Zone {
	t.Helper()
	dz := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Generation: 1},
		Spec: v1alpha1.DNSZoneSpec{
			ZoneName: "example.org.", Type: v1alpha1.ZoneTypePrimary,
			Nameservers: []string{"ns1.example.org."}, DefaultTTL: 300,
		},
		Status: v1alpha1.DNSZoneStatus{Serial: serial},
	}
	if mutate != nil {
		mutate(dz)
	}
	z, errs := zone.Build(dz, nil, zone.BuildOptions{})
	if z == nil {
		t.Fatalf("build: %v", errs.Zone)
	}
	return z
}

func specified(target netip.AddrPort, keys ...string) func(*v1alpha1.DNSZone) {
	return func(dz *v1alpha1.DNSZone) {
		dz.Spec.Policy.Notify = v1alpha1.NotifySpecified
		dz.Spec.Policy.NotifyTargets = []string{target.String()}
		dz.Spec.Policy.TransferTSIGKeyRefs = keys
	}
}

func holder(t testing.TB, g zone.Gate, zones ...*zone.Zone) *zone.Holder {
	t.Helper()
	snap, err := zone.NewSnapshot(1, true, zones...)
	if err != nil {
		t.Fatal(err)
	}
	h := new(zone.Holder)
	h.Store(snap.WithGate(g))
	return h
}

func keyring(t testing.TB) *dnsserver.StaticKeyring {
	t.Helper()
	k, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: keyName, Algorithm: "hmac-sha256", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// run starts n and stops it at the end of the test.
func run(t *testing.T, n *Notifier) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return")
		}
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func count(n *Notifier, result string) float64 {
	return testutil.ToFloat64(n.total.WithLabelValues("example.org.", result))
}

func fast(o *Options) {
	o.PollInterval = 10 * time.Millisecond
	o.Timeout = 150 * time.Millisecond
	o.Backoff = 10 * time.Millisecond
	o.MaxBackoff = 40 * time.Millisecond
}

func newNotifier(t *testing.T, o Options) *Notifier {
	t.Helper()
	fast(&o)
	reg := prometheus.NewRegistry()
	o.Registerer = reg
	n, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNotifyDeliveryWithTSIG(t *testing.T) {
	sec := startSecondary(t, 0)
	h := holder(t, zone.GateOpen, testZone(t, 100, specified(sec.addr, keyName)))
	n := newNotifier(t, Options{Source: h, Keyring: keyring(t), SourceAddr: netip.MustParseAddr("127.0.0.1")})
	run(t, n)

	eventually(t, "first NOTIFY", func() bool { return count(n, ResultSuccess) == 1 })
	got := sec.received()
	if len(got) != 1 || got[0].serial != 100 || !got[0].tsig || got[0].status != nil {
		t.Fatalf("secondary got %+v", got)
	}

	// A new serial is notified once; polling an unchanged serial is not.
	h.Store(mustSnapshot(t, testZone(t, 101, specified(sec.addr, keyName))))
	n.Trigger()
	eventually(t, "second NOTIFY", func() bool { return count(n, ResultSuccess) == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := sec.received(); len(got) != 2 || got[1].serial != 101 {
		t.Fatalf("secondary got %+v", got)
	}
}

func mustSnapshot(t testing.TB, zones ...*zone.Zone) *zone.Snapshot {
	t.Helper()
	s, err := zone.NewSnapshot(2, true, zones...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNotifyRetriesOnSilence(t *testing.T) {
	sec := startSecondary(t, 2) // ignores the first two attempts
	h := holder(t, zone.GateOpen, testZone(t, 7, specified(sec.addr)))
	n := newNotifier(t, Options{Source: h})
	run(t, n)
	eventually(t, "NOTIFY after two silent attempts", func() bool { return count(n, ResultSuccess) == 1 })
	if got := sec.received(); len(got) != 3 {
		t.Fatalf("%d attempts, want 3", len(got))
	}
}

func TestNotifyGivesUp(t *testing.T) {
	sec := startSecondary(t, 1000) // never answers
	h := holder(t, zone.GateOpen, testZone(t, 7, specified(sec.addr)))
	n := newNotifier(t, Options{Source: h, Attempts: 3})
	run(t, n)
	eventually(t, "give up", func() bool { return count(n, ResultTimeout) == 1 })
	if got := sec.received(); len(got) != 3 {
		t.Fatalf("%d attempts, want 3 (RFC 1996 3.6 retries, bounded)", len(got))
	}
}

func TestNotifySkipsWithoutKeyOrGate(t *testing.T) {
	sec := startSecondary(t, 0)
	// The zone names a key the node does not have: nothing is sent.
	h := holder(t, zone.GateOpen, testZone(t, 7, specified(sec.addr, "missing-key.")))
	n := newNotifier(t, Options{Source: h, Keyring: keyring(t)})
	run(t, n)
	eventually(t, "nokey", func() bool { return count(n, ResultNoKey) == 1 })

	// Nothing is notified from an unsynced or stale zone set, and
	// zones with notify none are ignored.
	sec2 := startSecondary(t, 0)
	for _, g := range []zone.Gate{zone.GateUnsynced, zone.GateStale} {
		n2 := newNotifier(t, Options{Source: holder(t, g, testZone(t, 7, specified(sec2.addr)))})
		run(t, n2)
	}
	n3 := newNotifier(t, Options{Source: holder(t, zone.GateOpen, testZone(t, 7, func(dz *v1alpha1.DNSZone) {
		dz.Spec.Policy.NotifyTargets = []string{sec2.addr.String()} // mode stays none
	}))})
	run(t, n3)
	time.Sleep(100 * time.Millisecond)
	if got := sec.received(); len(got) != 0 {
		t.Fatalf("NOTIFY without its key: %+v", got)
	}
	if got := sec2.received(); len(got) != 0 {
		t.Fatalf("NOTIFY from a gated set or a none zone: %+v", got)
	}
}

// fakeSend records the notifications instead of sending them.
type fakeSend struct {
	mu   sync.Mutex
	sent []zone.Notify
	fail func(zone.Notify) error
}

func (f *fakeSend) send(ctx context.Context, n zone.Notify) error {
	f.mu.Lock()
	f.sent = append(f.sent, n)
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		if err := fail(n); err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
			return err
		}
	}
	return nil
}

func (f *fakeSend) all() []zone.Notify {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

type fakeResolver map[string][]netip.Addr

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func TestZoneNameServerTargets(t *testing.T) {
	// Four apex NS hosts: ns1 (the SOA MNAME) and ns2 with glue, one
	// resolvable out-of-zone host and one that does not resolve.
	dz := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: v1alpha1.DNSZoneSpec{
		ZoneName: "example.org.", Type: v1alpha1.ZoneTypePrimary, DefaultTTL: 300,
		Nameservers: []string{"ns1.example.org.", "ns2.example.org.", "ns.example.net.", "gone.example.net."},
		SOA:         &v1alpha1.SOASpec{MName: "ns1.example.org."},
		Policy:      v1alpha1.ZonePolicy{Notify: v1alpha1.NotifyBoth, NotifyTargets: []string{"192.0.2.99", "192.0.2.10"}},
	}, Status: v1alpha1.DNSZoneStatus{Serial: 9}}
	z, errs := zone.Build(dz, []*v1alpha1.DNSRecordSet{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "dns", Name: "glue"},
		Spec: v1alpha1.DNSRecordSetSpec{Endpoints: []v1alpha1.Endpoint{
			{DNSName: "ns1.example.org", RecordType: "A", Targets: []string{"192.0.2.1"}},
			{DNSName: "ns2.example.org", RecordType: "A", Targets: []string{"192.0.2.2", "192.0.2.10"}},
			{DNSName: "ns2.example.org", RecordType: "AAAA", Targets: []string{"2001:db8::2"}},
		}},
	}}, zone.BuildOptions{})
	if z == nil || !errs.Empty() {
		t.Fatalf("build: %v %v", errs.Zone, errs.RecordSets)
	}
	n := newNotifier(t, Options{Source: holder(t, zone.GateOpen), Resolver: fakeResolver{"ns.example.net.": {netip.MustParseAddr("::ffff:198.51.100.5")}}})
	got := n.targets(context.Background(), z)
	want := []netip.AddrPort{
		netip.MustParseAddrPort("192.0.2.99:53"), netip.MustParseAddrPort("192.0.2.10:53"), // specified
		netip.MustParseAddrPort("198.51.100.5:53"),                                           // ns.example.net. resolved
		netip.MustParseAddrPort("192.0.2.2:53"), netip.MustParseAddrPort("[2001:db8::2]:53"), // ns2 glue; .10 deduplicated
	}
	slices.SortFunc(got, func(a, b netip.AddrPort) int { return a.Compare(b) })
	slices.SortFunc(want, func(a, b netip.AddrPort) int { return a.Compare(b) })
	if !slices.Equal(got, want) {
		t.Fatalf("targets %v, want %v (MNAME ns1 excluded, unresolvable host skipped)", got, want)
	}
}

func TestSupersedeAndSkipInitial(t *testing.T) {
	target := netip.MustParseAddrPort("192.0.2.53:53")
	fs := &fakeSend{}
	release := make(chan struct{})
	fs.fail = func(n zone.Notify) error {
		if n.Serial == 1 {
			select {
			case <-release:
				return nil
			default:
				return &zone.NotifyError{Class: "timeout", Err: errors.New("silent")}
			}
		}
		return nil
	}
	h := holder(t, zone.GateOpen, testZone(t, 1, specified(target)))
	n := newNotifier(t, Options{Source: h, Attempts: 1000})
	n.opts.send = fs.send
	run(t, n)
	eventually(t, "retries of serial 1", func() bool { return len(fs.all()) >= 3 })
	h.Store(mustSnapshot(t, testZone(t, 2, specified(target))))
	n.Trigger()
	eventually(t, "serial 2 delivered", func() bool { return count(n, ResultSuccess) == 1 })
	eventually(t, "serial 1 superseded", func() bool { return count(n, ResultSuperseded) == 1 })
	close(release)
	for _, s := range fs.all() {
		if s.Serial != 1 && s.Serial != 2 {
			t.Fatalf("unexpected serial %d", s.Serial)
		}
	}

	// SkipInitial: the serials found at the start of the term are not
	// notified; the next change is.
	fs2 := &fakeSend{}
	h2 := holder(t, zone.GateOpen, testZone(t, 5, specified(target)))
	n2 := newNotifier(t, Options{Source: h2, SkipInitial: true})
	n2.opts.send = fs2.send
	run(t, n2)
	time.Sleep(60 * time.Millisecond)
	if len(fs2.all()) != 0 {
		t.Fatal("SkipInitial notified the initial serial")
	}
	h2.Store(mustSnapshot(t, testZone(t, 6, specified(target))))
	eventually(t, "change after start", func() bool { return len(fs2.all()) == 1 && fs2.all()[0].Serial == 6 })

	// A zone that disappears cancels its round.
	h2.Store(mustSnapshot(t))
	n2.Trigger()
	time.Sleep(30 * time.Millisecond)
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Options{}); err == nil || !strings.Contains(err.Error(), "Source") {
		t.Fatalf("New without a source: %v", err)
	}
	reg := prometheus.NewRegistry()
	if _, err := New(Options{Source: new(zone.Holder), Registerer: reg}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Source: new(zone.Holder), Registerer: reg}); err == nil {
		t.Fatal("registering the counter twice must fail")
	}
	n, _ := New(Options{Source: new(zone.Holder)})
	if !n.NeedLeaderElection() {
		t.Fatal("the notifier is leader-only")
	}
	if n.opts.Attempts != DefaultAttempts || n.opts.Timeout != DefaultTimeout || n.opts.Backoff != DefaultBackoff {
		t.Fatalf("defaults %+v", n.opts)
	}
}

func TestNotifyExplicitKeyAndSourceOptions(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			sec := startSecondary(t, 0)
			z := testZone(t, 100, func(dz *v1alpha1.DNSZone) {
				specified(sec.addr, keyName)(dz)
				if explicit {
					// This transfer key is deliberately unavailable. Explicit NOTIFY
					// selection must use the independent key instead.
					dz.Spec.Policy.TransferTSIGKeyRefs = []string{"transfer-only."}
					dz.Spec.Policy.NotifyTSIGKeyRef = keyName
					dz.Spec.Policy.NotifySource = "127.0.0.2"
				}
			})
			fs := &fakeSend{}
			n := newNotifier(t, Options{Source: holder(t, zone.GateOpen, z), Keyring: keyring(t), SourceAddr: netip.MustParseAddr("127.0.0.1"), send: fs.send})
			n.notifyZone(t.Context(), z)
			got := fs.all()
			want := "127.0.0.1"
			if explicit {
				want = "127.0.0.2"
			}
			if len(got) != 1 || got[0].KeyName != keyName || got[0].Source != netip.MustParseAddr(want) {
				t.Fatal("legacy notifier did not wire independent key/source with compatibility fallback")
			}
			// Exercise the actual legacy sender, not only its injectable wire seam.
			n = newNotifier(t, Options{Source: holder(t, zone.GateOpen, z), Keyring: keyring(t), SourceAddr: netip.MustParseAddr("127.0.0.1")})
			n.notifyZone(t.Context(), z)
			gotWire := sec.received()
			if len(gotWire) != 1 || !gotWire[0].tsig || gotWire[0].status != nil || count(n, ResultSuccess) != 1 {
				t.Fatal("legacy selected key did not sign actual NOTIFY")
			}
		})
	}
}

func TestNotifyUnavailableExplicitKeyDoesNotUseTransferKey(t *testing.T) {
	sec := startSecondary(t, 0)
	z := testZone(t, 100, func(dz *v1alpha1.DNSZone) {
		specified(sec.addr, keyName)(dz)
		dz.Spec.Policy.NotifyTSIGKeyRef = "missing."
	})
	n := newNotifier(t, Options{Source: holder(t, zone.GateOpen, z), Keyring: keyring(t)})
	n.notifyZone(t.Context(), z)
	if len(sec.received()) != 0 || count(n, ResultNoKey) != 1 {
		t.Fatal("missing explicit key used transfer-key fallback")
	}
}
