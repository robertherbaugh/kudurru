package sinks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/sinktest"
)

// primary records what the collector commits locally.
type primary struct {
	mu     sync.Mutex
	events []*querylog.Event
}

func (p *primary) Name() string { return "primary" }
func (p *primary) Open(context.Context, json.RawMessage, prometheus.Registerer) error {
	return nil
}
func (p *primary) Write(b []*querylog.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, b...)
	return nil
}
func (p *primary) Flush(context.Context) error { return nil }
func (p *primary) Close() error                { return nil }
func (p *primary) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.events)
}

// fixture is the production path in front of an HTTP sink: the real
// collector, the real sink host and the real factories. Only the
// destination is a local double.
type fixture struct {
	t       *testing.T
	reg     *prometheus.Registry
	metrics *querylog.Metrics
	primary *primary
	async   *querylog.Async
	host    *querylog.SinkHost
	logs    *strings.Builder
	logMu   *sync.Mutex
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// newFixture allows exactly the credential files of the map.
func newFixture(t *testing.T, credentials map[string]string) *fixture {
	t.Helper()
	return newFixtureWith(t, func(_ context.Context, ref config.CredentialRef) ([]byte, error) {
		value, ok := credentials[ref.File]
		if !ok {
			return nil, errors.New("credential reference is not allowed")
		}
		return []byte(value), nil
	})
}

func newFixtureWith(t *testing.T, resolve func(context.Context, config.CredentialRef) ([]byte, error)) *fixture {
	t.Helper()
	f := &fixture{t: t, reg: prometheus.NewRegistry(), primary: &primary{}, logs: &strings.Builder{}, logMu: &sync.Mutex{}}
	f.metrics = querylog.NewMetrics(f.reg)
	logger := slog.New(slog.NewTextHandler(lockedWriter{f.logMu, f.logs}, nil))
	f.async = querylog.NewAsyncWith(querylog.AsyncOptions{Sink: f.primary, BatchSize: 1, QueueSize: 1024, Metrics: f.metrics, FirstID: 1, Logger: logger})
	f.host = querylog.NewSinkHost(querylog.SinkHostOptions{
		Collector: f.async, Metrics: f.metrics, Registerer: f.reg, Logger: logger, Drain: 2 * time.Second,
		Factories: sinks.Factories(sinks.Options{Logger: logger, UserAgent: "kudurru/test", Resolve: resolve}),
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.async.Close(ctx)
		_ = f.host.Close(ctx)
	})
	return f
}

func (f *fixture) logged() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return f.logs.String()
}

// route publishes cfg's sinks the way a settings change does: through the
// settings validator, SinkSpecsFrom and a captured route.
func (f *fixture) route(privacyMode string, exclude []string, entries ...config.QuerylogSink) *querylog.Captured {
	f.t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	cfg.Querylog.Privacy, cfg.Querylog.Exclude.Domains, cfg.Querylog.Exclude.SUDN, cfg.Querylog.Sinks = privacyMode, exclude, false, entries
	if err := cfg.Validate(); err != nil {
		f.t.Fatal(err)
	}
	specs, err := querylog.SinkSpecsFrom(cfg.Querylog)
	if err != nil {
		f.t.Fatal(err)
	}
	for i := range specs {
		// Faster than the settings allow, so that the tests do not wait.
		specs[i].Interval, specs[i].Backoff = 40*time.Millisecond, 5*time.Millisecond
	}
	set, err := f.host.Prepare(specs)
	if err != nil {
		f.t.Fatal(err)
	}
	privacy, err := querylog.PrivacyFrom(cfg.Querylog)
	if err != nil {
		f.t.Fatal(err)
	}
	route := f.async.PrepareCapturedSinks(f.primary, privacy, true, 1, nil, set)
	if !f.async.AdoptCaptured(route) {
		f.t.Fatal("route not adopted")
	}
	return route
}

func (f *fixture) status(name string) querylog.SinkStatus {
	for _, st := range f.host.Status() {
		if st.Name == name {
			return st
		}
	}
	return querylog.SinkStatus{}
}

func (f *fixture) dropped(name string) float64 {
	return testutil.ToFloat64(f.metrics.Dropped.WithLabelValues(querylog.DropStageSink, name))
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func event(name string) *querylog.Event {
	return &querylog.Event{
		Time: time.Now(), Node: "node-a", Client: netip.MustParseAddr("192.0.2.7"), ClientName: "laptop", Name: name, QType: 1, QClass: 1,
		Status: pipeline.StatusForwarded, ReplyType: querylog.ReplyIP, Answers: 1, Answer: "192.0.2.80",
	}
}

// domains returns the domain of every row or line a double received, in
// order, whichever of the two formats it speaks.
func domains(t *testing.T, kind string, requests []sinktest.Request) []string {
	t.Helper()
	var out []string
	for _, r := range requests {
		if kind == config.QuerylogSinkClickHouse {
			for _, row := range sinktest.ClickHouseRows(t, r) {
				out = append(out, row["domain"].(string))
			}
			continue
		}
		for _, st := range sinktest.LokiStreams(t, r) {
			for _, entry := range st.Entries {
				out = append(out, entry.LineJSON(t)["domain"].(string))
			}
		}
	}
	return out
}

// entry is the querylog.sinks entry of one type pointing at a double.
func entry(kind, name, target string, edit func(*config.QuerylogSink)) config.QuerylogSink {
	s := config.QuerylogSink{Name: name, Type: kind, QueueSize: 100}
	if kind == config.QuerylogSinkClickHouse {
		s.ClickHouse = &config.QuerylogClickHouse{URL: target, Username: "kudurru", PasswordRef: &config.CredentialRef{File: "/run/secrets/export"}}
	} else {
		s.Loki = &config.QuerylogLoki{URL: target, BearerTokenRef: &config.CredentialRef{File: "/run/secrets/export"}}
	}
	if edit != nil {
		edit(&s)
	}
	return s
}

var kinds = map[string]int{config.QuerylogSinkClickHouse: http.StatusOK, config.QuerylogSinkLoki: http.StatusNoContent}

// TestHTTPSinksBatchOnTheHost: the host's batch policy reaches the wire.
// Seven queries with a batch size of three are two full requests and one
// partial request after the flush interval, in order, each query once, with
// the credential the reference names.
func TestHTTPSinksBatchOnTheHost(t *testing.T) {
	for kind, ok := range kinds {
		t.Run(kind, func(t *testing.T) {
			double := sinktest.New(t, ok, false)
			f := newFixture(t, map[string]string{"/run/secrets/export": "export-secret\n"})
			route := f.route("full", nil, entry(kind, "export", double.URL, func(s *config.QuerylogSink) { s.BatchSize = 3 }))
			var want []string
			for i := range 7 {
				name := fmt.Sprintf("q%d.example.", i)
				want = append(want, strings.TrimSuffix(name, "."))
				if !route.Enqueue(event(name)) {
					t.Fatal("event refused")
				}
			}
			waitFor(t, "every query to arrive", func() bool { return len(domains(t, kind, double.Requests())) == 7 })
			requests := double.Requests()
			if got := domains(t, kind, requests); !reflect.DeepEqual(got, want) {
				t.Fatalf("queries arrived as %v, want %v", got, want)
			}
			var sizes []int
			for _, r := range requests {
				sizes = append(sizes, len(domains(t, kind, []sinktest.Request{r})))
				wantAuth := "Bearer export-secret"
				if kind == config.QuerylogSinkClickHouse {
					wantAuth = sinktest.Basic("kudurru", "export-secret")
				}
				if r.Header.Get("Authorization") != wantAuth || r.Header.Get("User-Agent") != "kudurru/test" || !r.Compressed {
					t.Fatalf("request headers: %v", r.Header)
				}
			}
			if !reflect.DeepEqual(sizes, []int{3, 3, 1}) {
				t.Fatalf("batches of %v queries, want [3 3 1]", sizes)
			}
			waitFor(t, "the sent counter", func() bool { return f.status("export").Sent == 7 })
			if st := f.status("export"); st.Dropped != 0 || st.Errors != 0 || st.LastSuccess.IsZero() || st.Type != kind {
				t.Fatalf("status: %+v", st)
			}
			if got := testutil.CollectAndCount(f.reg, "kudurru_sink_sent_total"); got != 1 {
				t.Fatalf("kudurru_sink_sent_total has %d series", got)
			}
			if f.primary.count() != 7 {
				t.Fatalf("the local sink holds %d events", f.primary.count())
			}
		})
	}
}

// TestHTTPSinksReceiveOnlyWhatThePrivacyModeKeeps: the transform and the
// exclusions run in the collector, before the sink's queue. Nothing they
// remove reaches the wire.
func TestHTTPSinksReceiveOnlyWhatThePrivacyModeKeeps(t *testing.T) {
	for kind, ok := range kinds {
		t.Run(kind, func(t *testing.T) {
			double := sinktest.New(t, ok, false)
			f := newFixture(t, map[string]string{"/run/secrets/export": "export-secret"})
			plain := func(s *config.QuerylogSink) {
				s.BatchSize = 1
				if s.ClickHouse != nil {
					s.ClickHouse.Compression = config.QuerylogCompressionNone
				} else {
					s.Loki.Compression = config.QuerylogCompressionNone
				}
			}
			route := f.route("hide_domains_clients", []string{"*.excluded.example"}, entry(kind, "export", double.URL, plain))
			for _, name := range []string{"secret-name.example.", "never.excluded.example.", "another-secret.example."} {
				if !route.Enqueue(event(name)) {
					t.Fatal("event refused")
				}
			}
			waitFor(t, "the two admitted queries", func() bool { return len(double.Requests()) == 2 })
			waitFor(t, "the local sink", func() bool { return f.primary.count() == 2 })
			for _, r := range double.Requests() {
				body := string(r.Body)
				for _, hidden := range []string{"secret-name", "another-secret", "excluded.example", "192.0.2.7", "laptop", "192.0.2.80"} {
					if strings.Contains(body, hidden) {
						t.Fatalf("the request carries %q: %s", hidden, body)
					}
				}
				if !strings.Contains(body, `"hidden"`) || !strings.Contains(body, "0.0.0.0") {
					t.Fatalf("the request does not carry the transformed values: %s", body)
				}
			}
			if got := domains(t, kind, double.Requests()); !reflect.DeepEqual(got, []string{"hidden", "hidden"}) {
				t.Fatalf("domains %v", got)
			}

			// Under nothing no per-query event is offered to any sink.
			route.Release()
			nothing := f.route("nothing", nil, entry(kind, "export", double.URL, plain))
			if !nothing.Enqueue(event("nothing-mode.example.")) {
				t.Fatal("event refused")
			}
			waitFor(t, "the collector to pass the event", func() bool { return f.primary.count() == 3 })
			time.Sleep(200 * time.Millisecond)
			if got := len(double.Requests()); got != 2 {
				t.Fatalf("%d requests: a query was exported under privacy nothing", got)
			}
		})
	}
}

// TestHTTPSinksRetryWithinTheBudgetAndDrop: an answer that says "not
// stored" is repeated, at most attempts times; a refusal and an unknown
// outcome are sent once. Every loss is counted for the sink, and the
// repeated request is the same batch.
func TestHTTPSinksRetryWithinTheBudgetAndDrop(t *testing.T) {
	for kind, ok := range kinds {
		t.Run(kind, func(t *testing.T) {
			double := sinktest.New(t, ok, false)
			f := newFixture(t, map[string]string{"/run/secrets/export": "export-secret"})
			route := f.route("full", nil, entry(kind, "export", double.URL, func(s *config.QuerylogSink) { s.BatchSize, s.Attempts = 1, 3 }))
			send := func(name string, wantRequests int) []sinktest.Request {
				t.Helper()
				before := len(double.Requests())
				if !route.Enqueue(event(name)) {
					t.Fatal("event refused")
				}
				waitFor(t, fmt.Sprintf("%d requests for %s", wantRequests, name), func() bool { return len(double.Requests()) >= before+wantRequests })
				// Nothing further may follow.
				time.Sleep(150 * time.Millisecond)
				got := double.Requests()[before:]
				if len(got) != wantRequests {
					t.Fatalf("%s: %d requests, want %d", name, len(got), wantRequests)
				}
				return got
			}

			// 503 twice, then stored: three requests with the same body.
			double.Answer(func(n int, _ sinktest.Request) sinktest.Answer {
				if n <= 2 {
					return sinktest.Answer{Status: http.StatusServiceUnavailable}
				}
				return sinktest.Answer{}
			})
			got := send("recovers.example.", 3)
			if string(got[0].Body) != string(got[1].Body) || string(got[1].Body) != string(got[2].Body) {
				t.Fatal("a repeated attempt sent a different batch")
			}
			waitFor(t, "the recovered batch to count as sent", func() bool { return f.status("export").Sent == 1 })
			if st := f.status("export"); st.Errors != 2 || st.Dropped != 0 {
				t.Fatalf("after a recovered batch: %+v", st)
			}

			// 500 forever: three attempts, then the batch is dropped.
			double.Answer(func(int, sinktest.Request) sinktest.Answer {
				return sinktest.Answer{Status: http.StatusInternalServerError, Body: "error text that names abandoned.example"}
			})
			send("abandoned.example.", 3)
			waitFor(t, "the abandoned batch to count as dropped", func() bool { return f.status("export").Dropped == 1 })
			if st := f.status("export"); st.Errors != 5 || st.Sent != 1 || f.dropped("export") != 1 {
				t.Fatalf("after an abandoned batch: %+v, dropped metric %v", st, f.dropped("export"))
			}

			// 429 is "try again" as well.
			double.Answer(func(n int, _ sinktest.Request) sinktest.Answer {
				return sinktest.Answer{Status: http.StatusTooManyRequests}
			})
			send("limited.example.", 3)
			waitFor(t, "the rate-limited batch to count as dropped", func() bool { return f.status("export").Dropped == 2 })

			// 400: a refusal is not repeated.
			double.Answer(func(int, sinktest.Request) sinktest.Answer { return sinktest.Answer{Status: http.StatusBadRequest} })
			send("refused.example.", 1)
			waitFor(t, "the refused batch to count as dropped", func() bool { return f.status("export").Dropped == 3 })
			if st := f.status("export"); st.Errors != 9 {
				t.Fatalf("after a refused batch: %+v", st)
			}

			// Sent, connection lost before an answer: unknown, never repeated.
			double.Answer(func(int, sinktest.Request) sinktest.Answer { return sinktest.Answer{Drop: true} })
			send("unknown.example.", 1)
			waitFor(t, "the uncertain batch to count as dropped", func() bool { return f.status("export").Dropped == 4 })

			// The sink carries on with the next query.
			double.Answer(nil)
			send("after.example.", 1)
			waitFor(t, "the next batch", func() bool { return f.status("export").Sent == 2 })
			if st := f.status("export"); st.Dropped != 4 || st.Errors != 10 || f.dropped("export") != 4 {
				t.Fatalf("final: %+v, dropped metric %v", st, f.dropped("export"))
			}
			all := domains(t, kind, double.Requests())
			sort.Strings(all)
			want := []string{"abandoned.example", "abandoned.example", "abandoned.example", "after.example", "limited.example", "limited.example", "limited.example",
				"recovers.example", "recovers.example", "recovers.example", "refused.example", "unknown.example"}
			if !reflect.DeepEqual(all, want) {
				t.Fatalf("requests carried %v", all)
			}
			if f.primary.count() != 6 {
				t.Fatalf("the local sink holds %d of 6 queries: a failing exporter must not cost a local row", f.primary.count())
			}
			logs := f.logged()
			for _, leak := range []string{"export-secret", "abandoned.example", "ZXhwb3J0LXNlY3JldA", "192.0.2.7"} {
				if strings.Contains(logs, leak) {
					t.Fatalf("the server log repeats %q:\n%s", leak, logs)
				}
			}
			if !strings.Contains(logs, "query log sink write failed") || !strings.Contains(logs, "query log sink dropped events") {
				t.Fatalf("the failures were not logged:\n%s", logs)
			}
		})
	}
}

// TestHTTPSinkWithARefusedCredentialNeverSends: a reference the node does
// not allow keeps the sink from opening. It contacts nothing, counts its
// attempts and overflow, and the collector is not held up.
func TestHTTPSinkWithARefusedCredentialNeverSends(t *testing.T) {
	for kind, ok := range kinds {
		t.Run(kind, func(t *testing.T) {
			double := sinktest.New(t, ok, false)
			f := newFixture(t, nil) // no reference is allowed
			route := f.route("full", nil, entry(kind, "export", double.URL, func(s *config.QuerylogSink) { s.BatchSize = 1 }))
			for i := range 150 {
				if !route.Enqueue(event(fmt.Sprintf("q%d.example.", i))) {
					t.Fatal("event refused")
				}
			}
			waitFor(t, "the local sink to commit everything", func() bool { return f.primary.count() == 150 })
			waitFor(t, "the open attempts to be counted", func() bool { return f.status("export").Errors >= 2 })
			waitFor(t, "the overflow to be counted", func() bool { return f.dropped("export") == 50 })
			if st := f.status("export"); st.Sent != 0 || st.Queued != 100 {
				t.Fatalf("status: %+v", st)
			}
			if got := len(double.Requests()); got != 0 {
				t.Fatalf("%d requests were sent without the credential", got)
			}
			if logs := f.logged(); !strings.Contains(logs, "credential reference is not allowed") {
				t.Fatalf("the log does not say why the sink did not open:\n%s", logs)
			}
		})
	}
}

// TestHTTPSinkPicksUpARotatedCredential: the destination starts refusing
// the old credential; the refused batch is sent again with the value the
// reference now holds, and nothing is lost.
func TestHTTPSinkPicksUpARotatedCredential(t *testing.T) {
	for kind, ok := range kinds {
		t.Run(kind, func(t *testing.T) {
			double := sinktest.New(t, ok, false)
			var mu sync.Mutex
			credentials := map[string]string{"/run/secrets/export": "first-secret"}
			accepted := "first-secret"
			authorization := func(secret string) string {
				if kind == config.QuerylogSinkClickHouse {
					return sinktest.Basic("kudurru", secret)
				}
				return "Bearer " + secret
			}
			double.Answer(func(_ int, r sinktest.Request) sinktest.Answer {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != authorization(accepted) {
					return sinktest.Answer{Status: http.StatusUnauthorized}
				}
				return sinktest.Answer{}
			})
			f := newFixtureWith(t, func(_ context.Context, ref config.CredentialRef) ([]byte, error) {
				mu.Lock()
				defer mu.Unlock()
				return []byte(credentials[ref.File]), nil
			})
			route := f.route("full", nil, entry(kind, "export", double.URL, func(s *config.QuerylogSink) { s.BatchSize, s.Attempts = 1, 2 }))
			if !route.Enqueue(event("before-rotation.example.")) {
				t.Fatal("event refused")
			}
			waitFor(t, "the first batch", func() bool { return f.status("export").Sent == 1 })

			mu.Lock()
			credentials["/run/secrets/export"], accepted = "second-secret", "second-secret"
			mu.Unlock()
			if !route.Enqueue(event("after-rotation.example.")) {
				t.Fatal("event refused")
			}
			waitFor(t, "the batch after the rotation", func() bool { return f.status("export").Sent == 2 })
			if st := f.status("export"); st.Dropped != 0 || st.Errors != 1 {
				t.Fatalf("status after a rotation: %+v", st)
			}
			requests := double.Requests()
			if len(requests) != 3 || requests[1].Header.Get("Authorization") != authorization("first-secret") || requests[2].Header.Get("Authorization") != authorization("second-secret") {
				t.Fatalf("%d requests; the refused batch was not repeated with the new credential", len(requests))
			}
			if got := domains(t, kind, requests); !reflect.DeepEqual(got, []string{"before-rotation.example", "after-rotation.example", "after-rotation.example"}) {
				t.Fatalf("requests carried %v", got)
			}
			if logs := f.logged(); strings.Contains(logs, "first-secret") || strings.Contains(logs, "second-secret") {
				t.Fatalf("the server log repeats a credential:\n%s", logs)
			}
		})
	}
}

// TestHTTPSinkStalledDestinationLosesOnlyItsOwnQueries: a destination that
// accepts the request and never answers holds one attempt until its
// timeout. Meanwhile the collector, the local sink and a second, healthy
// HTTP sink carry on, and the stalled sink's overflow is counted.
func TestHTTPSinkStalledDestinationLosesOnlyItsOwnQueries(t *testing.T) {
	stalled := sinktest.New(t, http.StatusOK, false)
	healthy := sinktest.New(t, http.StatusNoContent, false)
	release := make(chan struct{})
	defer close(release)
	stalled.Answer(func(int, sinktest.Request) sinktest.Answer { return sinktest.Answer{Hang: release} })
	f := newFixture(t, map[string]string{"/run/secrets/export": "export-secret"})
	route := f.route("full", nil,
		entry(config.QuerylogSinkClickHouse, "stalled", stalled.URL, func(s *config.QuerylogSink) {
			s.BatchSize, s.Attempts, s.Timeout = 1, 3, config.Duration(time.Second)
		}),
		// Its queue holds the whole burst: this test is about the neighbor.
		entry(config.QuerylogSinkLoki, "healthy", healthy.URL, func(s *config.QuerylogSink) { s.QueueSize, s.BatchSize = 1000, 50 }),
	)
	began := time.Now()
	const total = 300
	for i := range total {
		if !route.Enqueue(event(fmt.Sprintf("q%03d.example.", i))) {
			t.Fatalf("the collector refused event %d while an exporter was stalled", i)
		}
	}
	waitFor(t, "the local sink to commit every query", func() bool { return f.primary.count() == total })
	waitFor(t, "the healthy exporter to receive every query", func() bool { return f.status("healthy").Sent == total })
	if got := len(domains(t, config.QuerylogSinkLoki, healthy.Requests())); got != total {
		t.Fatalf("the healthy exporter received %d of %d queries", got, total)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Fatalf("the healthy path took %s behind a stalled exporter", took)
	}
	// One batch is inside the stalled request and 100 wait in the queue;
	// the rest overflowed and is counted for that sink only.
	waitFor(t, "the overflow of the stalled sink", func() bool { return f.dropped("stalled") >= total-101 })
	if f.dropped("healthy") != 0 || f.status("healthy").Sent != total {
		t.Fatalf("the healthy sink lost queries: %+v", f.status("healthy"))
	}
	// The stalled attempt ends at its timeout with an unknown outcome: the
	// batch is dropped and never sent a second time.
	waitFor(t, "the stalled attempt to time out", func() bool { return f.status("stalled").Errors >= 1 })
	first := sinktest.ClickHouseRows(t, stalled.Requests()[0])[0]["domain"]
	for _, r := range stalled.Requests()[1:] {
		for _, row := range sinktest.ClickHouseRows(t, r) {
			if row["domain"] == first {
				t.Fatalf("the batch with an unknown outcome (%v) was sent again", first)
			}
		}
	}
}
