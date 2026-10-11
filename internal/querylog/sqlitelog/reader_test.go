package sqlitelog

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	testclock "k8s.io/utils/clock/testing"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// seeded builds a small, hand-countable query log spanning yesterday (a
// derived day), two derived hours, a pending hour and the current,
// unflushed minute. now is 12:00:30.
func seeded(t *testing.T) (*Writer, *testclock.FakeClock) {
	t.Helper()
	open := t0.Add(-3 * time.Hour) // 09:00: hourDone 09:00, dayDone today 00:00
	w, fc := newWriter(t, open, func(o *Options) {
		o.Tail = querylog.NewTail(64, 16, nil)
		o.DomainsOnLists = func() int64 { return 123456 }
	})
	y := t0.Add(-24*time.Hour - 105*time.Minute) // yesterday 10:15
	at := func(h, m, s int) time.Time { return time.Date(2026, 10, 11, h, m, s, 0, time.UTC) }
	blocked := func(list string) []evOpt {
		return []evOpt{withStatus(pipeline.StatusBlockedList), withList(list, "||x^")}
	}
	evs := []*querylog.Event{
		event(y, "10.0.0.1", "a.example"),                                                                                                   // 1
		event(y.Add(time.Second), "10.0.0.1", "ads.example", blocked("oisd")...),                                                            // 2
		event(at(9, 10, 0), "10.0.0.2", "a.example", withStatus(pipeline.StatusCached), withUpstream("")),                                   // 3
		event(at(9, 10, 1), "10.0.0.2", "ads.example", blocked("oisd")...),                                                                  // 4
		event(at(9, 10, 2), "10.0.0.3", "b.example", withUpstream("internal-ad"), withQType(28)),                                            // 5
		event(at(10, 20, 0), "10.0.0.1", "tracker.example", withStatus(pipeline.StatusBlockedRegex), withList("stevenblack", "/track/")),    // 6
		event(at(10, 20, 1), "10.0.0.1", "a.example"),                                                                                       // 7
		event(at(11, 40, 0), "10.0.0.3", "nas.home.lan", withStatus(pipeline.StatusAuthoritative), withZone("home.lan."), withUpstream("")), // 8
		event(at(11, 40, 1), "10.0.0.2", "ads.example", blocked("oisd")...),                                                                 // 9
		event(at(11, 40, 2), "10.0.0.2", "b.example", withList("allow-corp", "@@||b.example^")),                                             // 10
		event(at(12, 0, 10), "10.0.0.1", "a.example"),                                                                                       // 11
		event(at(12, 0, 11), "10.0.0.4", "ads.example", append(blocked("oisd"), withClientName("phone"))...),                                // 12
	}
	ctx := context.Background()
	for i, ev := range evs {
		ev.ID = uint64(i + 1)
		ev.Latency = time.Duration(i+1) * time.Millisecond
		if ev.Time.After(fc.Now()) {
			fc.SetTime(ev.Time)
			if err := w.Maintain(ctx); err != nil {
				t.Fatal(err)
			}
		}
		mustWrite(t, w, ev)
	}
	fc.SetTime(at(12, 0, 30))
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if want := at(11, 0, 0).Unix(); w.hourDone != want {
		t.Fatalf("seed: hourDone %s, want 11:00", time.Unix(w.hourDone, 0).UTC())
	}
	if len(w.acc.minutes) != 1 || len(w.acc.hours) != 2 {
		t.Fatalf("seed: %d pending minutes, %d pending hours", len(w.acc.minutes), len(w.acc.hours))
	}
	return w, fc
}

var allTime = Window{From: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 10, 11, 12, 1, 0, 0, time.UTC)}

func keys(items []TopItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Key+"="+itoa(it.Count))
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestReaderSummary(t *testing.T) {
	w, _ := seeded(t)
	r := w.Reader()
	ctx := context.Background()
	s, err := r.Summary(ctx, SummaryQuery{Window: allTime})
	if err != nil {
		t.Fatal(err)
	}
	q := s.Queries
	if q.Total != 12 || q.Blocked != 5 || q.Cached != 1 || q.Forwarded != 5 || q.Authoritative != 1 || q.Other != 0 {
		t.Errorf("counters: %+v", q)
	}
	if q.PercentBlocked != 41.67 || q.UniqueDomains != 5 {
		t.Errorf("percent %v, unique domains %d", q.PercentBlocked, q.UniqueDomains)
	}
	if s.Clients != (SummaryClients{Active: 4, Total: 4}) || s.DomainsOnLists != 123456 {
		t.Errorf("clients %+v, domains on lists %d", s.Clients, s.DomainsOnLists)
	}
	if s.Rcodes["NOERROR"] != 12 || s.Protocols["udp"] != 12 || s.Types["A"] != 11 || s.Types["AAAA"] != 1 {
		t.Errorf("rcodes %v protocols %v types %v", s.Rcodes, s.Protocols, s.Types)
	}
	// Latencies 1..12 ms: avg 6.5; p50 → the ≤10 ms bucket; p95/p99 → ≤25 ms.
	if s.Latency != (LatencySummary{AvgMS: 6.5, P50MS: 10, P95MS: 25, P99MS: 25}) {
		t.Errorf("latency %+v", s.Latency)
	}
	if s.Scope.Scope != ScopeNode || len(s.Nodes) != 1 || s.Nodes[0].Name != "node-a" || !s.Nodes[0].OK || s.Partial || s.Approximate {
		t.Errorf("scope %+v approximate %v", s.Scope, s.Approximate)
	}
	// Default window: the last 24 h (yesterday 10:15 falls out).
	d, err := r.Summary(ctx, SummaryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Queries.Total != 10 || d.Queries.QPS1m != 0 {
		t.Errorf("default window total %d, qps_1m %v", d.Queries.Total, d.Queries.QPS1m)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"scope":"node"`, `"partial":false`, `"percent_blocked":41.67`, `"domains_on_lists":123456`, `"nodes":[{"name":"node-a","ok":true`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON lacks %s: %s", k, b)
		}
	}
}

func TestReaderTimeseries(t *testing.T) {
	w, _ := seeded(t)
	r := w.Reader()
	ts, err := r.Timeseries(context.Background(), TimeseriesQuery{
		Window: Window{From: t0.Add(-3 * time.Hour), Until: t0.Add(time.Minute)}, Step: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	type p struct{ total, blocked, cached, fwd, auth int64 }
	want := []p{{3, 1, 1, 1, 0}, {2, 1, 0, 1, 0}, {3, 1, 0, 1, 1}, {2, 1, 0, 1, 0}}
	if ts.Step != 3600 || len(ts.Points) != len(want) {
		t.Fatalf("step %d, %d points", ts.Step, len(ts.Points))
	}
	for i, pt := range ts.Points {
		got := p{pt.Total, pt.Blocked, pt.Cached, pt.Forwarded, pt.Authoritative}
		if got != want[i] || !pt.TS.Equal(t0.Add(time.Duration(i-3)*time.Hour)) || pt.Other != 0 {
			t.Errorf("point %d (%s) = %+v, want %+v", i, pt.TS, got, want[i])
		}
	}
	// Auto step: 24 h → 5-minute buckets, 288 points, dense.
	auto, err := r.Timeseries(context.Background(), TimeseriesQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, pt := range auto.Points {
		sum += pt.Total
	}
	if auto.Step != 300 || len(auto.Points) != 289 || sum != 10 {
		t.Errorf("auto: step %d, %d points, total %d", auto.Step, len(auto.Points), sum)
	}
	// The whole range at a daily step uses rollup_1d for yesterday.
	day, err := r.Timeseries(context.Background(), TimeseriesQuery{Window: allTime, Step: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(day.Points) != 2 || day.Points[0].Total != 2 || day.Points[1].Total != 10 {
		t.Errorf("daily points %+v", day.Points)
	}
}

func TestReaderTopLists(t *testing.T) {
	w, fc := seeded(t)
	r := w.Reader()
	ctx := context.Background()
	check := func(stage string) {
		for _, tc := range []struct {
			name string
			fn   func(context.Context, TopQuery) (*TopList, error)
			q    TopQuery
			want []string
		}{
			{"blocked domains", r.TopDomains, TopQuery{Window: allTime, Blocked: true}, []string{"ads.example=4", "tracker.example=1"}},
			{"permitted domains", r.TopDomains, TopQuery{Window: allTime}, []string{"a.example=4", "b.example=2", "nas.home.lan=1"}},
			{"blocked clients", r.TopClients, TopQuery{Window: allTime, Blocked: true}, []string{"10.0.0.1=2", "10.0.0.2=2", "10.0.0.4=1"}},
			{"permitted clients", r.TopClients, TopQuery{Window: allTime, N: 2}, []string{"10.0.0.1=3", "10.0.0.2=2"}},
			{"blocking lists", r.TopLists, TopQuery{Window: allTime, Blocked: true}, []string{"oisd=4", "stevenblack=1"}},
			{"allow lists", r.TopLists, TopQuery{Window: allTime}, []string{"allow-corp=1"}},
			{"zones", r.TopZones, TopQuery{Window: allTime}, []string{"home.lan=1"}},
			{"today's blocked domains", r.TopDomains, TopQuery{Window: Window{From: t0.Add(-3 * time.Hour), Until: t0.Add(time.Minute)}, Blocked: true}, []string{"ads.example=3", "tracker.example=1"}},
			{"partial-hour window (facts)", r.TopDomains, TopQuery{Window: Window{From: t0.Add(-20 * time.Minute), Until: t0.Add(time.Minute)}}, []string{"a.example=1", "b.example=1", "nas.home.lan=1"}},
		} {
			got, err := tc.fn(ctx, tc.q)
			if err != nil {
				t.Fatalf("%s %s: %v", stage, tc.name, err)
			}
			if k := keys(got.Items); !reflect.DeepEqual(k, tc.want) {
				t.Errorf("%s %s: %v, want %v", stage, tc.name, k, tc.want)
			}
		}
	}
	check("pending")
	bd, _ := r.TopDomains(ctx, TopQuery{Window: allTime, Blocked: true})
	if bd.TotalInWindow != 5 || bd.Items[0].Percent != 80 || bd.Items[0].Total != 4 {
		t.Errorf("blocked domains: total_in_window %d, first %+v", bd.TotalInWindow, bd.Items[0])
	}
	cl, _ := r.TopClients(ctx, TopQuery{Window: allTime, Blocked: true})
	if cl.Items[2].Name != "phone" {
		t.Errorf("client name not resolved: %+v", cl.Items[2])
	}

	// The same answers once hour 11 is derived and the minute flushed.
	fc.SetTime(t0.Add(2 * time.Minute))
	if err := w.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(w.acc.hours) != 1 || len(w.acc.minutes) != 0 {
		t.Fatalf("after derivation: %d pending hours, %d minutes", len(w.acc.hours), len(w.acc.minutes))
	}
	check("derived")
}

func TestReaderUpstreams(t *testing.T) {
	w, _ := seeded(t)
	u, err := w.Reader().TopUpstreams(context.Background(), TopQuery{Window: allTime})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range u.Items {
		got = append(got, it.Addr+"="+itoa(it.Total))
	}
	want := []string{"default=4", "cache=1", "internal-ad=1", "zone:home.lan=1"} // ties by first appearance
	if !reflect.DeepEqual(got, want) {
		t.Errorf("upstreams %v, want %v", got, want)
	}
	// default answered events 1, 7, 10, 11 in 1, 7, 10, 11 ms.
	if d := u.Items[0]; d.AvgMS != 7.25 || d.P95MS != 25 || d.Percent != 57.14 {
		t.Errorf("default: %+v", d)
	}
}

func TestReaderClientTimeseries(t *testing.T) {
	w, _ := seeded(t)
	ct, err := w.Reader().ClientTimeseries(context.Background(), ClientTimeseriesQuery{
		Window: Window{From: t0.Add(-3 * time.Hour), Until: t0.Add(time.Minute)}, Step: time.Hour, N: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range ct.Clients {
		names = append(names, c.Key)
	}
	if !reflect.DeepEqual(names, []string{"10.0.0.2", "10.0.0.1", "other"}) {
		t.Fatalf("clients %v", names)
	}
	want := [][]int64{{2, 0, 1}, {0, 2, 0}, {2, 0, 1}, {0, 1, 1}}
	for i, p := range ct.Points {
		if !reflect.DeepEqual(p.Counts, want[i]) {
			t.Errorf("bucket %s: %v, want %v", p.TS, p.Counts, want[i])
		}
	}
}

func TestReaderRecentBlocked(t *testing.T) {
	w, _ := seeded(t)
	rb, err := w.Reader().RecentBlocked(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range rb.Items {
		got = append(got, strconv.FormatUint(it.ID, 10)+":"+it.Domain+":"+it.Status+":"+it.List)
	}
	want := []string{"12:ads.example:blocked_list:oisd", "9:ads.example:blocked_list:oisd", "6:tracker.example:blocked_regex:stevenblack"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recent blocked %v, want %v", got, want)
	}
}

func TestReaderQueries(t *testing.T) {
	w, _ := seeded(t)
	r := w.Reader()
	ctx := context.Background()
	ids := func(p *QueryPage) []int64 {
		out := []int64{}
		for _, q := range p.Queries {
			out = append(out, int64(q.ID)) //nolint:gosec // test ids are tiny
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		f        QueryFilter
		want     []int64
		filtered int64
	}{
		{"all", QueryFilter{}, []int64{12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, 12},
		{"blocked class", QueryFilter{StatusClass: pipeline.ClassBlocked}, []int64{12, 9, 6, 4, 2}, 5},
		{"cached class", QueryFilter{StatusClass: pipeline.ClassCached}, []int64{3}, 1},
		{"status list", QueryFilter{Status: []pipeline.Status{pipeline.StatusBlockedRegex, pipeline.StatusAuthoritative}}, []int64{8, 6}, 2},
		{"exact domain", QueryFilter{Domain: "ADS.example."}, []int64{12, 9, 4, 2}, 4},
		{"wildcard domain", QueryFilter{Domain: "*.home.*"}, []int64{8}, 1},
		{"client ip", QueryFilter{Client: "10.0.0.2"}, []int64{10, 9, 4, 3}, 4},
		{"client cidr", QueryFilter{Client: "10.0.0.0/31"}, []int64{11, 7, 6, 2, 1}, 5},
		{"client name", QueryFilter{Client: "ph*"}, []int64{12}, 1},
		{"qtype", QueryFilter{QType: 28}, []int64{5}, 1},
		{"upstream", QueryFilter{Upstream: "internal-ad"}, []int64{5}, 1},
		{"list", QueryFilter{List: "oisd", Client: "10.0.0.2"}, []int64{9, 4}, 2},
		{"zone", QueryFilter{Zone: "home.lan"}, []int64{8}, 1},
		{"since/until", QueryFilter{Since: t0.Add(-2 * time.Hour), Until: t0}, []int64{10, 9, 8, 7, 6}, 5},
		{"no match", QueryFilter{Domain: "nope.example"}, []int64{}, 0},
	} {
		p, err := r.Queries(ctx, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := ids(p); !reflect.DeepEqual(got, tc.want) || p.RecordsFiltered != tc.filtered || p.RecordsTotal != 12 {
			t.Errorf("%s: ids %v (want %v), filtered %d (want %d), total %d", tc.name, got, tc.want, p.RecordsFiltered, tc.filtered, p.RecordsTotal)
		}
	}

	// Keyset pagination, stable under inserts between pages.
	p1, err := r.Queries(ctx, QueryFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, w, func() *querylog.Event {
		e := event(t0.Add(15*time.Second), "10.0.0.9", "new.example")
		e.ID = 13
		return e
	}())
	p2, _ := r.Queries(ctx, QueryFilter{Limit: 5, Cursor: p1.CursorNext})
	p3, _ := r.Queries(ctx, QueryFilter{Limit: 5, Cursor: p2.CursorNext})
	if !reflect.DeepEqual(ids(p1), []int64{12, 11, 10, 9, 8}) || !reflect.DeepEqual(ids(p2), []int64{7, 6, 5, 4, 3}) ||
		!reflect.DeepEqual(ids(p3), []int64{2, 1}) || p3.CursorNext != "" || p1.CursorNext != "8" {
		t.Errorf("pages %v %v %v (cursors %q %q %q)", ids(p1), ids(p2), ids(p3), p1.CursorNext, p2.CursorNext, p3.CursorNext)
	}
	if p2.RecordsTotal != 13 {
		t.Errorf("records_total after insert = %d", p2.RecordsTotal)
	}
	if _, err := r.Queries(ctx, QueryFilter{Cursor: "x"}); err == nil {
		t.Error("bad cursor accepted")
	}
	if _, err := r.Queries(ctx, QueryFilter{StatusClass: "purple", Status: []pipeline.Status{pipeline.StatusCached}}); err == nil {
		t.Error("unknown status class accepted")
	}

	// Row shape.
	row := p1.Queries[0]
	if row.Domain != "ads.example" || row.Client.IP != "10.0.0.4" || row.Client.Name != "phone" || row.Status != "blocked_list" ||
		row.StatusClass != "blocked" || row.List != "oisd" || row.Rule != "||x^" || row.QType != "A" || row.Rcode != "NOERROR" ||
		row.Proto != "udp" || row.Node != "node-a" || row.Reply.Type != "ip" || row.Reply.TimeMS == nil || *row.Reply.TimeMS != 12 ||
		row.DNSSEC != "unknown" || !row.TS.Equal(time.Date(2026, 10, 11, 12, 0, 11, 0, time.UTC)) {
		t.Errorf("row %+v", row)
	}
	if p1.EarliestTSDisk == nil || !p1.EarliestTSDisk.Equal(time.Date(2026, 10, 10, 10, 15, 0, 0, time.UTC)) {
		t.Errorf("earliest_ts_disk %v", p1.EarliestTSDisk)
	}
}

func TestReaderSuggestions(t *testing.T) {
	w, _ := seeded(t)
	r := w.Reader()
	for _, tc := range []struct {
		field, prefix string
		want          []string
	}{
		{FieldDomain, "a", []string{"a.example", "ads.example"}},
		{FieldDomain, "AD", []string{"ads.example"}},
		{FieldClient, "10.0.0.", []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"}},
		{FieldClient, "ph", []string{"phone"}},
		{FieldUpstream, "", []string{"cache", "default", "internal-ad", "zone:home.lan"}},
		{FieldList, "s", []string{"stevenblack"}},
		{FieldZone, "home", []string{"home.lan"}},
	} {
		s, err := r.Suggestions(context.Background(), tc.field, tc.prefix, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s.Values, tc.want) {
			t.Errorf("%s %q: %v, want %v", tc.field, tc.prefix, s.Values, tc.want)
		}
	}
	if _, err := r.Suggestions(context.Background(), "rule", "", 0); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestReaderStatus(t *testing.T) {
	w, _ := seeded(t)
	st, err := w.Reader().Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Rows != 12 || st.DBBytes == 0 || st.Privacy != "full" || st.Retention.RawSeconds != 30*86400 || st.Writer.Rows != 12 ||
		st.OldestTS == nil || st.Retention.Rollup1d != "forever" {
		t.Errorf("status %+v", st)
	}
}

// TestLiveTail subscribes through the Reader and receives the collector's
// events, after privacy, with their ids.
func TestLiveTail(t *testing.T) {
	tail := querylog.NewTail(8, 4, nil)
	w, fc := newWriter(t, t0, func(o *Options) { o.Tail = tail })
	p, _ := querylog.NewPrivacy(querylog.PrivacyOptions{Mode: querylog.PrivacyHideDomains})
	a := pipelineFor(t, w, fc, p, 100)
	ctx, cancel := context.WithCancel(context.Background())
	ch := w.Reader().Subscribe(ctx)
	waitFor(t, "subscriber", func() bool { return tail.Subscribers() == 1 })
	for i := range 3 {
		a.Enqueue(event(t0.Add(time.Duration(i)*time.Second), "10.0.0.1", "secret.example"))
	}
	for i := range 3 {
		select {
		case ev := <-ch:
			if ev.ID != uint64(i+1) || ev.Name != querylog.HiddenName {
				t.Errorf("tail event %d: id %d name %q", i, ev.ID, ev.Name)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("tail event missing")
		}
	}
	cancel()
	waitFor(t, "unsubscribe", func() bool { return tail.Subscribers() == 0 })
	if _, open := <-ch; open {
		t.Error("tail channel still open after cancel")
	}
	if recent := tail.Recent(10, 1); len(recent) != 2 || recent[0].ID != 2 {
		t.Errorf("Recent after id 1: %d events", len(recent))
	}
}

func TestReaderAfterClose(t *testing.T) {
	w, _ := newWriter(t, t0)
	_ = w.Close()
	if _, err := w.Reader().Summary(context.Background(), SummaryQuery{}); err == nil {
		t.Error("read after close must fail")
	}
	if err := w.Write([]*querylog.Event{event(t0, "10.0.0.1", "a.example")}); err == nil {
		t.Error("write after close must fail")
	}
}

// TestReopenRebuildsPendingHours: after a restart the pending-hour cache is
// rebuilt from the facts, so the read API answers exactly as before.
func TestReopenRebuildsPendingHours(t *testing.T) {
	w, fc := seeded(t)
	ctx := context.Background()
	q := TopQuery{Window: allTime}
	before, err := w.Reader().TopDomains(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := w.Reader().Summary(ctx, SummaryQuery{Window: allTime})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w2, _ := newWriter(t, fc.Now(), func(o *Options) { o.Path = w.opts.Path })
	if len(w2.acc.hours) != 2 || w2.hourDone != w.hourDone {
		t.Fatalf("reopened: %d pending hours, hourDone %d (was %d)", len(w2.acc.hours), w2.hourDone, w.hourDone)
	}
	after, err := w2.Reader().TopDomains(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys(before.Items), keys(after.Items)) {
		t.Errorf("top domains changed across a restart: %v -> %v", keys(before.Items), keys(after.Items))
	}
	sa, _ := w2.Reader().Summary(ctx, SummaryQuery{Window: allTime})
	if sa.Queries != sb.Queries {
		t.Errorf("summary changed across a restart:\n%+v\n%+v", sb.Queries, sa.Queries)
	}
}
