package deploy_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The Grafana dashboard (dev-docs/design-record.md §11.6) is generated from the
// panel list below so that rows stay consistent and every query is checked
// against the metrics the binary registers. Regenerate with
//
//	go test ./deploy -run TestGrafanaDashboard -update
//
// (or `make dashboards`), which also refreshes the chart's copy.

const (
	dashboardPath      = "deploy/grafana/kudurru-overview.json"
	chartDashboardPath = "deploy/helm/kudurru/files/kudurru-overview.json"
	sel                = `instance=~"$instance"`
	rate               = "[$__rate_interval]"
)

// panel is the compact description of one dashboard panel.
type panel struct {
	title, kind, unit, desc string
	w, h                    int
	targets                 []target
	text                    string // kind "text"
	min, max                *float64
}

type target struct {
	expr, legend string
}

type row struct {
	title  string
	panels []panel
}

func f(v float64) *float64 { return &v }

func ts(title, unit string, targets ...target) panel {
	return panel{title: title, kind: "timeseries", unit: unit, w: 8, h: 8, targets: targets}
}

func stat(title, unit, expr string) panel {
	return panel{title: title, kind: "stat", unit: unit, w: 4, h: 4, targets: []target{{expr: expr}}}
}

func note(title, text string) panel {
	return panel{title: title, kind: "text", w: 24, h: 3, text: text}
}

func dashboardRows() []row {
	return []row{
		{"Overview", []panel{
			stat("Queries per second", "reqps", `sum(rate(kudurru_dns_requests_total{`+sel+`}`+rate+`))`),
			stat("Blocked", "percentunit", `sum(rate(kudurru_dns_responses_total{`+sel+`,status=~"blocked_.*"}`+rate+`)) / sum(rate(kudurru_dns_responses_total{`+sel+`}`+rate+`))`),
			stat("Cache hit ratio", "percentunit", `sum(rate(kudurru_cache_hits_total{`+sel+`}`+rate+`)) / sum(rate(kudurru_cache_requests_total{`+sel+`}`+rate+`))`),
			stat("SERVFAIL ratio", "percentunit", `sum(rate(kudurru_dns_responses_total{`+sel+`,rcode="SERVFAIL"}`+rate+`)) / sum(rate(kudurru_dns_responses_total{`+sel+`}`+rate+`))`),
			stat("p95 latency", "s", `histogram_quantile(0.95, sum by (le) (rate(kudurru_dns_request_duration_seconds_bucket{`+sel+`}`+rate+`)))`),
			stat("Nodes", "none", `count(kudurru_build_info{`+sel+`})`),
		}},
		{"Traffic", []panel{
			ts("Requests by transport", "reqps", target{`sum by (proto) (rate(kudurru_dns_requests_total{` + sel + `}` + rate + `))`, "{{proto}}"}),
			ts("Requests by query type (top 10)", "reqps", target{`topk(10, sum by (qtype) (rate(kudurru_dns_requests_total{` + sel + `}` + rate + `)))`, "{{qtype}}"}),
			ts("Responses by rcode", "reqps", target{`sum by (rcode) (rate(kudurru_dns_responses_total{` + sel + `}` + rate + `))`, "{{rcode}}"}),
			ts("Responses by status", "reqps", target{`sum by (status) (rate(kudurru_dns_responses_total{` + sel + `}` + rate + `))`, "{{status}}"}),
			ts("Requests per node", "reqps", target{`sum by (instance) (rate(kudurru_dns_requests_total{` + sel + `}` + rate + `))`, "{{instance}}"}),
			ts("In-flight requests", "short", target{`sum by (proto) (kudurru_dns_inflight_requests{` + sel + `})`, "{{proto}}"}),
		}},
		{"Latency", []panel{
			{title: "Query latency", kind: "heatmap", unit: "s", w: 12, h: 8, targets: []target{{`sum by (le) (rate(kudurru_dns_request_duration_seconds_bucket{` + sel + `}` + rate + `))`, "{{le}}"}}},
			{title: "Latency quantiles by status class", kind: "timeseries", unit: "s", w: 12, h: 8, targets: []target{
				{`histogram_quantile(0.5, sum by (le, status_class) (rate(kudurru_dns_request_duration_seconds_bucket{` + sel + `}` + rate + `)))`, "p50 {{status_class}}"},
				{`histogram_quantile(0.95, sum by (le, status_class) (rate(kudurru_dns_request_duration_seconds_bucket{` + sel + `}` + rate + `)))`, "p95 {{status_class}}"},
				{`histogram_quantile(0.99, sum by (le, status_class) (rate(kudurru_dns_request_duration_seconds_bucket{` + sel + `}` + rate + `)))`, "p99 {{status_class}}"},
			}},
		}},
		{"Blocking", []panel{
			ts("Block decisions", "reqps", target{`sum by (action, source) (rate(kudurru_block_decisions_total{` + sel + `}` + rate + `))`, "{{action}} {{source}}"}),
			ts("Hits by list (top 10)", "reqps", target{`topk(10, sum by (list) (rate(kudurru_block_list_hits_total{` + sel + `}` + rate + `)))`, "{{list}}"}),
			ts("Entries by list", "short", target{`max by (list) (kudurru_blocklist_entries{` + sel + `})`, "{{list}}"}),
			ts("Time since last list update", "s", target{`time() - max by (list) (kudurru_blocklist_last_update_timestamp_seconds{` + sel + `})`, "{{list}}"}),
			ts("Stale lists and update failures", "short",
				target{`max by (list) (kudurru_blocklist_stale{` + sel + `})`, "stale {{list}}"},
				target{`sum by (list) (increase(kudurru_blocklist_update_failures_total{` + sel + `}[1h]))`, "failures/h {{list}}"}),
			ts("Blocking enabled", "short", target{`min by (instance) (kudurru_blocking_enabled{` + sel + `})`, "{{instance}}"}),
		}},
		{"Cache", []panel{
			ts("Hit ratio", "percentunit", target{`sum by (instance) (rate(kudurru_cache_hits_total{` + sel + `}` + rate + `)) / sum by (instance) (rate(kudurru_cache_requests_total{` + sel + `}` + rate + `))`, "{{instance}}"}),
			ts("Hits by kind", "reqps", target{`sum by (kind, stale) (rate(kudurru_cache_hits_total{` + sel + `}` + rate + `))`, "{{kind}} stale={{stale}}"}),
			ts("Entries", "short", target{`sum by (kind) (kudurru_cache_entries{` + sel + `})`, "{{kind}}"}),
			ts("Evictions", "ops", target{`sum by (reason) (rate(kudurru_cache_evictions_total{` + sel + `}` + rate + `))`, "{{reason}}"}),
			ts("Prefetches", "ops", target{`sum(rate(kudurru_cache_prefetches_total{` + sel + `}` + rate + `))`, "prefetch"}),
		}},
		{"Upstreams", []panel{
			ts("Requests by upstream", "reqps", target{`sum by (upstream) (rate(kudurru_upstream_requests_total{` + sel + `}` + rate + `))`, "{{upstream}}"}),
			ts("Upstream p95 latency", "s", target{`histogram_quantile(0.95, sum by (le, upstream) (rate(kudurru_upstream_request_duration_seconds_bucket{` + sel + `}` + rate + `)))`, "{{upstream}}"}),
			ts("Errors by upstream", "ops", target{`sum by (upstream, error) (rate(kudurru_upstream_errors_total{` + sel + `}` + rate + `))`, "{{upstream}} {{error}}"}),
			ts("Responses by rcode", "reqps", target{`sum by (upstream, rcode) (rate(kudurru_upstream_responses_total{` + sel + `}` + rate + `))`, "{{upstream}} {{rcode}}"}),
			{title: "Healthy", kind: "timeseries", unit: "short", w: 8, h: 8, min: f(0), max: f(1), targets: []target{{`min by (upstream) (kudurru_upstream_healthy{` + sel + `})`, "{{upstream}}"}}},
			ts("In flight", "short", target{`sum by (upstream) (kudurru_upstream_inflight{` + sel + `})`, "{{upstream}}"}),
		}},
		{"Zones", []panel{
			ts("Zone transfers", "ops", target{`sum by (zone, direction, result) (rate(kudurru_zone_transfers_total{` + sel + `}` + rate + `))`, "{{zone}} {{direction}} {{result}}"}),
			ts("NOTIFY out", "ops", target{`sum by (zone, result) (rate(kudurru_zone_notify_total{` + sel + `}` + rate + `))`, "{{zone}} {{result}}"}),
			ts("Objects in the zone cache", "short", target{`max by (kind) (kudurru_zonecache_objects{` + sel + `})`, "{{kind}}"}),
			ts("Zone cache rebuilds", "ops",
				target{`sum by (instance) (rate(kudurru_zonecache_rebuilds_total{` + sel + `}` + rate + `))`, "rebuilds {{instance}}"},
				target{`sum by (builder) (rate(kudurru_zonecache_build_errors_total{` + sel + `}` + rate + `))`, "errors {{builder}}"}),
			ts("Rebuild p95", "s", target{`histogram_quantile(0.95, sum by (le) (rate(kudurru_zonecache_rebuild_duration_seconds_bucket{` + sel + `}` + rate + `)))`, "p95"}),
			ts("TSIG failures", "ops", target{`sum by (error) (rate(kudurru_dns_tsig_failures_total{` + sel + `}` + rate + `))`, "{{error}}"}),
			{title: "Serial divergence", kind: "timeseries", unit: "short", w: 12, h: 8, min: f(0),
				desc:    "Highest minus lowest SOA serial served for each zone across the selected nodes. Non-zero for longer than a zone cache rebuild means a node serves an old copy (KudurruSerialDivergence).",
				targets: []target{{`max by (zone) (kudurru_zone_serial{` + sel + `}) - min by (zone) (kudurru_zone_serial{` + sel + `})`, "{{zone}}"}}},
			{title: "Zone size", kind: "timeseries", unit: "short", w: 12, h: 8, targets: []target{{`max by (zone, kind) (kudurru_zone_records{` + sel + `})`, "{{zone}} {{kind}}"}}},
		}},
		{"Pipeline and query log", []panel{
			ts("Dropped queries", "ops", target{`sum by (reason) (rate(kudurru_dns_dropped_total{` + sel + `}` + rate + `))`, "{{reason}}"}),
			ts("Rate-limit buckets", "short", target{`sum by (instance) (kudurru_ratelimit_buckets{` + sel + `})`, "{{instance}}"}),
			ts("Query log events", "ops",
				target{`sum by (instance) (rate(kudurru_querylog_events_total{` + sel + `}` + rate + `))`, "events {{instance}}"},
				target{`sum by (stage, sink) (rate(kudurru_querylog_dropped_total{` + sel + `}` + rate + `))`, "dropped {{stage}} {{sink}}"}),
			ts("querylog.db size", "bytes",
				target{`max by (instance) (kudurru_querylog_db_bytes{` + sel + `})`, "db {{instance}}"},
				target{`max by (instance) (kudurru_querylog_wal_bytes{` + sel + `})`, "wal {{instance}}"}),
			ts("Query log rows", "short", target{`max by (instance) (kudurru_querylog_rows{` + sel + `})`, "{{instance}}"}),
			ts("Query log write p95", "s", target{`histogram_quantile(0.95, sum by (le) (rate(kudurru_querylog_write_duration_seconds_bucket{` + sel + `}` + rate + `)))`, "p95"}),
		}},
		{"Central database and audit", []panel{
			note("Central database metrics", "Central availability and shipping panels have no data on nodes without database.dsn. A zero availability value means a configured database is unreachable or incompatible; DNS continues locally. Query shipping lag is sampled when a batch ships and may have no data before the first shipment or when facts are disabled. Audit spool drops mean older events were discarded at the local cap."),
			{title: "Central database available", kind: "timeseries", unit: "short", w: 8, h: 8, min: f(0), max: f(1),
				desc:    "1 means reachable and migrated; 0 means unavailable. Missing series are not treated as database failures.",
				targets: []target{{`min by (instance) (kudurru_central_up{` + sel + `})`, "{{instance}}"}}},
			ts("Query shipping lag", "s", target{`max by (instance) (kudurru_central_ship_lag_seconds{` + sel + `,stream="query"})`, "{{instance}}"}),
			ts("Audit spool drops", "ops", target{`sum by (instance) (rate(kudurru_audit_spool_dropped_total{` + sel + `}` + rate + `))`, "{{instance}}"}),
		}},
		{"Cluster and store", []panel{
			ts("Leader", "short", target{`max by (instance, role) (kudurru_leader{` + sel + `})`, "{{instance}} {{role}}"}),
			ts("Active runnables", "short", target{`sum by (instance) (kudurru_role_active{` + sel + `})`, "{{instance}}"}),
			ts("Serving from snapshot / snapshot age", "short",
				target{`max by (instance) (kudurru_store_snapshot_serving{` + sel + `})`, "serving {{instance}}"},
				target{`max by (instance) (kudurru_store_snapshot_age_seconds{` + sel + `})`, "age s {{instance}}"}),
			ts("Store operations", "ops", target{`sum by (op, result) (rate(kudurru_store_operations_total{` + sel + `}` + rate + `))`, "{{op}} {{result}}"}),
			ts("Store operation p95", "s", target{`histogram_quantile(0.95, sum by (le, op) (rate(kudurru_store_operation_duration_seconds_bucket{` + sel + `}` + rate + `)))`, "{{op}}"}),
			ts("Controller reconciles", "ops", target{`sum by (kind, result) (rate(kudurru_controller_reconciles_total{` + sel + `}` + rate + `))`, "{{kind}} {{result}}"}),
			ts("Internal listener", "reqps", target{`sum by (route, code) (rate(kudurru_internal_requests_total{` + sel + `}` + rate + `))`, "{{route}} {{code}}"}),
			ts("Blocklist pulls", "ops", target{`sum by (origin, result) (rate(kudurru_blocklist_pulls_total{` + sel + `}` + rate + `))`, "{{origin}} {{result}}"}),
			ts("Object file diagnoses", "short", target{`max by (severity) (kudurru_objects_diagnoses{` + sel + `})`, "{{severity}}"}),
			{title: "SQLite authority fenced", kind: "timeseries", unit: "short", w: 8, h: 8, min: f(0), max: f(1),
				desc:    "1 means an authority fence, promotion conflict, divergent image or failed durable fence blocks writes. Healthy followers report 0; zero is not writable readiness. Valid last-good DNS can continue. Kubernetes nodes do not emit this metric.",
				targets: []target{{`max by (instance) (kudurru_cluster_fenced{` + sel + `})`, "{{instance}}"}}},
		}},
		{"dnsdist edge", []panel{
			ts("Edge queries and responses", "reqps",
				target{`sum(rate(dnsdist_queries[$__rate_interval]))`, "queries"},
				target{`sum(rate(dnsdist_responses[$__rate_interval]))`, "responses"}),
			ts("Edge packet cache", "ops",
				target{`sum(rate(dnsdist_cache_hits[$__rate_interval]))`, "hits"},
				target{`sum(rate(dnsdist_cache_misses[$__rate_interval]))`, "misses"}),
			ts("Queries per backend", "reqps", target{`sum by (server) (rate(dnsdist_server_queries[$__rate_interval]))`, "{{server}}"}),
			{title: "kudurru backends up (edge supervisor)", kind: "timeseries", unit: "short", w: 8, h: 8, min: f(0), max: f(1), targets: []target{{`min by (backend) (kudurru_edge_backend_up)`, "{{backend}}"}}},
			ts("Edge renders and dnsdist restarts", "ops",
				target{`sum by (result) (rate(kudurru_edge_renders_total` + rate + `))`, "render {{result}}"},
				target{`sum by (reason) (rate(kudurru_edge_dnsdist_restarts_total` + rate + `))`, "restart {{reason}}"}),
		}},
		{"VIP", []panel{
			{title: "VIP masters", kind: "timeseries", unit: "short", w: 8, h: 8, min: f(0),
				desc:    "Nodes holding each VIP as VRRP MASTER (kudurru_vip_master from `kudurru vip run`). Exactly 1 is healthy; 0 means nobody answers on the VIP, 2 or more is a split brain.",
				targets: []target{{`sum by (vip) (kudurru_vip_master)`, "{{vip}}"}}},
			ts("VRRP priority", "short", target{`max by (instance, vip) (kudurru_vip_priority)`, "{{instance}} {{vip}}"}),
			ts("keepalived renders", "ops", target{`sum by (result) (rate(kudurru_vip_reloads_total` + rate + `))`, "{{result}}"}),
			note("Other VIP providers", "These panels come from the keepalived supervisor's metrics listener (:8085). With kube-vip or MetalLB, ownership is visible in the Service status and in those projects' own metrics."),
		}},
	}
}

// buildDashboard renders the rows into the Grafana JSON model.
func buildDashboard() map[string]any {
	ds := map[string]any{"type": "prometheus", "uid": "${datasource}"}
	var panels []any
	id, y := 1, 0
	for _, r := range dashboardRows() {
		panels = append(panels, map[string]any{
			"type": "row", "id": id, "title": r.title, "collapsed": false,
			"gridPos": map[string]any{"h": 1, "w": 24, "x": 0, "y": y},
			"panels":  []any{},
		})
		id++
		y++
		x, rowH := 0, 0
		for _, p := range r.panels {
			if x+p.w > 24 {
				x, y = 0, y+rowH
				rowH = 0
			}
			out := map[string]any{
				"type": p.kind, "id": id, "title": p.title,
				"gridPos": map[string]any{"h": p.h, "w": p.w, "x": x, "y": y},
			}
			id++
			if p.kind == "text" {
				out["options"] = map[string]any{"mode": "markdown", "content": p.text}
			} else {
				out["datasource"] = ds
				defaults := map[string]any{"unit": p.unit}
				if p.min != nil {
					defaults["min"] = *p.min
				}
				if p.max != nil {
					defaults["max"] = *p.max
				}
				out["fieldConfig"] = map[string]any{"defaults": defaults, "overrides": []any{}}
				var targets []any
				for i, tg := range p.targets {
					t := map[string]any{"datasource": ds, "expr": tg.expr, "refId": string(rune('A' + i))}
					if tg.legend != "" {
						t["legendFormat"] = tg.legend
					}
					if p.kind == "heatmap" {
						t["format"] = "heatmap"
					}
					targets = append(targets, t)
				}
				out["targets"] = targets
				switch p.kind {
				case "stat":
					out["options"] = map[string]any{"reduceOptions": map[string]any{"calcs": []any{"lastNotNull"}, "fields": "", "values": false}, "colorMode": "value", "graphMode": "area"}
				case "heatmap":
					out["options"] = map[string]any{"calculate": false, "yAxis": map[string]any{"unit": p.unit}, "color": map[string]any{"scheme": "Oranges", "mode": "scheme"}}
				default:
					out["options"] = map[string]any{"legend": map[string]any{"displayMode": "list", "placement": "bottom", "showLegend": true}, "tooltip": map[string]any{"mode": "multi", "sort": "desc"}}
				}
			}
			if p.desc != "" {
				out["description"] = p.desc
			}
			panels = append(panels, out)
			x += p.w
			if p.h > rowH {
				rowH = p.h
			}
		}
		y += rowH
	}
	return map[string]any{
		"title":         "kudurru overview",
		"uid":           "kudurru-overview",
		"description":   "kudurru DNS server (dev-docs/design-record.md §11.6). Generated by deploy/dashboard_test.go.",
		"tags":          []any{"kudurru", "dns"},
		"timezone":      "browser",
		"schemaVersion": 39,
		"version":       1,
		"editable":      true,
		"refresh":       "30s",
		"time":          map[string]any{"from": "now-6h", "to": "now"},
		"links":         []any{},
		"annotations":   map[string]any{"list": []any{}},
		"templating": map[string]any{"list": []any{
			map[string]any{"name": "datasource", "label": "Data source", "type": "datasource", "query": "prometheus", "current": map[string]any{}, "hide": 0},
			map[string]any{
				"name": "instance", "label": "Node", "type": "query", "datasource": ds,
				"query":   map[string]any{"query": "label_values(kudurru_build_info, instance)", "refId": "instance"},
				"refresh": 2, "multi": true, "includeAll": true, "allValue": ".*", "current": map[string]any{}, "hide": 0, "sort": 1,
			},
		}},
		"panels": panels,
	}
}

func TestGrafanaDashboard(t *testing.T) {
	root := repoRoot(t)
	b, err := json.MarshalIndent(buildDashboard(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	regen := "go test ./deploy -run TestGrafanaDashboard -update"
	writeOrCompare(t, filepath.Join(root, dashboardPath), b, regen)
	writeOrCompare(t, filepath.Join(root, chartDashboardPath), b, regen)
}

// TestDashboardMetricsExist checks the committed JSON itself: it parses,
// panel ids are unique, and every kudurru_* metric it queries is registered
// by the binary. dnsdist_* series come from dnsdist's own exporter.
func TestDashboardMetricsExist(t *testing.T) {
	b := readRepoFile(t, dashboardPath)
	var dash map[string]any
	if err := json.Unmarshal(b, &dash); err != nil {
		t.Fatalf("dashboard JSON does not parse: %v", err)
	}
	ids := map[float64]string{}
	exprs := 0
	rows := map[string]bool{}
	for _, p := range list(dash["panels"]) {
		pm := mapOf(p)
		id, _ := pm["id"].(float64)
		if prev, dup := ids[id]; dup {
			t.Errorf("panel id %v used by %q and %q", id, prev, pm["title"])
		}
		ids[id] = str(pm["title"])
		if pm["type"] == "row" {
			rows[str(pm["title"])] = true
		}
		for _, tg := range list(pm["targets"]) {
			expr := str(mapOf(tg)["expr"])
			exprs++
			if strings.Count(expr, "(") != strings.Count(expr, ")") || strings.Count(expr, "{") != strings.Count(expr, "}") {
				t.Errorf("panel %q: unbalanced expression %s", pm["title"], expr)
			}
			if unknown := unknownMetrics(t, expr); len(unknown) > 0 {
				t.Errorf("panel %q references metrics the binary does not register: %v", pm["title"], unknown)
			}
		}
	}
	if exprs < 40 {
		t.Errorf("dashboard has %d queries; expected the full §11.6 set", exprs)
	}
	for _, want := range []string{"Overview", "Traffic", "Latency", "Blocking", "Cache", "Upstreams", "Zones", "Pipeline and query log", "Cluster and store", "dnsdist edge", "VIP"} {
		if !rows[want] {
			t.Errorf("dashboard lacks the %q row (§11.6)", want)
		}
	}
}

// TestPrometheusRules checks deploy/prometheus/rules.yaml: a PrometheusRule
// whose alerts are uniquely named, have severity labels and summaries, and
// only use metrics the binary registers; the chart copy is identical.
func TestPrometheusRules(t *testing.T) {
	root := repoRoot(t)
	b := readRepoFile(t, "deploy/prometheus/rules.yaml")
	docs := decodeDocs(t, b)
	if len(docs) != 1 || docs[0]["kind"] != "PrometheusRule" {
		t.Fatalf("rules.yaml must hold exactly one PrometheusRule")
	}
	seen := map[string]bool{}
	for _, g := range list(dig(docs[0], "spec", "groups")) {
		for _, r := range list(mapOf(g)["rules"]) {
			rm := mapOf(r)
			name := str(rm["alert"])
			if name == "" {
				t.Errorf("group %v: rule without alert name", mapOf(g)["name"])
				continue
			}
			if seen[name] {
				t.Errorf("alert %s defined twice", name)
			}
			seen[name] = true
			if str(dig(rm, "labels", "severity")) == "" {
				t.Errorf("alert %s has no severity label", name)
			}
			if str(dig(rm, "annotations", "summary")) == "" {
				t.Errorf("alert %s has no summary", name)
			}
			expr := str(rm["expr"])
			if unknown := unknownMetrics(t, expr); len(unknown) > 0 {
				t.Errorf("alert %s references metrics the binary does not register: %v", name, unknown)
			}
		}
	}
	for _, want := range []string{"KudurruClusterFenced", "KudurruNoLeader", "KudurruServfailRatio", "KudurruUpstreamUnhealthy", "KudurruQuerylogDrops", "KudurruQuerylogNearCap", "KudurruSerialDivergence", "KudurruServingFromSnapshot", "KudurruSnapshotStale", "KudurruLatencyP95High", "KudurruEdgeBackendDown", "KudurruVIPNoMaster", "KudurruBlocklistStale"} {
		if !seen[want] {
			t.Errorf("§11.6 alert %s is missing", want)
		}
	}
	writeOrCompare(t, filepath.Join(root, "deploy/helm/kudurru/files/prometheus-rules.yaml"), b, "go test ./deploy -run TestPrometheusRules -update")
}
