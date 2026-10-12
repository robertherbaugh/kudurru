package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

type clusterLogFixture struct {
	h            *harness
	a, b         *sqlitelog.Writer
	atail, btail *querylog.Tail
	calls        atomic.Int64
}

func openNodeLog(t *testing.T, node string, events []*querylog.Event) (*sqlitelog.Writer, *querylog.Tail) {
	t.Helper()
	tail := querylog.NewTail(128, 64, nil)
	w, err := sqlitelog.Open(t.Context(), sqlitelog.Options{Path: filepath.Join(t.TempDir(), sqlitelog.FileName), Node: node, Tail: tail, Privacy: querylog.PrivacyFull, RawRetention: 24 * time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	for _, ev := range events {
		ev.Node = node
	}
	if err := w.Write(events); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	return w, tail
}
func newClusterLogFixture(t *testing.T, mutate ...func(*Options)) *clusterLogFixture {
	t.Helper()
	f := &clusterLogFixture{}
	now := time.Now().Add(-2 * time.Minute)
	a := []*querylog.Event{qev(1, now, "10.0.0.1", "shared.test", pipeline.StatusForwarded, ""), qev(2, now.Add(6*time.Second), "10.0.0.1", "ads.test", pipeline.StatusBlockedList, "ads"), qev(3, now.Add(3*time.Second), "10.0.0.1", "only-a.test", pipeline.StatusCached, "")}
	b := []*querylog.Event{qev(1, now.Add(time.Second), "10.0.0.1", "shared.test", pipeline.StatusForwarded, ""), qev(2, now.Add(6*time.Second), "10.0.0.1", "ads.test", pipeline.StatusBlockedList, "ads"), qev(3, now.Add(4*time.Second), "10.0.0.1", "only-b.test", pipeline.StatusCached, "")}
	for _, ev := range b {
		ev.Latency = 100 * time.Millisecond
	}
	f.a, f.atail = openNodeLog(t, "node-a", a)
	f.b, f.btail = openNodeLog(t, "node-b", b)
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{7}, 32), NodeName: "node-b"})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := internalapi.NewServer(internalapi.ServerOptions{Credentials: creds, Querylog: func(ctx context.Context, q internalapi.QuerylogRequest) (*internalapi.QuerylogResponse, error) {
		f.calls.Add(1)
		return internalapi.ReadLocalQuerylog(ctx, f.b.Reader(), q)
	}, QueryTail: func() *querylog.Tail { return f.btail }})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(peer.Handler())
	ts.TLS = creds.ServerTLS()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	f.h = newHarness(t, func(o *Options) {
		o.Querylog = f.a.Reader()
		o.Tail = f.atail
		o.InternalClient = internalapi.NewClient(creds, internalapi.ClientOptions{})
		o.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-b", Address: strings.TrimPrefix(ts.URL, "https://")}, {Name: "node-a", Self: true}}}
		for _, m := range mutate {
			m(o)
		}
	})
	return f
}

func TestClusterQueriesPagination(t *testing.T) {
	f := newClusterLogFixture(t)
	cursor := ""
	var got []string
	for pageNum := 0; pageNum < 3; pageNum++ {
		var page QueriesBody
		f.h.do("GET", "/api/v1/queries?scope=cluster&limit=2&cursor="+url.QueryEscape(cursor), "operator", nil).want(200).json(&page)
		if page.Scope.Scope != "cluster" || page.Partial || len(page.Nodes) != 2 || page.RecordsFiltered != 6 {
			t.Fatalf("page %+v", page)
		}
		for _, row := range page.Queries {
			got = append(got, fmt.Sprintf("%s/%d", row.Node, row.ID))
		}
		cursor = page.CursorNext
		if pageNum == 0 {
			if cursor == "" {
				t.Fatal("missing cluster cursor")
			}
			ev := qev(4, time.Now().Add(time.Second), "10.0.0.1", "live.test", pipeline.StatusForwarded, "")
			if err := f.a.Write([]*querylog.Event{ev}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if cursor != "" || strings.Join(got, ",") != "node-a/2,node-b/2,node-b/3,node-a/3,node-b/1,node-a/1" {
		t.Fatalf("rows %v; cursor %s", got, cursor)
	}
}
func TestQueryScopeRoutingAndValidation(t *testing.T) {
	f := newClusterLogFixture(t)
	var page QueriesBody
	f.h.do("GET", "/api/v1/queries?scope=node&node=node-b", "operator", nil).want(200).json(&page)
	if page.Scope.Scope != "node" || page.Nodes[0].Name != "node-b" || len(page.Queries) != 3 || page.Queries[0].Node != "node-b" {
		t.Fatalf("remote page %+v", page)
	}
	f.h.do("GET", "/api/v1/queries", "operator", nil).want(200).json(&page)
	if page.Queries[0].ID != 3 || page.Nodes[0].Name != "node-a" {
		t.Fatalf("local page %+v", page)
	}
	for _, path := range []string{"/queries?scope=invalid", "/queries?scope=cluster&node=node-b", "/queries?scope=node&node=node-b&cursor=bad", "/queries?scope=cluster&cursor=bad"} {
		f.h.do("GET", BasePath+path, "operator", nil).want(400)
	}
	f.h.do("GET", "/api/v1/queries?scope=node&node=unknown", "operator", nil).want(404)
	f.h.do("GET", "/api/v1/queries?scope=cluster&limit=1", "operator", nil).want(200).json(&page)
	f.h.do("GET", "/api/v1/queries?scope=cluster&domain=ads.test&cursor="+url.QueryEscape(page.CursorNext), "operator", nil).want(400)
	before := f.calls.Load()
	f.h.do("GET", "/api/v1/queries?scope=cluster", "viewer", nil).want(403)
	if f.calls.Load() != before {
		t.Fatal("unauthorized request reached peer")
	}
}
func TestClusterReadsWithLocalLogDisabled(t *testing.T) {
	f := newClusterLogFixture(t, func(o *Options) { o.Querylog = nil; o.Tail = nil })
	var page QueriesBody
	f.h.do("GET", "/api/v1/queries?scope=cluster", "operator", nil).want(200).json(&page)
	if !page.Partial || page.Nodes[0].OK || !page.Nodes[1].OK || len(page.Queries) != 3 {
		t.Fatalf("partial %+v", page)
	}
	f.h.do("GET", "/api/v1/queries?scope=node&node=node-b", "operator", nil).want(200)
	f.h.do("GET", "/api/v1/queries", "operator", nil).want(503)
	f.h.srv.opts.InternalClient = nil
	f.h.do("GET", "/api/v1/queries?scope=cluster", "operator", nil).want(503)
}
func TestClusterStatsMerge(t *testing.T) {
	f := newClusterLogFixture(t)
	var summary SummaryBody
	f.h.do("GET", "/api/v1/stats/summary?scope=cluster&window=1h", "viewer", nil).want(200).json(&summary)
	if summary.Queries.Total != 6 || summary.Queries.Blocked != 2 || summary.Queries.UniqueDomains != 4 || summary.Clients.Active != 1 || summary.Clients.Total != 1 || summary.Latency.AvgMS != 50.5 || summary.Latency.P50MS != 1 || summary.Latency.P95MS != 100 {
		t.Fatalf("summary %+v", summary)
	}
	var series TimeseriesBody
	f.h.do("GET", "/api/v1/stats/timeseries?scope=cluster&window=1h&step=5m", "viewer", nil).want(200).json(&series)
	var total int64
	for _, p := range series.Points {
		total += p.Total
		if p.TS.Unix()%300 != 0 {
			t.Fatal("unaligned bucket")
		}
	}
	if total != 6 {
		t.Fatalf("series total %d", total)
	}
	var top TopBody
	f.h.do("GET", "/api/v1/stats/top?scope=cluster&dim=domains&n=2&window=1h", "viewer", nil).want(200).json(&top)
	if len(top.Items) != 2 || top.Items[0].Key != "shared.test" || top.Items[0].Count != 2 || !top.Approximate || top.CutoffRank != 8 || top.TotalInWindow != 6 {
		t.Fatalf("top %+v", top)
	}
	var up UpstreamsBody
	f.h.do("GET", "/api/v1/stats/upstreams?scope=cluster&window=1h", "viewer", nil).want(200).json(&up)
	if len(up.Items) != 2 || up.Items[0].Total != 2 || up.Items[0].AvgMS != 50.5 || up.Items[0].P95MS != 100 {
		t.Fatalf("upstreams %+v", up)
	}
	var clients ClientTimeseriesBody
	f.h.do("GET", "/api/v1/stats/clients/timeseries?scope=cluster&window=1h&n=1", "viewer", nil).want(200).json(&clients)
	total = 0
	for _, p := range clients.Points {
		for _, n := range p.Counts {
			total += n
		}
	}
	if total != 6 || len(clients.Clients) != 2 {
		t.Fatalf("clients total %d; %+v", total, clients)
	}
	var recent RecentBlockedBody
	f.h.do("GET", "/api/v1/stats/recent_blocked?scope=cluster&n=1", "operator", nil).want(200).json(&recent)
	if len(recent.Items) != 1 || recent.Items[0].Node != "node-a" {
		t.Fatalf("recent %+v", recent)
	}
	for _, path := range []string{"summary", "timeseries", "top?dim=clients", "clients/timeseries", "upstreams", "recent_blocked"} {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		var body struct {
			Scope string                 `json:"scope"`
			Nodes []sqlitelog.NodeResult `json:"nodes"`
		}
		f.h.do("GET", "/api/v1/stats/"+path+sep+"scope=node&node=node-b", "operator", nil).want(200).json(&body)
		if body.Scope != "node" || len(body.Nodes) != 1 || body.Nodes[0].Name != "node-b" {
			t.Fatalf("%s routed incorrectly: %+v", path, body)
		}
	}
}
func TestClusterPeerTimeout(t *testing.T) {
	f := newClusterLogFixture(t)
	creds, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}))
	peer.TLS = creds.ServerTLS()
	peer.StartTLS()
	t.Cleanup(peer.Close)
	f.h.srv.opts.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-a", Self: true}, {Name: "slow", Address: strings.TrimPrefix(peer.URL, "https://")}}}
	start := time.Now()
	var page QueriesBody
	f.h.do("GET", "/api/v1/queries?scope=cluster", "operator", nil).want(200).json(&page)
	if !page.Partial || page.Nodes[1].OK || len(page.Queries) != 3 || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout page %+v; elapsed %s", page, time.Since(start))
	}
}

func TestClusterQueryStream(t *testing.T) {
	f := newClusterLogFixture(t)
	ts := httptest.NewServer(f.h.srv)
	t.Cleanup(ts.Close)
	open := func(last string) (*http.Response, *bufio.Reader, context.CancelFunc) {
		ctx, cancel := context.WithCancel(t.Context())
		req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/queries/stream?scope=cluster", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", apitest.Bearer("operator"))
		if last != "" {
			req.Header.Set("Last-Event-ID", last)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("stream status %d", resp.StatusCode)
		}
		t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
		return resp, bufio.NewReader(resp.Body), cancel
	}
	resp, rd, cancel := open("")
	scope := readEvents(t, rd, 1)[0]
	if scope.event != "scope" || !strings.Contains(scope.data, `"scope":"cluster"`) {
		t.Fatalf("scope %+v", scope)
	}
	f.atail.Publish(qev(10, time.Now(), "10.0.0.1", "a-live.test", pipeline.StatusForwarded, ""))
	f.btail.Publish(qev(10, time.Now(), "10.0.0.1", "b-live.test", pipeline.StatusForwarded, ""))
	events := readEvents(t, rd, 2)
	nodes := map[string]bool{}
	for _, ev := range events {
		var row sqlitelog.QueryRow
		if err := json.Unmarshal([]byte(ev.data), &row); err != nil {
			t.Fatal(err)
		}
		nodes[row.Node] = true
		if row.ID != 10 {
			t.Fatalf("row %+v", row)
		}
	}
	if len(nodes) != 2 {
		t.Fatalf("lost colliding ID: %+v", events)
	}
	last := events[1].id
	cancel()
	_ = resp.Body.Close()
	f.atail.Publish(qev(11, time.Now(), "10.0.0.1", "a-resume.test", pipeline.StatusForwarded, ""))
	f.btail.Publish(qev(11, time.Now(), "10.0.0.1", "b-resume.test", pipeline.StatusForwarded, ""))
	_, rd, _ = open(last)
	_ = readEvents(t, rd, 1)
	events = readEvents(t, rd, 2)
	for _, ev := range events {
		var row sqlitelog.QueryRow
		if err := json.Unmarshal([]byte(ev.data), &row); err != nil {
			t.Fatal(err)
		}
		if row.ID != 11 {
			t.Fatalf("resume duplicated ID: %+v", row)
		}
	}
}

func TestSelectedNodeIdentity(t *testing.T) {
	f := newClusterLogFixture(t)
	peers, _ := f.h.srv.opts.Membership.Peers(t.Context())
	peers[0].Name = "wrong-node"
	f.h.srv.opts.Membership = staticMembership{peers: peers}
	f.h.do("GET", "/api/v1/queries?scope=node&node=wrong-node", "operator", nil).want(503)
	f.h.do("GET", "/api/v1/queries/stream?scope=node&node=wrong-node", "operator", nil).want(503)
}

func TestPartialAndSelectedNodeStreams(t *testing.T) {
	f := newClusterLogFixture(t)
	peers, _ := f.h.srv.opts.Membership.Peers(t.Context())
	peers = append(peers, cluster.Peer{Name: "offline"})
	f.h.srv.opts.Membership = staticMembership{peers: peers}
	ts := httptest.NewServer(f.h.srv)
	t.Cleanup(ts.Close)
	for _, scope := range []string{"cluster", "node&node=node-b"} {
		t.Run(scope, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/queries/stream?scope="+scope, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", apitest.Bearer("operator"))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			rd := bufio.NewReader(resp.Body)
			notice := readEvents(t, rd, 1)[0]
			var meta sqlitelog.Scope
			if err := json.Unmarshal([]byte(notice.data), &meta); err != nil {
				t.Fatal(err)
			}
			if scope == "cluster" {
				if !meta.Partial || len(meta.Nodes) != 3 || meta.Nodes[2].OK {
					t.Fatalf("partial scope %+v", meta)
				}
			} else if meta.Scope != "node" || len(meta.Nodes) != 1 || meta.Nodes[0].Name != "node-b" {
				t.Fatalf("selected scope %+v", meta)
			}
			f.btail.Publish(qev(20, time.Now(), "10.0.0.1", "selected.test", pipeline.StatusForwarded, ""))
			ev := readEvents(t, rd, 1)[0]
			var row sqlitelog.QueryRow
			if err := json.Unmarshal([]byte(ev.data), &row); err != nil {
				t.Fatal(err)
			}
			if ev.event != "query" || row.Node != "node-b" || row.Domain != "selected.test" {
				t.Fatalf("event %+v, row %+v", ev, row)
			}
		})
	}
	deadline := time.Now().Add(2 * time.Second)
	for (f.atail.Subscribers() != 0 || f.btail.Subscribers() != 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.atail.Subscribers() != 0 || f.btail.Subscribers() != 0 {
		t.Fatal("cancelled streams retained tail subscriptions")
	}
}

type changingQueryMembership struct {
	mu    sync.Mutex
	peers []cluster.Peer
}

func (m *changingQueryMembership) Leader(context.Context) (cluster.Peer, error) {
	return cluster.Peer{Name: "node-a", Self: true}, nil
}
func (m *changingQueryMembership) Peers(context.Context) ([]cluster.Peer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]cluster.Peer(nil), m.peers...), nil
}
func (m *changingQueryMembership) set(peers []cluster.Peer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peers = peers
}

func TestStreamRefreshesMembership(t *testing.T) {
	f := newClusterLogFixture(t)
	peers, _ := f.h.srv.opts.Membership.Peers(t.Context())
	members := &changingQueryMembership{peers: peers}
	f.h.srv.opts.Membership = members
	old := sseHeartbeat
	sseHeartbeat = 20 * time.Millisecond
	t.Cleanup(func() { sseHeartbeat = old })
	ts := httptest.NewServer(f.h.srv)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/queries/stream?scope=cluster", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", apitest.Bearer("operator"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	rd := bufio.NewReader(resp.Body)
	_ = readEvents(t, rd, 1)
	members.set([]cluster.Peer{{Name: "node-a", Self: true}})
	if _, err := io.ReadAll(rd); err != nil {
		t.Fatalf("membership change did not end the stream for reconnect: %v", err)
	}
}

func TestClusterQueriesWithEmptyServingNode(t *testing.T) {
	f := newClusterLogFixture(t)
	empty, _ := openNodeLog(t, "node-a", nil)
	f.h.srv.opts.Querylog = empty.Reader()
	var page QueriesBody
	f.h.do("GET", "/api/v1/queries?scope=cluster", "operator", nil).want(200).json(&page)
	if page.Partial || page.RecordsTotal != 3 || len(page.Queries) != 3 || page.Queries[0].Node != "node-b" {
		t.Fatalf("empty serving node hid peer queries: %+v", page)
	}
}
