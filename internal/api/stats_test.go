package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// querylogFixture opens a sqlite query log with a few events.
func querylogFixture(t *testing.T) (*sqlitelog.Writer, *querylog.Tail) {
	t.Helper()
	tail := querylog.NewTail(1024, 64, nil)
	w, err := sqlitelog.Open(context.Background(), sqlitelog.Options{
		Path: filepath.Join(t.TempDir(), sqlitelog.FileName), Node: "node-a", Privacy: querylog.PrivacyFull,
		RawRetention: 24 * time.Hour, Tail: tail, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	now := time.Now().Add(-2 * time.Minute)
	evs := []*querylog.Event{
		qev(1, now, "10.0.0.1", "a.example.", pipeline.StatusForwarded, ""),
		qev(2, now.Add(time.Second), "10.0.0.2", "ads.example.", pipeline.StatusBlockedList, "default/ads"),
		qev(3, now.Add(2*time.Second), "10.0.0.1", "b.example.", pipeline.StatusCached, ""),
		qev(4, now.Add(3*time.Second), "10.0.0.3", "ads.example.", pipeline.StatusBlockedList, "default/ads"),
	}
	if err := w.Write(evs); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w, tail
}

func qev(id uint64, ts time.Time, client, name string, st pipeline.Status, list string) *querylog.Event {
	ev := &querylog.Event{
		ID: id, Time: ts, Latency: time.Millisecond, Node: "node-a", Proto: pipeline.ProtoUDP,
		Client: netip.MustParseAddr(client), Name: name, QType: 1, QClass: 1, Status: st, List: list,
		ReplyType: querylog.ReplyIP,
	}
	if list != "" {
		ev.Rule = "||ads.example^"
	} else if st == pipeline.StatusForwarded {
		ev.Upstream = "default"
	}
	return ev
}

func TestStatsRoutes(t *testing.T) {
	w, tail := querylogFixture(t)
	h := newHarness(t, func(o *Options) { o.Querylog = w.Reader(); o.Tail = tail })

	var sum SummaryBody
	h.do(http.MethodGet, "/api/v1/stats/summary?window=1h", "viewer", nil).want(http.StatusOK).json(&sum)
	if sum.Queries.Total != 4 || sum.Queries.Blocked != 2 || sum.Scope.Scope != "node" || len(sum.Nodes) != 1 || sum.Nodes[0].Name != "node-a" || sum.Partial {
		t.Fatalf("summary %+v", sum)
	}
	var top TopBody
	h.do(http.MethodGet, "/api/v1/stats/top?dim=domains&blocked=true&window=1h", "viewer", nil).want(http.StatusOK).json(&top)
	if len(top.Items) != 1 || top.Items[0].Key != "ads.example" || top.Items[0].Count != 2 {
		t.Fatalf("top %+v", top)
	}
	h.do(http.MethodGet, "/api/v1/stats/top?dim=upstreams&window=1h", "viewer", nil).want(http.StatusOK).json(&top)
	if top.Dimension != "upstreams" {
		t.Fatalf("top upstreams %+v", top)
	}
	h.do(http.MethodGet, "/api/v1/stats/top?dim=nope", "viewer", nil).want(http.StatusBadRequest)
	var ts TimeseriesBody
	h.do(http.MethodGet, "/api/v1/stats/timeseries?step=auto&window=1h", "viewer", nil).want(http.StatusOK).json(&ts)
	var total int64
	for _, p := range ts.Points {
		total += p.Total
	}
	if total != 4 {
		t.Fatalf("timeseries total %d", total)
	}
	h.do(http.MethodGet, "/api/v1/stats/timeseries?from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z", "viewer", nil).want(http.StatusBadRequest)
	h.do(http.MethodGet, "/api/v1/stats/clients/timeseries?window=1h", "viewer", nil).want(http.StatusOK)
	h.do(http.MethodGet, "/api/v1/stats/upstreams?window=1h", "viewer", nil).want(http.StatusOK)
	var rb RecentBlockedBody
	h.do(http.MethodGet, "/api/v1/stats/recent_blocked?n=5", "operator", nil).want(http.StatusOK).json(&rb)
	if len(rb.Items) != 2 {
		t.Fatalf("recent blocked %+v", rb)
	}
	var clients struct {
		Items []ClientItem `json:"items"`
	}
	h.do(http.MethodGet, "/api/v1/clients?window=1h", "viewer", nil).want(http.StatusOK).json(&clients)
	if len(clients.Items) != 3 {
		t.Fatalf("clients %+v", clients)
	}
}

func TestQueryLogRoutes(t *testing.T) {
	w, tail := querylogFixture(t)
	h := newHarness(t, func(o *Options) { o.Querylog = w.Reader(); o.Tail = tail })

	var page QueriesBody
	h.do(http.MethodGet, "/api/v1/queries?limit=2", "operator", nil).want(http.StatusOK).json(&page)
	if len(page.Queries) != 2 || page.Queries[0].ID != 4 || page.CursorNext != "3" || page.RecordsTotal != 4 {
		t.Fatalf("page 1 %+v", page)
	}
	h.do(http.MethodGet, "/api/v1/queries?limit=2&cursor="+page.CursorNext, "operator", nil).want(http.StatusOK).json(&page)
	if len(page.Queries) != 2 || page.Queries[1].ID != 1 {
		t.Fatalf("page 2 %+v", page)
	}
	h.do(http.MethodGet, "/api/v1/queries?status_class=blocked&domain=ads.*", "operator", nil).want(http.StatusOK).json(&page)
	if page.RecordsFiltered != 2 {
		t.Fatalf("filtered %+v", page)
	}
	h.do(http.MethodGet, "/api/v1/queries?status=nope", "operator", nil).want(http.StatusBadRequest)
	h.do(http.MethodGet, "/api/v1/queries?cursor=abc", "operator", nil).want(http.StatusBadRequest)
	var sug SuggestionsBody
	h.do(http.MethodGet, "/api/v1/queries/suggestions?field=domain&prefix=a", "operator", nil).want(http.StatusOK).json(&sug)
	if len(sug.Values) != 2 {
		t.Fatalf("suggestions %+v", sug)
	}
	var status QuerylogStatusBody
	h.do(http.MethodGet, "/api/v1/querylog/status", "operator", nil).want(http.StatusOK).json(&status)
	if status.Rows != 4 {
		t.Fatalf("status %+v", status)
	}
	// Allow the domain of query 2 through the UI list.
	var p Processed
	h.do(http.MethodPost, "/api/v1/queries/2/actions", "operator", map[string]any{"action": "allow"}).want(http.StatusOK).json(&p)
	if len(p.Processed.Success) != 1 || p.Processed.Success[0].Item != "ads.example" {
		t.Fatalf("action %+v", p)
	}
	h.do(http.MethodPost, "/api/v1/queries/99/actions", "operator", map[string]any{"action": "allow"}).want(http.StatusNotFound)
	// No purger wired.
	h.do(http.MethodDelete, "/api/v1/querylog", "admin", nil).want(http.StatusNotImplemented)
}

func TestQueryLogDisabled(t *testing.T) {
	h := newHarness(t)
	r := h.do(http.MethodGet, "/api/v1/stats/summary", "viewer", nil).want(http.StatusServiceUnavailable)
	if !strings.Contains(r.Body.String(), "querylog.enabled") {
		t.Fatalf("no hint: %s", r.Body.String())
	}
}

type purgeFunc func(time.Time) (int64, error)

func (f purgeFunc) Purge(_ context.Context, before time.Time) (int64, error) { return f(before) }

func TestQuerylogPurge(t *testing.T) {
	var got time.Time
	h := newHarness(t, func(o *Options) {
		o.Purger = purgeFunc(func(b time.Time) (int64, error) { got = b; return 7, nil })
	})
	var res PurgeResult
	h.do(http.MethodDelete, "/api/v1/querylog?before=2026-10-01T00:00:00Z", "admin", nil).want(http.StatusOK).json(&res)
	if res.Deleted != 7 || !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("purge %+v %v", res, got)
	}
	h.do(http.MethodDelete, "/api/v1/querylog", "operator", nil).want(http.StatusForbidden)
}

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	id, event, data string
}

func readEvents(t *testing.T, rd *bufio.Reader, n int) []sseEvent {
	t.Helper()
	var out []sseEvent
	var cur sseEvent
	for len(out) < n {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read after %d events: %v", len(out), err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if cur.data != "" {
				out = append(out, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

func TestQueryStream(t *testing.T) {
	tail := querylog.NewTail(1024, 64, nil)
	h := newHarness(t, func(o *Options) { o.Tail = tail })
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	open := func(query, lastID string) (*http.Response, *bufio.Reader) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/queries/stream"+query, nil)
		req.Header.Set("Authorization", apitest.Bearer("operator"))
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		return resp, bufio.NewReader(resp.Body)
	}

	resp, rd := open("?status_class=blocked", "")
	defer func() { _ = resp.Body.Close() }()
	waitSubscribers(t, tail, 1)
	now := time.Now()
	tail.Publish(qev(10, now, "10.0.0.1", "a.example.", pipeline.StatusForwarded, ""))
	tail.Publish(qev(11, now, "10.0.0.2", "ads.example.", pipeline.StatusBlockedList, "default/ads"))
	tail.Publish(qev(12, now, "10.0.0.3", "x.example.", pipeline.StatusBlockedList, "default/ads"))
	evs := readEvents(t, rd, 2)
	if evs[0].id != "11" || evs[1].id != "12" || evs[0].event != "query" {
		t.Fatalf("events %+v", evs)
	}
	var row StreamRow
	if err := json.Unmarshal([]byte(evs[0].data), &row); err != nil {
		t.Fatal(err)
	}
	if row.Domain != "ads.example" || row.Status != "blocked_list" || row.StatusClass != "blocked" || row.Client.IP != "10.0.0.2" || row.List != "default/ads" {
		t.Fatalf("row %+v", row)
	}
	_ = resp.Body.Close()

	// Resume: Last-Event-ID 10 replays 11 and 12 from the ring first.
	resp2, rd2 := open("", "10")
	defer func() { _ = resp2.Body.Close() }()
	evs = readEvents(t, rd2, 2)
	if evs[0].id != "11" || evs[1].id != "12" {
		t.Fatalf("resumed events %+v", evs)
	}
	waitSubscribers(t, tail, 1)
	tail.Publish(qev(13, now, "10.0.0.1", "c.example.", pipeline.StatusForwarded, ""))
	if evs = readEvents(t, rd2, 1); evs[0].id != "13" {
		t.Fatalf("live after resume %+v", evs)
	}
	// viewers have no querylog.read.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/queries/stream", nil)
	req.Header.Set("Authorization", apitest.Bearer("viewer"))
	r3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r3.Body.Close()
	if r3.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer stream: %d", r3.StatusCode)
	}
}

func waitSubscribers(t *testing.T, tail *querylog.Tail, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for tail.Subscribers() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d subscribers, want %d", tail.Subscribers(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestQueryStreamLimit is the regression test of the M5 hardening "the
// live tail accepts unbounded concurrent streams": every stream holds a
// goroutine, a subscriber buffer and a connection with no write deadline,
// so a querylog reader could exhaust the node. Streams beyond the cap get
// 503 with Retry-After, and a closed stream frees its slot.
func TestQueryStreamLimit(t *testing.T) {
	defer func(n int) { maxQueryStreams = n }(maxQueryStreams)
	maxQueryStreams = 2
	tail := querylog.NewTail(16, 4, nil)
	h := newHarness(t, func(o *Options) { o.Tail = tail })
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	open := func() *http.Response {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/queries/stream", nil)
		req.Header.Set("Authorization", apitest.Bearer("operator"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first, second := open(), open()
	defer func() { _ = second.Body.Close() }()
	if first.StatusCode != http.StatusOK || second.StatusCode != http.StatusOK {
		t.Fatalf("streams within the cap: %d %d", first.StatusCode, second.StatusCode)
	}
	waitSubscribers(t, tail, 2)
	third := open()
	_ = third.Body.Close()
	if third.StatusCode != http.StatusServiceUnavailable || third.Header.Get("Retry-After") == "" {
		t.Fatalf("stream over the cap: %d (Retry-After %q)", third.StatusCode, third.Header.Get("Retry-After"))
	}
	_ = first.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := open()
		code := r.StatusCode
		_ = r.Body.Close()
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a closed stream did not free its slot: %d", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
