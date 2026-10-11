package sqlitelog

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// The structs below are the JSON bodies of the docs/architecture.md §10
// stats and log endpoints (the huma handlers in M3 return them as they
// are, adding took_ms). Every response embeds Scope so the cluster
// scatter-gather (scope=cluster) can fill nodes[] and partial without a
// shape change; a single node answers scope "node", one node entry, never
// partial.

// ScopeNode is the only scope a Reader answers; ScopeCluster responses are
// merged by the API layer from per-node results.
const (
	ScopeNode    = "node"
	ScopeCluster = "cluster"
)

// Scope describes which nodes contributed to a response.
type Scope struct {
	Scope string `json:"scope"`
	// Nodes lists every node asked, with its outcome.
	Nodes []NodeResult `json:"nodes"`
	// Partial is true when at least one node failed or timed out.
	Partial bool `json:"partial"`
}

// NodeResult is one node's part in a response.
type NodeResult struct {
	Name   string  `json:"name"`
	OK     bool    `json:"ok"`
	TookMS float64 `json:"took_ms"`
	Error  string  `json:"error,omitempty"`
}

// Window is a half-open time range [From, Until). Zero values default to
// the last 24 hours ending now.
type Window struct {
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`
}

// SummaryQuery parameterises Summary (GET /stats/summary).
type SummaryQuery struct {
	Window
}

// Summary feeds the four stat tiles and the sidebar (§10 item 1).
type Summary struct {
	Scope
	Window
	Queries SummaryQueries `json:"queries"`
	Clients SummaryClients `json:"clients"`
	// DomainsOnLists is the number of blocked domains loaded (fourth tile).
	DomainsOnLists int64            `json:"domains_on_lists"`
	Rcodes         map[string]int64 `json:"rcodes"`
	Protocols      map[string]int64 `json:"protocols"`
	Types          map[string]int64 `json:"types"`
	Latency        LatencySummary   `json:"latency"`
	// Approximate is set when part of the window lies beyond a rollup's
	// retention, or unique counts were taken at hour granularity.
	Approximate bool `json:"approximate"`
}

// SummaryQueries are the query counters of the summary.
type SummaryQueries struct {
	Total          int64   `json:"total"`
	Blocked        int64   `json:"blocked"`
	PercentBlocked float64 `json:"percent_blocked"`
	Cached         int64   `json:"cached"`
	Forwarded      int64   `json:"forwarded"`
	Authoritative  int64   `json:"authoritative"`
	Dropped        int64   `json:"dropped"`
	Other          int64   `json:"other"`
	UniqueDomains  int64   `json:"unique_domains"`
	// QPS1m is the rate over the last complete minute; QPSAvg over the
	// window.
	QPS1m  float64 `json:"qps_1m"`
	QPSAvg float64 `json:"qps_avg"`
}

// SummaryClients is the tile footer: clients active in the window and
// known overall.
type SummaryClients struct {
	Active int64 `json:"active"`
	Total  int64 `json:"total"`
}

// LatencySummary is derived from the rollup latency histogram (bucket
// upper bounds, so percentiles are upper estimates).
type LatencySummary struct {
	AvgMS float64 `json:"avg_ms"`
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
}

// TimeseriesQuery parameterises Timeseries (GET /stats/timeseries).
type TimeseriesQuery struct {
	Window
	// Step is the bucket width; 0 picks one automatically (≤ 300 points,
	// at least one minute, coarser when the window predates the minute or
	// hour rollups).
	Step time.Duration `json:"step"`
}

// Timeseries is the 24 h stacked bar chart (§10 item 2).
type Timeseries struct {
	Scope
	Window
	// Step is the bucket width in seconds.
	Step        int64   `json:"step"`
	Points      []Point `json:"points"`
	Approximate bool    `json:"approximate"`
}

// Point is one bucket of the status-class series.
type Point struct {
	TS            time.Time `json:"ts"`
	Total         int64     `json:"total"`
	Blocked       int64     `json:"blocked"`
	Cached        int64     `json:"cached"`
	Forwarded     int64     `json:"forwarded"`
	Authoritative int64     `json:"authoritative"`
	Other         int64     `json:"other"`
}

// TopQuery parameterises the top lists (GET /stats/top?dim=…).
type TopQuery struct {
	Window
	// N is the list length (default 10, at most 100).
	N int `json:"n"`
	// Blocked ranks by blocked queries; false ranks by permitted ones.
	Blocked bool `json:"blocked"`
}

// TopList is a ranked dimension (§10 item 3).
type TopList struct {
	Scope
	Window
	Dimension string    `json:"dim"`
	Blocked   bool      `json:"blocked"`
	Items     []TopItem `json:"items"`
	// TotalInWindow is the denominator for Percent: all queries (or all
	// blocked queries when Blocked) in the window.
	TotalInWindow int64 `json:"total_in_window"`
	// Approximate and CutoffRank are set by cluster merging (top-N from
	// n*4 per node) and when the window exceeds a table's retention.
	Approximate bool `json:"approximate"`
	CutoffRank  int  `json:"cutoff_rank,omitempty"`
}

// TopItem is one ranked key.
type TopItem struct {
	Key     string  `json:"key"`
	Name    string  `json:"name,omitempty"`
	Group   string  `json:"group,omitempty"`
	Count   int64   `json:"count"`
	Total   int64   `json:"total"`
	Blocked int64   `json:"blocked"`
	Percent float64 `json:"percent"`
}

// UpstreamTop is GET /stats/upstreams.
type UpstreamTop struct {
	Scope
	Window
	Items       []UpstreamItem `json:"upstreams"`
	Approximate bool           `json:"approximate"`
}

// UpstreamItem is one upstream's (or "cache", "zone:…") traffic.
type UpstreamItem struct {
	Addr     string  `json:"addr"`
	Total    int64   `json:"total"`
	Errors   int64   `json:"errors"`
	Timeouts int64   `json:"timeouts"`
	Percent  float64 `json:"percent"`
	AvgMS    float64 `json:"avg_ms"`
	P95MS    float64 `json:"p95_ms"`
}

// ClientTimeseriesQuery parameterises ClientTimeseries.
type ClientTimeseriesQuery struct {
	Window
	Step time.Duration `json:"step"`
	// N is the number of named clients (default 10); the rest are "other".
	N int `json:"n"`
}

// ClientTimeseries is the per-client stacked chart with "other clients".
type ClientTimeseries struct {
	Scope
	Window
	Step int64 `json:"step"`
	// Clients names the series; the last entry is always "other".
	Clients     []ClientKey   `json:"clients"`
	Points      []ClientPoint `json:"points"`
	Approximate bool          `json:"approximate"`
}

// ClientKey identifies a client series.
type ClientKey struct {
	Key  string `json:"key"`
	Name string `json:"name,omitempty"`
}

// ClientPoint is one bucket; Counts aligns with ClientTimeseries.Clients.
type ClientPoint struct {
	TS     time.Time `json:"ts"`
	Counts []int64   `json:"counts"`
}

// RecentBlocked is GET /stats/recent_blocked.
type RecentBlocked struct {
	Scope
	Items []BlockedItem `json:"items"`
}

// BlockedItem is one recently blocked query.
type BlockedItem struct {
	ID         uint64    `json:"id"`
	TS         time.Time `json:"ts"`
	Domain     string    `json:"domain"`
	Client     string    `json:"client"`
	ClientName string    `json:"client_name,omitempty"`
	List       string    `json:"list,omitempty"`
	Status     string    `json:"status"`
}

// QueryFilter parameterises Queries (GET /queries).
type QueryFilter struct {
	// Client matches an IP, a CIDR prefix, or a client name (with *
	// wildcards).
	Client string `json:"client,omitempty"`
	// Domain matches the name exactly, or with * wildcards.
	Domain string `json:"domain,omitempty"`
	// Status keeps the listed statuses; StatusClass a whole class.
	Status      []pipeline.Status `json:"status,omitempty"`
	StatusClass pipeline.Class    `json:"status_class,omitempty"`
	// QType keeps one query type (0: any).
	QType uint16 `json:"qtype,omitempty"`
	// Upstream, List and Zone match dictionary names exactly.
	Upstream string `json:"upstream,omitempty"`
	List     string `json:"list,omitempty"`
	Zone     string `json:"zone,omitempty"`
	// Since and Until bound ts (inclusive, exclusive).
	Since time.Time `json:"since,omitzero"`
	Until time.Time `json:"until,omitzero"`
	// Cursor is the cursor_next of the previous page ("" for the newest
	// rows). At node scope it is the decimal id; cluster cursors encode
	// one per node.
	Cursor string `json:"cursor,omitempty"`
	// Limit is the page size (default 100, at most 1 000).
	Limit int `json:"limit,omitempty"`
}

// QueryPage is one page of the query log (§10 item 5).
type QueryPage struct {
	Scope
	Queries []QueryRow `json:"queries"`
	// CursorNext fetches the next (older) page; empty on the last page.
	CursorNext string `json:"cursor_next,omitempty"`
	// RecordsTotal counts every row; RecordsFiltered those matching the
	// filter (ignoring the cursor).
	RecordsTotal    int64 `json:"records_total"`
	RecordsFiltered int64 `json:"records_filtered"`
	// EarliestTSDisk is the oldest row in querylog.db, EarliestTSMemory
	// the oldest event in the hot ring (date-picker bounds).
	EarliestTSDisk   *time.Time `json:"earliest_ts_disk,omitempty"`
	EarliestTSMemory *time.Time `json:"earliest_ts_memory,omitempty"`
}

// QueryRow is one query with its "why" fields.
type QueryRow struct {
	ID          uint64         `json:"id"`
	TS          time.Time      `json:"ts"`
	Node        string         `json:"node"`
	Proto       string         `json:"proto"`
	Client      QueryClient    `json:"client"`
	Domain      string         `json:"domain"`
	CNAME       string         `json:"cname,omitempty"`
	QType       string         `json:"qtype"`
	QClass      int            `json:"qclass"`
	Status      string         `json:"status"`
	StatusClass string         `json:"status_class"`
	Rcode       string         `json:"rcode"`
	Reply       QueryReply     `json:"reply"`
	Upstream    string         `json:"upstream,omitempty"`
	Zone        string         `json:"zone,omitempty"`
	List        string         `json:"list,omitempty"`
	Rule        string         `json:"rule,omitempty"`
	DNSSEC      string         `json:"dnssec"`
	EDE         *int           `json:"ede,omitempty"`
	ECS         string         `json:"ecs,omitempty"`
	TraceID     string         `json:"trace_id,omitempty"`
	Flags       querylog.Flags `json:"flags"`
}

// QueryClient is the client of a row.
type QueryClient struct {
	IP    string `json:"ip"`
	Name  string `json:"name,omitempty"`
	Group string `json:"group,omitempty"`
}

// QueryReply describes the answer.
type QueryReply struct {
	Type   string   `json:"type"`
	TimeMS *float64 `json:"time_ms,omitempty"`
	Answer string   `json:"answer,omitempty"`
}

// Suggestion fields for Suggestions.
const (
	FieldDomain   = "domain"
	FieldClient   = "client"
	FieldUpstream = "upstream"
	FieldList     = "list"
	FieldZone     = "zone"
)

// Suggestions is GET /queries/suggestions.
type Suggestions struct {
	Scope
	Field  string   `json:"field"`
	Values []string `json:"values"`
}

// Status is GET /querylog/status.
type Status struct {
	Scope
	Rows        int64      `json:"rows"`
	DBBytes     int64      `json:"db_bytes"`
	WALBytes    int64      `json:"wal_bytes"`
	OldestTS    *time.Time `json:"oldest_ts,omitempty"`
	Privacy     string     `json:"privacy"`
	Retention   Retention  `json:"retention"`
	Writer      Stats      `json:"writer"`
	Subscribers int        `json:"subscribers"`
}

// Retention echoes the effective retention settings.
type Retention struct {
	RawSeconds int64  `json:"raw_seconds"`
	MaxDBBytes int64  `json:"max_db_bytes"`
	Rollup1m   string `json:"rollup_1m"`
	Rollup1h   string `json:"rollup_1h"`
	Rollup1d   string `json:"rollup_1d"`
}
