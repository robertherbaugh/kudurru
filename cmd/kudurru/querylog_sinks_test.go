package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
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
	// The smallest entry of each type the settings validator admits.
	minimal := map[string]config.QuerylogSink{
		config.QuerylogSinkSlog:       {},
		config.QuerylogSinkClickHouse: {ClickHouse: &config.QuerylogClickHouse{URL: "https://clickhouse.invalid:8443"}},
		config.QuerylogSinkLoki:       {Loki: &config.QuerylogLoki{URL: "https://loki.invalid"}},
		config.QuerylogSinkDnstap:     {Dnstap: &config.QuerylogDnstap{Target: "tcp:collector.invalid:6000"}},
	}
	for name, factory := range querylogSinkFactories(querylogSinkInputs{Logger: slog.New(slog.DiscardHandler)}) {
		built = append(built, name)
		entry, known := minimal[name]
		if !known {
			t.Errorf("%s: add the smallest valid entry of this type to the test", name)
			continue
		}
		entry.Name, entry.Type = "probe", name
		cfg, err := config.Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Querylog.Sinks = []config.QuerylogSink{entry}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: the settings validator refuses the smallest entry: %v", name, err)
			continue
		}
		specs, err := querylog.SinkSpecsFrom(cfg.Querylog)
		if err != nil {
			t.Fatal(err)
		}
		// What the validator admits, the factory builds: without I/O (the
		// hosts above do not resolve) and with the defaults of the type.
		sink, err := factory(specs[0])
		if err != nil || sink == nil {
			t.Errorf("%s: a factory must build an admitted entry without I/O: %v", name, err)
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
	querylogSinkFactories = func(querylogSinkInputs) map[string]querylog.SinkFactory {
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

// logRecords decodes the JSON log lines written so far and returns the
// records with the given message.
func logRecords(t *testing.T, logs *syncBuffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// TestQuerylogRuntimeSlogSinkWritesQueryLines runs the one shipped sink type
// through its production factory, on the real runtime: next to the local
// database every recorded query is one info-level log line that names the
// sink, and the line carries what the privacy mode keeps.
func TestQuerylogRuntimeSlogSinkWritesQueryLines(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Querylog.Exclude.SUDN = false
	cfg.Querylog.Privacy = "hide_domains"
	cfg.Querylog.Sinks = []config.QuerylogSink{{Name: "log-lines", Type: config.QuerylogSinkSlog, BatchSize: 1}}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r, err := newQuerylogRuntime(t.Context(), cfg, prometheus.NewRegistry(), logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = r.close(context.Background())
		}
	})
	names := []string{"one.example.", "two.example."}
	for _, name := range names {
		if !r.async.Enqueue(&querylog.Event{Time: time.Now(), Name: name, Client: netip.MustParseAddr("192.0.2.9"), QType: dns.TypeA, QClass: dns.ClassINET}) {
			t.Fatal("collector refused", name)
		}
	}
	// The lines are in addition to the database, not instead of it.
	rows := func() int64 {
		t.Helper()
		page, err := r.reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		return page.RecordsTotal
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(logRecords(t, logs, "query")) < len(names) || rows() < int64(len(names)) {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d query lines, %d of %d rows in querylog.db:\n%s", len(logRecords(t, logs, "query")), len(names), rows(), len(names), logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The collector's drain and the sink's drain are both complete after a
	// clean close, so the lines below are all there will ever be.
	if err := r.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	closed = true
	lines := logRecords(t, logs, "query")
	if len(lines) != len(names) {
		t.Fatalf("%d query lines for %d queries: one line per query, and none from the primary sqlite sink:\n%s", len(lines), len(names), logs.String())
	}
	for i, line := range lines {
		if line["level"] != "INFO" || line["sink"] != "log-lines" || line["log"] != "query" {
			t.Errorf("line %d is not an info-level query line of sink log-lines: %v", i, line)
		}
		if line["name"] != querylog.HiddenName || line["client"] != "192.0.2.9" || line["qtype"] != "A" {
			t.Errorf("line %d does not carry the event as hide_domains keeps it: %v", i, line)
		}
	}
	if strings.Contains(logs.String(), "one.example.") || strings.Contains(logs.String(), "two.example.") {
		t.Errorf("a hidden query name reached the log:\n%s", logs.String())
	}
	if warned := logRecords(t, logs, "additional query-log sink did not drain"); len(warned) != 0 {
		t.Errorf("a healthy sink was reported as not drained: %v", warned)
	}
}

// TestQuerylogRuntimeQueryInFlightAtSinkChangeIsCounted is the exact form of
// the race on the reload subscriber of the Kubernetes driver. A query reads
// the collector's adopted route and is then queued (Async.Enqueue). A reload
// that changes the sink's entry between those two steps returns the route's
// references, so the query finds its sink retired. The test stands where
// that query is: it holds the route the query read and queues through it
// after the reload. The query is recorded in querylog.db, it is not handed
// to the entry's new sink, and it is counted as dropped for the name. Once
// the entry is removed, a late query counts nothing and brings no series
// back.
func TestQuerylogRuntimeQueryInFlightAtSinkChangeIsCounted(t *testing.T) {
	doubles := useExportDoubles(t)
	first := doubles.add("export", &exportSink{})
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Querylog.Exclude.SUDN = false
	cfg.Querylog.Sinks = []config.QuerylogSink{{Name: "export", Type: config.QuerylogSinkSlog, BatchSize: 1}}
	reg := prometheus.NewRegistry()
	logger := slog.New(slog.DiscardHandler)
	r, err := newQuerylogRuntime(t.Context(), cfg, reg, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.close(context.Background()) })
	reload := config.NewReloadable(cfg)
	subscribeReload(t.Context(), reload, reloadTargets{logger: logger, qlOwner: r})
	current := cfg
	apply := func(sinks []config.QuerylogSink) {
		t.Helper()
		next := *current
		next.Querylog.Sinks = sinks
		reload.Set(&next)
		current = &next
		if ok, why := r.ready(); !ok {
			t.Fatal("reload was not applied:", why)
		}
	}
	event := func(name string) *querylog.Event {
		return &querylog.Event{Time: time.Now(), Name: name, Client: netip.MustParseAddr("192.0.2.8"), QType: 1, QClass: 1}
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
	rows := func() int64 {
		t.Helper()
		page, err := r.reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		return page.RecordsTotal
	}
	dropped := func() (float64, bool) {
		t.Helper()
		return gatherSinkMetric(t, reg, "kudurru_querylog_dropped_total", map[string]string{"stage": "sink", "sink": "export"})
	}
	sent := func() (float64, bool) {
		t.Helper()
		return gatherSinkMetric(t, reg, "kudurru_sink_sent_total", map[string]string{"sink": "export"})
	}

	// The route Async.Enqueue loads before it queues.
	read := r.current.Load()
	if !r.async.Enqueue(event("before.example.")) {
		t.Fatal("the collector refused the first query")
	}
	wait("the first sink to write", func() bool { return len(first.names()) == 1 })

	// The reload lands: the same name with one field changed is a new sink.
	second := doubles.add("export", &exportSink{})
	apply([]config.QuerylogSink{{Name: "export", Type: config.QuerylogSinkSlog, BatchSize: 1, QueueSize: 500}})
	wait("the replaced sink to close", func() bool { _, closes, _ := first.counts(); return closes == 1 })
	// Only now does the query that had read the replaced route reach the
	// queue. The collector is first in, first out, so it has dealt with this
	// query before the new sink receives the next one.
	if !read.Enqueue(event("in-flight.example.")) {
		t.Fatal("the collector refused the query in flight")
	}
	if !r.async.Enqueue(event("after.example.")) {
		t.Fatal("the collector refused a query after the reload")
	}
	wait("the new sink to write", func() bool { return len(second.names()) == 1 })
	wait("querylog.db to hold all three queries", func() bool { return rows() == 3 })
	if got := first.names(); !reflect.DeepEqual(got, []string{"before.example."}) {
		t.Errorf("the replaced sink wrote %v", got)
	}
	if got := second.names(); !reflect.DeepEqual(got, []string{"after.example."}) {
		t.Errorf("the new sink wrote %v: a query of the replaced route must not move to it", got)
	}
	if got, _ := dropped(); got != 1 {
		t.Errorf("kudurru_querylog_dropped_total{stage=sink,sink=export} = %v, want 1: the query in flight reached no sink and must be counted", got)
	}
	if got, _ := sent(); got != 2 {
		t.Errorf("kudurru_sink_sent_total{sink=export} = %v, want 2", got)
	}
	if got, _ := gatherSinkMetric(t, reg, "kudurru_querylog_dropped_total", map[string]string{"stage": "queue"}); got != 0 {
		t.Errorf("the collector queue dropped %v queries", got)
	}

	// Remove the entry. A query that read the route before the removal is
	// owed to no sink: nothing is counted and the removed series stay gone.
	read = r.current.Load()
	apply(nil)
	wait("the removed sink to close", func() bool { _, closes, _ := second.counts(); return closes == 1 })
	wait("the removed sink's series to go", func() bool { _, found := sent(); return !found })
	if _, found := dropped(); found {
		t.Error("a removed sink keeps its drop series")
	}
	if !read.Enqueue(event("after-removal.example.")) {
		t.Fatal("the collector refused the late query")
	}
	wait("querylog.db to hold the late query", func() bool { return rows() == 4 })
	if _, found := dropped(); found {
		t.Error("a late query brought back the drop series of a removed sink")
	}
	if _, found := sent(); found {
		t.Error("a late query brought back the removed sink")
	}
	if got := len(second.names()); got != 1 {
		t.Errorf("the removed sink wrote %d queries, want the one before its removal", got)
	}
}

// TestQuerylogRuntimeChangedSinkEntryCountsQueriesInFlight drives the reload
// subscriber of the Kubernetes driver while queries keep arriving. Every
// change of the entry replaces the sink, and a query that read the replaced
// route just before the swap can find its sink retired. Each query the
// collector accepted is then written once by an instance of the sink or
// counted as dropped for its name; none disappears and none is written
// twice.
//
// The primary sink is the log-line sink with a discarding logger, so that
// the collector keeps up with the queries. With the SQLite writer under the
// race detector the collector spends most of a second inside each batch, the
// queue stays full, a sink retires only behind that full queue, and no query
// can be caught in flight: the test then passes whether or not such a query
// is counted. Whether a run catches a query is still a matter of timing; the
// exact case is TestQuerylogRuntimeQueryInFlightAtSinkChangeIsCounted.
func TestQuerylogRuntimeChangedSinkEntryCountsQueriesInFlight(t *testing.T) {
	doubles := useExportDoubles(t)
	export := doubles.add("export", &exportSink{})
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Querylog.Exclude.SUDN = false
	cfg.Querylog.Sink = "slog"
	entry := func(batch int) []config.QuerylogSink {
		return []config.QuerylogSink{{Name: "export", Type: config.QuerylogSinkSlog, BatchSize: batch}}
	}
	cfg.Querylog.Sinks = entry(100)
	reg := prometheus.NewRegistry()
	logger := slog.New(slog.DiscardHandler)
	r, err := newQuerylogRuntime(t.Context(), cfg, reg, logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.close(context.Background()) })
	reload := config.NewReloadable(cfg)
	subscribeReload(t.Context(), reload, reloadTargets{logger: logger, qlOwner: r})

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
				// The log observer of this path enqueues through the collector.
				if r.async.Enqueue(&querylog.Event{Time: time.Now(), Name: "q.example.", Client: netip.MustParseAddr("192.0.2.8"), QType: 1, QClass: 1}) {
					accepted.Add(1)
				}
			}
		})
	}
	const reloads = 200
	current := cfg
	for i := range reloads {
		next := *current
		next.Querylog.Sinks = entry(101 + i%7) // any change is a new sink under the same name
		reload.Set(&next)
		current = &next
		if ok, why := r.ready(); !ok {
			t.Fatal("reload was not applied:", why)
		}
		time.Sleep(200 * time.Microsecond)
	}
	close(stop)
	enqueuers.Wait()
	// Counters are exact in a float64 far beyond what a test enqueues.
	want := float64(accepted.Load())
	var sent, dropped float64
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		sent, _ = gatherSinkMetric(t, reg, "kudurru_sink_sent_total", map[string]string{"sink": "export"})
		dropped, _ = gatherSinkMetric(t, reg, "kudurru_querylog_dropped_total", map[string]string{"stage": "sink", "sink": "export"})
		if sent+dropped >= want || time.Now().After(deadline) {
			break
		}
	}
	if sent+dropped != want {
		t.Fatalf("the collector accepted %.0f queries; sink export wrote %.0f and counted %.0f dropped: %.0f are unaccounted for", want, sent, dropped, want-sent-dropped)
	}
	ids := export.ids()
	if float64(len(ids)) != sent {
		t.Errorf("the sink instances wrote %d events, kudurru_sink_sent_total says %.0f", len(ids), sent)
	}
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("query %d was written twice", id)
		}
		seen[id] = true
	}
	if builds := doubles.builds("export"); builds != reloads+1 {
		t.Errorf("the entry changed %d times and its sink was built %d times, want %d", reloads, builds, reloads+1)
	}
	t.Logf("accepted %.0f, written %.0f, counted as dropped %.0f", want, sent, dropped)
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
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
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
	// not close it under that write, and still closes the database. The
	// collector and the database flushed completely, so close reports no
	// local loss; the exporter that did not drain is reported on its own.
	// The bound is far above the time the local close takes (up to about
	// 240 ms under the race detector on two busy cores), so a slow machine
	// cannot turn the database close into the reported failure, and still
	// well below the sink host's own five-second drain.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := r.close(ctx); err != nil {
		t.Fatalf("close reported an incomplete local flush although only an exporter did not drain: %v", err)
	}
	closed = true
	if took := time.Since(start); took < 1900*time.Millisecond || took > 4*time.Second {
		t.Errorf("close with a stalled sink took %s: it must give the sinks the caller's bound and no more", took)
	}
	undrained := logRecords(t, logs, "additional query-log sink did not drain")
	if len(undrained) != 1 {
		t.Fatalf("shutdown reported %d undrained sinks, want the stalled one:\n%s", len(undrained), logs.String())
	}
	// 100 events were still queued behind the pending write; the overflow
	// had been counted while the sink ran.
	if rec := undrained[0]; rec["sink"] != "stalled" || rec["queued"] != float64(100) || rec["dropped"] != float64(overflow) || rec["level"] != "WARN" {
		t.Errorf("undrained sink report: %v", rec)
	}
	if _, closes, _ := stalled.counts(); closes != 0 {
		t.Fatal("a sink was closed under its pending write")
	}
	if _, err := r.writer.Load().Reader().Queries(context.Background(), sqlitelog.QueryFilter{Limit: 1}); err == nil {
		t.Fatal("the local database stayed open behind a stalled exporter")
	}
	unstall()
	wait("the released sink to close", func() bool { _, closes, _ := stalled.counts(); return closes == 1 })
}

// heldPrimarySink is a primary sink whose writes wait until release closes.
type heldPrimarySink struct {
	entered chan struct{}
	release chan struct{}
}

func (s *heldPrimarySink) Name() string { return "held" }
func (s *heldPrimarySink) Open(context.Context, json.RawMessage, prometheus.Registerer) error {
	return nil
}
func (s *heldPrimarySink) Write([]*querylog.Event) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return nil
}
func (s *heldPrimarySink) Flush(context.Context) error { return nil }
func (s *heldPrimarySink) Close() error                { return nil }

// TestQuerylogRuntimeCloseWithoutCollectorFlushReportsSinksOnce: when the
// collector itself cannot flush within the shutdown bound, the additional
// sinks are stopped with a bound that has already ended. Whether an idle
// sink still finishes in that instant is a race, so it must not be reported
// on its own as "did not drain" (a healthy sink with nothing queued would be
// named as often as not). Exactly one line names every sink and says that
// they were stopped because the query log did not flush, and close returns
// the collector's error for the caller's "query log flush incomplete".
func TestQuerylogRuntimeCloseWithoutCollectorFlushReportsSinksOnce(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.DataDir = t.TempDir()
	cfg.Querylog.Exclude.SUDN = false
	cfg.Querylog.Sinks = []config.QuerylogSink{
		{Name: "lines-a", Type: config.QuerylogSinkSlog, BatchSize: 1},
		{Name: "lines-b", Type: config.QuerylogSinkSlog, BatchSize: 1},
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r, err := newQuerylogRuntime(t.Context(), cfg, prometheus.NewRegistry(), logger, nil)
	if err != nil {
		t.Fatal(err)
	}
	held := &heldPrimarySink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	released := false
	release := func() {
		if !released {
			released = true
			close(held.release)
		}
	}
	t.Cleanup(func() {
		release()
		_ = r.close(context.Background())
	})
	// The route of the running node with a primary sink that holds the
	// collector in its write, and the same two additional sinks.
	specs, err := querylog.SinkSpecsFrom(cfg.Querylog)
	if err != nil {
		t.Fatal(err)
	}
	set, err := r.sinks.Prepare(specs)
	if err != nil {
		t.Fatal(err)
	}
	route := r.async.PrepareCapturedSinks(held, nil, true, 1, r.current.Load(), set)
	if !r.async.AdoptCaptured(route) {
		t.Fatal("route not adopted")
	}
	t.Cleanup(route.Release)
	enqueue := func(name string) {
		t.Helper()
		if !r.async.Enqueue(&querylog.Event{Time: time.Now(), Name: name, Client: netip.MustParseAddr("192.0.2.9"), QType: dns.TypeA, QClass: dns.ClassINET}) {
			t.Fatal("collector refused", name)
		}
	}
	enqueue("first.example.")
	select {
	case <-held.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the collector did not reach the primary write")
	}
	// The first query reached both sinks before the primary write; wait
	// until they have written it, so that both are idle with nothing queued.
	deadline := time.Now().Add(10 * time.Second)
	for len(logRecords(t, logs, "query")) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the additional sinks did not write the first query:\n%s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	enqueue("second.example.") // waits in the collector's queue

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := r.close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close with a collector held in its write: %v, want the bound's error", err)
	}
	if undrained := logRecords(t, logs, "additional query-log sink did not drain"); len(undrained) != 0 {
		t.Errorf("idle sinks were reported one by one as not drained: %v", undrained)
	}
	stopped := logRecords(t, logs, "additional query-log sinks stopped because the query log did not flush")
	if len(stopped) != 1 {
		t.Fatalf("%d lines about the stopped sinks, want exactly one:\n%s", len(stopped), logs.String())
	}
	rec := stopped[0]
	if names, _ := rec["sinks"].([]any); rec["level"] != "WARN" || !reflect.DeepEqual(names, []any{"lines-a", "lines-b"}) || rec["queued"] != float64(0) {
		t.Errorf("stopped sinks line: %v", rec)
	}
	if strings.Contains(logs.String(), `"msg":"additional query-log sinks did not stop"`) {
		t.Errorf("the sink host refused to stop:\n%s", logs.String())
	}
	release()
}
