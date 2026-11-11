package dnstap_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap/dnstaptest"
)

var queryTime = time.Date(2026, 11, 10, 12, 0, 0, 123456789, time.UTC)

// spec is a querylog.sinks entry of type dnstap with its defaults applied.
func spec(t *testing.T, name string, settings config.QuerylogDnstap) querylog.SinkSpec {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Querylog.Sinks = []config.QuerylogSink{{Name: name, Type: config.QuerylogSinkDnstap, Dnstap: &settings}}
	if err := cfg.Validate(); err != nil {
		t.Fatal("the settings validator refuses the entry:", err)
	}
	specs, err := querylog.SinkSpecsFrom(cfg.Querylog)
	if err != nil {
		t.Fatal(err)
	}
	return specs[0]
}

func packed(t *testing.T, m *dns.Msg) []byte {
	t.Helper()
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// forwarded is a forwarded A query with its real wire messages attached.
func forwarded(t *testing.T) *querylog.Event {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion("www.example.com.", dns.TypeA)
	q.Id = 4242
	a := new(dns.Msg)
	a.SetReply(q)
	a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "www.example.com.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: []byte{192, 0, 2, 7}}}
	return &querylog.Event{
		Time: queryTime, Latency: 3 * time.Millisecond, Node: "dns1", Proto: pipeline.ProtoUDP,
		Client: netip.MustParseAddr("192.168.1.20"), ClientName: "laptop", Name: "www.example.com", QType: dns.TypeA, QClass: dns.ClassINET,
		Status: pipeline.StatusForwarded, Upstream: "default/quad9", Answers: 1, ReplyType: querylog.ReplyIP, Flags: querylog.Flags{RD: true},
		Wire: &querylog.Wire{Query: packed(t, q), Response: packed(t, a), ClientPort: 53001},
	}
}

func authoritative() *querylog.Event {
	return &querylog.Event{
		Time: queryTime, Latency: time.Millisecond, Node: "dns1", Proto: pipeline.ProtoTCP,
		Client: netip.MustParseAddr("2001:db8::5"), Name: "nas.home.example", QType: dns.TypeAAAA, QClass: dns.ClassINET,
		Status: pipeline.StatusAuthoritative, Zone: "home.example", Rcode: dns.RcodeNameError, ReplyType: querylog.ReplyNXDomain,
	}
}

func open(t *testing.T, settings config.QuerylogDnstap, o dnstap.Options) *dnstap.Sink {
	t.Helper()
	if o.Version == "" {
		o.Version = "kudurru/test"
	}
	if o.Node == "" {
		o.Node = "dns1"
	}
	sink, err := dnstap.New(spec(t, "collector", settings), o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := sink.Open(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	return sink
}

func write(t *testing.T, sink *dnstap.Sink, events ...*querylog.Event) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return sink.WriteBatch(ctx, events, querylog.PrivacyFull)
}

func question(t *testing.T, wire []byte) (dns.Question, *dns.Msg) {
	t.Helper()
	m := new(dns.Msg)
	if err := m.Unpack(wire); err != nil {
		t.Fatal("a message that does not unpack:", err)
	}
	if len(m.Question) != 1 {
		t.Fatalf("questions %v", m.Question)
	}
	return m.Question[0], m
}

// TestSinkFrameStreamsOverUnixTCPAndTLS sends one forwarded and one
// authoritative query over each transport and reads them back with the
// reader side of the bidirectional handshake.
func TestSinkFrameStreamsOverUnixTCPAndTLS(t *testing.T) {
	for _, network := range []string{"unix", "tcp", "tls"} {
		t.Run(network, func(t *testing.T) {
			collector := dnstaptest.Listen(t, network)
			settings := config.QuerylogDnstap{Target: collector.Target()}
			o := dnstap.Options{}
			if network == "tls" {
				dir := t.TempDir()
				ca := filepath.Join(dir, "ca.pem")
				if err := os.WriteFile(ca, collector.CertPEM(), 0o600); err != nil {
					t.Fatal(err)
				}
				settings.CARef = &config.CredentialRef{File: ca}
				o.Resolve = func(_ context.Context, ref config.CredentialRef) ([]byte, error) { return os.ReadFile(ref.File) }
			}
			sink := open(t, settings, o)
			if err := write(t, sink, forwarded(t), authoritative()); err != nil {
				t.Fatal(err)
			}
			frames := collector.WaitFrames(4, 5*time.Second)
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "FINISH", func() bool { return collector.Finished() == 1 })
			if errs := collector.Errors(); len(errs) > 0 {
				t.Fatal(errs)
			}
			wantTypes := []int{int(dnstap.ClientQuery), int(dnstap.ClientResponse), int(dnstap.AuthQuery), int(dnstap.AuthResponse)}
			for i, f := range frames {
				if f.Message.Type != wantTypes[i] {
					t.Fatalf("frame %d is type %d, want %d", i, f.Message.Type, wantTypes[i])
				}
				if f.Identity != "dns1" || f.Version != "kudurru/test" {
					t.Fatalf("identity %q version %q", f.Identity, f.Version)
				}
				if !f.Message.QueryTime.Equal(queryTime) {
					t.Fatalf("query time %v", f.Message.QueryTime)
				}
			}
			cq, cr := frames[0].Message, frames[1].Message
			if cq.SocketFamily != int(dnstap.FamilyINET) || cq.SocketProtocol != int(dnstap.ProtocolUDP) || cq.QueryAddress != netip.MustParseAddr("192.168.1.20") {
				t.Fatalf("client query %+v", cq)
			}
			if cq.QueryPort != 0 {
				t.Fatal("a sink that is not raw sent the client port")
			}
			if q, m := question(t, cq.QueryMessage); q.Name != "www.example.com." || q.Qtype != dns.TypeA || m.Id != 0 || !m.RecursionDesired {
				t.Fatalf("rebuilt query %v", m)
			}
			_, response := question(t, cr.ResponseMessage)
			if !response.Response || response.Rcode != dns.RcodeSuccess || len(response.Answer) != 0 || !response.RecursionAvailable {
				t.Fatalf("a rebuilt response must carry the outcome and no answer records: %v", response)
			}
			if !cr.ResponseTime.Equal(queryTime.Add(3 * time.Millisecond)) {
				t.Fatalf("response time %v", cr.ResponseTime)
			}
			if frames[0].Extra != "status=forwarded upstream=default/quad9 client=laptop" {
				t.Fatalf("extra %q", frames[0].Extra)
			}
			aq, ar := frames[2].Message, frames[3].Message
			if aq.SocketFamily != int(dnstap.FamilyINET6) || aq.SocketProtocol != int(dnstap.ProtocolTCP) || aq.QueryAddress != netip.MustParseAddr("2001:db8::5") {
				t.Fatalf("auth query %+v", aq)
			}
			zone, _, err := dns.UnpackDomainName(aq.QueryZone, 0)
			if err != nil || zone != "home.example." {
				t.Fatalf("query zone %q %v", zone, err)
			}
			if _, m := question(t, ar.ResponseMessage); m.Rcode != dns.RcodeNameError || !m.Authoritative {
				t.Fatalf("auth response %v", m)
			}
		})
	}
}

// TestSinkRawSendsTheWireMessages: a raw sink sends the messages as they
// were, with the real client port; a sink that is not raw ignores them even
// when an event carries them.
func TestSinkRawSendsTheWireMessages(t *testing.T) {
	collector := dnstaptest.Listen(t, "unix")
	var audited []dnstap.RawStart
	raw := open(t, config.QuerylogDnstap{Target: collector.Target(), Raw: true, Types: []string{config.DnstapClientQuery, config.DnstapClientResponse}}, dnstap.Options{
		Audit: func(_ context.Context, start dnstap.RawStart) error { audited = append(audited, start); return nil },
	})
	if !raw.Raw() {
		t.Fatal("raw: true must make a querylog.RawSink")
	}
	if len(audited) != 1 || audited[0].Sink != "collector" || audited[0].Target != collector.Target() {
		t.Fatalf("audit events %+v", audited)
	}
	ev := forwarded(t)
	if err := write(t, raw, ev); err != nil {
		t.Fatal(err)
	}
	frames := collector.WaitFrames(2, 5*time.Second)
	if q := frames[0].Message; string(q.QueryMessage) != string(ev.Wire.Query) || q.QueryPort != 53001 {
		t.Fatalf("raw query %+v", q)
	}
	if r := frames[1].Message; string(r.ResponseMessage) != string(ev.Wire.Response) {
		t.Fatal("raw response differs from the wire message")
	}
	if _, m := question(t, frames[1].Message.ResponseMessage); len(m.Answer) != 1 || m.Id != 4242 {
		t.Fatalf("raw response %v", m)
	}
	_ = raw.Close()
}

// TestSinkRawRequiresItsAuditEvent: no raw sink is built without an audit
// trail, and none opens or connects when the audit event is not recorded.
func TestSinkRawRequiresItsAuditEvent(t *testing.T) {
	collector := dnstaptest.Listen(t, "tcp")
	settings := config.QuerylogDnstap{Target: collector.Target(), Raw: true}
	if _, err := dnstap.New(spec(t, "collector", settings), dnstap.Options{}); err == nil {
		t.Fatal("a raw sink was built without an audit trail")
	}
	sink, err := dnstap.New(spec(t, "collector", settings), dnstap.Options{Audit: func(context.Context, dnstap.RawStart) error { return errors.New("audit store unavailable") }})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Open(t.Context(), nil, nil); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("open without an audit event: %v", err)
	}
	if collector.Accepted() != 0 {
		t.Fatal("a raw sink contacted the collector before its audit event")
	}
}

// TestSinkTypesAndPrivacy: the types filter what is sent; an event after a
// hiding transform sends what the local log keeps, nothing more.
func TestSinkTypesAndPrivacy(t *testing.T) {
	collector := dnstaptest.Listen(t, "unix")
	sink := open(t, config.QuerylogDnstap{Target: collector.Target(), Identity: "edge-a", Types: []string{config.DnstapClientResponse, config.DnstapAuthResponse, config.DnstapForwarderQuery, config.DnstapForwarderResponse}}, dnstap.Options{})
	privacy, err := querylog.NewPrivacy(querylog.PrivacyOptions{Mode: querylog.PrivacyHideDomainsClients})
	if err != nil {
		t.Fatal(err)
	}
	hidden := forwarded(t)
	if !privacy.Apply(hidden) {
		t.Fatal("not admitted")
	}
	hidden.Wire = forwarded(t).Wire // as if a stale route had attached it
	if err := write(t, sink, hidden, authoritative()); err != nil {
		t.Fatal(err)
	}
	frames := collector.WaitFrames(4, 5*time.Second)
	types := []int{}
	for _, f := range frames {
		types = append(types, f.Message.Type)
		if f.Identity != "edge-a" {
			t.Fatal("identity", f.Identity)
		}
	}
	want := []int{int(dnstap.ClientResponse), int(dnstap.ForwarderQuery), int(dnstap.ForwarderResponse), int(dnstap.AuthResponse)}
	if len(frames) != 4 {
		t.Fatalf("types %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types %v, want %v", types, want)
		}
	}
	cr := frames[0].Message
	if cr.QueryAddress != netip.IPv4Unspecified() || cr.QueryPort != 0 {
		t.Fatalf("hidden client sent as %v:%d", cr.QueryAddress, cr.QueryPort)
	}
	if q, m := question(t, cr.ResponseMessage); q.Name != "hidden." || len(m.Answer) != 0 {
		t.Fatalf("hidden response %v", m)
	}
	if strings.Contains(frames[0].Extra, "laptop") || strings.Contains(frames[0].Extra, "example") {
		t.Fatalf("extra leaks a hidden fact: %q", frames[0].Extra)
	}
	fq := frames[1].Message
	if fq.QueryAddress.IsValid() || fq.SocketFamily != 0 {
		t.Fatalf("forwarder query names an address: %+v", fq)
	}
	if q, _ := question(t, fq.QueryMessage); q.Name != "hidden." {
		t.Fatal("forwarder query", q)
	}
	// A pseudonym leaves no address.
	pseudonym, err := querylog.NewPrivacy(querylog.PrivacyOptions{Mode: querylog.PrivacyAnonymizeClient, Pseudonym: true})
	if err != nil {
		t.Fatal(err)
	}
	anon := authoritative()
	pseudonym.Apply(anon)
	if err := write(t, sink, anon); err != nil {
		t.Fatal(err)
	}
	frames = collector.WaitFrames(5, 5*time.Second)
	if m := frames[4].Message; m.QueryAddress.IsValid() || m.SocketFamily != 0 || !strings.Contains(frames[4].Extra, "client=anon-") {
		t.Fatalf("pseudonymized client: %+v extra %q", m, frames[4].Extra)
	}
	_ = sink.Close()
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSinkReconnectsAndDropsBoundedOnTheHost runs the sink on the real sink
// host. A collector that goes away is reconnected to with the next batch; a
// collector that stops reading costs only this sink's queued events, which
// are counted, and the sink sends again once the collector reads again.
func TestSinkReconnectsAndDropsBoundedOnTheHost(t *testing.T) {
	collector := dnstaptest.Listen(t, "unix")
	reg := prometheus.NewRegistry()
	metrics := querylog.NewMetrics(reg)
	var mu sync.Mutex
	var built *dnstap.Sink
	host := querylog.NewSinkHost(querylog.SinkHostOptions{Metrics: metrics, Logger: slog.New(slog.DiscardHandler), Factories: map[string]querylog.SinkFactory{
		config.QuerylogSinkDnstap: func(s querylog.SinkSpec) (querylog.Sink, error) {
			sink, err := dnstap.New(s, dnstap.Options{Node: "dns1", Version: "kudurru/test"})
			mu.Lock()
			built = sink
			mu.Unlock()
			return sink, err
		},
	}})
	s := spec(t, "collector", config.QuerylogDnstap{Target: collector.Target(), Types: []string{config.DnstapClientQuery}})
	s.QueueSize, s.BatchSize, s.Interval, s.Timeout, s.Attempts, s.Backoff = 100, 10, 20*time.Millisecond, 300*time.Millisecond, 2, 20*time.Millisecond
	set, err := host.Prepare([]querylog.SinkSpec{s})
	if err != nil {
		t.Fatal(err)
	}
	a := querylog.NewAsyncWith(querylog.AsyncOptions{Sink: &nopSink{}, Metrics: metrics, Logger: slog.New(slog.DiscardHandler)})
	route := a.PrepareCapturedSinks(&nopSink{}, nil, true, 1, nil, set)
	a.AdoptCaptured(route)
	send := func(n int, name string) {
		for i := 0; i < n; i++ {
			route.Enqueue(&querylog.Event{Time: time.Now(), Client: netip.MustParseAddr("192.0.2.1"), Name: name, QType: dns.TypeA, QClass: dns.ClassINET, Status: pipeline.StatusForwarded, ReplyType: querylog.ReplyIP})
		}
	}
	send(5, "first.example")
	collector.WaitFrames(5, 5*time.Second)

	// The collector goes away: the next batch connects again.
	collector.DropConnections()
	time.Sleep(50 * time.Millisecond)
	send(5, "second.example")
	collector.WaitFrames(10, 5*time.Second)
	if collector.Accepted() < 2 {
		t.Fatalf("accepted %d connections, want a reconnect", collector.Accepted())
	}

	// The collector stops reading. Large events fill the socket buffers;
	// writes time out, the queue overflows, and the losses are counted for
	// this sink only.
	collector.Stall()
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + ".example"
	deadline := time.Now().Add(20 * time.Second)
	// Below the queue bound per round: the drops come from writes that
	// time out on the stalled socket, not from one burst.
	for testutil.ToFloat64(metrics.Dropped.WithLabelValues(querylog.DropStageSink, "collector")) == 0 || host.Status()[0].Errors == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("a stalled collector never cost a failed write and a counted drop: %+v", host.Status())
		}
		send(50, long)
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("while stalled: %+v", host.Status()[0])
	if queued := host.Status()[0].Queued; queued > s.QueueSize {
		t.Fatalf("%d events queued beyond the bound of %d", queued, s.QueueSize)
	}
	collector.Resume()
	before := len(collector.Frames())
	deadline = time.Now().Add(20 * time.Second)
	for {
		send(1, "after.example")
		frames := collector.Frames()
		if len(frames) > before && frameNamed(t, frames[before:], "after.example.") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sink did not recover once the collector read again")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if errs := collector.Errors(); len(errs) > 0 {
		t.Fatal("a reconnect sent a malformed stream:", errs)
	}
	if err := a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	route.Release()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := host.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if built == nil {
		t.Fatal("the factory never ran")
	}
}

func frameNamed(t *testing.T, frames []dnstaptest.Frame, name string) bool {
	for _, f := range frames {
		m := new(dns.Msg)
		if m.Unpack(f.Message.QueryMessage) == nil && len(m.Question) == 1 && m.Question[0].Name == name {
			return true
		}
	}
	return false
}

type nopSink struct{}

func (*nopSink) Name() string                                                       { return "nop" }
func (*nopSink) Open(context.Context, json.RawMessage, prometheus.Registerer) error { return nil }
func (*nopSink) Write([]*querylog.Event) error                                      { return nil }
func (*nopSink) Flush(context.Context) error                                        { return nil }
func (*nopSink) Close() error                                                       { return nil }
