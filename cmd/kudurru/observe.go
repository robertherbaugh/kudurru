package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"github.com/spf13/cobra"

	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
)

// statsScope is the scope block every stats/log response carries.
type statsScope struct {
	Scope string `json:"scope"`
	Nodes []struct {
		Name  string `json:"name"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	} `json:"nodes"`
	Partial bool `json:"partial"`
}

func (s statsScope) String() string {
	names := make([]string, 0, len(s.Nodes))
	for _, n := range s.Nodes {
		name := n.Name
		if !n.OK && n.Error != "" {
			name += " (failed: " + n.Error + ")"
		}
		names = append(names, name)
	}
	out := firstNonEmpty(s.Scope, "node")
	if len(names) > 0 {
		out += " (" + strings.Join(names, ", ") + ")"
	}
	if s.Partial {
		out += ", partial"
	}
	return out
}

// statsSummary is GET /stats/summary (internal/querylog/sqlitelog.Summary).
type statsSummary struct {
	Scope   statsScope `json:"scope"`
	From    time.Time  `json:"from"`
	Until   time.Time  `json:"until"`
	Queries struct {
		Total          int64   `json:"total"`
		Blocked        int64   `json:"blocked"`
		PercentBlocked float64 `json:"percent_blocked"`
		Cached         int64   `json:"cached"`
		Forwarded      int64   `json:"forwarded"`
		Authoritative  int64   `json:"authoritative"`
		Dropped        int64   `json:"dropped"`
		Other          int64   `json:"other"`
		UniqueDomains  int64   `json:"unique_domains"`
		QPS1m          float64 `json:"qps_1m"`
		QPSAvg         float64 `json:"qps_avg"`
	} `json:"queries"`
	Clients struct {
		Active int64 `json:"active"`
		Total  int64 `json:"total"`
	} `json:"clients"`
	DomainsOnLists int64 `json:"domains_on_lists"`
	Latency        struct {
		AvgMS float64 `json:"avg_ms"`
		P50MS float64 `json:"p50_ms"`
		P95MS float64 `json:"p95_ms"`
		P99MS float64 `json:"p99_ms"`
	} `json:"latency"`
	Approximate bool `json:"approximate"`
}

// UnmarshalJSON accepts the scope either embedded (the Go reader embeds
// Scope, so "scope" is the string and "nodes"/"partial" sit beside it) or
// nested as scope:{scope,nodes,partial} as the contract words it.
func (s *statsSummary) UnmarshalJSON(b []byte) error {
	type plain statsSummary
	var aux struct {
		plain
		Scope json.RawMessage `json:"scope"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*s = statsSummary(aux.plain)
	sc, err := decodeScope(b, aux.Scope)
	s.Scope = sc
	return err
}

func decodeScope(whole []byte, raw json.RawMessage) (statsScope, error) {
	var sc statsScope
	if len(raw) > 0 && raw[0] == '{' {
		return sc, json.Unmarshal(raw, &sc)
	}
	return sc, json.Unmarshal(whole, &sc)
}

// topResult is GET /stats/top.
type topResult struct {
	Scope       statsScope `json:"-"`
	Dimension   string     `json:"dim"`
	Blocked     bool       `json:"blocked"`
	Approximate bool       `json:"approximate"`
	Items       []struct {
		Key     string  `json:"key"`
		Name    string  `json:"name,omitempty"`
		Count   int64   `json:"count"`
		Blocked int64   `json:"blocked"`
		Percent float64 `json:"percent"`
	} `json:"items"`
	// Upstreams replaces items for dim=upstreams.
	Upstreams []struct {
		Addr     string  `json:"addr"`
		Total    int64   `json:"total"`
		Errors   int64   `json:"errors"`
		Timeouts int64   `json:"timeouts"`
		Percent  float64 `json:"percent"`
		AvgMS    float64 `json:"avg_ms"`
		P95MS    float64 `json:"p95_ms"`
	} `json:"upstreams"`
}

func newStatsCmd(o *cliOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "stats", Short: "Query statistics (this node)"}
	var window string
	summary := &cobra.Command{
		Use:   "summary",
		Short: "Totals, block rate, clients and latency for a window",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := c.Get(cmd.Context(), "/stats/summary", url.Values{"window": {window}}, &raw); err != nil {
				return err
			}
			f, err := o.format()
			if err != nil {
				return err
			}
			if f.Structured() {
				return cli.WriteStructured(cmd.OutOrStdout(), f, raw)
			}
			var s statsSummary
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			q := s.Queries
			approx := ""
			if s.Approximate {
				approx = " (approximate)"
			}
			pairs := [][2]string{
				{"Scope", s.Scope.String()},
				{"Window", fmt.Sprintf("%s to %s%s", formatTime(s.From), formatTime(s.Until), approx)},
				{"Total queries", strconv.FormatInt(q.Total, 10)},
				{"Blocked", fmt.Sprintf("%d (%.1f%%)", q.Blocked, q.PercentBlocked)},
				{"Cached", strconv.FormatInt(q.Cached, 10)},
				{"Forwarded", strconv.FormatInt(q.Forwarded, 10)},
				{"Authoritative", strconv.FormatInt(q.Authoritative, 10)},
				{"Dropped", strconv.FormatInt(q.Dropped, 10)},
				{"Unique domains", strconv.FormatInt(q.UniqueDomains, 10)},
				{"Domains on lists", strconv.FormatInt(s.DomainsOnLists, 10)},
				{"Clients", fmt.Sprintf("%d active / %d total", s.Clients.Active, s.Clients.Total)},
				{"QPS", fmt.Sprintf("%.1f (1m), %.1f (avg)", q.QPS1m, q.QPSAvg)},
				{"Latency", fmt.Sprintf("avg %.1f ms, p50 %.1f, p95 %.1f, p99 %.1f", s.Latency.AvgMS, s.Latency.P50MS, s.Latency.P95MS, s.Latency.P99MS)},
			}
			return cli.KeyValues(cmd.OutOrStdout(), "", pairs)
		},
	}
	summary.Flags().StringVar(&window, "window", "24h", "window ending now (e.g. 1h, 24h, 168h)")

	var (
		dim     string
		n       int
		blocked bool
		twindow string
	)
	top := &cobra.Command{
		Use:   "top",
		Short: "Top domains, clients, upstreams, lists or zones",
		Example: `  kudurru stats top --dim domains --blocked
  kudurru stats top --dim clients --top 20 --window 1h`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch dim {
			case "domains", "clients", "upstreams", "lists", "zones":
			default:
				return fmt.Errorf("--dim must be domains, clients, upstreams, lists or zones, not %q", dim)
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			q := url.Values{"dim": {dim}, "n": {strconv.Itoa(n)}, "window": {twindow}}
			if blocked {
				q.Set("blocked", "true")
			}
			var raw json.RawMessage
			if err := c.Get(cmd.Context(), "/stats/top", q, &raw); err != nil {
				return err
			}
			var res topResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return err
			}
			return o.render(cmd, raw, func(wide bool) (*cli.Table, error) {
				if dim == "upstreams" && len(res.Items) == 0 {
					t := &cli.Table{Headers: []string{"RANK", "UPSTREAM", "COUNT", "PERCENT", "ERRORS", "TIMEOUTS", "AVG-MS", "P95-MS"}}
					for i, u := range res.Upstreams {
						t.Add(strconv.Itoa(i+1), u.Addr, strconv.FormatInt(u.Total, 10), fmt.Sprintf("%.1f%%", u.Percent), strconv.FormatInt(u.Errors, 10),
							strconv.FormatInt(u.Timeouts, 10), fmt.Sprintf("%.1f", u.AvgMS), fmt.Sprintf("%.1f", u.P95MS))
					}
					return t, nil
				}
				t := &cli.Table{Headers: []string{"RANK", strings.ToUpper(strings.TrimSuffix(dim, "s")), "COUNT", "PERCENT"}}
				if wide {
					t.Headers = append(t.Headers, "NAME", "BLOCKED")
				}
				for i, it := range res.Items {
					row := []string{strconv.Itoa(i + 1), it.Key, strconv.FormatInt(it.Count, 10), fmt.Sprintf("%.1f%%", it.Percent)}
					if wide {
						row = append(row, firstNonEmpty(it.Name, "-"), strconv.FormatInt(it.Blocked, 10))
					}
					t.Add(row...)
				}
				return t, nil
			})
		},
	}
	top.Flags().StringVar(&dim, "dim", "domains", "dimension: domains, clients, upstreams, lists or zones")
	top.Flags().IntVar(&n, "top", 10, "number of rows (at most 100)")
	top.Flags().BoolVar(&blocked, "blocked", false, "rank blocked queries only")
	top.Flags().StringVar(&twindow, "window", "24h", "window ending now")
	_ = top.RegisterFlagCompletionFunc("dim", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"domains", "clients", "upstreams", "lists", "zones"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.AddCommand(summary, top)
	return cmd
}

// logRow is one query, decoded from either the GET /queries row shape
// (sqlitelog.QueryRow) or the live tail's event shape (querylog.Event),
// since the contract does not fix the SSE payload.
type logRow struct {
	ID          uint64
	TS          time.Time
	Node        string
	Proto       string
	ClientIP    string
	ClientName  string
	Domain      string
	QType       string
	Status      string
	StatusClass string
	Rcode       string
	Upstream    string
	List        string
	Rule        string
	Zone        string
	Answer      string
	TimeMS      float64
}

func (r *logRow) UnmarshalJSON(b []byte) error {
	var w struct {
		ID          uint64          `json:"id"`
		TS          time.Time       `json:"ts"`
		Node        string          `json:"node"`
		Proto       json.RawMessage `json:"proto"`
		Client      json.RawMessage `json:"client"`
		ClientName  string          `json:"client_name"`
		Domain      string          `json:"domain"`
		Name        string          `json:"name"`
		QType       json.RawMessage `json:"qtype"`
		Status      string          `json:"status"`
		StatusClass string          `json:"status_class"`
		Rcode       json.RawMessage `json:"rcode"`
		Upstream    string          `json:"upstream"`
		List        string          `json:"list"`
		Rule        string          `json:"rule"`
		Zone        string          `json:"zone"`
		Answer      string          `json:"answer"`
		Latency     *int64          `json:"latency"`
		Reply       *struct {
			TimeMS *float64 `json:"time_ms"`
			Answer string   `json:"answer"`
		} `json:"reply"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*r = logRow{ID: w.ID, TS: w.TS, Node: w.Node, ClientName: w.ClientName, Domain: firstNonEmpty(w.Domain, w.Name),
		Status: w.Status, StatusClass: w.StatusClass, Upstream: w.Upstream, List: w.List, Rule: w.Rule, Zone: w.Zone, Answer: w.Answer}
	r.Domain = strings.TrimSuffix(r.Domain, ".")
	r.Proto = rawString(w.Proto)
	var cl struct {
		IP   string `json:"ip"`
		Name string `json:"name"`
	}
	if json.Unmarshal(w.Client, &cl) == nil && cl.IP != "" {
		r.ClientIP, r.ClientName = cl.IP, firstNonEmpty(cl.Name, r.ClientName)
	} else {
		r.ClientIP = rawString(w.Client)
	}
	var qt uint16
	if json.Unmarshal(w.QType, &qt) == nil {
		r.QType = dns.TypeToString[qt]
		if r.QType == "" {
			r.QType = "TYPE" + strconv.Itoa(int(qt))
		}
	} else {
		r.QType = rawString(w.QType)
	}
	r.Rcode = rcodeString(w.Rcode)
	if w.Reply != nil {
		if w.Reply.TimeMS != nil {
			r.TimeMS = *w.Reply.TimeMS
		}
		r.Answer = firstNonEmpty(r.Answer, w.Reply.Answer)
	} else if w.Latency != nil {
		r.TimeMS = float64(*w.Latency) / 1e6
	}
	return nil
}

func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

// logFilter mirrors the GET /queries filters the CLI exposes; it is also
// applied client-side to the live stream.
type logFilter struct {
	client      string
	domain      string
	status      []string
	statusClass string
	qtype       string
	upstream    string
	list        string
	zone        string
	since       string
	until       string
}

func (f logFilter) query(now time.Time) (url.Values, error) {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("client", f.client)
	set("domain", f.domain)
	if len(f.status) > 0 {
		q.Set("status", strings.Join(f.status, ","))
	}
	set("status_class", f.statusClass)
	set("qtype", strings.ToUpper(f.qtype))
	set("upstream", f.upstream)
	set("list", f.list)
	set("zone", f.zone)
	for k, v := range map[string]string{"since": f.since, "until": f.until} {
		if v == "" {
			continue
		}
		t, err := parseSince(v, now)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", k, err)
		}
		q.Set(k, t.UTC().Format(time.RFC3339))
	}
	return q, nil
}

func wildMatch(pattern, s string) bool {
	pattern, s = strings.ToLower(pattern), strings.ToLower(s)
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	ok, _ := path.Match(pattern, s)
	return ok
}

// match applies the filter to one streamed row.
func (f logFilter) match(r logRow) bool {
	if f.client != "" && !wildMatch(f.client, r.ClientIP) && !wildMatch(f.client, r.ClientName) && !strings.Contains(f.client, "/") {
		return false
	}
	if f.domain != "" && !wildMatch(strings.TrimSuffix(f.domain, "."), r.Domain) {
		return false
	}
	if len(f.status) > 0 && !slices.Contains(f.status, r.Status) {
		return false
	}
	if f.statusClass != "" && r.StatusClass != "" && r.StatusClass != f.statusClass {
		return false
	}
	if f.qtype != "" && !strings.EqualFold(f.qtype, r.QType) {
		return false
	}
	for _, p := range [][2]string{{f.upstream, r.Upstream}, {f.list, r.List}, {f.zone, r.Zone}} {
		if p[0] != "" && p[0] != p[1] {
			return false
		}
	}
	return true
}

// logLine formats one row with fixed widths so live and paged output line
// up without buffering.
func logLine(r logRow, wide bool) string {
	client := r.ClientIP
	if r.ClientName != "" {
		client = r.ClientName + "(" + r.ClientIP + ")"
	}
	detail := r.Upstream
	switch {
	case r.List != "" && r.Rule != "":
		detail = r.List + ": " + r.Rule
	case r.List != "":
		detail = r.List
	case r.Zone != "" && detail == "":
		detail = "zone:" + r.Zone
	}
	ms := "-"
	if r.TimeMS > 0 {
		ms = strconv.FormatFloat(r.TimeMS, 'f', 1, 64)
	}
	line := fmt.Sprintf("%-19s  %-21s  %-5s  %-40s  %-18s  %-8s  %6s  %s",
		formatTime(r.TS), client, firstNonEmpty(r.QType, "-"), r.Domain, firstNonEmpty(r.Status, "-"), firstNonEmpty(r.Rcode, "-"), ms, firstNonEmpty(detail, "-"))
	if wide {
		line += fmt.Sprintf("  %s %s %s", firstNonEmpty(r.Node, "-"), firstNonEmpty(r.Proto, "-"), firstNonEmpty(r.Answer, "-"))
	}
	return strings.TrimRight(line, " ")
}

func logHeader(wide bool) string {
	line := fmt.Sprintf("%-19s  %-21s  %-5s  %-40s  %-18s  %-8s  %6s  %s", "TIME", "CLIENT", "TYPE", "DOMAIN", "STATUS", "RCODE", "MS", "UPSTREAM/LIST")
	if wide {
		line += "  NODE PROTO ANSWER"
	}
	return line
}

// queryPage is GET /queries.
type queryPage struct {
	Queries         []json.RawMessage `json:"queries"`
	CursorNext      string            `json:"cursor_next"`
	RecordsTotal    int64             `json:"records_total"`
	RecordsFiltered int64             `json:"records_filtered"`
}

// newLogsCmd is `kudurru logs`: query log pages and the live stream.
func newLogsCmd(o *cliOpts) *cobra.Command {
	var (
		f        logFilter
		follow   bool
		limit    int
		noHeader bool
	)
	cmd := &cobra.Command{
		Use:     "logs",
		Aliases: []string{"log", "querylog"},
		Short:   "Show the query log; --follow streams new queries",
		Example: `  kudurru logs --client 192.168.1.23 --limit 100
  kudurru logs --domain '*.doubleclick.net' --status-class blocked
  kudurru logs -f --status blocked_list`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			outFmt, err := o.format()
			if err != nil {
				return err
			}
			q, err := f.query(o.now())
			if err != nil {
				return err
			}
			c, _, err := o.client()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			wide := outFmt == cli.FormatWide
			emit := func(raw json.RawMessage, r logRow) error {
				if outFmt.Structured() {
					// One JSON object per line (ndjson) for both formats so
					// the output can be piped while following.
					b, err := json.Marshal(raw)
					if err != nil {
						return err
					}
					_, err = fmt.Fprintf(w, "%s\n", b)
					return err
				}
				_, err := fmt.Fprintln(w, logLine(r, wide))
				return err
			}
			if !outFmt.Structured() && !noHeader {
				_, _ = fmt.Fprintln(w, logHeader(wide))
			}
			var lastID uint64
			if limit > 0 {
				rows, raws, err := fetchLogs(cmd.Context(), c, q, limit)
				if err != nil {
					return err
				}
				// Pages come newest first; print oldest first like tail.
				for i := len(rows) - 1; i >= 0; i-- {
					if err := emit(raws[i], rows[i]); err != nil {
						return err
					}
					lastID = max(lastID, rows[i].ID)
				}
			}
			if !follow {
				return nil
			}
			// Ctrl-C ends the stream with exit status 0.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			opts := apiclient.SubscribeOptions{Reconnect: true}
			if lastID > 0 {
				opts.LastEventID = strconv.FormatUint(lastID, 10)
			}
			return c.Subscribe(ctx, "/queries/stream", q, opts, func(ev apiclient.Event) error {
				// The contract does not name the event type; anything that
				// decodes to a row with a domain is a query, the rest
				// (notices, lag reports) is skipped.
				var r logRow
				if err := json.Unmarshal(ev.Data, &r); err != nil || r.Domain == "" {
					return nil //nolint:nilerr // not a query row
				}
				if r.ID != 0 && r.ID <= lastID {
					return nil // already printed from the page
				}
				if !f.match(r) {
					return nil
				}
				return emit(ev.Data, r)
			})
		},
	}
	fl := cmd.Flags()
	fl.BoolVarP(&follow, "follow", "f", false, "stream new queries (SSE) after the last page")
	fl.IntVar(&limit, "limit", 50, "number of past queries to show (0 with --follow: only new ones)")
	fl.BoolVar(&noHeader, "no-headers", false, "omit the header line")
	fl.StringVar(&f.client, "client", "", "client IP, CIDR or name (* wildcards)")
	fl.StringVar(&f.domain, "domain", "", "domain, exact or with * wildcards")
	fl.StringSliceVar(&f.status, "status", nil, "statuses to keep (comma-separated), e.g. blocked_list,cached")
	fl.StringVar(&f.statusClass, "status-class", "", "status class: permitted, blocked, cached, error, ...")
	fl.StringVar(&f.qtype, "qtype", "", "query type, e.g. AAAA")
	fl.StringVar(&f.upstream, "upstream", "", "upstream name")
	fl.StringVar(&f.list, "list", "", "list name")
	fl.StringVar(&f.zone, "zone", "", "zone name")
	fl.StringVar(&f.since, "since", "", "start: RFC 3339 or a duration back from now (1h)")
	fl.StringVar(&f.until, "until", "", "end: RFC 3339 or a duration back from now")
	return cmd
}

// fetchLogs follows cursor_next until limit rows are collected.
func fetchLogs(ctx context.Context, c *apiclient.Client, q url.Values, limit int) ([]logRow, []json.RawMessage, error) {
	var (
		rows []logRow
		raws []json.RawMessage
	)
	q = cloneQuery(q)
	for len(rows) < limit {
		q.Set("limit", strconv.Itoa(min(limit-len(rows), 1000)))
		var page queryPage
		if err := c.Get(ctx, "/queries", q, &page); err != nil {
			return nil, nil, err
		}
		for _, raw := range page.Queries {
			var r logRow
			if err := json.Unmarshal(raw, &r); err != nil {
				return nil, nil, fmt.Errorf("decode query row: %w", err)
			}
			rows, raws = append(rows, r), append(raws, raw)
		}
		if page.CursorNext == "" || len(page.Queries) == 0 || page.CursorNext == q.Get("cursor") {
			break
		}
		q.Set("cursor", page.CursorNext)
	}
	if len(rows) > limit {
		rows, raws = rows[:limit], raws[:limit]
	}
	return rows, raws, nil
}

func cloneQuery(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// nodeInfo is one item of GET /nodes.
type nodeInfo struct {
	Name     string   `json:"name"`
	Address  string   `json:"address"`
	Self     bool     `json:"self"`
	Leader   bool     `json:"leader"`
	Ready    any      `json:"ready"`
	Unready  []string `json:"unready"`
	Version  string   `json:"version"`
	State    string   `json:"state"`
	Roles    []string `json:"roles"`
	QPS      *float64 `json:"qps"`
	Snapshot string   `json:"snapshotAge"`
}

func newNodesCmd(o *cliOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "nodes",
		Aliases: []string{"node"},
		Short:   "List the cluster's nodes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := c.Get(cmd.Context(), "/nodes", nil, &raw); err != nil {
				return err
			}
			items, err := rawItems(raw)
			if err != nil {
				var env struct {
					Nodes []json.RawMessage `json:"nodes"`
				}
				if json.Unmarshal(raw, &env) != nil {
					return err
				}
				items = env.Nodes
			}
			var nodes []nodeInfo
			for _, it := range items {
				var n nodeInfo
				if err := json.Unmarshal(it, &n); err != nil {
					return err
				}
				nodes = append(nodes, n)
			}
			return o.render(cmd, raw, func(wide bool) (*cli.Table, error) {
				t := &cli.Table{Headers: []string{"NAME", "READY", "LEADER", "STATE", "VERSION"}}
				if wide {
					t.Headers = append(t.Headers, "ADDRESS", "SELF", "ROLES", "QPS", "SNAPSHOT-AGE", "UNREADY")
				}
				for _, n := range nodes {
					row := []string{n.Name, readyString(n.Ready), strconv.FormatBool(n.Leader), firstNonEmpty(n.State, "-"), firstNonEmpty(n.Version, "-")}
					if wide {
						qps := "-"
						if n.QPS != nil {
							qps = strconv.FormatFloat(*n.QPS, 'f', 1, 64)
						}
						row = append(row, firstNonEmpty(n.Address, "-"), strconv.FormatBool(n.Self), firstNonEmpty(strings.Join(n.Roles, ","), "-"), qps,
							firstNonEmpty(n.Snapshot, "-"), firstNonEmpty(strings.Join(n.Unready, ","), "-"))
					}
					t.Add(row...)
				}
				return t, nil
			})
		},
	}
}

// auditEntry is one item of GET /audit: {ts, actor:{kind,id,name},
// action, resource:{kind,namespace,name}, result, authz}. Flat string
// forms (actor "admin", top-level kind/name, verb, outcome) are accepted.
type auditEntry struct {
	TS        time.Time       `json:"ts"`
	Time      time.Time       `json:"time"`
	Actor     json.RawMessage `json:"actor"`
	Verb      string          `json:"verb"`
	Action    string          `json:"action"`
	Resource  json.RawMessage `json:"resource"`
	Kind      string          `json:"kind"`
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Outcome   string          `json:"outcome"`
	Result    string          `json:"result"`
	Authz     string          `json:"authz"`
}

func (e auditEntry) actor() string {
	var s string
	if json.Unmarshal(e.Actor, &s) == nil {
		return s
	}
	var a struct{ Kind, ID, Name string }
	if json.Unmarshal(e.Actor, &a) == nil {
		return firstNonEmpty(a.Name, a.ID)
	}
	return ""
}

func (e auditEntry) object() string {
	var r struct{ Kind, Namespace, Name string }
	if json.Unmarshal(e.Resource, &r) == nil && r.Kind != "" {
		if r.Name == "" {
			return strings.ToLower(r.Kind)
		}
		return cli.ObjectRef(r.Kind, r.Namespace, r.Name)
	}
	if e.Name != "" {
		return cli.ObjectRef(e.Kind, e.Namespace, e.Name)
	}
	return "-"
}

func newAuditCmd(o *cliOpts) *cobra.Command {
	var (
		since, kind, name, actor string
		limit                    int
	)
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show the audit log of API writes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := o.client()
			if err != nil {
				return err
			}
			q := url.Values{}
			if since != "" {
				t, err := parseSince(since, o.now())
				if err != nil {
					return fmt.Errorf("--since: %w", err)
				}
				q.Set("since", t.UTC().Format(time.RFC3339))
			}
			for k, v := range map[string]string{"kind": kind, "name": name, "actor": actor} {
				if v != "" {
					q.Set(k, v)
				}
			}
			var all []json.RawMessage
			for len(all) < limit {
				q.Set("limit", strconv.Itoa(min(limit-len(all), 500)))
				var page struct {
					Items      []json.RawMessage `json:"items"`
					Entries    []json.RawMessage `json:"entries"`
					Continue   string            `json:"continue"`
					CursorNext string            `json:"cursor_next"`
				}
				if err := c.Get(cmd.Context(), "/audit", q, &page); err != nil {
					return err
				}
				got := append(page.Items, page.Entries...)
				all = append(all, got...)
				next := firstNonEmpty(page.CursorNext, page.Continue)
				if next == "" || len(got) == 0 || next == q.Get("cursor") {
					break
				}
				q.Set("cursor", next)
			}
			if len(all) > limit {
				all = all[:limit]
			}
			return o.render(cmd, all, func(bool) (*cli.Table, error) {
				t := &cli.Table{Headers: []string{"TIME", "ACTOR", "ACTION", "OBJECT", "OUTCOME"}}
				for _, raw := range all {
					var e auditEntry
					if err := json.Unmarshal(raw, &e); err != nil {
						return nil, err
					}
					ts := e.TS
					if ts.IsZero() {
						ts = e.Time
					}
					t.Add(formatTime(ts), firstNonEmpty(e.actor(), "-"), firstNonEmpty(e.Action, e.Verb, "-"), e.object(), firstNonEmpty(e.Result, e.Outcome, "-"))
				}
				return t, nil
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "RFC 3339 time or a duration back from now (24h)")
	cmd.Flags().StringVar(&kind, "kind", "", "object kind")
	cmd.Flags().StringVar(&name, "name", "", "object name")
	cmd.Flags().StringVar(&actor, "actor", "", "principal name")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum entries")
	return cmd
}
