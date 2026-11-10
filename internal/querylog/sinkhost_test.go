package querylog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// fanSink is an additional-sink double: it records what the host hands it
// and can fail, or block inside a write, on demand.
type fanSink struct {
	mu       sync.Mutex
	opens    int
	closes   int
	flushes  int
	calls    int
	batches  [][]*Event
	modes    []Mode
	openErrs []error
	// writeErr decides the result of the n-th write (1-based); nil writes.
	writeErr func(call int) error
	// entered receives once per write that started.
	entered chan struct{}
	// block holds every write until it is closed. With honorCtx the write
	// also returns when its context ends.
	block    chan struct{}
	honorCtx bool
}

func (s *fanSink) Name() string { return "fan" }
func (s *fanSink) Open(context.Context, json.RawMessage, prometheus.Registerer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	if len(s.openErrs) > 0 {
		err := s.openErrs[0]
		s.openErrs = s.openErrs[1:]
		return err
	}
	return nil
}
func (s *fanSink) Flush(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushes++
	return nil
}
func (s *fanSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}
func (s *fanSink) Write(b []*Event) error { return s.WriteBatch(context.Background(), b, PrivacyFull) }
func (s *fanSink) WriteBatch(ctx context.Context, b []*Event, mode Mode) error {
	s.mu.Lock()
	s.calls++
	call, fail, block, entered := s.calls, s.writeErr, s.block, s.entered
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if block != nil {
		if s.honorCtx {
			select {
			case <-block:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			<-block
		}
	}
	if fail != nil {
		if err := fail(call); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]*Event(nil), b...))
	s.modes = append(s.modes, mode)
	return nil
}

func (s *fanSink) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.batches {
		for _, e := range b {
			out = append(out, e.Name)
		}
	}
	return out
}

func (s *fanSink) state() (opens, closes, calls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens, s.closes, s.calls
}

func (s *fanSink) flushed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushes
}

// gatedSink is a primary sink whose writes can be held and let go.
type gatedSink struct {
	*recSink
	mu   sync.Mutex
	gate chan struct{}
}

func (s *gatedSink) hold() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = make(chan struct{})
}

func (s *gatedSink) open() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.gate)
	s.gate = nil
}

func (s *gatedSink) Write(b []*Event) error {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return s.recSink.Write(b)
}

// fanFixture is a collector with a recording primary sink, a tail and a
// sink host whose "test" type hands out the doubles by name.
type fanFixture struct {
	t       *testing.T
	reg     *prometheus.Registry
	metrics *Metrics
	primary *recSink
	tail    *Tail
	async   *Async
	host    *SinkHost
	sinks   map[string]*fanSink
	built   map[string]int
	mu      sync.Mutex
}

func newFanFixture(t *testing.T, primary Sink, privacy *Privacy) *fanFixture {
	t.Helper()
	f := &fanFixture{t: t, reg: prometheus.NewRegistry(), sinks: map[string]*fanSink{}, built: map[string]int{}}
	f.metrics = NewMetrics(f.reg)
	f.tail = NewTail(256, 256, f.metrics)
	if primary == nil {
		f.primary = &recSink{}
		primary = f.primary
	}
	logger := slog.New(slog.DiscardHandler)
	f.async = NewAsyncWith(AsyncOptions{Sink: primary, BatchSize: 1, QueueSize: 256, Tail: f.tail, Metrics: f.metrics, FirstID: 1, Privacy: privacy, Logger: logger})
	f.host = NewSinkHost(SinkHostOptions{
		Collector: f.async, Metrics: f.metrics, Registerer: f.reg, Logger: logger, Drain: 2 * time.Second,
		Factories: map[string]SinkFactory{"test": func(spec SinkSpec) (Sink, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.built[spec.Name]++
			if strings.HasPrefix(spec.Name, "refused") {
				return nil, errors.New("bad settings")
			}
			s := f.sinks[spec.Name]
			if s == nil {
				s = &fanSink{}
				f.sinks[spec.Name] = s
			}
			return s, nil
		}},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.async.Close(ctx)
		_ = f.host.Close(ctx)
	})
	return f
}

func (f *fanFixture) sink(name string, s *fanSink) *fanSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinks[name] = s
	return s
}

func (f *fanFixture) get(name string) *fanSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sinks[name]
}

// route prepares and adopts a route feeding specs, like a reload.
func (f *fanFixture) route(p *Privacy, previous *Captured, specs ...SinkSpec) *Captured {
	f.t.Helper()
	set, err := f.host.Prepare(specs)
	if err != nil {
		f.t.Fatal(err)
	}
	var primary Sink = f.primary
	if f.primary == nil {
		primary = f.async.sink
	}
	route := f.async.PrepareCapturedSinks(primary, p, true, 1, previous, set)
	if !f.async.AdoptCaptured(route) {
		f.t.Fatal("route not adopted")
	}
	return route
}

func (f *fanFixture) status(name string) SinkStatus {
	for _, st := range f.host.Status() {
		if st.Name == name {
			return st
		}
	}
	return SinkStatus{}
}

func (f *fanFixture) dropped(name string) float64 {
	return testutil.ToFloat64(f.metrics.Dropped.WithLabelValues(DropStageSink, name))
}

func spec(name string) SinkSpec {
	return SinkSpec{Name: name, Type: "test", QueueSize: 64, BatchSize: 1, Interval: 10 * time.Millisecond, Timeout: time.Second, Attempts: 3, Backoff: time.Millisecond}
}

// TestSinkFanoutBlockedSinkNeverStallsPrimaryOrTail is the D371 isolation
// rule at the collector seam: a sink that never returns from a write loses
// only its own events, and exactly the ones beyond its queue.
func TestSinkFanoutBlockedSinkNeverStallsPrimaryOrTail(t *testing.T) {
	f := newFanFixture(t, nil, nil)
	blocked := f.sink("stalled", &fanSink{block: make(chan struct{}), entered: make(chan struct{}, 1)})
	stalled := spec("stalled")
	stalled.QueueSize = 4
	route := f.route(nil, nil, stalled, spec("healthy"))
	sub := f.tail.Subscribe(t.Context())

	if !route.Enqueue(ev("192.0.2.1", "first.example.")) {
		t.Fatal("first event refused")
	}
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled sink never received its first batch")
	}
	const total = 50
	for i := 1; i < total; i++ {
		if !route.Enqueue(ev("192.0.2.1", fmt.Sprintf("q%02d.example.", i))) {
			t.Fatalf("the collector refused event %d while an additional sink was stalled", i)
		}
	}
	waitFor(t, "primary sink to commit every event", func() bool { _, n := f.primary.count(); return n == total })
	waitFor(t, "healthy sink to receive every event", func() bool { return len(f.get("healthy").names()) == total })
	if got := len(f.tail.Recent(0, 0)); got != total {
		t.Errorf("tail holds %d events, want %d", got, total)
	}
	for i := range total {
		select {
		case <-sub:
		default:
			t.Fatalf("live-tail subscriber received %d events, want %d", i, total)
		}
	}
	// One batch is inside the blocked write, four wait in the queue, the
	// rest overflowed and were counted for this sink alone.
	const overflow = total - 1 - 4
	if got := f.dropped("stalled"); got != overflow {
		t.Errorf("dropped{stage=sink,sink=stalled} = %v, want %d", got, overflow)
	}
	if got := f.dropped("healthy"); got != 0 {
		t.Errorf("the healthy sink dropped %v events", got)
	}
	if got := testutil.ToFloat64(f.metrics.Dropped.WithLabelValues(DropStageQueue, "")); got != 0 {
		t.Errorf("collector queue dropped %v events", got)
	}
	if st := f.status("stalled"); st.Queued != 4 || st.QueueSize != 4 || st.Dropped != overflow || st.Sent != 0 {
		t.Errorf("stalled status %+v", st)
	}
	expected := `
# HELP kudurru_sink_queue_length Query events waiting in the queue of an additional query-log sink.
# TYPE kudurru_sink_queue_length gauge
kudurru_sink_queue_length{sink="healthy"} 0
kudurru_sink_queue_length{sink="stalled"} 4
# HELP kudurru_sink_sent_total Query events an additional query-log sink accepted.
# TYPE kudurru_sink_sent_total counter
kudurru_sink_sent_total{sink="healthy"} 50
kudurru_sink_sent_total{sink="stalled"} 0
`
	if err := testutil.GatherAndCompare(f.reg, strings.NewReader(expected), "kudurru_sink_queue_length", "kudurru_sink_sent_total"); err != nil {
		t.Error(err)
	}
	close(blocked.block)
	waitFor(t, "released sink to write what it held", func() bool { return len(blocked.names()) == 5 })
	got := blocked.names()
	if got[0] != "first.example." || got[4] != "q04.example." {
		t.Errorf("the stalled sink wrote %v: it must keep the oldest queued events in order", got)
	}
	if st := f.status("stalled"); st.Sent != 5 || st.LastSuccess.IsZero() {
		t.Errorf("after release: %+v", st)
	}
}

// TestSinkRetriesAreBoundedAndOnlyForRetryableFailures: a batch is written
// again only when the sink says it was not applied, and at most Attempts
// times; every abandoned batch is counted for its sink.
func TestSinkRetriesAreBoundedAndOnlyForRetryableFailures(t *testing.T) {
	busy := errors.New("try again")
	for _, tc := range []struct {
		name                string
		fail                func(call int) error
		calls               int
		sent, drops, errors uint64
	}{
		{"retryable then accepted", func(call int) error {
			if call < 3 {
				return Retryable(busy)
			}
			return nil
		}, 3, 1, 0, 2},
		{"retryable until the budget ends", func(int) error { return RetryableAfter(busy, time.Millisecond) }, 3, 0, 1, 3},
		{"uncertain outcome is never replayed", func(int) error { return errors.New("connection reset mid-write") }, 1, 0, 1, 1},
		{"wrapped retryable", func(call int) error {
			if call == 1 {
				return fmt.Errorf("push: %w", Retryable(busy))
			}
			return nil
		}, 2, 1, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFanFixture(t, nil, nil)
			sink := f.sink("flaky", &fanSink{writeErr: tc.fail})
			route := f.route(nil, nil, spec("flaky"))
			route.Enqueue(ev("192.0.2.1", "one.example."))
			waitFor(t, "the batch to be settled", func() bool {
				st := f.status("flaky")
				return st.Sent+st.Dropped == 1
			})
			_, _, calls := sink.state()
			st := f.status("flaky")
			if calls != tc.calls || st.Sent != tc.sent || st.Dropped != tc.drops || st.Errors != tc.errors {
				t.Errorf("calls %d sent %d dropped %d errors %d, want %d/%d/%d/%d", calls, st.Sent, st.Dropped, st.Errors, tc.calls, tc.sent, tc.drops, tc.errors)
			}
			if got := f.dropped("flaky"); got != float64(tc.drops) {
				t.Errorf("dropped{stage=sink,sink=flaky} = %v, want %d", got, tc.drops)
			}
			if _, n := f.primary.count(); n != 1 {
				t.Errorf("primary sink has %d events", n)
			}
			// The next batch is independent of the failed one.
			f.sinks["flaky"].mu.Lock()
			f.sinks["flaky"].writeErr = nil
			f.sinks["flaky"].mu.Unlock()
			route.Enqueue(ev("192.0.2.1", "two.example."))
			waitFor(t, "the following batch", func() bool { return f.status("flaky").Sent == tc.sent+1 })
			names := sink.names()
			if names[len(names)-1] != "two.example." || uint64(len(names)) != tc.sent+1 {
				t.Errorf("sink holds %v: a dropped batch must not be replayed later", names)
			}
		})
	}
}

// TestSinkReloadAddsAndRemovesWithoutReplay follows three routes through
// reloads: an unchanged sink keeps its instance, a new sink starts with new
// events only, and a removed sink still gets what was queued for it once.
func TestSinkReloadAddsAndRemovesWithoutReplay(t *testing.T) {
	primary := &gatedSink{recSink: &recSink{}}
	f := newFanFixture(t, primary, nil)
	emit := func(route *Captured, names ...string) {
		t.Helper()
		for _, n := range names {
			if !route.Enqueue(ev("192.0.2.1", n)) {
				t.Fatalf("event %s refused", n)
			}
		}
	}
	first := f.route(nil, nil, spec("a"))
	emit(first, "1.", "2.", "3.")
	waitFor(t, "a to receive the first events", func() bool { return len(f.get("a").names()) == 3 })

	second := f.route(nil, first, spec("a"), spec("b"))
	first.Release()
	emit(second, "4.", "5.", "6.")
	waitFor(t, "both sinks", func() bool { return len(f.get("a").names()) == 6 && len(f.get("b").names()) == 3 })
	if f.built["a"] != 1 {
		t.Fatalf("the unchanged sink was built %d times", f.built["a"])
	}
	if opens, closes, _ := f.get("a").state(); opens != 1 || closes != 0 {
		t.Fatalf("the unchanged sink was reopened or closed: opens %d closes %d", opens, closes)
	}

	// A route that was prepared and rejected starts nothing.
	rejected, err := f.host.Prepare([]SinkSpec{spec("a"), spec("never")})
	if err != nil {
		t.Fatal(err)
	}
	rejected.Release()
	if f.get("never") == nil || f.built["never"] != 1 {
		t.Fatal("the candidate's sink was not built")
	}
	if opens, closes, calls := f.get("never").state(); opens != 0 || closes != 0 || calls != 0 {
		t.Fatalf("a rejected candidate opened its sink: %d/%d/%d", opens, closes, calls)
	}
	waitFor(t, "the rejected candidate's sink to be forgotten", func() bool { return f.status("never").Name == "" })
	if opens, closes, calls := f.get("never").state(); opens != 0 || closes != 0 || calls != 0 {
		t.Fatalf("a rejected candidate's sink was opened on release: %d/%d/%d", opens, closes, calls)
	}

	// Remove a. Hold the collector in its primary write so that events are
	// still queued when the last reference on a is released: a must receive
	// them, and nothing after.
	primary.hold()
	emit(second, "7.")
	waitFor(t, "collector to enter the primary write", func() bool { return len(f.async.queue) == 0 })
	emit(second, "8.", "9.")
	third := f.route(nil, second, spec("b"))
	second.Release()
	emit(third, "10.")
	primary.open()
	waitFor(t, "b to receive everything", func() bool { return len(f.get("b").names()) == 7 })
	waitFor(t, "a to be closed", func() bool { _, closes, _ := f.get("a").state(); return closes == 1 })
	if got := f.get("a").flushed(); got != 1 {
		t.Errorf("the removed sink was flushed %d times before it closed, want once", got)
	}

	if got, want := f.get("a").names(), []string{"1.", "2.", "3.", "4.", "5.", "6.", "7.", "8.", "9."}; !reflect.DeepEqual(got, want) {
		t.Errorf("removed sink received %v, want %v", got, want)
	}
	if got, want := f.get("b").names(), []string{"4.", "5.", "6.", "7.", "8.", "9.", "10."}; !reflect.DeepEqual(got, want) {
		t.Errorf("added sink received %v, want %v (no replay of earlier events)", got, want)
	}
	if st := f.status("a"); st.Name != "" {
		t.Errorf("removed sink still reported: %+v", st)
	}
	metrics, err := f.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range metrics {
		for _, m := range family.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "sink" && l.GetValue() == "a" {
					t.Errorf("%s keeps a series for the removed sink", family.GetName())
				}
			}
		}
	}
	// A held request of the old route cannot revive the removed sink.
	emit(second, "11.")
	waitFor(t, "b to receive the held event", func() bool { return len(f.get("b").names()) == 8 })
	if got := len(f.get("a").names()); got != 9 {
		t.Errorf("the closed sink received an event after removal (%d)", got)
	}
	if opens, closes, _ := f.get("b").state(); opens != 1 || closes != 0 || f.built["b"] != 1 {
		t.Errorf("the surviving sink was restarted: opens %d closes %d built %d", opens, closes, f.built["b"])
	}
	_, n := primary.count()
	if n != 11 {
		t.Errorf("primary sink has %d events, want 11", n)
	}
	ids := map[uint64]bool{}
	for _, e := range f.tail.Recent(0, 0) {
		if ids[e.ID] {
			t.Errorf("tail id %d published twice", e.ID)
		}
		ids[e.ID] = true
	}
	if len(ids) != 11 {
		t.Errorf("tail holds %d distinct events, want 11", len(ids))
	}
}

// TestSinkChangedEntryStartsANewSinkAndDrainsTheOld: any change of an entry
// is a removal and an addition under the same name and series.
func TestSinkChangedEntryStartsANewSinkAndDrainsTheOld(t *testing.T) {
	f := newFanFixture(t, nil, nil)
	first := f.route(nil, nil, spec("x"))
	first.Enqueue(ev("192.0.2.1", "old."))
	waitFor(t, "first instance", func() bool { return len(f.get("x").names()) == 1 })
	old := f.get("x")
	f.sink("x", &fanSink{})
	changed := spec("x")
	changed.BatchSize = 2
	second := f.route(nil, first, changed)
	first.Release()
	second.Enqueue(ev("192.0.2.1", "new-1."))
	second.Enqueue(ev("192.0.2.1", "new-2."))
	waitFor(t, "second instance", func() bool { return len(f.get("x").names()) == 2 })
	waitFor(t, "first instance closed", func() bool { _, closes, _ := old.state(); return closes == 1 })
	if got := old.names(); !reflect.DeepEqual(got, []string{"old."}) {
		t.Errorf("old instance wrote %v", got)
	}
	if st := f.status("x"); st.Sent != 3 || st.Type != "test" {
		t.Errorf("the name's counters must continue across the change: %+v", st)
	}
}

// sinkSeries reports whether any metric family keeps a series labeled with
// the sink name.
func (f *fanFixture) sinkSeries(name string) []string {
	f.t.Helper()
	families, err := f.reg.Gather()
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, family := range families {
		for _, m := range family.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "sink" && l.GetValue() == name {
					out = append(out, family.GetName())
				}
			}
		}
	}
	return out
}

// TestSinkEventInFlightAtReplacementIsCounted: where requests do not hold
// their route (the reload subscriber of the Kubernetes driver), an
// enqueuer may have read the collector's route just before a reload
// replaced it and queue its event after the old instance of a changed entry
// has retired. That event is not handed to the new instance, which belongs
// to a route with its own privacy decision; it is counted for the name. A
// name that was removed receives nothing and counts nothing.
func TestSinkEventInFlightAtReplacementIsCounted(t *testing.T) {
	f := newFanFixture(t, nil, nil)
	first := f.route(nil, nil, spec("x"))
	first.Enqueue(ev("192.0.2.1", "old."))
	waitFor(t, "first instance", func() bool { return len(f.get("x").names()) == 1 })
	old := f.get("x")
	f.sink("x", &fanSink{})
	changed := spec("x")
	changed.BatchSize = 2
	second := f.route(nil, first, changed) // the reload adopts the successor ...
	first.Release()                        // ... then returns the predecessor's references
	waitFor(t, "old instance closed", func() bool { _, closes, _ := old.state(); return closes == 1 })
	// The enqueuer that had read the old route queues its event only now.
	if !first.Enqueue(ev("192.0.2.1", "in-flight.")) {
		t.Fatal("the collector refused the in-flight event")
	}
	second.Enqueue(ev("192.0.2.1", "new-1."))
	second.Enqueue(ev("192.0.2.1", "new-2."))
	waitFor(t, "second instance", func() bool { return len(f.get("x").names()) == 2 })
	if _, n := f.primary.count(); n != 4 {
		t.Fatalf("primary sink has %d events, want 4", n)
	}
	if got := old.names(); !reflect.DeepEqual(got, []string{"old."}) {
		t.Errorf("the retired instance wrote %v", got)
	}
	if got := f.get("x").names(); !reflect.DeepEqual(got, []string{"new-1.", "new-2."}) {
		t.Errorf("the new instance wrote %v: an event of the replaced route must not move to it", got)
	}
	if st := f.status("x"); st.Sent != 3 || st.Dropped != 1 {
		t.Errorf("4 events were recorded locally; sink x wrote %d and counted %d dropped, want 3 and 1", st.Sent, st.Dropped)
	}
	if got := f.dropped("x"); got != 1 {
		t.Errorf("dropped{stage=sink,sink=x} = %v, want 1", got)
	}

	// Remove x. A late event of a route that named it is not a loss: the
	// sink is gone, and its series must not come back.
	third := f.route(nil, second)
	second.Release()
	waitFor(t, "removed sink to be forgotten", func() bool { return f.status("x").Name == "" })
	if !second.Enqueue(ev("192.0.2.1", "after-removal.")) {
		t.Fatal("the collector refused the late event")
	}
	waitFor(t, "primary sink", func() bool { _, n := f.primary.count(); return n == 5 })
	if st := f.status("x"); st.Name != "" {
		t.Errorf("a removed sink is reported again: %+v", st)
	}
	if got := f.sinkSeries("x"); len(got) != 0 {
		t.Errorf("a late event revived series of the removed sink: %v", got)
	}
	if got := len(f.get("x").names()); got != 2 {
		t.Errorf("the removed sink received an event after removal (%d)", got)
	}
	third.Release()
}

// TestSinkReplacementAccountsForEveryAcceptedEvent drives the same path
// under load through Async.Enqueue, which reads the adopted route and then
// queues: while an entry is replaced again and again, every event the
// collector accepted is either written by an instance of the sink or counted
// as dropped for its name.
func TestSinkReplacementAccountsForEveryAcceptedEvent(t *testing.T) {
	f := newFanFixture(t, nil, nil)
	entry := spec("x")
	entry.QueueSize, entry.BatchSize, entry.Interval = 4096, 64, 5*time.Millisecond
	current := f.route(nil, nil, entry)
	stop := make(chan struct{})
	var accepted atomic.Uint64
	var enqueuers sync.WaitGroup
	for range 4 {
		enqueuers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if f.async.Enqueue(ev("192.0.2.1", "q.")) {
					accepted.Add(1)
				}
			}
		})
	}
	for i := range 300 {
		next := entry
		next.BatchSize = 16 + i%8 // any change is a new sink under the same name
		successor := f.route(nil, current, next)
		current.Release()
		current = successor
		time.Sleep(200 * time.Microsecond)
	}
	close(stop)
	enqueuers.Wait()
	if err := f.async.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The collector has offered everything it accepted. The last instance
	// is still referenced, so the name's counters are still reported.
	want := accepted.Load()
	var st SinkStatus
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		st = f.status("x")
		if st.Sent+st.Dropped >= want || time.Now().After(deadline) {
			break
		}
	}
	written := uint64(len(f.get("x").names()))
	if st.Sent != written {
		t.Errorf("sink x wrote %d events, its counter says %d", written, st.Sent)
	}
	if st.Sent+st.Dropped != want {
		t.Fatalf("the collector accepted %d events; sink x wrote %d and counted %d dropped: the rest is unaccounted for", want, st.Sent, st.Dropped)
	}
	if _, n := f.primary.count(); uint64(n) != want { // #nosec G115 -- a count is never negative
		t.Errorf("primary sink has %d of %d events", n, want)
	}
	t.Logf("accepted %d, written %d, counted as dropped %d", want, st.Sent, st.Dropped)
}

// TestSinkPrepareRefusals: a refused set changes nothing.
func TestSinkPrepareRefusals(t *testing.T) {
	f := newFanFixture(t, nil, nil)
	kept := f.route(nil, nil, spec("kept"))
	for name, specs := range map[string][]SinkSpec{
		"unknown type":   {spec("kept"), {Name: "x", Type: "kafka"}},
		"duplicate name": {spec("kept"), spec("kept")},
		"empty name":     {{Type: "test"}},
		"factory error":  {spec("fresh"), spec("refused")},
	} {
		if set, err := f.host.Prepare(specs); err == nil || set != nil {
			t.Errorf("%s: prepared", name)
		}
	}
	if st := f.host.Status(); len(st) != 1 || st[0].Name != "kept" {
		t.Errorf("refused sets changed the host: %+v", st)
	}
	kept.Enqueue(ev("192.0.2.1", "still."))
	waitFor(t, "the kept sink", func() bool { return len(f.get("kept").names()) == 1 })
	if err := f.host.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.Prepare([]SinkSpec{spec("late")}); !errors.Is(err, ErrSinkHostClosed) {
		t.Errorf("prepare after close: %v", err)
	}
}

// TestSinkOpenFailureNeverBlocksAndRecovers: an exporter that is down when
// its first event arrives delays nobody; it is opened again with backoff
// and then receives what its queue kept.
func TestSinkOpenFailureNeverBlocksAndRecovers(t *testing.T) {
	f := newFanFixture(t, nil, nil)
	down := errors.New("connection refused")
	sink := f.sink("late", &fanSink{openErrs: []error{down, down}})
	route := f.route(nil, nil, spec("late"))
	for i := range 3 {
		route.Enqueue(ev("192.0.2.1", fmt.Sprintf("%d.", i)))
	}
	waitFor(t, "primary", func() bool { _, n := f.primary.count(); return n == 3 })
	waitFor(t, "sink after reopening", func() bool { return len(sink.names()) == 3 })
	opens, _, _ := sink.state()
	if st := f.status("late"); opens != 3 || st.Errors != 2 || st.Dropped != 0 {
		t.Errorf("opens %d, status %+v", opens, st)
	}
	// A sink that never opens is not closed, and what it held is counted.
	never := f.sink("never-up", &fanSink{openErrs: []error{down, down, down}})
	slow := spec("never-up")
	slow.Backoff = time.Hour
	next := f.route(nil, route, spec("late"), slow)
	route.Release()
	next.Enqueue(ev("192.0.2.1", "lost."))
	waitFor(t, "first open attempt", func() bool { opens, _, _ := never.state(); return opens == 1 })
	last := f.route(nil, next, spec("late"))
	next.Release()
	waitFor(t, "unopened sink to be retired", func() bool { return f.status("never-up").Name == "" })
	if opens, closes, calls := never.state(); opens != 1 || closes != 0 || calls != 0 {
		t.Errorf("unopened sink: opens %d closes %d calls %d", opens, closes, calls)
	}
	last.Release()
}

// TestSinkHostCloseIsBounded: shutdown waits for a drain only as long as
// its context allows, cancels the pending write and never closes a sink
// under a write that ignores cancellation.
func TestSinkHostCloseIsBounded(t *testing.T) {
	f := newFanFixture(t, nil, nil)
	polite := f.sink("polite", &fanSink{block: make(chan struct{}), honorCtx: true, entered: make(chan struct{}, 1)})
	deaf := f.sink("deaf", &fanSink{block: make(chan struct{}), entered: make(chan struct{}, 1)})
	route := f.route(nil, nil, spec("polite"), spec("deaf"))
	for i := range 3 {
		route.Enqueue(ev("192.0.2.1", fmt.Sprintf("%d.", i)))
	}
	for _, s := range []*fanSink{polite, deaf} {
		select {
		case <-s.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("sink did not start its write")
		}
	}
	if err := f.async.Close(t.Context()); err != nil {
		t.Fatal("the collector must drain while additional sinks are blocked:", err)
	}
	if _, n := f.primary.count(); n != 3 || !f.primary.closed {
		t.Fatalf("primary sink: %d events, closed %v", n, f.primary.closed)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := f.host.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close with blocked sinks: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("close took %s", took)
	}
	waitFor(t, "the canceled sink to close", func() bool { _, closes, _ := polite.state(); return closes == 1 })
	if st := f.status("polite"); st.Name != "" {
		t.Errorf("canceled sink still running: %+v", st)
	}
	if got := f.dropped("deaf"); got != 0 {
		t.Errorf("a write that is still pending was counted as dropped: %v", got)
	}
	if _, closes, _ := deaf.state(); closes != 0 {
		t.Fatal("a sink was closed under its own pending write")
	}
	close(deaf.block)
	if err := f.host.Close(t.Context()); err != nil {
		t.Fatal("second close did not join:", err)
	}
	if _, closes, _ := deaf.state(); closes != 1 {
		t.Error("the released sink was not closed")
	}
	// The write that was pending completed; the two queued behind it met
	// the canceled drain and are counted.
	if got := deaf.names(); len(got) != 1 {
		t.Errorf("deaf sink wrote %v", got)
	}
}

// TestSinkReceivesOnlyTransformedEvents: the privacy transform and the
// exclusions run once, before any sink queue; under nothing no per-query
// event reaches an additional sink.
func TestSinkReceivesOnlyTransformedEvents(t *testing.T) {
	hide, err := NewPrivacy(PrivacyOptions{Mode: PrivacyHideDomainsClients, ExcludeDomains: []string{"skip.example."}})
	if err != nil {
		t.Fatal(err)
	}
	f := newFanFixture(t, nil, hide)
	first := f.route(hide, nil, spec("out"))
	first.Enqueue(ev("192.168.1.77", "secret.example."))
	first.Enqueue(ev("192.168.1.77", "skip.example."))
	waitFor(t, "hidden event", func() bool { return len(f.get("out").names()) == 1 })
	sink := f.get("out")
	sink.mu.Lock()
	got, mode := sink.batches[0][0], sink.modes[0]
	sink.mu.Unlock()
	if got.Name != HiddenName || got.Client.String() != "0.0.0.0" || got.ClientName != "" || got.Answer != "" || got.ID == 0 {
		t.Errorf("additional sink saw an untransformed event: %+v", got)
	}
	if mode != PrivacyHideDomainsClients {
		t.Errorf("mode %s", mode)
	}
	// The same event object reaches the primary sink and the tail.
	f.primary.mu.Lock()
	same := f.primary.batches[0][0] == got
	f.primary.mu.Unlock()
	if !same || f.tail.Recent(0, 0)[0] != got {
		t.Error("the sinks must share the one transformed event")
	}
	nothing, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyNothing})
	second := f.route(nothing, first, spec("out"))
	first.Release()
	second.Enqueue(ev("192.168.1.77", "totals-only.example."))
	waitFor(t, "primary under nothing", func() bool { _, n := f.primary.count(); return n == 2 })
	full, _ := NewPrivacy(PrivacyOptions{Mode: PrivacyFull})
	third := f.route(full, second, spec("out"))
	second.Release()
	third.Enqueue(ev("192.168.1.77", "visible.example."))
	waitFor(t, "full event", func() bool { return len(sink.names()) == 2 })
	if got := sink.names(); !reflect.DeepEqual(got, []string{HiddenName, "visible.example."}) {
		t.Errorf("sink received %v: nothing must export no per-query event", got)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.modes[1] != PrivacyFull || len(sink.batches) != 2 {
		t.Errorf("a batch mixed privacy modes: %v", sink.modes)
	}
}

// TestSinkDisabledRouteFeedsNoSink: querylog.enabled off stops every sink.
func TestSinkDisabledRouteFeedsNoSink(t *testing.T) {
	f := newFanFixture(t, nil, nil)
	set, err := f.host.Prepare([]SinkSpec{spec("out")})
	if err != nil {
		t.Fatal(err)
	}
	off := f.async.PrepareCapturedSinks(f.primary, nil, false, 1, nil, set)
	f.async.AdoptCaptured(off)
	if off.Enqueue(ev("192.0.2.1", "off.")) {
		t.Fatal("disabled route accepted an event")
	}
	off.Release()
	off.Release() // idempotent
	(*Captured)(nil).Release()
	(&Captured{}).Release()
	waitFor(t, "the unused sink to be forgotten", func() bool { return len(f.host.Status()) == 0 })
	if opens, _, _ := f.get("out").state(); opens != 0 {
		t.Error("a sink that never received an event was opened")
	}
}

func TestAfterQueuedOrdersBehindAcceptedEvents(t *testing.T) {
	gate := make(chan struct{})
	primary := &blockingSink{recSink: &recSink{}, block: gate}
	a := NewAsyncWith(AsyncOptions{Sink: primary, BatchSize: 1, QueueSize: 16, Logger: slog.New(slog.DiscardHandler)})
	a.Enqueue(ev("192.0.2.1", "held."))
	waitFor(t, "collector to enter the write", func() bool { return len(a.queue) == 0 })
	a.Enqueue(ev("192.0.2.1", "queued-1."))
	a.Enqueue(ev("192.0.2.1", "queued-2."))
	seen := make(chan int, 2)
	a.AfterQueued(func() { _, n := primary.count(); seen <- n })
	a.Enqueue(ev("192.0.2.1", "later."))
	select {
	case n := <-seen:
		t.Fatalf("callback ran with %d events written, before the queued ones were admitted", n)
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	select {
	case n := <-seen:
		// The third event is admitted and may or may not be flushed yet;
		// the two that were queued before the call must be behind us.
		if n < 2 {
			t.Fatalf("callback ran after %d writes", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback never ran")
	}
	if err := a.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	ran := false
	a.AfterQueued(func() { ran = true })
	if !ran {
		t.Fatal("a callback after the final drain must run on its caller")
	}
}

func TestSinkSpecsFromSettings(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if specs, err := SinkSpecsFrom(cfg.Querylog); err != nil || len(specs) != 0 {
		t.Fatalf("defaults: %v %v", specs, err)
	}
	cfg.Querylog.Sinks = []config.QuerylogSink{
		{Name: "lines", Type: config.QuerylogSinkSlog},
		{Name: "tuned", Type: config.QuerylogSinkSlog, QueueSize: 500, BatchSize: 50, FlushInterval: config.Duration(2 * time.Second), Timeout: config.Duration(3 * time.Second), Attempts: 1, RetryBackoff: config.Duration(4 * time.Second)},
	}
	specs, err := SinkSpecsFrom(cfg.Querylog)
	if err != nil {
		t.Fatal(err)
	}
	want := []SinkSpec{
		{Name: "lines", Type: "slog", QueueSize: 10000, BatchSize: 1000, Interval: time.Second, Timeout: 10 * time.Second, Attempts: 3, Backoff: time.Second},
		{Name: "tuned", Type: "slog", QueueSize: 500, BatchSize: 50, Interval: 2 * time.Second, Timeout: 3 * time.Second, Attempts: 1, Backoff: 4 * time.Second},
	}
	if !reflect.DeepEqual(specs, want) {
		t.Errorf("specs\n got %+v\nwant %+v", specs, want)
	}
	if specs[0].key() == specs[1].key() || specs[0].key() != want[0].withDefaults().key() {
		t.Error("the key must identify exactly one effective entry")
	}
}

func TestCrossedPowerOfTen(t *testing.T) {
	for _, tc := range []struct {
		prev, total uint64
		want        bool
	}{{0, 0, false}, {0, 1, true}, {1, 2, false}, {9, 10, true}, {5, 45, true}, {10, 99, false}, {99, 100, true}, {100, 999, false}, {998, 1003, true}, {0, 1 << 63, true}} {
		if got := crossedPowerOfTen(tc.prev, tc.total); got != tc.want {
			t.Errorf("crossedPowerOfTen(%d, %d) = %v", tc.prev, tc.total, got)
		}
	}
}
