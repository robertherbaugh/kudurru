package sqlitelog

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	testclock "k8s.io/utils/clock/testing"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// pipelineFor wires the collector the way serve does: privacy, ids, tail,
// shared metrics, the writer as the sink.
func pipelineFor(t *testing.T, w *Writer, fc *testclock.FakeClock, p *querylog.Privacy, queue int) *querylog.Async {
	t.Helper()
	a := querylog.NewAsyncWith(querylog.AsyncOptions{
		Sink: w, QueueSize: queue, Privacy: p, Tail: w.opts.Tail, Metrics: w.metrics,
		FirstID: w.NextID(), Clock: fc, Logger: quietLogger(),
	})
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

// TestBatchWriterFakeClock checks the batch policy end to end: 1 000 rows
// or one tick of the 1 s interval, one transaction per batch.
func TestBatchWriterFakeClock(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := querylog.NewMetrics(reg)
	w, fc := newWriter(t, t0, func(o *Options) { o.Metrics = m })
	a := pipelineFor(t, w, fc, nil, 10000)

	for i := range 2500 {
		if !a.Enqueue(event(t0.Add(time.Duration(i)*time.Millisecond), "10.0.0.1", fmt.Sprintf("d%d.example", i%50))) {
			t.Fatal("unexpected drop")
		}
	}
	waitFor(t, "two full batches", func() bool { return w.Stats().Rows == 2000 })
	if s := w.Stats(); s.Batches != 2 {
		t.Errorf("after 2 000 rows: %+v", s)
	}
	// The remaining 500 wait for the interval tick.
	time.Sleep(20 * time.Millisecond)
	if s := w.Stats(); s.Rows != 2000 {
		t.Fatalf("partial batch flushed before the interval: %+v", s)
	}
	waitFor(t, "ticker registered", func() bool { return fc.HasWaiters() })
	fc.Step(time.Second)
	waitFor(t, "interval flush", func() bool { return w.Stats().Rows == 2500 })
	if s := w.Stats(); s.Batches != 3 {
		t.Errorf("after the tick: %+v", s)
	}
	if got := queryInt(t, w.w, `SELECT count(*) FROM query`); got != 2500 {
		t.Errorf("rows = %d", got)
	}
	// Ids were stamped by the collector and are the rowids.
	if lo, hi := queryInt(t, w.w, `SELECT min(id) FROM query`), queryInt(t, w.w, `SELECT max(id) FROM query`); lo != 1 || hi != 2500 {
		t.Errorf("ids %d..%d", lo, hi)
	}
	if got := testutil.ToFloat64(m.Events); got != 2500 {
		t.Errorf("events_total = %v", got)
	}
	if got := testutil.CollectAndCount(m.BatchRows); got != 1 {
		t.Errorf("batch_rows series = %d", got)
	}
}

// TestBatchWriterUnderLoad runs eight producers against a small queue:
// every event is either written or counted as dropped, never lost.
func TestBatchWriterUnderLoad(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := querylog.NewMetrics(reg)
	w, fc := newWriter(t, t0, func(o *Options) { o.Metrics = m })
	a := pipelineFor(t, w, fc, nil, 2000)
	const producers, each = 8, 5000
	var wg sync.WaitGroup
	for p := range producers {
		wg.Go(func() {
			for i := range each {
				a.Enqueue(event(t0.Add(time.Duration(i)*time.Microsecond), fmt.Sprintf("10.0.%d.1", p), fmt.Sprintf("d%d.example", i%500)))
			}
		})
	}
	wg.Wait()
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	written := w.Stats().Rows
	dropped := a.Dropped()
	if written+dropped != producers*each {
		t.Errorf("written %d + dropped %d != %d", written, dropped, producers*each)
	}
	if got := testutil.ToFloat64(m.Dropped.WithLabelValues(querylog.DropStageQueue, "")); uint64(got) != dropped {
		t.Errorf("dropped_total{stage=queue} = %v, want %d", got, dropped)
	}
	t.Logf("written %d, dropped %d (queue 2 000, 8 unthrottled producers)", written, dropped)
}

// TestPrivacyModes stores the same events under every mode.
func TestPrivacyModes(t *testing.T) {
	type row struct{ ip, name, domain, answer, rule, ecs string }
	for _, tc := range []struct {
		mode      querylog.Mode
		pseudonym bool
		want      row
		facts     bool
	}{
		{querylog.PrivacyFull, false, row{"192.168.7.42", "laptop", "ads.example", "1.2.3.4", "||ads.example^", "192.168.7.0/24"}, true},
		{querylog.PrivacyAnonymizeClient, false, row{"192.168.7.0", "", "ads.example", "", "||ads.example^", ""}, true},
		{querylog.PrivacyAnonymizeClient, true, row{"", "anon-", "ads.example", "", "||ads.example^", ""}, true},
		{querylog.PrivacyHideDomains, false, row{"192.168.7.42", "laptop", "hidden", "", "", "192.168.7.0/24"}, true},
		{querylog.PrivacyHideDomainsClients, false, row{"0.0.0.0", "", "hidden", "", "", ""}, true},
		{querylog.PrivacyNothing, false, row{}, false},
	} {
		t.Run(fmt.Sprintf("%s/pseudonym=%v", tc.mode, tc.pseudonym), func(t *testing.T) {
			w, fc := newWriter(t, t0, func(o *Options) {
				o.Privacy = tc.mode
				o.Tail = querylog.NewTail(16, 16, nil)
			})
			p, err := querylog.NewPrivacy(querylog.PrivacyOptions{Mode: tc.mode, Pseudonym: tc.pseudonym, Clock: fc})
			if err != nil {
				t.Fatal(err)
			}
			a := pipelineFor(t, w, fc, p, 100)
			ev := event(t0, "192.168.7.42", "ads.example", withClientName("laptop"), withAnswer("1.2.3.4"),
				withStatus(pipeline.StatusBlockedList), withList("oisd", "||ads.example^"))
			ev.ECS = netip.MustParsePrefix("192.168.7.0/24")
			a.Enqueue(ev)
			if err := a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			w2, _ := newWriter(t, t0, func(o *Options) { o.Path = w.opts.Path; o.Privacy = tc.mode })
			db := w2.w
			if n := queryInt(t, db, `SELECT count(*) FROM query`); (n == 1) != tc.facts {
				t.Fatalf("fact rows = %d, want facts=%v", n, tc.facts)
			}
			if n := queryInt(t, db, `SELECT sum(total) FROM rollup_1m`); n != 1 {
				t.Errorf("rollup_1m total = %d (totals survive every mode)", n)
			}
			if n := queryInt(t, db, `SELECT count(*) FROM rollup_client_1m`); (n == 1) != tc.facts {
				t.Errorf("rollup_client_1m rows = %d", n)
			}
			if !tc.facts {
				if n := queryInt(t, db, `SELECT count(*) FROM dict_domain`) + queryInt(t, db, `SELECT count(*) FROM dict_client`); n != 0 {
					t.Errorf("nothing mode stored %d dictionary rows", n)
				}
				return
			}
			var got row
			if err := db.QueryRow(`SELECT c.ip, coalesce(c.name,''), d.name, coalesce(q.answer,''), coalesce(q.rule,''), coalesce(q.ecs,'')
				FROM query q JOIN dict_client c ON c.id=q.client JOIN dict_domain d ON d.id=q.domain`).Scan(
				&got.ip, &got.name, &got.domain, &got.answer, &got.rule, &got.ecs); err != nil {
				t.Fatal(err)
			}
			if tc.pseudonym {
				if !strings.HasPrefix(got.name, tc.want.name) || len(got.name) != len("anon-")+12 {
					t.Errorf("pseudonym %q", got.name)
				}
				got.name = tc.want.name
			}
			if got != tc.want {
				t.Errorf("stored %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestNothingModeHasNoTail: the live tail must not leak what is not stored.
func TestNothingModeHasNoTail(t *testing.T) {
	tail := querylog.NewTail(16, 16, nil)
	w, fc := newWriter(t, t0, func(o *Options) { o.Privacy = querylog.PrivacyNothing; o.Tail = tail })
	p, _ := querylog.NewPrivacy(querylog.PrivacyOptions{Mode: querylog.PrivacyNothing})
	a := pipelineFor(t, w, fc, p, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := w.Reader().Subscribe(ctx)
	a.Enqueue(event(t0, "10.0.0.1", "a.example"))
	_ = a.Close(context.Background())
	select {
	case ev := <-ch:
		t.Fatalf("tail delivered %+v under privacy nothing", ev)
	case <-time.After(20 * time.Millisecond):
	}
	if n := len(tail.Recent(0, 0)); n != 0 {
		t.Errorf("hot ring holds %d events", n)
	}
}

// TestExcludedEventsAreNotStored covers querylog.exclude.* end to end.
func TestExcludedEventsAreNotStored(t *testing.T) {
	w, fc := newWriter(t, t0)
	p, err := querylog.NewPrivacy(querylog.PrivacyOptions{
		ExcludeDomains: []string{"*.lan", "telemetry.example", "/^wpad\\./"},
		ExcludeClients: []string{"10.9.0.0/16", "printer"},
		ExcludeSUDN:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := pipelineFor(t, w, fc, p, 100)
	for _, ev := range []*querylog.Event{
		event(t0, "10.0.0.1", "nas.lan"),
		event(t0, "10.0.0.1", "lan"),
		event(t0, "10.0.0.1", "telemetry.example"),
		event(t0, "10.0.0.1", "wpad.corp.example"),
		event(t0, "10.9.4.4", "kept-but-client-excluded.example"),
		event(t0, "10.0.0.2", "x.example", withClientName("printer")),
		event(t0, "10.0.0.1", "4.3.168.192.in-addr.arpa"),
		event(t0, "10.0.0.1", "mybox.local"),
		event(t0, "10.0.0.1", "kept.example"),
	} {
		a.Enqueue(ev)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.Stats().Rows; got != 1 {
		t.Errorf("stored %d events, want only kept.example", got)
	}
}

// TestWriteRollbackKeepsDictionaryConsistent: a failed batch must not leave
// cached ids pointing at rolled-back dictionary rows.
func TestWriteRollbackKeepsDictionaryConsistent(t *testing.T) {
	w, _ := newWriter(t, t0)
	mustWrite(t, w, func() *querylog.Event { e := event(t0, "10.0.0.1", "a.example"); e.ID = 7; return e }())
	// Same explicit id: the insert fails after new.example was added to
	// the dictionary inside the transaction.
	dup := event(t0, "10.0.0.1", "new.example")
	dup.ID = 7
	if err := w.Write([]*querylog.Event{dup}); err == nil {
		t.Fatal("duplicate id must fail")
	}
	mustWrite(t, w, event(t0, "10.0.0.1", "new.example"))
	if n := queryInt(t, w.w, `SELECT count(*) FROM query q JOIN dict_domain d ON d.id=q.domain WHERE d.name='new.example'`); n != 1 {
		t.Errorf("row for new.example joins %d dictionary rows", n)
	}
	if got := w.Stats().Rows; got != 2 {
		t.Errorf("rows = %d", got)
	}
}

func TestStoredFields(t *testing.T) {
	w, _ := newWriter(t, t0)
	ev := event(t0, "2001:db8::42", "WWW.Example.COM", withProto(pipeline.ProtoDoH3), withZone("example.com."),
		withStatus(pipeline.StatusAuthoritative), withUpstream(""), withAnswer(strings.Repeat("x", 400)))
	ev.HasEDE, ev.EDE = true, 18
	ev.TraceID = [16]byte{1, 2, 3}
	ev.Flags = querylog.Flags{RD: true, DO: true, AD: true}
	dropped := event(t0, "10.0.0.1", "slow.example", withStatus(pipeline.StatusDropped), withReply(querylog.ReplyNone))
	mustWrite(t, w, ev, dropped)
	var proto, ede, flags int64
	var domain, zone, upstream, answer string
	var trace []byte
	if err := w.w.QueryRow(`SELECT q.proto, q.ede, q.flags, d.name, z.name, u.addr, q.answer, q.trace_id FROM query q
		JOIN dict_domain d ON d.id=q.domain JOIN dict_zone z ON z.id=q.zone JOIN dict_upstream u ON u.id=q.upstream WHERE q.id=1`).Scan(
		&proto, &ede, &flags, &domain, &zone, &upstream, &answer, &trace); err != nil {
		t.Fatal(err)
	}
	if proto != 6 || ede != 18 || flags != int64(querylog.FlagRD|querylog.FlagDO|querylog.FlagAD) || domain != "www.example.com" ||
		zone != "example.com" || upstream != "zone:example.com" || len(answer) != maxAnswerBytes || len(trace) != 16 {
		t.Errorf("stored proto=%d ede=%d flags=%d domain=%q zone=%q upstream=%q answer=%d trace=%x", proto, ede, flags, domain, zone, upstream, len(answer), trace)
	}
	if n := queryInt(t, w.w, `SELECT count(*) FROM query WHERE id=2 AND latency_us IS NULL`); n != 1 {
		t.Error("dropped query must have a NULL latency")
	}
	if e := queryInt(t, w.w, `SELECT count(*) FROM dict_domain WHERE name='www.example.com' AND etld1='example.com'`); e != 1 {
		t.Error("etld1 not recorded")
	}
}
