package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/sinktest"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// sinkUpstream starts the upstream every mounted sink test forwards to.
func sinkUpstream(t *testing.T, answer net.IP) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	up := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: answer}}
		_ = w.WriteMsg(a)
	})}
	go func() { _ = up.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = up.Shutdown() })
	return pc.LocalAddr().String()
}

// mountedMetric reads one sample from a mounted metrics listener.
func mountedMetric(t *testing.T, address, family string, labels ...string) (float64, bool) {
	t.Helper()
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + address + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
lines:
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, family+"{") {
			continue
		}
		for _, label := range labels {
			if !strings.Contains(line, label) {
				continue lines
			}
		}
		value, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
		if err != nil {
			t.Fatal("metrics line", line, err)
		}
		return value, true
	}
	return 0, false
}

// TestRunServeQuerylogClickHouseAndLokiSinks is the mounted proof of the
// HTTP sinks of D371. It runs the actual serve command on the complete
// SQLite runtime and drives it through its real surfaces: the settings
// command adds a ClickHouse and a Loki sink whose credentials are files the
// node's configuration grants; real UDP queries are answered; the rows are
// read back through GET /queries; the per-sink counters are read from the
// metrics listener. The destinations are local HTTP doubles: ClickHouse
// behind TLS with a private CA, Loki over plain HTTP with a tenant. They
// check the credential like the real servers and can answer "busy" or
// "rejected" on demand.
func TestRunServeQuerylogClickHouseAndLokiSinks(t *testing.T) {
	warehouse := sinktest.New(t, http.StatusOK, true)
	logs := sinktest.New(t, http.StatusNoContent, false)
	secrets := t.TempDir()
	passwordFile, caFile, tokenFile := filepath.Join(secrets, "clickhouse-password"), filepath.Join(secrets, "clickhouse-ca.pem"), filepath.Join(secrets, "loki-token")
	writeSecret(t, passwordFile, "mounted-clickhouse-password\n")
	writeSecret(t, caFile, string(warehouse.CertPEM()))
	writeSecret(t, tokenFile, "mounted-loki-token\n")

	var mu sync.Mutex
	password, busy, rejecting := "mounted-clickhouse-password", false, false
	warehouse.Answer(func(_ int, r sinktest.Request) sinktest.Answer {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Header.Get("Authorization") != sinktest.Basic("kudurru", password):
			return sinktest.Answer{Status: http.StatusForbidden, Header: http.Header{"X-ClickHouse-Exception-Code": {"516"}}}
		case busy:
			return sinktest.Answer{Status: http.StatusServiceUnavailable, Header: http.Header{"X-ClickHouse-Exception-Code": {"202"}}, Body: "Too many simultaneous queries"}
		}
		return sinktest.Answer{}
	})
	logs.Answer(func(_ int, r sinktest.Request) sinktest.Answer {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Header.Get("Authorization") != "Bearer mounted-loki-token" || r.Header.Get("X-Scope-OrgID") != "home":
			return sinktest.Answer{Status: http.StatusUnauthorized}
		case rejecting:
			return sinktest.Answer{Status: http.StatusBadRequest, Body: "entry too far behind"}
		}
		return sinktest.Answer{}
	})
	set := func(change func()) {
		mu.Lock()
		defer mu.Unlock()
		change()
	}

	upstream := sinkUpstream(t, net.IPv4(192, 0, 2, 92))
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.RateLimit.Enabled = false
	cfg.Querylog.Enabled = true
	cfg.Querylog.Exclude.SUDN = false
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{upstream}, Strategy: "strict", Timeout: config.Duration(4 * time.Second)}}
	// The node's operator delegates exactly these three files.
	cfg.Credentials.AllowedFiles = []string{passwordFile, caFile, tokenFile}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("sink mount shutdown", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("sink mount failed to join")
			}
		})
	}
	defer stop()
	wait := func(what string, fn func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !fn() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s\nClickHouse double:\n%sLoki double:\n%s", what, sinktest.Describe(warehouse.Requests()), sinktest.Describe(logs.Requests()))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	ready := func() bool { return offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") == http.StatusOK }
	wait("the mount to become ready", ready)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	mountedFactorRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	patch := func(querylogPatch map[string]any) {
		t.Helper()
		var result api.SettingsBody
		mountedFactorRequest(t, client, base, http.MethodPatch, "/settings", login.CSRF, map[string]any{"querylog": querylogPatch}, http.StatusOK, &result)
		if result.Persisted == nil || !*result.Persisted || len(result.RestartRequired) != 0 {
			t.Fatal("a querylog.sinks change was not a durable hot setting")
		}
		wait("the successor publication", ready)
	}
	// settingsBody is the raw answer of GET /settings.
	settingsBody := func() string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/settings", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("GET /settings: HTTP %d, %v", response.StatusCode, err)
		}
		return string(raw)
	}
	refused := func(what string, sinks []map[string]any) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"querylog": map[string]any{"sinks": sinks}})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, base+"/settings", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Kudurru-CSRF", login.CSRF)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode < 400 || response.StatusCode >= 500 {
			t.Fatalf("%s: HTTP %d, want a refusal", what, response.StatusCode)
		}
	}
	query := func(name string) {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		a, _, err := (&dns.Client{Net: "udp", Timeout: 5 * time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil || a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
			t.Fatalf("UDP query %s was not answered: %v", name, err)
		}
	}
	localRows := func(limit int) api.QueriesBody {
		t.Helper()
		var out api.QueriesBody
		mountedFactorRequest(t, client, base, http.MethodGet, "/queries?limit="+strconv.Itoa(limit), "", nil, http.StatusOK, &out)
		return out
	}
	metric := func(family string, labels ...string) float64 {
		t.Helper()
		value, _ := mountedMetric(t, cfg.Listeners.Metrics.Address, family, labels...)
		return value
	}
	sent := func(sink string) float64 { return metric("kudurru_sink_sent_total", `sink="`+sink+`"`) }
	dropped := func(sink string) float64 {
		return metric("kudurru_querylog_dropped_total", `sink="`+sink+`"`, `stage="sink"`)
	}
	failed := func(sink string) float64 { return metric("kudurru_sink_errors_total", `sink="`+sink+`"`) }
	// exported lists what each double received, as domain names in order.
	exportedRows := func() (rows []map[string]any) {
		t.Helper()
		for _, r := range warehouse.Requests() {
			rows = append(rows, sinktest.ClickHouseRows(t, r)...)
		}
		return rows
	}
	exportedLines := func() (lines []sinktest.LokiEntry) {
		t.Helper()
		for _, r := range logs.Requests() {
			for _, st := range sinktest.LokiStreams(t, r) {
				lines = append(lines, st.Entries...)
			}
		}
		return lines
	}

	total := 0
	query("before-any-sink.example.")
	total++
	wait("the first row", func() bool { return int(localRows(1).RecordsTotal) == total })
	if len(warehouse.Requests())+len(logs.Requests()) != 0 {
		t.Fatal("a destination was contacted before any sink was configured")
	}

	// The settings command adds both sinks. Credentials are references to
	// the granted files; no value enters the settings.
	sinks := []map[string]any{
		{"name": "warehouse", "type": "clickhouse", "batchSize": 2, "flushInterval": "200ms", "attempts": 2, "retryBackoff": "100ms", "clickhouse": map[string]any{
			"url": warehouse.URL, "database": "dns", "table": "query_log", "username": "kudurru",
			"passwordRef": map[string]any{"file": passwordFile}, "caRef": map[string]any{"file": caFile},
		}},
		{"name": "logs", "type": "loki", "batchSize": 2, "flushInterval": "200ms", "attempts": 2, "retryBackoff": "100ms", "loki": map[string]any{
			"url": logs.URL, "tenant": "home", "bearerTokenRef": map[string]any{"file": tokenFile}, "labels": map[string]any{"job": "dns", "site": "lab"},
		}},
	}
	patch(map[string]any{"sinks": sinks})
	for _, name := range []string{"q1.example.", "q2.example.", "q3.example."} {
		query(name)
	}
	total += 3
	wait("ClickHouse to receive the three queries", func() bool { return len(exportedRows()) == 3 })
	wait("Loki to receive the three queries", func() bool { return len(exportedLines()) == 3 })
	wait("the local rows", func() bool { return int(localRows(1).RecordsTotal) == total })

	// The ClickHouse requests: the statement, the credential over TLS, and
	// batches of at most two rows.
	for i, r := range warehouse.Requests() {
		if r.Method != http.MethodPost || r.Query.Get("query") != "INSERT INTO `dns`.`query_log` FORMAT JSONEachRow" || r.Query.Get("date_time_input_format") != "best_effort" {
			t.Fatalf("ClickHouse request %d: %s %v", i+1, r.Method, r.Query)
		}
		if !r.Compressed || r.Header.Get("Content-Type") != "application/x-ndjson" || !strings.HasPrefix(r.Header.Get("User-Agent"), "kudurru/") {
			t.Fatalf("ClickHouse request %d headers: %v", i+1, r.Header)
		}
		if n := len(sinktest.ClickHouseRows(t, r)); n < 1 || n > 2 {
			t.Fatalf("ClickHouse request %d carries %d rows with batchSize 2", i+1, n)
		}
	}
	// Every exported row is the query the local log holds under the same ID.
	local := map[uint64]sqlitelog.QueryRow{}
	for _, row := range localRows(10).Queries {
		local[row.ID] = row
	}
	var exportedDomains []string
	for _, row := range exportedRows() {
		id, err := strconv.ParseUint(row["id"].(json.Number).String(), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		want, ok := local[id]
		if !ok {
			t.Fatalf("ClickHouse row %v has no local row", row)
		}
		exportedDomains = append(exportedDomains, row["domain"].(string))
		if row["domain"] != want.Domain || row["node"] != "mount-test" || row["status"] != want.Status || row["status_class"] != want.StatusClass ||
			row["qtype"] != "A" || row["rcode"] != "NOERROR" || row["proto"] != "udp" || row["client"] != want.Client.IP || row["upstream"] != want.Upstream || row["answers"] != json.Number("1") {
			t.Fatalf("ClickHouse row %v does not match the local row %+v", row, want)
		}
		at, err := time.Parse(time.RFC3339Nano, row["ts"].(string))
		if err != nil || at.Sub(want.TS).Abs() > time.Millisecond {
			t.Fatalf("ClickHouse ts %v, local %s: %v", row["ts"], want.TS, err)
		}
	}
	if !reflect.DeepEqual(exportedDomains, []string{"q1.example", "q2.example", "q3.example"}) {
		t.Fatalf("ClickHouse received %v: only the queries answered after the sink was added, in order", exportedDomains)
	}
	// The Loki pushes: the tenant, the fixed labels, and per-query values
	// as structured metadata.
	for i, r := range logs.Requests() {
		if r.Path != "/loki/api/v1/push" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("Loki request %d: %s %v", i+1, r.Path, r.Header)
		}
		for _, st := range sinktest.LokiStreams(t, r) {
			if !reflect.DeepEqual(st.Labels, map[string]string{"job": "dns", "site": "lab", "node": "mount-test", "status_class": "permitted"}) {
				t.Fatalf("Loki stream labels: %v", st.Labels)
			}
		}
	}
	for i, entry := range exportedLines() {
		line := entry.LineJSON(t)
		wantDomain := []string{"q1.example", "q2.example", "q3.example"}[i]
		if line["domain"] != wantDomain || entry.Metadata["domain"] != wantDomain || entry.Metadata["client"] != "127.0.0.1" || entry.Metadata["status"] != "forwarded" || entry.Metadata["qtype"] != "A" {
			t.Fatalf("Loki entry %d: line %v, metadata %v", i, line, entry.Metadata)
		}
	}
	wait("the sent counters", func() bool { return sent("warehouse") == 3 && sent("logs") == 3 })
	if dropped("warehouse") != 0 || dropped("logs") != 0 || failed("warehouse") != 0 || failed("logs") != 0 {
		t.Fatal("healthy sinks counted a loss or an error")
	}
	if last, found := mountedMetric(t, cfg.Listeners.Metrics.Address, "kudurru_sink_last_success_timestamp_seconds", `sink="warehouse"`); !found || time.Since(time.Unix(int64(last), 0)) > time.Minute {
		t.Fatalf("kudurru_sink_last_success_timestamp_seconds{sink=warehouse} = %v, %v", last, found)
	}

	// GET /settings shows the references and nothing else.
	shown := settingsBody()
	if !strings.Contains(shown, passwordFile) || !strings.Contains(shown, `"bearerTokenRef"`) {
		t.Fatalf("the settings do not show the credential references")
	}
	for _, secret := range []string{"mounted-clickhouse-password", "mounted-loki-token", "BEGIN CERTIFICATE"} {
		if strings.Contains(shown, secret) {
			t.Fatalf("GET /settings repeats a credential value (%s)", secret)
		}
	}

	// ClickHouse answers 503: two attempts, then the batch is dropped and
	// counted for that sink. DNS, the local log and Loki carry on.
	set(func() { busy = true })
	before := len(warehouse.Requests())
	query("busy.example.")
	total++
	wait("the bounded attempts at ClickHouse", func() bool { return dropped("warehouse") == 1 })
	if got := len(warehouse.Requests()) - before; got != 2 {
		t.Fatalf("%d requests for a batch ClickHouse kept answering 503 to, want attempts = 2", got)
	}
	if failed("warehouse") != 2 || sent("warehouse") != 3 {
		t.Fatalf("after the dropped batch: errors %v, sent %v", failed("warehouse"), sent("warehouse"))
	}
	wait("Loki to receive the query ClickHouse dropped", func() bool { return sent("logs") == 4 })
	wait("the local row behind a failing exporter", func() bool { return int(localRows(1).RecordsTotal) == total })
	set(func() { busy = false })
	query("after-busy.example.")
	total++
	wait("ClickHouse to accept batches again", func() bool { return sent("warehouse") == 4 && sent("logs") == 5 })

	// Loki answers 400: one attempt, and the batch is dropped at once.
	set(func() { rejecting = true })
	before = len(logs.Requests())
	query("rejected.example.")
	total++
	wait("the refused batch to be counted", func() bool { return dropped("logs") == 1 })
	wait("ClickHouse to receive the query Loki refused", func() bool { return sent("warehouse") == 5 })
	time.Sleep(400 * time.Millisecond) // longer than the backoff: no second attempt follows
	if got := len(logs.Requests()) - before; got != 1 {
		t.Fatalf("%d requests for a batch Loki refused with 400, want 1", got)
	}
	if failed("logs") != 1 || sent("logs") != 5 {
		t.Fatalf("after the refused batch: errors %v, sent %v", failed("logs"), sent("logs"))
	}
	set(func() { rejecting = false })

	// The ClickHouse password is rotated: the server refuses the old one,
	// the granted file holds the new one, and the refused batch is sent
	// again. Nothing is lost and nothing is restarted.
	set(func() { password = "rotated-clickhouse-password" })
	writeSecret(t, passwordFile, "rotated-clickhouse-password\n")
	query("after-rotation.example.")
	total++
	wait("the batch after the rotation", func() bool { return sent("warehouse") == 6 && sent("logs") == 6 })
	if dropped("warehouse") != 1 || failed("warehouse") != 3 {
		t.Fatalf("after the rotation: dropped %v, errors %v; the one refused attempt must be the only new error", dropped("warehouse"), failed("warehouse"))
	}

	// A privacy change is another publication with the same entries. The
	// sinks then receive what the new mode keeps, and nothing else.
	patch(map[string]any{"privacy": "hide_domains"})
	query("private-name.example.")
	total++
	wait("the query after the privacy change", func() bool { return sent("warehouse") == 7 && sent("logs") == 7 })
	rows, lines := exportedRows(), exportedLines()
	if got := rows[len(rows)-1]; got["domain"] != querylog.HiddenName || got["client"] != "127.0.0.1" {
		t.Fatalf("ClickHouse row under hide_domains: %v", got)
	}
	if got := lines[len(lines)-1]; got.Metadata["domain"] != querylog.HiddenName || got.LineJSON(t)["domain"] != querylog.HiddenName {
		t.Fatalf("Loki entry under hide_domains: %v %s", got.Metadata, got.Line)
	}
	for _, r := range append(warehouse.Requests(), logs.Requests()...) {
		if strings.Contains(string(r.Body), "private-name") {
			t.Fatal("a hidden query name was exported")
		}
	}

	// Settings the server could not serve safely are refused and change
	// nothing: a credential in the URL, a Secret reference without a
	// cluster, settings of another type, a statement in the table name, a
	// label per query.
	withClickHouse := func(change map[string]any) []map[string]any {
		settings := map[string]any{"url": warehouse.URL, "username": "kudurru", "passwordRef": map[string]any{"file": passwordFile}}
		for k, v := range change {
			settings[k] = v
		}
		return []map[string]any{{"name": "warehouse", "type": "clickhouse", "clickhouse": settings}}
	}
	refused("a password in the URL", withClickHouse(map[string]any{"url": "https://kudurru:literal-secret-value@clickhouse.example:8443"}))               //nolint:gosec // G101: a made-up password the validator must refuse
	refused("a password in a URL of another scheme", withClickHouse(map[string]any{"url": "tcp://kudurru:literal-secret-value@clickhouse.example:9000"})) //nolint:gosec // G101: a made-up password the validator must refuse
	refused("a Secret reference on SQLite", withClickHouse(map[string]any{"passwordRef": map[string]any{"secretRef": map[string]any{"name": "export", "key": "password"}}}))
	refused("two reference forms", withClickHouse(map[string]any{"passwordRef": map[string]any{"file": passwordFile, "env": "PASSWORD"}}))
	refused("a statement in the table name", withClickHouse(map[string]any{"table": "q` FORMAT Null --"}))
	refused("loki settings on a clickhouse sink", []map[string]any{{"name": "warehouse", "type": "clickhouse", "loki": map[string]any{"url": logs.URL}}})
	refused("a clickhouse sink without settings", []map[string]any{{"name": "warehouse", "type": "clickhouse"}})
	refused("a label the sink sets per query", []map[string]any{{"name": "logs", "type": "loki", "loki": map[string]any{"url": logs.URL, "labels": map[string]any{"node": "x"}}}})
	refused("basic and bearer", []map[string]any{{"name": "logs", "type": "loki", "loki": map[string]any{"url": logs.URL, "username": "u",
		"passwordRef": map[string]any{"file": tokenFile}, "bearerTokenRef": map[string]any{"file": tokenFile}}}})
	var settings api.SettingsBody
	mountedFactorRequest(t, client, base, http.MethodGet, "/settings", "", nil, http.StatusOK, &settings)
	configured, _ := settings.Settings["querylog"].(map[string]any)["sinks"].([]any)
	if len(configured) != 2 || configured[0].(map[string]any)["name"] != "warehouse" || configured[1].(map[string]any)["name"] != "logs" {
		t.Fatalf("refused settings changed querylog.sinks: %v", configured)
	}
	// The running sinks were not disturbed by the refused attempts.
	query("after-refusals.example.")
	total++
	wait("the sinks to carry on after refused settings", func() bool { return sent("warehouse") == 8 && sent("logs") == 8 })

	// A value typed where a reference belongs is not kept: the settings
	// have no field for it.
	withLiteral := []map[string]any{
		{"name": "warehouse", "type": "clickhouse", "batchSize": 2, "flushInterval": "200ms", "clickhouse": map[string]any{
			"url": warehouse.URL, "database": "dns", "table": "query_log", "username": "kudurru", "password": "literal-secret-value",
			"passwordRef": map[string]any{"file": passwordFile}, "caRef": map[string]any{"file": caFile},
		}},
	}
	patch(map[string]any{"sinks": withLiteral})
	if strings.Contains(settingsBody(), "literal-secret-value") {
		t.Fatal("GET /settings shows a value that was typed into a sink's settings")
	}
	query("after-literal.example.")
	total++
	wait("the changed sink to deliver", func() bool { return sent("warehouse") == 9 })

	// Remove every sink: the series go with them; DNS and the local log
	// carry on.
	patch(map[string]any{"sinks": []map[string]any{}})
	wait("the removed sinks' series to disappear", func() bool {
		_, warehouseSent := mountedMetric(t, cfg.Listeners.Metrics.Address, "kudurru_sink_sent_total", `sink="warehouse"`)
		_, logsSent := mountedMetric(t, cfg.Listeners.Metrics.Address, "kudurru_sink_sent_total", `sink="logs"`)
		return !warehouseSent && !logsSent
	})
	exportedBefore := len(warehouse.Requests()) + len(logs.Requests())
	query("after-all-removed.example.")
	total++
	wait("the last row", func() bool { return int(localRows(1).RecordsTotal) == total })
	if got := len(warehouse.Requests()) + len(logs.Requests()); got != exportedBefore {
		t.Fatal("a removed sink received a query")
	}
	stop()

	opts, err := sqlitelog.OptionsFrom(cfg.Querylog, cfg.QuerylogDataDir(), cfg.Node.Name)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitelog.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 1})
	if err != nil || int(persisted.RecordsTotal) != total {
		t.Fatalf("local history after shutdown: %d of %d rows, %v", persisted.RecordsTotal, total, err)
	}
	_ = reopened.Close()
	// Nothing under the data directory holds a credential value: not the
	// granted files' contents, and not the value typed into the settings.
	err = filepath.WalkDir(cfg.Node.DataDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path) //nolint:gosec // the test's own data directory
		if err != nil {
			return err
		}
		for _, secret := range []string{"mounted-clickhouse-password", "rotated-clickhouse-password", "mounted-loki-token", "literal-secret-value"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("%s holds the credential value %q", path, secret)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
