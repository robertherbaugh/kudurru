package querylog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	testclock "k8s.io/utils/clock/testing"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func ev(client, name string) *Event {
	return &Event{
		Time: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), Client: netip.MustParseAddr(client), Name: name,
		ClientName: "laptop", Answer: "1.2.3.4", Rule: "||" + name + "^", CNAME: "cdn.example.",
		ECS: netip.MustParsePrefix("192.168.1.0/24"), Flags: Flags{ECS: true},
	}
}

func TestPrivacyTransform(t *testing.T) {
	type want struct {
		client, clientName, name, cname, answer, rule string
		ecs                                           bool
	}
	for _, tc := range []struct {
		mode   Mode
		client string
		want   want
	}{
		{PrivacyFull, "192.168.1.77", want{"192.168.1.77", "laptop", "ads.example.", "cdn.example.", "1.2.3.4", "||ads.example.^", true}},
		{PrivacyAnonymizeClient, "192.168.1.77", want{"192.168.1.0", "", "ads.example.", "cdn.example.", "", "||ads.example.^", false}},
		{PrivacyAnonymizeClient, "2001:db8:abcd:12::1", want{"2001:db8:abcd::", "", "ads.example.", "cdn.example.", "", "||ads.example.^", false}},
		{PrivacyAnonymizeClient, "::ffff:10.1.2.3", want{"10.1.2.0", "", "ads.example.", "cdn.example.", "", "||ads.example.^", false}},
		{PrivacyHideDomains, "192.168.1.77", want{"192.168.1.77", "laptop", "hidden", "hidden", "", "", true}},
		{PrivacyHideDomainsClients, "192.168.1.77", want{"0.0.0.0", "", "hidden", "hidden", "", "", false}},
		{PrivacyNothing, "192.168.1.77", want{"0.0.0.0", "", "hidden", "hidden", "", "", false}},
	} {
		p, err := NewPrivacy(PrivacyOptions{Mode: tc.mode})
		if err != nil {
			t.Fatal(err)
		}
		e := ev(tc.client, "ads.example.")
		if !p.Apply(e) {
			t.Fatalf("%s: event dropped", tc.mode)
		}
		got := want{e.Client.String(), e.ClientName, e.Name, e.CNAME, e.Answer, e.Rule, e.ECS.IsValid() && e.Flags.ECS}
		if got != tc.want {
			t.Errorf("%s %s:\n got %+v\nwant %+v", tc.mode, tc.client, got, tc.want)
		}
	}
}

// TestHideDomainsHidesZone is the regression test of the M5 finding
// "hide_domains keeps the matched zone": the authoritative or forward zone
// of a query (its apex, e.g. "payroll.corp.example") was stored in the
// zone dictionary and as the "zone:<apex>" upstream although the mode
// promises that no domain is kept.
func TestHideDomainsHidesZone(t *testing.T) {
	for _, mode := range []Mode{PrivacyHideDomains, PrivacyHideDomainsClients, PrivacyNothing} {
		p, err := NewPrivacy(PrivacyOptions{Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		e := ev("192.168.1.77", "www.payroll.corp.example.")
		e.Zone = "payroll.corp.example."
		e.Status = pipeline.StatusAuthoritative
		if !p.Apply(e) {
			t.Fatalf("%s: event dropped", mode)
		}
		if e.Zone != HiddenName {
			t.Errorf("%s: zone %q kept", mode, e.Zone)
		}
	}
	p, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyAnonymizeClient})
	e := ev("192.168.1.77", "www.payroll.corp.example.")
	e.Zone = "payroll.corp.example."
	p.Apply(e)
	if e.Zone != "payroll.corp.example." {
		t.Errorf("anonymize_client hid the zone: %q", e.Zone)
	}
}

func TestPseudonymRotatesDaily(t *testing.T) {
	fc := testclock.NewFakeClock(time.Date(2026, 10, 11, 23, 0, 0, 0, time.UTC))
	p, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyAnonymizeClient, Pseudonym: true, Clock: fc})
	pseud := func(client string) string {
		e := ev(client, "a.example.")
		p.Apply(e)
		if e.Client.IsValid() || e.ECS.IsValid() {
			t.Errorf("pseudonymised event keeps an address: %v %v", e.Client, e.ECS)
		}
		return e.ClientName
	}
	a1, a2, b := pseud("10.0.0.1"), pseud("10.0.0.1"), pseud("10.0.0.2")
	if a1 != a2 || a1 == b || !strings.HasPrefix(a1, PseudonymPrefix) || len(a1) != len(PseudonymPrefix)+12 {
		t.Errorf("same day: %q %q %q", a1, a2, b)
	}
	// Pseudonym names the logged client name (the block page matches it).
	if got := p.Pseudonym(netip.MustParseAddr("::ffff:10.0.0.1")); got != a1 {
		t.Errorf("Pseudonym = %q, logged %q", got, a1)
	}
	if got := (*Privacy)(nil).Pseudonym(netip.MustParseAddr("10.0.0.1")); got != "" {
		t.Errorf("nil filter: %q", got)
	}
	prefix, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyAnonymizeClient})
	if got := prefix.Pseudonym(netip.MustParseAddr("10.0.0.1")); got != "" {
		t.Errorf("prefix mode: %q", got)
	}
	fc.Step(2 * time.Hour) // past UTC midnight
	if a3 := pseud("10.0.0.1"); a3 == a1 {
		t.Error("pseudonym did not rotate at UTC midnight")
	}
}

func TestExcludeLists(t *testing.T) {
	p, err := NewPrivacy(PrivacyOptions{
		ExcludeDomains: []string{"*.lan", "Telemetry.Example.", "/^wpad\\./", ""},
		ExcludeClients: []string{"10.9.0.0/16", "192.168.1.5", "Printer", "fd00::/8"},
		ExcludeSUDN:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		client, name, clientName string
		excluded                 bool
	}{
		{"10.0.0.1", "nas.lan.", "", true},
		{"10.0.0.1", "lan.", "", true},
		{"10.0.0.1", "planet.", "", false},
		{"10.0.0.1", "telemetry.example.", "", true},
		{"10.0.0.1", "sub.telemetry.example.", "", false},
		{"10.0.0.1", "wpad.corp.example.", "", true},
		{"10.9.200.1", "x.example.", "", true},
		{"192.168.1.5", "x.example.", "", true},
		{"::ffff:192.168.1.5", "x.example.", "", true},
		{"192.168.1.6", "x.example.", "printer", true},
		{"fd12::1", "x.example.", "", true},
		{"10.0.0.1", "localhost.", "", true},
		{"10.0.0.1", "box.local.", "", true},
		{"10.0.0.1", "router.home.arpa.", "", true},
		{"10.0.0.1", "1.0.168.192.in-addr.arpa.", "", true},
		{"10.0.0.1", "1.0.20.172.in-addr.arpa.", "", true},
		{"10.0.0.1", "1.0.32.172.in-addr.arpa.", "", false},
		{"10.0.0.1", "8.8.8.8.in-addr.arpa.", "", false},
		{"10.0.0.1", "x.example.", "", false},
	} {
		e := ev(tc.client, tc.name)
		e.ClientName = tc.clientName
		if got := p.Excluded(e); got != tc.excluded {
			t.Errorf("%s %s %q: excluded=%v, want %v", tc.client, tc.name, tc.clientName, got, tc.excluded)
		}
	}
	if _, err := NewPrivacy(PrivacyOptions{ExcludeDomains: []string{"/([/"}}); err == nil {
		t.Error("bad regex accepted")
	}
}

func TestPrivacyFrom(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PrivacyFrom(cfg.Querylog)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != PrivacyFull || !p.sudn || p.pseudonym {
		t.Errorf("defaults: mode %s sudn %v pseudonym %v", p.Mode(), p.sudn, p.pseudonym)
	}
	cfg.Querylog.Privacy, cfg.Querylog.Anonymize = "anonymize_client", AnonymizePseudonym
	if p, _ = PrivacyFrom(cfg.Querylog); p.Mode() != PrivacyAnonymizeClient || !p.pseudonym {
		t.Errorf("pseudonym config not applied")
	}
	cfg.Querylog.Privacy = "loose"
	if _, err := PrivacyFrom(cfg.Querylog); err == nil {
		t.Error("unknown mode accepted")
	}
	for i, n := range modeNames {
		if m, err := ParseMode(n); err != nil || int(m) != i || m.String() != n {
			t.Errorf("ParseMode(%q) = %v, %v", n, m, err)
		}
	}
}

func TestFlagsRoundTrip(t *testing.T) {
	for b := uint16(0); b < 1<<10; b++ {
		if got := FlagsFromBits(b).Bits(); got != b {
			t.Fatalf("bits %b round-trip to %b", b, got)
		}
	}
}

func TestNames(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint16
		ok   bool
	}{{"a", 1, true}, {"HTTPS", 65, true}, {"type65280", 65280, true}, {"TYPEx", 0, false}, {"nope", 0, false}} {
		if v, ok := ParseQType(tc.in); v != tc.want || ok != tc.ok {
			t.Errorf("ParseQType(%q) = %d, %v", tc.in, v, ok)
		}
	}
	if QTypeName(28) != "AAAA" || QTypeName(65280) != "TYPE65280" || RcodeName(3) != "NXDOMAIN" || RcodeName(99) != "RCODE99" {
		t.Error("name tables")
	}
	if s, ok := ParseStatus("blocked_cname"); !ok || s != pipeline.StatusBlockedCNAME {
		t.Errorf("ParseStatus(blocked_cname) = %v %v", s, ok)
	}
	if s, ok := ParseStatus("unknown"); !ok || s != pipeline.StatusUnknown {
		t.Errorf("ParseStatus(unknown) = %v %v", s, ok)
	}
	if _, ok := ParseStatus("bogus"); ok {
		t.Error("ParseStatus accepted bogus")
	}
	if ReplyNXDomain.String() != "nxdomain" || ReplyType(200).String() != "unknown" || DNSSECBogus.String() != "bogus" {
		t.Error("enum names")
	}
}

func TestTail(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	tail := NewTail(4, 2, m)
	ctx, cancel := context.WithCancel(context.Background())
	slow := tail.Subscribe(ctx)
	for i := range 6 {
		tail.Publish(&Event{ID: uint64(i + 1)})
	}
	// The ring keeps the newest four, oldest first.
	var ids []uint64
	for _, e := range tail.Recent(0, 0) {
		ids = append(ids, e.ID)
	}
	if len(ids) != 4 || ids[0] != 3 || ids[3] != 6 {
		t.Errorf("ring %v", ids)
	}
	if r := tail.Recent(2, 0); len(r) != 2 || r[0].ID != 5 {
		t.Errorf("Recent(2) %v", r)
	}
	if r := tail.Recent(0, 4); len(r) != 2 || r[0].ID != 5 {
		t.Errorf("Recent after 4: %d", len(r))
	}
	// The slow subscriber (buffer 2) got the first two and lost four.
	if e := <-slow; e.ID != 1 {
		t.Errorf("first event %d", e.ID)
	}
	if got := testutil.ToFloat64(m.Dropped.WithLabelValues(DropStageTail, "")); got != 4 {
		t.Errorf("tail drops = %v", got)
	}
	cancel()
	for range slow {
	}
	if tail.Subscribers() != 0 {
		t.Error("subscriber not removed")
	}
	none := NewTail(-1, 0, nil)
	none.Publish(&Event{ID: 1})
	if len(none.Recent(0, 0)) != 0 {
		t.Error("ring disabled but kept events")
	}
}

// recSink records batches.
type recSink struct {
	mu      sync.Mutex
	batches [][]*Event
	fail    bool
	closed  bool
}

func (s *recSink) Name() string                                                       { return "rec" }
func (s *recSink) Open(context.Context, json.RawMessage, prometheus.Registerer) error { return nil }
func (s *recSink) Flush(context.Context) error                                        { return nil }
func (s *recSink) Close() error                                                       { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return nil }
func (s *recSink) Write(b []*Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("disk full")
	}
	s.batches = append(s.batches, append([]*Event(nil), b...))
	return nil
}
func (s *recSink) count() (batches, events int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.batches {
		events += len(b)
	}
	return len(s.batches), events
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAsyncBatchPolicyAndIDs(t *testing.T) {
	fc := testclock.NewFakeClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	sink := &recSink{}
	tail := NewTail(16, 16, nil)
	p, _ := NewPrivacy(PrivacyOptions{ExcludeDomains: []string{"skip.example."}})
	a := NewAsyncWith(AsyncOptions{Sink: sink, BatchSize: 3, Interval: time.Second, Clock: fc, FirstID: 100, Privacy: p, Tail: tail,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, n := range []string{"a.example.", "skip.example.", "b.example.", "c.example.", "d.example."} {
		a.Enqueue(ev("10.0.0.1", n))
	}
	waitFor(t, "size flush", func() bool { b, _ := sink.count(); return b == 1 })
	waitFor(t, "ticker", fc.HasWaiters)
	fc.Step(time.Second)
	waitFor(t, "tick flush", func() bool { b, _ := sink.count(); return b == 2 })
	sink.mu.Lock()
	first, second := sink.batches[0], sink.batches[1]
	sink.mu.Unlock()
	if len(first) != 3 || first[0].ID != 100 || first[2].ID != 102 || len(second) != 1 || second[0].ID != 103 || second[0].Name != "d.example." {
		t.Errorf("batches %d/%d, ids %d..%d, %d", len(first), len(second), first[0].ID, first[2].ID, second[0].ID)
	}
	if r := tail.Recent(0, 0); len(r) != 4 {
		t.Errorf("tail saw %d events (excluded ones must not appear)", len(r))
	}
	if err := a.Close(context.Background()); err != nil || !sink.closed {
		t.Errorf("close: %v closed=%v", err, sink.closed)
	}
}

func TestAsyncCountsSinkFailures(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	sink := &recSink{fail: true}
	a := NewAsyncWith(AsyncOptions{Sink: sink, BatchSize: 2, Metrics: m, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	a.Enqueue(ev("10.0.0.1", "a."))
	a.Enqueue(ev("10.0.0.1", "b."))
	_ = a.Close(context.Background())
	if got := testutil.ToFloat64(m.Dropped.WithLabelValues(DropStageSink, "rec")); got != 2 {
		t.Errorf("dropped{stage=sink,sink=rec} = %v", got)
	}
	if got := testutil.ToFloat64(m.Events); got != 2 {
		t.Errorf("events_total = %v", got)
	}
}

func TestAsyncQueueOverflow(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	block := make(chan struct{})
	sink := &blockingSink{recSink: &recSink{}, block: block}
	a := NewAsyncWith(AsyncOptions{Sink: sink, QueueSize: 2, BatchSize: 1, Metrics: m, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	accepted := 0
	for range 10 {
		if a.Enqueue(ev("10.0.0.1", "a.")) {
			accepted++
		}
	}
	close(block)
	_ = a.Close(context.Background())
	if a.Dropped() != uint64(10-accepted) || accepted < 2 || accepted > 3 {
		t.Errorf("accepted %d, dropped %d", accepted, a.Dropped())
	}
	if got := testutil.ToFloat64(m.Dropped.WithLabelValues(DropStageQueue, "")); got != float64(a.Dropped()) {
		t.Errorf("dropped{stage=queue} = %v", got)
	}
}

type blockingSink struct {
	*recSink
	block chan struct{}
}

func (s *blockingSink) Write(b []*Event) error { <-s.block; return s.recSink.Write(b) }

func TestIsPowerOfTen(t *testing.T) {
	for n, want := range map[uint64]bool{1: true, 10: true, 1000: true, 0: false, 20: false, 110: false} {
		if isPowerOfTen(n) != want {
			t.Errorf("isPowerOfTen(%d) != %v", n, want)
		}
	}
}
