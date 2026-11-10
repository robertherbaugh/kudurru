package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// TestQuerylogSinkTypesHaveFactories: the settings schema accepts exactly
// the sink types the runtime can build, so querylog.sinks can never name a
// destination that is silently ignored.
func TestQuerylogSinkTypesHaveFactories(t *testing.T) {
	key, ok := config.Lookup("querylog.sinks")
	if !ok || key.Items == nil {
		t.Fatal("querylog.sinks has no item shape")
	}
	var accepted []string
	for _, field := range key.Items.Fields {
		if field.Name == "type" {
			accepted = field.EnumValues()
		}
	}
	var built []string
	for name, factory := range querylogSinkFactories(slog.New(slog.DiscardHandler)) {
		built = append(built, name)
		sink, err := factory(querylog.SinkSpec{Name: "probe", Type: name})
		if err != nil || sink == nil {
			t.Errorf("%s: a factory must build its sink from the defaults without I/O: %v", name, err)
		}
	}
	sort.Strings(accepted)
	sort.Strings(built)
	if len(accepted) == 0 || !reflect.DeepEqual(accepted, built) {
		t.Fatalf("schema accepts %v, runtime builds %v", accepted, built)
	}
}

// exportSink stands where an exporter (dnstap, ClickHouse, Loki) will: it
// records what the host hands it and can stall inside a write or fail.
type exportSink struct {
	mu      sync.Mutex
	opens   int
	closes  int
	calls   int
	events  []*querylog.Event
	fail    func(call int) error
	stall   chan struct{} // writes wait until it is closed, ignoring cancellation
	entered chan struct{}
}

func (s *exportSink) Name() string { return "export-double" }
func (s *exportSink) Open(context.Context, json.RawMessage, prometheus.Registerer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	return nil
}
func (s *exportSink) Flush(context.Context) error { return nil }
func (s *exportSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}
func (s *exportSink) Write(b []*querylog.Event) error {
	return s.WriteBatch(context.Background(), b, querylog.PrivacyFull)
}
func (s *exportSink) WriteBatch(_ context.Context, b []*querylog.Event, _ querylog.Mode) error {
	s.mu.Lock()
	s.calls++
	call, fail, stall, entered := s.calls, s.fail, s.stall, s.entered
	s.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	if stall != nil {
		<-stall
	}
	if fail != nil {
		if err := fail(call); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, b...)
	return nil
}

func (s *exportSink) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = e.Name
	}
	return out
}

func (s *exportSink) ids() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint64, len(s.events))
	for i, e := range s.events {
		out[i] = e.ID
	}
	return out
}

func (s *exportSink) counts() (opens, closes, calls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens, s.closes, s.calls
}

// exportDoubles replaces the sink factories for one test: every configured
// entry, whatever its name, is served by the double registered under that
// name. Everything in front of the sink is the production path.
type exportDoubles struct {
	mu    sync.Mutex
	sinks map[string]*exportSink
	built map[string]int
}

func useExportDoubles(t *testing.T) *exportDoubles {
	t.Helper()
	d := &exportDoubles{sinks: map[string]*exportSink{}, built: map[string]int{}}
	previous := querylogSinkFactories
	querylogSinkFactories = func(*slog.Logger) map[string]querylog.SinkFactory {
		return map[string]querylog.SinkFactory{config.QuerylogSinkSlog: func(spec querylog.SinkSpec) (querylog.Sink, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.built[spec.Name]++
			sink := d.sinks[spec.Name]
			if sink == nil {
				return nil, errors.New("no double registered for " + spec.Name)
			}
			return sink, nil
		}}
	}
	t.Cleanup(func() { querylogSinkFactories = previous })
	return d
}

func (d *exportDoubles) add(name string, sink *exportSink) *exportSink {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sinks[name] = sink
	return sink
}

func (d *exportDoubles) builds(name string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.built[name]
}

func gatherSinkMetric(t *testing.T, reg prometheus.Gatherer, family string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
	metrics:
		for _, m := range f.GetMetric() {
			got := map[string]string{}
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue metrics
				}
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue(), true
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue(), true
			}
		}
	}
	return 0, false
}

// TestQuerylogRuntimeSinksLegacyReload drives the reload subscriber that
// Kubernetes and file-reloaded nodes use (config.Reloadable -> applyReload
// -> querylogRuntime.reload) against the real collector, tail and SQLite
// writer: a stalled additional sink costs only its own events, and adding,
// keeping and removing sinks never replays or duplicates one.
func TestQuerylogRuntimeSinksLegacyReload(t *testing.T) {
	doubles := useExportDoubles(t)
	stalled := doubles.add("stalled", &exportSink{stall: make(chan struct{}), entered: make(chan struct{}, 1)})
	var release sync.Once
	unstall := func() { release.Do(func() { close(stalled.stall) }) }
	t.Cleanup(unstall)
	recorder := doubles.add("recorder", &exportSink{})

	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Querylog.Exclude.SUDN = false
	reg := prometheus.NewRegistry()
	logger := slog.New(slog.DiscardHandler)
	r, err := newQuerylogRuntime(t.Context(), cfg, reg, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			unstall()
			_ = r.close(context.Background())
		}
	})
	reload := config.NewReloadable(cfg)
	subscribeReload(t.Context(), reload, reloadTargets{logger: logger, qlOwner: r})
	current := cfg
	apply := func(edit func(*config.Querylog)) {
		t.Helper()
		next := *current
		next.Querylog.Sinks = append([]config.QuerylogSink(nil), current.Querylog.Sinks...)
		edit(&next.Querylog)
		// Fed to the subscriber directly: the settings validator admits one
		// slog sink, and this test needs a stalled and a healthy sink at once.
		reload.Set(&next)
		current = &next
		if ok, why := r.ready(); !ok {
			t.Fatal("reload was not applied:", why)
		}
	}
	emit := func(names ...string) {
		t.Helper()
		for _, name := range names {
			// The legacy log observer enqueues through the collector.
			if !r.async.Enqueue(&querylog.Event{Time: time.Now(), Name: name, Client: netip.MustParseAddr("192.0.2.8"), QType: 1, QClass: 1}) {
				t.Fatal("collector refused", name)
			}
		}
	}
	wait := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	rows := func() int {
		t.Helper()
		page, err := r.reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		return int(page.RecordsTotal)
	}

	emit("before-any-sink.example.")
	wait("first row", func() bool { return rows() == 1 })

	// Add two sinks in one reload; one of them never returns from a write.
	apply(func(q *config.Querylog) {
		q.Sinks = []config.QuerylogSink{
			{Name: "stalled", Type: config.QuerylogSinkSlog, QueueSize: 100, BatchSize: 1},
			{Name: "recorder", Type: config.QuerylogSinkSlog, BatchSize: 1},
		}
	})
	emit("first.example.")
	select {
	case <-stalled.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled sink never received a batch")
	}
	const burst = 300
	sub := r.tail.Subscribe(t.Context())
	for i := range burst {
		emit("burst-" + string(rune('a'+i%26)) + ".example.")
	}
	wait("every row to commit while a sink is stalled", func() bool { return rows() == 2+burst })
	wait("the healthy sink to receive everything", func() bool { return len(recorder.names()) == 1+burst })
	for i := range burst {
		select {
		case <-sub:
		default:
			t.Fatalf("live tail published %d of %d events while a sink was stalled", i, burst)
		}
	}
	// One event is inside the stalled write, 100 wait, the rest overflowed.
	const overflow = 1 + burst - 1 - 100
	if got, _ := gatherSinkMetric(t, reg, "kudurru_querylog_dropped_total", map[string]string{"stage": "sink", "sink": "stalled"}); got != overflow {
		t.Errorf("dropped{stage=sink,sink=stalled} = %v, want %d", got, overflow)
	}
	if got, found := gatherSinkMetric(t, reg, "kudurru_querylog_dropped_total", map[string]string{"stage": "sink", "sink": "recorder"}); found && got != 0 {
		t.Errorf("the healthy sink dropped %v events", got)
	}
	if got, _ := gatherSinkMetric(t, reg, "kudurru_querylog_dropped_total", map[string]string{"stage": "queue"}); got != 0 {
		t.Errorf("the collector queue dropped %v events", got)
	}
	if got, _ := gatherSinkMetric(t, reg, "kudurru_sink_queue_length", map[string]string{"sink": "stalled"}); got != 100 {
		t.Errorf("kudurru_sink_queue_length{sink=stalled} = %v", got)
	}
	if got, _ := gatherSinkMetric(t, reg, "kudurru_sink_sent_total", map[string]string{"sink": "recorder"}); got != 1+burst {
		t.Errorf("kudurru_sink_sent_total{sink=recorder} = %v", got)
	}

	// Keep the recorder, drop the stalled sink, and change something else:
	// the kept sink is neither rebuilt nor reopened and sees no old event.
	apply(func(q *config.Querylog) {
		q.Sinks = q.Sinks[1:]
		q.Privacy = "hide_domains"
	})
	emit("after-removal.example.")
	wait("the kept sink", func() bool { return len(recorder.names()) == 2+burst })
	if opens, closes, _ := recorder.counts(); opens != 1 || closes != 0 || doubles.builds("recorder") != 1 {
		t.Fatalf("an unchanged sink was restarted: opens %d closes %d builds %d", opens, closes, doubles.builds("recorder"))
	}
	if got := recorder.names(); got[0] != "first.example." || got[len(got)-1] != querylog.HiddenName {
		t.Errorf("recorder: first %q last %q: privacy must be applied before the sink queue", got[0], got[len(got)-1])
	}
	ids := recorder.ids()
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1]+1 {
			t.Fatalf("recorder ids %d then %d: an event was replayed, duplicated or lost", ids[i-1], ids[i])
		}
	}
	if ids[0] != 2 {
		t.Errorf("the added sink received event %d: it must start with the first event after it was added", ids[0])
	}

	// Remove every sink. The collector and the database carry on.
	apply(func(q *config.Querylog) { q.Sinks = nil })
	wait("the removed sink to close", func() bool { _, closes, _ := recorder.counts(); return closes == 1 })
	emit("after-all-removed.example.")
	wait("the database to keep committing", func() bool { return rows() == 4+burst })
	if got := len(recorder.names()); got != 2+burst {
		t.Errorf("a removed sink received %d events", got)
	}
	if _, found := gatherSinkMetric(t, reg, "kudurru_sink_sent_total", map[string]string{"sink": "recorder"}); found {
		t.Error("a removed sink keeps its series")
	}
	// The stalled sink is still inside its write. Shutdown is bounded, does
	// not close it under that write, and still closes the database.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if err := r.close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close with a stalled sink: %v", err)
	}
	closed = true
	if _, closes, _ := stalled.counts(); closes != 0 {
		t.Fatal("a sink was closed under its pending write")
	}
	if _, err := r.writer.Load().Reader().Queries(context.Background(), sqlitelog.QueryFilter{Limit: 1}); err == nil {
		t.Fatal("the local database stayed open behind a stalled exporter")
	}
	unstall()
	wait("the released sink to close", func() bool { _, closes, _ := stalled.counts(); return closes == 1 })
}
