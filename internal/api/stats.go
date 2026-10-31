package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/robertherbaugh/kudurru/ee/longterm"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// Response bodies of the stats and log routes: the sqlitelog read-model
// structs (dev-docs/milestones/m2/querylog.md) plus took_ms. Every one carries
// scope:{scope:"node"|"cluster", nodes:[...], partial:<node failures>}.
type (
	// SummaryBody is GET /stats/summary.
	SummaryBody struct {
		sqlitelog.Summary
		Took
	}
	// TimeseriesBody is GET /stats/timeseries.
	TimeseriesBody struct {
		sqlitelog.Timeseries
		Took
	}
	// TopBody is GET /stats/top for every dimension but upstreams.
	TopBody struct {
		sqlitelog.TopList
		// Upstreams is set for dim=upstreams instead of items.
		Upstreams []sqlitelog.UpstreamItem `json:"upstreams,omitempty"`
		Took
	}
	// UpstreamsBody is GET /stats/upstreams: traffic per upstream plus the
	// live health of every configured server.
	UpstreamsBody struct {
		sqlitelog.UpstreamTop
		Servers []UpstreamServer `json:"servers" nullable:"false"`
		Took
	}
	// ClientTimeseriesBody is GET /stats/clients/timeseries.
	ClientTimeseriesBody struct {
		sqlitelog.ClientTimeseries
		Took
	}
	// RecentBlockedBody is GET /stats/recent_blocked.
	RecentBlockedBody struct {
		sqlitelog.RecentBlocked
		Took
	}
	// QueriesBody is GET /queries.
	QueriesBody struct {
		sqlitelog.QueryPage
		Took
	}
	// SuggestionsBody is GET /queries/suggestions.
	SuggestionsBody struct {
		sqlitelog.Suggestions
		Took
	}
	// QuerylogStatusBody is GET /querylog/status.
	QuerylogStatusBody struct {
		sqlitelog.Status
		Took
	}
)

// UpstreamServer is the live state of one configured upstream server.
type UpstreamServer struct {
	Node                string     `json:"node,omitempty"`
	Group               string     `json:"group"`
	Server              string     `json:"server"`
	Healthy             bool       `json:"healthy"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	BackoffUntil        *time.Time `json:"backoff_until,omitempty"`
	RTTMS               float64    `json:"rtt_ms"`
}

// WindowParams are the time-window query parameters of the stats
// routes (exported so huma can fill them through embedding).
type WindowParams struct {
	Window string `query:"window" doc:"Window ending now, as a Go duration (24h, 7d is 168h); default 24h."`
	From   string `query:"from" doc:"Window start (RFC 3339); overrides window."`
	To     string `query:"to" doc:"Window end (RFC 3339); default now."`
	Scope  string `query:"scope" enum:"node,cluster," doc:"node (default) or cluster; cluster merges the known nodes and reports partial results on failure."`
	Node   string `query:"node" doc:"Node to answer for (scope=node); another node reads central history when configured, otherwise its internal listener."`
}

type summaryOutput struct{ Body *SummaryBody }
type timeseriesOutput struct{ Body *TimeseriesBody }
type topOutput struct{ Body *TopBody }
type upstreamsOutput struct{ Body *UpstreamsBody }
type clientTimeseriesOutput struct{ Body *ClientTimeseriesBody }
type recentBlockedOutput struct{ Body *RecentBlockedBody }
type queriesOutput struct{ Body *QueriesBody }
type suggestionsOutput struct{ Body *SuggestionsBody }
type querylogStatusOutput struct{ Body *QuerylogStatusBody }

type timeseriesInput struct {
	WindowParams
	Step string `query:"step" doc:"Bucket width (Go duration) or auto (default)."`
}

type topInput struct {
	WindowParams
	Dim     string `query:"dim" required:"true" enum:"domains,clients,upstreams,lists,zones"`
	N       int    `query:"n" minimum:"0" maximum:"100"`
	Blocked bool   `query:"blocked"`
}

type clientTimeseriesInput struct {
	WindowParams
	Step string `query:"step"`
	N    int    `query:"n" minimum:"0" maximum:"100"`
}

type recentBlockedInput struct {
	N     int    `query:"n" minimum:"0" maximum:"100"`
	Scope string `query:"scope"`
	Node  string `query:"node"`
}

// QueryParams are the filters of GET /queries and /queries/stream
// (exported so huma can fill them through embedding).
type QueryParams struct {
	Client      string `query:"client" doc:"IP, CIDR or client name (* wildcards)."`
	Domain      string `query:"domain" doc:"Exact name or * wildcard."`
	Status      string `query:"status" doc:"Comma-separated statuses (blocked_list,forwarded)."`
	StatusClass string `query:"status_class" enum:"permitted,cached,blocked,error,"`
	QType       string `query:"qtype"`
	Upstream    string `query:"upstream"`
	List        string `query:"list"`
	Zone        string `query:"zone"`
	Policy      string `query:"policy" doc:"Deciding policy, namespace/name."`
	Since       string `query:"since" doc:"RFC 3339, inclusive."`
	Until       string `query:"until" doc:"RFC 3339, exclusive."`
	Cursor      string `query:"cursor"`
	Limit       int    `query:"limit" minimum:"0" maximum:"1000"`
	Scope       string `query:"scope"`
	Node        string `query:"node"`
}

type streamInput struct {
	QueryParams
	LastEventID string `header:"Last-Event-ID" doc:"Resume after this event id (EventSource reconnects send it)."`
	// LastEventIDQuery is the same for clients that cannot set headers.
	LastEventIDQuery string `query:"last_event_id"`
}

// HistoryScopeParams selects a local or retained central history source.
type HistoryScopeParams struct {
	Scope string `query:"scope"`
	Node  string `query:"node"`
}

type suggestionsInput struct {
	HistoryScopeParams
	Field  string `query:"field" required:"true" enum:"domain,client,upstream,list,zone,policy"`
	Prefix string `query:"prefix"`
	N      int    `query:"n" minimum:"0" maximum:"100"`
}

type queryActionInput struct {
	Node  string `query:"node" doc:"Exact historical node for a central row ID; valid only with node scope."`
	ID    uint64 `path:"id"`
	Scope string `query:"scope" enum:"node,cluster," doc:"cluster: id is a row of the cluster-wide query log (GET /queries?scope=cluster with a central database)."`
	Body  struct {
		Action    string `json:"action" enum:"allow,deny"`
		Namespace string `json:"namespace,omitempty" doc:"Namespace of the UI list; defaults to the server's default namespace (the release namespace in Kubernetes, or \"default\" for legacy installs and other modes)."`
	}
}

type purgeInput struct {
	Before string `query:"before" doc:"Delete rows older than this instant (RFC 3339); absent deletes every row."`
}

// PurgeResult is DELETE /querylog.
type PurgeResult struct {
	Deleted int64  `json:"deleted"`
	Node    string `json:"node"`
	// CentralDeleted counts the rows of every node deleted from the
	// central database; absent without one. Other nodes' own query logs
	// are not purged.
	CentralDeleted *int64 `json:"central_deleted,omitempty"`
	Took
}

type purgeOutput struct{ Body *PurgeResult }

// ClientItem is one client of GET /clients (the inventory from the query
// log).
type ClientItem struct {
	Key     string `json:"key"`
	Name    string `json:"name,omitempty"`
	Group   string `json:"group,omitempty"`
	Count   int64  `json:"count"`
	Blocked int64  `json:"blocked"`
	// LastSeen is the client's newest query in the selected scope.
	LastSeen *time.Time `json:"last_seen,omitempty" doc:"The newest query from the client in the selected scope (hour precision once its query rows aged out)."`
}

// ClientsBody carries the inventory and the nodes that answered it.
type ClientsBody struct {
	List[ClientItem]
	sqlitelog.Scope
}

type clientsOutput struct{ Body *ClientsBody }

func (s *Server) registerStats() {
	s.registerRollupRecovery()
	stats := perm{resource: "stats", verb: "read"}
	ql := perm{resource: "querylog", verb: "read"}
	register(s, operation(http.MethodGet, "/stats/summary", "statsSummary", "Stat tiles and sidebar counters", "stats"), stats, s.statsSummary)
	register(s, operation(http.MethodGet, "/stats/timeseries", "statsTimeseries", "Query volume by status class over time", "stats"), stats, s.statsTimeseries)
	register(s, operation(http.MethodGet, "/stats/top", "statsTop", "Top domains, clients, upstreams, lists or zones", "stats"), stats, s.statsTop)
	register(s, operation(http.MethodGet, "/stats/upstreams", "statsUpstreams", "Traffic and health per upstream", "stats"), stats, s.statsUpstreams)
	register(s, operation(http.MethodGet, "/stats/clients/timeseries", "statsClientTimeseries", "Per-client query volume over time", "stats"), stats, s.statsClientTimeseries)
	register(s, operation(http.MethodGet, "/stats/recent_blocked", "statsRecentBlocked", "Most recently blocked queries", "stats"), ql, s.statsRecentBlocked)
	register(s, operation(http.MethodGet, "/clients", "listClients", "Client inventory from the query log", "stats"), perm{resource: "clients", verb: "read"}, s.listClients)
	register(s, operation(http.MethodGet, "/queries", "listQueries", "Query log page (keyset cursor, newest first)", "querylog"), ql, s.listQueries)
	register(s, operation(http.MethodGet, "/queries/suggestions", "querySuggestions", "Filter value suggestions", "querylog"), ql, s.querySuggestions)
	streamOp := operation(http.MethodGet, "/queries/stream", "queryStream", "Live query tail (Server-Sent Events)", "querylog")
	streamOp.Description = "text/event-stream of query rows: `id:` is the query id, `event: query`, `data:` one JSON row shaped like GET /queries items. " +
		"Reconnects send Last-Event-ID and receive the rows after it from the in-memory ring first. A comment line is sent every 15 s."
	streamOp.Responses = map[string]*huma.Response{"200": {Description: "Event stream", Content: map[string]*huma.MediaType{"text/event-stream": {Schema: &huma.Schema{Type: "string"}}}}}
	register(s, streamOp, ql, s.queryStream)
	register(s, operation(http.MethodPost, "/queries/{id}/actions", "queryAction", "Allow or deny the domain of a logged query", "querylog"),
		perm{resource: "blocklists", verb: "write"}, s.queryAction)
	register(s, operation(http.MethodDelete, "/querylog", "purgeQuerylog", "Delete query-log rows", "querylog"), perm{resource: "querylog", verb: "purge"}, s.purgeQuerylog)
	register(s, operation(http.MethodGet, "/querylog/status", "querylogStatus", "Query-log size, retention and writer state", "querylog"), ql, s.querylogStatus)
}

// reader returns the query-log reader or 503 when the log is off.
func (s *Server) reader() (*sqlitelog.Reader, error) {
	if s.opts.Querylog == nil {
		return nil, newError(http.StatusServiceUnavailable, KeyUnavailable, "the query log is not stored on this node",
			"set querylog.enabled: true and querylog.sink: sqlite")
	}
	return s.opts.Querylog, nil
}

// window parses window/from/to.
func (s *Server) window(in WindowParams) (sqlitelog.Window, error) {
	now := s.opts.Now()
	w := sqlitelog.Window{Until: now}
	if in.To != "" {
		t, err := time.Parse(time.RFC3339, in.To)
		if err != nil {
			return w, errInvalid("to: %v", err)
		}
		w.Until = t
	}
	switch {
	case in.From != "":
		t, err := time.Parse(time.RFC3339, in.From)
		if err != nil {
			return w, errInvalid("from: %v", err)
		}
		w.From = t
	default:
		d := 24 * time.Hour
		if in.Window != "" {
			var err error
			d, err = parseWindow(in.Window)
			if err != nil {
				return w, errInvalid("window: %v", err)
			}
		}
		w.From = w.Until.Add(-d)
	}
	if !w.From.Before(w.Until) {
		return w, errInvalid("from: must be before to")
	}
	if longterm.Needed(w.From, now) {
		if err := s.requireFeature(license.FeatureLongTerm, "statistics older than 24 hours (long-term views)"); err != nil {
			return w, err
		}
	}
	return w, nil
}

// parseWindow accepts Go durations plus a d (day) suffix ("7d").
func parseWindow(v string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("%q is not a duration", v)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a positive duration", v)
	}
	return d, nil
}

func parseStep(v string) (time.Duration, error) {
	if v == "" || v == "auto" {
		return 0, nil
	}
	d, err := parseWindow(v)
	if err != nil {
		return 0, errInvalid("step: %v", err)
	}
	return d, nil
}

// readerError maps reader failures.
func readerError(err error) error {
	if errors.Is(err, sqlitelog.ErrUnknownNode) {
		return errNotFound("unknown historical node")
	}
	if errors.Is(err, sqlitelog.ErrCentralRead) {
		return errUnavailable("central query history unavailable")
	}
	if strings.Contains(err.Error(), "invalid cursor") {
		return errInvalid("cursor: not a cursor returned by this API")
	}
	return storeError(err)
}

func (s *Server) statsSummary(ctx context.Context, in *WindowParams) (*summaryOutput, error) {
	r, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	w, err := s.window(*in)
	if err != nil {
		return nil, err
	}
	res, err := r.Summary(ctx, sqlitelog.SummaryQuery{Window: w})
	if err != nil {
		return nil, readerError(err)
	}
	return &summaryOutput{Body: &SummaryBody{Summary: *res}}, nil
}

func (s *Server) statsTimeseries(ctx context.Context, in *timeseriesInput) (*timeseriesOutput, error) {
	r, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	w, err := s.window(in.WindowParams)
	if err != nil {
		return nil, err
	}
	step, err := parseStep(in.Step)
	if err != nil {
		return nil, err
	}
	res, err := r.Timeseries(ctx, sqlitelog.TimeseriesQuery{Window: w, Step: step})
	if err != nil {
		return nil, readerError(err)
	}
	return &timeseriesOutput{Body: &TimeseriesBody{Timeseries: *res}}, nil
}

func (s *Server) statsTop(ctx context.Context, in *topInput) (*topOutput, error) {
	r, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	w, err := s.window(in.WindowParams)
	if err != nil {
		return nil, err
	}
	q := sqlitelog.TopQuery{Window: w, N: in.N, Blocked: in.Blocked}
	var res *sqlitelog.TopList
	switch in.Dim {
	case "domains":
		res, err = r.TopDomains(ctx, q)
	case "clients":
		res, err = r.TopClients(ctx, q)
	case "lists":
		res, err = r.TopLists(ctx, q)
	case "zones":
		res, err = r.TopZones(ctx, q)
	case "upstreams":
		up, uerr := r.TopUpstreams(ctx, q)
		if uerr != nil {
			return nil, readerError(uerr)
		}
		cutoff := 0
		if in.Scope == "cluster" {
			cutoff = limitN(in.N) * 4
		}
		return &topOutput{Body: &TopBody{
			TopList:   sqlitelog.TopList{Scope: up.Scope, Window: up.Window, Dimension: "upstreams", Items: []sqlitelog.TopItem{}, Approximate: up.Approximate, CutoffRank: cutoff},
			Upstreams: up.Items,
		}}, nil
	default:
		return nil, errInvalid("dim: unknown dimension %q", in.Dim)
	}
	if err != nil {
		return nil, readerError(err)
	}
	return &topOutput{Body: &TopBody{TopList: *res}}, nil
}

func (s *Server) upstreamStates(ctx context.Context) ([]upstream.ServerStatus, error) {
	if s.opts.UpstreamReads != nil {
		if ctx == nil || ctx.Err() != nil {
			return nil, errUnavailable("original upstream routes unavailable")
		}
		states, err := s.opts.UpstreamReads.UpstreamStates(ctx)
		if err != nil || ctx.Err() != nil {
			return nil, errUnavailable("original upstream routes unavailable")
		}
		return states, nil
	}
	if s.opts.Upstreams == nil {
		return nil, nil
	}
	return s.opts.Upstreams.Status(), nil
}

func (s *Server) upstreamServers(ctx context.Context) ([]UpstreamServer, error) {
	states, err := s.upstreamStates(ctx)
	if err != nil {
		return nil, err
	}
	out := []UpstreamServer{}
	for _, st := range states {
		u := upstreamServer(s.opts.NodeName, st)
		out = append(out, u)
	}
	return out, nil
}

func upstreamServer(node string, st upstream.ServerStatus) UpstreamServer {
	u := UpstreamServer{Node: node, Group: st.Group, Server: st.Server, Healthy: st.Healthy, ConsecutiveFailures: st.ConsecutiveFailures, RTTMS: float64(st.RTT.Microseconds()) / 1000}
	if !st.BackoffUntil.IsZero() {
		b := st.BackoffUntil.UTC()
		u.BackoffUntil = &b
	}
	return u
}
func (s *Server) statsUpstreams(ctx context.Context, in *WindowParams) (*upstreamsOutput, error) {
	w, err := s.window(*in)
	if err != nil {
		return nil, err
	}
	if in.Scope != "cluster" && (in.Node == "" || in.Node == s.opts.NodeName) && s.opts.Querylog == nil {
		servers, err := s.upstreamServers(ctx)
		if err != nil {
			return nil, err
		}
		return &upstreamsOutput{Body: &UpstreamsBody{UpstreamTop: sqlitelog.UpstreamTop{Scope: sqlitelog.Scope{Scope: "node", Nodes: []sqlitelog.NodeResult{{Name: s.opts.NodeName, OK: true}}}, Window: w, Items: []sqlitelog.UpstreamItem{}}, Servers: servers}}, nil
	}
	r, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	up, servers, err := r.upstreams(ctx, sqlitelog.TopQuery{Window: w, N: sqlitelog.MaxTopN})
	if err != nil {
		return nil, readerError(err)
	}
	return &upstreamsOutput{Body: &UpstreamsBody{UpstreamTop: *up, Servers: servers}}, nil
}

func (s *Server) statsClientTimeseries(ctx context.Context, in *clientTimeseriesInput) (*clientTimeseriesOutput, error) {
	r, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	w, err := s.window(in.WindowParams)
	if err != nil {
		return nil, err
	}
	step, err := parseStep(in.Step)
	if err != nil {
		return nil, err
	}
	res, err := r.ClientTimeseries(ctx, sqlitelog.ClientTimeseriesQuery{Window: w, Step: step, N: in.N})
	if err != nil {
		return nil, readerError(err)
	}
	return &clientTimeseriesOutput{Body: &ClientTimeseriesBody{ClientTimeseries: *res}}, nil
}

func (s *Server) statsRecentBlocked(ctx context.Context, in *recentBlockedInput) (*recentBlockedOutput, error) {
	r, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	n := in.N
	if n <= 0 {
		n = 10
	}
	res, err := r.RecentBlocked(ctx, n)
	if err != nil {
		return nil, readerError(err)
	}
	return &recentBlockedOutput{Body: &RecentBlockedBody{RecentBlocked: *res}}, nil
}

func (s *Server) listClients(ctx context.Context, in *WindowParams) (*clientsOutput, error) {
	r, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	w, err := s.window(*in)
	if err != nil {
		return nil, err
	}
	// The top list ranks by permitted or by blocked queries; a client
	// that only had blocked queries is in the second list only.
	byKey := map[string]*ClientItem{}
	var order []string
	var scope sqlitelog.Scope
	nodeIndex := map[string]int{}
	for _, blocked := range []bool{false, true} {
		res, err := r.TopClients(ctx, sqlitelog.TopQuery{Window: w, N: sqlitelog.MaxTopN, Blocked: blocked})
		if err != nil {
			return nil, readerError(err)
		}
		scope.Scope, scope.Source = res.Scope.Scope, res.Source
		scope.Partial = scope.Partial || res.Partial
		// Both rankings contribute to the inventory. Keep a failure from
		// either read visible even if that node answered the other one.
		for _, node := range res.Nodes {
			if i, ok := nodeIndex[node.Name]; ok {
				if !node.OK {
					scope.Nodes[i] = node
				}
			} else {
				nodeIndex[node.Name] = len(scope.Nodes)
				scope.Nodes = append(scope.Nodes, node)
			}
		}
		for _, it := range res.Items {
			if _, ok := byKey[it.Key]; !ok {
				order = append(order, it.Key)
				byKey[it.Key] = &ClientItem{Key: it.Key, Name: it.Name, Group: it.Group}
			}
			c := byKey[it.Key]
			c.Count, c.Blocked = max(c.Count, it.Total), max(c.Blocked, it.Blocked)
			if it.LastSeen != nil && (c.LastSeen == nil || it.LastSeen.After(*c.LastSeen)) {
				c.LastSeen = it.LastSeen
			}
		}
	}
	items := make([]ClientItem, 0, len(order))
	for _, k := range order {
		items = append(items, *byKey[k])
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Count > items[j].Count })
	return &clientsOutput{Body: &ClientsBody{List: *newList(items, ""), Scope: scope}}, nil
}

// queryFilter converts the query parameters.
func queryFilter(in QueryParams) (sqlitelog.QueryFilter, error) {
	f := sqlitelog.QueryFilter{
		Client: in.Client, Domain: in.Domain, StatusClass: pipeline.Class(in.StatusClass),
		Upstream: in.Upstream, List: in.List, Zone: in.Zone, Policy: in.Policy, Cursor: in.Cursor, Limit: in.Limit,
	}
	if in.Status != "" {
		for _, p := range strings.Split(in.Status, ",") {
			st, ok := querylog.ParseStatus(p)
			if !ok {
				return f, errInvalid("status: unknown status %q", p)
			}
			f.Status = append(f.Status, st)
		}
	}
	if in.QType != "" {
		t, ok := querylog.ParseQType(in.QType)
		if !ok {
			return f, errInvalid("qtype: unknown type %q", in.QType)
		}
		f.QType = t
	}
	for _, tc := range []struct {
		v    string
		into *time.Time
		name string
	}{{in.Since, &f.Since, "since"}, {in.Until, &f.Until, "until"}} {
		if tc.v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, tc.v)
		if err != nil {
			return f, errInvalid("%s: %v", tc.name, err)
		}
		*tc.into = t
	}

	return f, nil
}

func (s *Server) listQueries(ctx context.Context, in *QueryParams) (*queriesOutput, error) {
	r, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	f, err := queryFilter(*in)
	if err != nil {
		return nil, err
	}
	if in.Scope != "cluster" && f.Cursor != "" {
		if id, err := strconv.ParseInt(f.Cursor, 10, 64); err != nil || id <= 0 {
			return nil, errInvalid("invalid cursor")
		}
	}
	res, err := r.Queries(ctx, f)
	if err != nil {
		return nil, readerError(err)
	}
	return &queriesOutput{Body: &QueriesBody{QueryPage: *res}}, nil
}

func (s *Server) querySuggestions(ctx context.Context, in *suggestionsInput) (*suggestionsOutput, error) {
	scoped, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	r := scoped.direct()
	if r == nil {
		return nil, errUnavailable("dictionary suggestions require local or central history")
	}
	res, err := r.Suggestions(ctx, in.Field, in.Prefix, in.N)
	if err != nil {
		return nil, readerError(err)
	}
	return &suggestionsOutput{Body: &SuggestionsBody{Suggestions: *res}}, nil
}

func (s *Server) querylogStatus(ctx context.Context, in *HistoryScopeParams) (*querylogStatusOutput, error) {
	scoped, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	r := scoped.direct()
	if r == nil {
		return nil, errUnavailable("query-log status requires local or central history")
	}
	res, err := r.Status(ctx)
	if err != nil {
		return nil, readerError(err)
	}
	return &querylogStatusOutput{Body: &QuerylogStatusBody{Status: *res}}, nil
}

func (s *Server) purgeQuerylog(ctx context.Context, in *purgeInput) (*purgeOutput, error) {
	if s.opts.Purger == nil {
		return nil, errNotImplemented("query-log purge")
	}
	var before time.Time
	if in.Before != "" {
		t, err := time.Parse(time.RFC3339, in.Before)
		if err != nil {
			return nil, errInvalid("before: %v", err)
		}
		before = t
	}
	n, err := s.opts.Purger.Purge(ctx, before)
	out := &PurgeResult{Deleted: n, Node: s.opts.NodeName}
	after := map[string]any{"before": in.Before, "deleted": n}
	if err == nil && s.opts.CentralPurger != nil {
		var c int64
		c, err = s.opts.CentralPurger.Purge(ctx, before)
		out.CentralDeleted, after["central_deleted"] = &c, c
	}
	s.recordAction(ctx, "querylog.purge", audit.Resource{Kind: "querylog"}, err, nil, after)
	if err != nil {
		return nil, storeError(err)
	}
	return &purgeOutput{Body: out}, nil
}

func (s *Server) queryAction(ctx context.Context, in *queryActionInput) (*processedOutput, error) {
	if s.useDomainCommand() {
		return s.commandQueryAction(ctx, in)
	}
	r, err := s.queryActionReader(ctx, in)
	if err != nil {
		return nil, err
	}
	page, err := r.Queries(ctx, sqlitelog.QueryFilter{Cursor: strconv.FormatUint(in.ID+1, 10), Limit: 1})
	if err != nil {
		return nil, readerError(err)
	}
	if len(page.Queries) == 0 || page.Queries[0].ID != in.ID {
		return nil, errNotFound(fmt.Sprintf("query %d", in.ID))
	}
	return s.addDomains(ctx, &domainsInput{Action: in.Body.Action, Kind: "exact",
		Body: DomainsRequest{Domains: []string{page.Queries[0].Domain}, Namespace: in.Body.Namespace}})
}

// StreamRow is one SSE event of /queries/stream: the GET /queries row.
type StreamRow = sqlitelog.QueryRow

// streamFilter is the in-process filter of the live tail.
type streamFilter struct {
	client      string
	clientNet   netip.Prefix
	domain      string
	statusClass pipeline.Class
	statuses    map[pipeline.Status]bool
	qtype       uint16
	upstream    string
	list        string
	zone        string
	policy      string
}

func newStreamFilter(in QueryParams) (*streamFilter, error) {
	f, err := queryFilter(in)
	if err != nil {
		return nil, err
	}
	sf := &streamFilter{client: strings.ToLower(f.Client), domain: strings.ToLower(f.Domain), statusClass: f.StatusClass,
		qtype: f.QType, upstream: f.Upstream, list: f.List, zone: f.Zone, policy: f.Policy}
	if p, err := netip.ParsePrefix(f.Client); err == nil {
		sf.clientNet = p.Masked()
	}
	if len(f.Status) > 0 {
		sf.statuses = map[pipeline.Status]bool{}
		for _, st := range f.Status {
			sf.statuses[st] = true
		}
	}
	return sf, nil
}

func globMatch(pattern, s string) bool {
	if !strings.ContainsAny(pattern, "*?[") {
		return pattern == s
	}
	ok, err := path.Match(pattern, s)
	return err == nil && ok
}

func (f *streamFilter) match(ev *querylog.Event) bool {
	if f.client != "" {
		switch {
		case f.clientNet.IsValid():
			if !f.clientNet.Contains(ev.Client) {
				return false
			}
		case !globMatch(f.client, ev.Client.String()) && !globMatch(f.client, strings.ToLower(ev.ClientName)):
			return false
		}
	}
	if f.domain != "" && !globMatch(f.domain, strings.TrimSuffix(strings.ToLower(ev.Name), ".")) {
		return false
	}
	if f.statusClass != "" && ev.Status.Class() != f.statusClass {
		return false
	}
	if f.statuses != nil && !f.statuses[ev.Status] {
		return false
	}
	switch {
	case f.qtype != 0 && ev.QType != f.qtype,
		f.upstream != "" && ev.Upstream != f.upstream,
		f.list != "" && ev.List != f.list,
		f.zone != "" && ev.Zone != f.zone,
		f.policy != "" && ev.Policy != f.policy:
		return false
	}
	return true
}

// eventRow renders a tail event like a GET /queries row.
func eventRow(ev *querylog.Event) StreamRow {
	row := StreamRow{
		ID: ev.ID, TS: ev.Time.UTC(), Node: ev.Node, Proto: ev.Proto.String(),
		Client: sqlitelog.QueryClient{IP: ev.Client.String(), Name: ev.ClientName, Group: ev.ClientGroup},
		Domain: strings.TrimSuffix(ev.Name, "."), CNAME: ev.CNAME, QType: querylog.QTypeName(ev.QType), QClass: int(ev.QClass),
		Status: ev.Status.String(), StatusClass: string(ev.Status.Class()), Rcode: querylog.RcodeName(ev.Rcode),
		Reply:    sqlitelog.QueryReply{Type: ev.ReplyType.String(), Answer: ev.Answer},
		Upstream: ev.Upstream, Zone: ev.Zone, List: ev.List, Rule: ev.Rule, Policy: ev.Policy, DNSSEC: ev.DNSSEC.String(), Flags: ev.Flags,
	}
	ms := float64(ev.Latency.Microseconds()) / 1000
	row.Reply.TimeMS = &ms
	if ev.HasEDE {
		v := int(ev.EDE)
		row.EDE = &v
	}
	if ev.ECS.IsValid() {
		row.ECS = ev.ECS.String()
	}
	if ev.Traced() {
		row.TraceID = hex.EncodeToString(ev.TraceID[:])
	}
	return row
}

// sseHeartbeat is the interval of keep-alive comments on the stream.
var sseHeartbeat = 15 * time.Second

// maxQueryStreams caps the concurrent live-tail streams of one server:
// each holds a goroutine, a subscriber buffer and a connection. Streams
// beyond it get 503 with Retry-After.
var maxQueryStreams = 64

// sseWriteTimeout bounds every write cycle of a stream (events, flush,
// heartbeat), so a client that stops reading is disconnected instead of
// holding its goroutine forever.
const sseWriteTimeout = 30 * time.Second

func (s *Server) queryStream(ctx context.Context, in *streamInput) (*huma.StreamResponse, error) {
	peers, err := s.queryPeers(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	if in.Scope == "cluster" || !peers[0].Self {
		return s.scopedQueryStream(ctx, in, peers)
	}
	if s.opts.Tail == nil && s.opts.Querylog == nil {
		return nil, newError(http.StatusServiceUnavailable, KeyUnavailable, "the live tail is not available on this node", "set querylog.enabled: true")
	}
	filter, err := newStreamFilter(in.QueryParams)
	if err != nil {
		return nil, err
	}
	select {
	case s.streams <- struct{}{}:
	default:
		e := newError(http.StatusServiceUnavailable, KeyUnavailable,
			fmt.Sprintf("too many live query streams on this node (%d)", cap(s.streams)), "close another stream or retry shortly")
		e.GetHeaders().Set("Retry-After", "5")
		return nil, e
	}
	var lastID uint64
	if v := in.LastEventID; v != "" {
		lastID, _ = strconv.ParseUint(v, 10, 64)
	} else if v := in.LastEventIDQuery; v != "" {
		lastID, _ = strconv.ParseUint(v, 10, 64)
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		defer func() { <-s.streams }()
		r, w := humago.Unwrap(hctx)
		sctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		var events <-chan *querylog.Event
		if s.opts.Tail != nil {
			events = s.opts.Tail.Subscribe(sctx)
		} else {
			events = s.opts.Querylog.Subscribe(sctx)
		}
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		arm := func() { _ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) }
		arm()
		send := func(ev *querylog.Event) bool {
			if ev.ID != 0 && ev.ID <= lastID {
				return true
			}
			if !filter.match(ev) {
				return true
			}
			b, err := json.Marshal(eventRow(ev))
			if err != nil {
				return true
			}
			if ev.ID != 0 {
				if _, err := fmt.Fprintf(w, "id: %d\n", ev.ID); err != nil {
					return false
				}
				lastID = ev.ID
			}
			if _, err := fmt.Fprintf(w, "event: query\ndata: %s\n\n", b); err != nil {
				return false
			}
			return true
		}
		if _, err := fmt.Fprintf(w, "retry: 3000\n: node %s\n\n", s.opts.NodeName); err != nil {
			return
		}
		if lastID != 0 && s.opts.Tail != nil {
			for _, ev := range s.opts.Tail.Recent(0, lastID) {
				if !send(ev) {
					return
				}
			}
		}
		_ = rc.Flush()
		tick := time.NewTicker(sseHeartbeat)
		defer tick.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				arm()
				if !send(ev) {
					return
				}
				// Drain what is already queued before flushing.
				for drained := false; !drained; {
					select {
					case ev, ok := <-events:
						if !ok {
							_ = rc.Flush()
							return
						}
						if !send(ev) {
							return
						}
					default:
						drained = true
					}
				}
				if rc.Flush() != nil {
					return
				}
			case <-tick.C:
				arm()
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					return
				}
			}
		}
	}}, nil
}

// queryActionReader preserves the central ID namespace of historical reads.
// The selected immutable domain remains input to the existing current command.
func (s *Server) queryActionReader(ctx context.Context, in *queryActionInput) (*sqlitelog.Reader, error) {
	if in.Scope == sqlitelog.ScopeCluster && in.Node == "" && s.opts.CentralQuerylog == nil {
		return nil, errUnavailable("central query history unavailable")
	}
	r, err := s.scopedReader(ctx, in.Scope, in.Node)
	if err != nil {
		return nil, err
	}
	if d := r.direct(); d != nil {
		return d, nil
	}
	return nil, errUnavailable("query actions require local or central history")
}
