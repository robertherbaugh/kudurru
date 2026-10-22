package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

type scopedQuerylog struct {
	s       *Server
	peers   []cluster.Peer
	cluster bool
	// central answers a cluster request from the central database
	// (D161) instead of the peers.
	central *sqlitelog.Reader
}

func (s *Server) scopedReader(ctx context.Context, scope, node string) (*scopedQuerylog, error) {
	// With a central database every node's rows are in one place: no
	// scatter-gather (D161).
	if scope == "cluster" && node == "" && s.opts.CentralQuerylog != nil {
		return &scopedQuerylog{s: s, cluster: true, central: s.opts.CentralQuerylog}, nil
	}
	peers, err := s.queryPeers(ctx, scope, node)
	if err != nil {
		return nil, err
	}
	if scope != "cluster" && peers[0].Self {
		if _, err := s.reader(); err != nil {
			return nil, err
		}
	}
	return &scopedQuerylog{s: s, peers: peers, cluster: scope == "cluster"}, nil
}
func (s *Server) queryPeers(ctx context.Context, scope, node string) ([]cluster.Peer, error) {
	if scope != "" && scope != "node" && scope != "cluster" {
		return nil, errInvalid("scope: expected node or cluster")
	}
	if scope == "cluster" && node != "" {
		return nil, errInvalid("node is only valid with scope=node")
	}
	if scope != "cluster" && (node == "" || node == s.opts.NodeName) {
		return []cluster.Peer{{Name: s.opts.NodeName, Self: true}}, nil
	}
	peers, err := s.opts.Membership.Peers(ctx)
	if err != nil {
		return nil, errUnavailable("membership: %v", err)
	}
	byName := map[string]cluster.Peer{s.opts.NodeName: {Name: s.opts.NodeName, Self: true}}
	for _, p := range peers {
		if p.Name != "" {
			p.Self = p.Name == s.opts.NodeName
			byName[p.Name] = p
		}
	}
	if scope != "cluster" {
		p, ok := byName[node]
		if !ok {
			return nil, newError(http.StatusNotFound, KeyNotFound, "unknown node "+node, "")
		}
		return []cluster.Peer{p}, nil
	}
	peers = make([]cluster.Peer, 0, len(byName))
	for _, p := range byName {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	return peers, nil
}

func (r *scopedQuerylog) localOnly() bool { return !r.cluster && r.peers[0].Self }

// direct is the reader that answers the whole request by itself: the
// central database for a cluster request, this node's log for this node;
// nil when the peers must be gathered.
func (r *scopedQuerylog) direct() *sqlitelog.Reader {
	if r.central != nil {
		return r.central
	}
	if r.localOnly() {
		return r.s.opts.Querylog
	}
	return nil
}

type shardResult struct {
	node     string
	response *internalapi.QuerylogResponse
}

func (r *scopedQuerylog) gather(ctx context.Context, q internalapi.QuerylogRequest, cursors map[string]string) ([]shardResult, sqlitelog.Scope, error) {
	scope := sqlitelog.Scope{Scope: "node", Nodes: make([]sqlitelog.NodeResult, len(r.peers))}
	if r.cluster {
		scope.Scope = "cluster"
	}
	results := make([]shardResult, len(r.peers))
	var wg sync.WaitGroup
	// Limit concurrent peer reads without serialising the usual 3-pod case.
	sem := make(chan struct{}, 16)
	for i, p := range r.peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			nr := sqlitelog.NodeResult{Name: p.Name}
			defer func() { nr.TookMS = float64(time.Since(start).Microseconds()) / 1000; scope.Nodes[i] = nr }()
			peerCtx := ctx
			if !p.Self {
				var cancel context.CancelFunc
				peerCtx, cancel = context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-peerCtx.Done():
				nr.Error = "query log: unavailable"
				return
			}
			req := q
			if cursors != nil {
				req.Filter.Cursor = cursors[p.Name]
			}
			var out *internalapi.QuerylogResponse
			var err error
			if p.Self {
				out, err = internalapi.ReadLocalQuerylog(peerCtx, r.s.opts.Querylog, req)
				if err == nil && req.Kind == "upstreams" {
					out.Servers, err = r.s.upstreamStates(peerCtx)
				}
			} else if r.s.opts.InternalClient == nil || p.Address == "" {
				err = fmt.Errorf("peer transport is unavailable")
			} else {
				out, err = r.s.opts.InternalClient.ReadQuerylog(peerCtx, p.Address, req)
			}
			if err == nil && !validShard(out, req.Kind, p.Name) {
				err = fmt.Errorf("peer returned an incomplete query-log response")
			}
			if err != nil {
				nr.Error = "query log: unavailable"
				r.s.log.Warn("peer query-log read failed", "node", p.Name, "kind", req.Kind, "err", err)
				return
			}
			nr.OK = true
			results[i] = shardResult{node: p.Name, response: out}
		}()
	}
	wg.Wait()
	successful := results[:0]
	for _, res := range results {
		if res.response != nil {
			successful = append(successful, res)
		} else {
			scope.Partial = true
		}
	}
	if len(successful) == 0 {
		return nil, scope, errUnavailable("query log unavailable on the requested nodes")
	}
	return successful, scope, nil
}
func validShard(out *internalapi.QuerylogResponse, kind, name string) bool {
	var scope sqlitelog.Scope
	if out == nil {
		return false
	}
	switch kind {
	case "summary":
		if out.Summary != nil {
			scope = out.Summary.Scope
		}
	case "timeseries":
		if out.Timeseries != nil {
			scope = out.Timeseries.Scope
		}
	case "clients", "domains", "lists", "zones":
		if out.Top != nil {
			scope = out.Top.Scope
		}
	case "upstreams":
		if out.Upstreams != nil {
			scope = out.Upstreams.Scope
		}
	case "client_timeseries":
		if out.Clients != nil {
			scope = out.Clients.Scope
		}
	case "recent_blocked":
		if out.Blocked != nil {
			scope = out.Blocked.Scope
		}
	case "queries":
		if out.Queries != nil {
			scope = out.Queries.Scope
		}
	default:
		return false
	}
	return scope.Scope == "node" && len(scope.Nodes) == 1 && scope.Nodes[0].Name == name && scope.Nodes[0].OK
}

func (r *scopedQuerylog) Summary(ctx context.Context, q sqlitelog.SummaryQuery) (*sqlitelog.Summary, error) {
	if d := r.direct(); d != nil {
		return d.Summary(ctx, q)
	}
	shards, scope, err := r.gather(ctx, internalapi.QuerylogRequest{Kind: "summary", Window: q.Window}, nil)
	if err != nil {
		return nil, err
	}
	out := &sqlitelog.Summary{Scope: scope, Window: q.Window, Rcodes: map[string]int64{}, Protocols: map[string]int64{}, Types: map[string]int64{}}
	var active, known, domains sqlitelog.Distinct
	var latency sqlitelog.LatencyMerge
	for _, sh := range shards {
		v := sh.response.Summary
		a, b := &out.Queries, v.Queries
		a.Total += b.Total
		a.Blocked += b.Blocked
		a.Cached += b.Cached
		a.Forwarded += b.Forwarded
		a.Authoritative += b.Authoritative
		a.Dropped += b.Dropped
		a.Other += b.Other
		a.QPS1m += b.QPS1m
		a.QPSAvg += b.QPSAvg
		out.DomainsOnLists = max(out.DomainsOnLists, v.DomainsOnLists)
		addCounts(out.Rcodes, v.Rcodes)
		addCounts(out.Protocols, v.Protocols)
		addCounts(out.Types, v.Types)
		active.Merge(v.ActiveClients)
		known.Merge(v.KnownClients)
		domains.Merge(v.Domains)
		latency.Add(v.Histogram)
		out.Approximate = out.Approximate || v.Approximate
	}
	var approx bool
	out.Clients.Active, approx = active.Count()
	out.Approximate = out.Approximate || approx
	out.Clients.Total, approx = known.Count()
	out.Approximate = out.Approximate || approx
	out.Queries.UniqueDomains, approx = domains.Count()
	out.Approximate = out.Approximate || approx
	out.Queries.PercentBlocked = ratio(out.Queries.Blocked, out.Queries.Total)
	out.Latency = latency.Summary()
	return out, nil
}
func addCounts(dst, src map[string]int64) {
	for k, v := range src {
		dst[k] += v
	}
}
func ratio(n, total int64) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(float64(n)/float64(total)*10000) / 100
}
func limitN(n int) int {
	if n <= 0 {
		return sqlitelog.DefaultTopN
	}
	return min(n, sqlitelog.MaxTopN)
}
func (r *scopedQuerylog) Timeseries(ctx context.Context, q sqlitelog.TimeseriesQuery) (*sqlitelog.Timeseries, error) {
	if d := r.direct(); d != nil {
		return d.Timeseries(ctx, q)
	}
	q.Step = time.Duration(sqlitelog.StepSeconds(q.Step, q.Window, r.s.opts.Now())) * time.Second
	shards, scope, err := r.gather(ctx, internalapi.QuerylogRequest{Kind: "timeseries", Window: q.Window, Step: q.Step}, nil)
	if err != nil {
		return nil, err
	}
	out := &sqlitelog.Timeseries{Scope: scope, Window: q.Window, Step: int64(q.Step / time.Second), Points: []sqlitelog.Point{}}
	byTS := map[int64]sqlitelog.Point{}
	for _, sh := range shards {
		v := sh.response.Timeseries
		out.Approximate = out.Approximate || v.Approximate
		for _, p := range v.Points {
			key := p.TS.Unix()
			a := byTS[key]
			a.TS = p.TS
			a.Total += p.Total
			a.Blocked += p.Blocked
			a.Cached += p.Cached
			a.Forwarded += p.Forwarded
			a.Authoritative += p.Authoritative
			a.Other += p.Other
			byTS[key] = a
		}
	}
	for _, p := range byTS {
		out.Points = append(out.Points, p)
	}
	sort.Slice(out.Points, func(i, j int) bool { return out.Points[i].TS.Before(out.Points[j].TS) })
	return out, nil
}
func (r *scopedQuerylog) top(ctx context.Context, kind string, q sqlitelog.TopQuery) (*sqlitelog.TopList, error) {
	if d := r.direct(); d != nil {
		switch kind {
		case "domains":
			return d.TopDomains(ctx, q)
		case "clients":
			return d.TopClients(ctx, q)
		case "lists":
			return d.TopLists(ctx, q)
		case "zones":
			return d.TopZones(ctx, q)
		}
	}
	n := limitN(q.N)
	candidates := n
	if r.cluster {
		candidates = n * 4
	}
	shards, scope, err := r.gather(ctx, internalapi.QuerylogRequest{Kind: kind, Window: q.Window, N: candidates, Blocked: q.Blocked}, nil)
	if err != nil {
		return nil, err
	}
	out := &sqlitelog.TopList{Scope: scope, Window: q.Window, Items: []sqlitelog.TopItem{}, Approximate: r.cluster, Blocked: q.Blocked}
	byKey := map[string]sqlitelog.TopItem{}
	for _, sh := range shards {
		v := sh.response.Top
		out.Dimension = v.Dimension
		out.TotalInWindow += v.TotalInWindow
		out.Approximate = out.Approximate || v.Approximate
		for _, it := range v.Items {
			a := byKey[it.Key]
			a.Key = it.Key
			if it.Name != "" {
				a.Name = it.Name
			}
			if it.Group != "" {
				a.Group = it.Group
			}
			a.Count += it.Count
			a.Total += it.Total
			a.Blocked += it.Blocked
			if it.LastSeen != nil && (a.LastSeen == nil || it.LastSeen.After(*a.LastSeen)) {
				a.LastSeen = it.LastSeen
			}
			byKey[it.Key] = a
		}
	}
	for _, it := range byKey {
		it.Percent = ratio(it.Count, out.TotalInWindow)
		out.Items = append(out.Items, it)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Key < b.Key
	})
	if len(out.Items) > n {
		out.Items = out.Items[:n]
	}
	if r.cluster {
		out.CutoffRank = candidates
	}
	return out, nil
}
func (r *scopedQuerylog) TopDomains(ctx context.Context, q sqlitelog.TopQuery) (*sqlitelog.TopList, error) {
	return r.top(ctx, "domains", q)
}
func (r *scopedQuerylog) TopClients(ctx context.Context, q sqlitelog.TopQuery) (*sqlitelog.TopList, error) {
	return r.top(ctx, "clients", q)
}
func (r *scopedQuerylog) TopLists(ctx context.Context, q sqlitelog.TopQuery) (*sqlitelog.TopList, error) {
	return r.top(ctx, "lists", q)
}
func (r *scopedQuerylog) TopZones(ctx context.Context, q sqlitelog.TopQuery) (*sqlitelog.TopList, error) {
	q.Blocked = false
	return r.top(ctx, "zones", q)
}

func (r *scopedQuerylog) upstreams(ctx context.Context, q sqlitelog.TopQuery) (*sqlitelog.UpstreamTop, []UpstreamServer, error) {
	if d := r.direct(); d != nil {
		// The central database holds traffic, not live server health:
		// that is this node's.
		out, err := d.TopUpstreams(ctx, q)
		if err != nil {
			return nil, nil, err
		}
		servers, err := r.s.upstreamServers(ctx)
		return out, servers, err
	}
	n := limitN(q.N)
	candidates := n
	if r.cluster {
		candidates = n * 4
	}
	shards, scope, err := r.gather(ctx, internalapi.QuerylogRequest{Kind: "upstreams", Window: q.Window, N: candidates}, nil)
	if err != nil {
		return nil, nil, err
	}
	out := &sqlitelog.UpstreamTop{Scope: scope, Window: q.Window, Items: []sqlitelog.UpstreamItem{}, Approximate: r.cluster}
	byKey := map[string]sqlitelog.UpstreamItem{}
	hist := map[string]sqlitelog.LatencyMerge{}
	var total int64
	servers := []UpstreamServer{}
	for _, sh := range shards {
		v := sh.response.Upstreams
		total += v.TotalInWindow
		out.Approximate = out.Approximate || v.Approximate
		for _, it := range v.Items {
			a := byKey[it.Addr]
			a.Addr = it.Addr
			a.Total += it.Total
			a.Errors += it.Errors
			a.Timeouts += it.Timeouts
			byKey[it.Addr] = a
			h := hist[it.Addr]
			h.Add(v.Histograms[it.Addr])
			hist[it.Addr] = h
		}
		for _, st := range sh.response.Servers {
			servers = append(servers, upstreamServer(sh.node, st))
		}
	}
	for _, it := range byKey {
		it.Percent = ratio(it.Total, total)
		lat := hist[it.Addr].Summary()
		it.AvgMS, it.P95MS = lat.AvgMS, lat.P95MS
		out.Items = append(out.Items, it)
	}
	sort.Slice(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Addr < b.Addr
	})
	if len(out.Items) > n {
		out.Items = out.Items[:n]
	}
	return out, servers, nil
}
func (r *scopedQuerylog) TopUpstreams(ctx context.Context, q sqlitelog.TopQuery) (*sqlitelog.UpstreamTop, error) {
	out, _, err := r.upstreams(ctx, q)
	return out, err
}

func (r *scopedQuerylog) ClientTimeseries(ctx context.Context, q sqlitelog.ClientTimeseriesQuery) (*sqlitelog.ClientTimeseries, error) {
	if d := r.direct(); d != nil {
		return d.ClientTimeseries(ctx, q)
	}
	n := limitN(q.N)
	candidates := n
	if r.cluster {
		candidates = n * 4
	}
	q.Step = time.Duration(sqlitelog.StepSeconds(q.Step, q.Window, r.s.opts.Now())) * time.Second
	shards, scope, err := r.gather(ctx, internalapi.QuerylogRequest{Kind: "client_timeseries", Window: q.Window, Step: q.Step, N: candidates}, nil)
	if err != nil {
		return nil, err
	}
	out := &sqlitelog.ClientTimeseries{Scope: scope, Window: q.Window, Step: int64(q.Step / time.Second), Clients: []sqlitelog.ClientKey{}, Points: []sqlitelog.ClientPoint{}, Approximate: r.cluster}
	totals := map[string]int64{}
	names := map[string]sqlitelog.ClientKey{}
	buckets := map[int64]map[string]int64{}
	for _, sh := range shards {
		v := sh.response.Clients
		out.Approximate = out.Approximate || v.Approximate
		for _, p := range v.Points {
			key := p.TS.Unix()
			b := buckets[key]
			if b == nil {
				b = map[string]int64{}
				buckets[key] = b
			}
			for i, c := range v.Clients {
				if i >= len(p.Counts) {
					continue
				}
				b[c.Key] += p.Counts[i]
				if c.Key != "other" {
					names[c.Key] = c
					totals[c.Key] += p.Counts[i]
				}
			}
		}
	}
	for _, c := range names {
		out.Clients = append(out.Clients, c)
	}
	sort.Slice(out.Clients, func(i, j int) bool {
		a, b := out.Clients[i].Key, out.Clients[j].Key
		if totals[a] != totals[b] {
			return totals[a] > totals[b]
		}
		return a < b
	})
	if len(out.Clients) > n {
		out.Clients = out.Clients[:n]
	}
	out.Clients = append(out.Clients, sqlitelog.ClientKey{Key: "other"})
	selected := map[string]int{}
	for i, c := range out.Clients {
		selected[c.Key] = i
	}
	for ts, b := range buckets {
		p := sqlitelog.ClientPoint{TS: time.Unix(ts, 0).UTC(), Counts: make([]int64, len(out.Clients))}
		for key, count := range b {
			i, ok := selected[key]
			if !ok {
				i = len(out.Clients) - 1
			}
			p.Counts[i] += count
		}
		out.Points = append(out.Points, p)
	}
	sort.Slice(out.Points, func(i, j int) bool { return out.Points[i].TS.Before(out.Points[j].TS) })
	return out, nil
}
func (r *scopedQuerylog) RecentBlocked(ctx context.Context, n int) (*sqlitelog.RecentBlocked, error) {
	if d := r.direct(); d != nil {
		return d.RecentBlocked(ctx, n)
	}
	n = limitN(n)
	shards, scope, err := r.gather(ctx, internalapi.QuerylogRequest{Kind: "recent_blocked", N: n}, nil)
	if err != nil {
		return nil, err
	}
	out := &sqlitelog.RecentBlocked{Scope: scope, Items: []sqlitelog.BlockedItem{}}
	for _, sh := range shards {
		for _, it := range sh.response.Blocked.Items {
			it.Node = sh.node
			out.Items = append(out.Items, it)
		}
	}
	sort.Slice(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if !a.TS.Equal(b.TS) {
			return a.TS.After(b.TS)
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		return a.ID > b.ID
	})
	if len(out.Items) > n {
		out.Items = out.Items[:n]
	}
	return out, nil
}

// Bound caller-controlled cursor maps before they can create peer work.
const maxQueryCursorNodes = 64

// Cluster cursors carry only emitted positions; rows fetched speculatively
// from a peer stay eligible for the next page. A fixed upper timestamp keeps
// inserts between pages out of this traversal.
type queryCursor struct {
	Version   int               `json:"v"`
	Positions map[string]string `json:"positions"`
	Until     time.Time         `json:"until"`
	Filter    string            `json:"filter"`
}

func encodeQuerylogCursor(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeQuerylogCursor(raw string, v any) error {
	if len(raw) > 64<<10 {
		return errInvalid("cursor is too large")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return errInvalid("invalid cursor")
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errInvalid("invalid cursor")
	}
	return nil
}
func (r *scopedQuerylog) Queries(ctx context.Context, f sqlitelog.QueryFilter) (*sqlitelog.QueryPage, error) {
	if r.central != nil {
		return r.central.Queries(ctx, f)
	}
	if !r.cluster {
		if r.localOnly() {
			return r.s.opts.Querylog.Queries(ctx, f)
		}
		shards, scope, err := r.gather(ctx, internalapi.QuerylogRequest{Kind: "queries", Filter: f}, nil)
		if err != nil {
			return nil, err
		}
		out := shards[0].response.Queries
		out.Scope = scope
		return out, nil
	}
	base := f
	base.Cursor = ""
	base.Limit = 0
	base.TimeOrder = false
	filterBytes, _ := json.Marshal(base)
	cursor := queryCursor{Version: 1, Positions: map[string]string{}, Filter: string(filterBytes), Until: f.Until}
	if f.Cursor != "" {
		if err := decodeQuerylogCursor(f.Cursor, &cursor); err != nil {
			return nil, err
		}
		if len(cursor.Positions) > maxQueryCursorNodes {
			return nil, errInvalid("cursor contains too many nodes")
		}
		if cursor.Version != 1 || cursor.Positions == nil || cursor.Filter != string(filterBytes) || cursor.Until.IsZero() {
			return nil, errInvalid("cursor does not match these filters")
		}
	}
	if cursor.Until.IsZero() {
		cursor.Until = r.s.opts.Now()
	}
	f.Until = cursor.Until
	f.Cursor = ""
	f.TimeOrder = true
	f.Limit = min(f.Limit, sqlitelog.MaxLimit)
	if f.Limit <= 0 {
		f.Limit = sqlitelog.DefaultLimit
	}
	// Freeze membership for a traversal so adding a new node cannot insert a
	// different history into later pages. Missing peers remain partial.
	if len(cursor.Positions) > 0 {
		byName := map[string]cluster.Peer{}
		for _, p := range r.peers {
			byName[p.Name] = p
		}
		peers := make([]cluster.Peer, 0, len(cursor.Positions))
		for name, pos := range cursor.Positions {
			if pos != "" {
				parts := strings.Split(pos, ":")
				if len(parts) != 2 {
					return nil, errInvalid("invalid cursor")
				}
				if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
					return nil, errInvalid("invalid cursor")
				}
				if id, err := strconv.ParseInt(parts[1], 10, 64); err != nil || id <= 0 {
					return nil, errInvalid("invalid cursor")
				}
			}
			p, ok := byName[name]
			if !ok {
				p = cluster.Peer{Name: name}
			}
			peers = append(peers, p)
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
		r.peers = peers
	} else {
		if len(r.peers) > maxQueryCursorNodes {
			return nil, errUnavailable("too many nodes for cluster query pagination")
		}
		for _, p := range r.peers {
			cursor.Positions[p.Name] = ""
		}
	}
	shards, scope, err := r.gather(ctx, internalapi.QuerylogRequest{Kind: "queries", Filter: f}, cursor.Positions)
	if err != nil {
		return nil, err
	}
	out := &sqlitelog.QueryPage{Scope: scope, Queries: []sqlitelog.QueryRow{}}
	more := false
	for _, sh := range shards {
		v := sh.response.Queries
		out.RecordsTotal += v.RecordsTotal
		out.RecordsFiltered += v.RecordsFiltered
		out.EarliestTSDisk = earlier(out.EarliestTSDisk, v.EarliestTSDisk)
		out.EarliestTSMemory = earlier(out.EarliestTSMemory, v.EarliestTSMemory)
		more = more || v.CursorNext != ""
		for _, row := range v.Queries {
			row.Node = sh.node
			out.Queries = append(out.Queries, row)
		}
	}
	sort.Slice(out.Queries, func(i, j int) bool {
		a, b := out.Queries[i], out.Queries[j]
		if !a.TS.Equal(b.TS) {
			return a.TS.After(b.TS)
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		return a.ID > b.ID
	})
	if len(out.Queries) > f.Limit {
		more = true
		out.Queries = out.Queries[:f.Limit]
	}
	for _, row := range out.Queries {
		cursor.Positions[row.Node] = sqlitelog.RowCursor(row, true)
	}
	if more {
		out.CursorNext = encodeQuerylogCursor(cursor)
	}
	return out, nil
}
func earlier(a, b *time.Time) *time.Time {
	if a == nil {
		return b
	}
	if b != nil && b.Before(*a) {
		return b
	}
	return a
}
