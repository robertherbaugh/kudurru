package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
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
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// TestRunServeQuerylogSinksStalledFailingAndReload is the mounted D371
// proof. It runs the actual serve command on the complete SQLite runtime
// and drives it through its real surfaces: the settings command adds,
// keeps, replaces and removes entries of querylog.sinks; real UDP queries
// are answered; rows are read back through GET /queries; the live tail is
// read through the SSE stream; and the per-sink counters are read from the
// metrics listener. Only the sink behind the host is a double, placed where
// an exporter will stand, so that it can stall and fail on demand.
func TestRunServeQuerylogSinksStalledFailingAndReload(t *testing.T) {
	doubles := useExportDoubles(t)
	stalled := doubles.add("stalled", &exportSink{stall: make(chan struct{}), entered: make(chan struct{}, 1)})
	var release sync.Once
	unstall := func() { release.Do(func() { close(stalled.stall) }) }
	defer unstall()
	recorder := doubles.add("recorder", &exportSink{})
	failing := doubles.add("failing", &exportSink{fail: func(int) error { return querylog.Retryable(errors.New("503 from the collector")) }})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	up := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 90)}}
		_ = w.WriteMsg(a)
	})}
	go func() { _ = up.ActivateAndServe() }()
	<-started
	defer func() { _ = up.Shutdown() }()

	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Cache.MaxEntries = 0
	cfg.DNSSEC.Enabled = false
	cfg.RateLimit.Enabled = false
	cfg.Querylog.Enabled = true
	cfg.Querylog.Exclude.SUDN = false
	cfg.Listeners.Metrics.Address = offlineFreeAddress(t, "tcp")
	cfg.Upstreams.Groups = map[string]config.UpstreamGroup{"default": {Servers: []string{pc.LocalAddr().String()}, Strategy: "strict", Timeout: config.Duration(4 * time.Second)}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			unstall()
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
				t.Fatal("timed out waiting for " + what)
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
	// query sends one real UDP question and requires its answer.
	query := func(name string) {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion(name, dns.TypeA)
		a, _, err := (&dns.Client{Net: "udp", Timeout: 5 * time.Second}).Exchange(q, cfg.Listeners.DNS[0].Address)
		if err != nil || a == nil || a.Rcode != dns.RcodeSuccess || len(a.Answer) != 1 {
			t.Fatalf("UDP query %s was not answered: %v", name, err)
		}
	}
	rows := func() int {
		t.Helper()
		var out api.QueriesBody
		mountedFactorRequest(t, client, base, http.MethodGet, "/queries?limit=1", "", nil, http.StatusOK, &out)
		return int(out.RecordsTotal)
	}
	// metric reads one sample from the mounted metrics listener.
	metric := func(family string, labels ...string) (float64, bool) {
		t.Helper()
		response, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + cfg.Listeners.Metrics.Address + "/metrics")
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

	// The live tail, read through the mounted SSE transport for the whole
	// test; it subscribes before the first query is sent.
	streamCtx, streamCancel := context.WithCancel(t.Context())
	defer streamCancel()
	var tailMu sync.Mutex
	var tail []sqlitelog.QueryRow
	streamReq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, base+"/queries/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamResponse, err := (&http.Client{Jar: jar}).Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = streamResponse.Body.Close() }()
	if streamResponse.StatusCode != http.StatusOK {
		t.Fatal("live tail unavailable", streamResponse.StatusCode)
	}
	go func() {
		scanner := bufio.NewScanner(streamResponse.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "data: ") {
				var row sqlitelog.QueryRow
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &row) == nil && row.ID != 0 {
					tailMu.Lock()
					tail = append(tail, row)
					tailMu.Unlock()
				}
			}
		}
	}()
	tailed := func() []sqlitelog.QueryRow {
		tailMu.Lock()
		defer tailMu.Unlock()
		return append([]sqlitelog.QueryRow(nil), tail...)
	}
	wait("the live tail subscription", func() bool {
		var status api.QuerylogStatusBody
		mountedFactorRequest(t, client, base, http.MethodGet, "/querylog/status", "", nil, http.StatusOK, &status)
		return status.Subscribers >= 1
	})

	total := 0
	// id is the query-log ID of the n-th query of this test: IDs start at 1
	// in a new database, one per answered query.
	id := func(n int) uint64 { return uint64(n) } // #nosec G115 -- a small positive test counter
	query("before-any-sink.example.")
	total++
	wait("the first row", func() bool { return rows() == total })

	// Reload adds a sink that never returns from its first write.
	patch(map[string]any{"sinks": []map[string]any{{"name": "stalled", "type": "slog", "queueSize": 100, "batchSize": 1}}})
	query("stall-first.example.")
	total++
	select {
	case <-stalled.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the added sink never received its first batch")
	}
	const burst = 160
	began := time.Now()
	for i := range burst {
		query(fmt.Sprintf("burst-%03d.example.", i))
	}
	total += burst
	t.Logf("%d UDP queries answered in %s while an additional sink was stalled", burst, time.Since(began).Round(time.Millisecond))
	wait("SQLite rows to keep committing behind a stalled sink", func() bool { return rows() == total })
	wait("the live tail to keep publishing behind a stalled sink", func() bool { return len(tailed()) == total })
	for i, row := range tailed() {
		if row.ID != uint64(i+1) {
			t.Fatalf("tail event %d has id %d: the tail lost or repeated an event", i, row.ID)
		}
	}
	// One event is inside the stalled write and 100 wait in its queue; the
	// other 60 overflowed and are counted for that sink only.
	const overflow = burst - 100
	wait("the per-sink overflow counter", func() bool {
		got, _ := metric("kudurru_querylog_dropped_total", `sink="stalled"`, `stage="sink"`)
		return got == overflow
	})
	if got, found := metric("kudurru_querylog_dropped_total", `stage="queue"`); found && got != 0 {
		t.Errorf("the collector queue dropped %v events", got)
	}
	if got, _ := metric("kudurru_sink_queue_length", `sink="stalled"`); got != 100 {
		t.Errorf("kudurru_sink_queue_length{sink=stalled} = %v, want 100", got)
	}
	if got, _ := metric("kudurru_sink_sent_total", `sink="stalled"`); got != 0 {
		t.Errorf("kudurru_sink_sent_total{sink=stalled} = %v", got)
	}

	// Replace the stalled sink while it is still stalled. The new sink
	// receives the queries answered after it was added, and none before.
	patch(map[string]any{"sinks": []map[string]any{{"name": "recorder", "type": "slog", "batchSize": 1}}})
	for _, name := range []string{"r1.example.", "r2.example.", "r3.example."} {
		query(name)
	}
	total += 3
	wait("the added sink to receive the new queries", func() bool { return len(recorder.names()) == 3 })
	if got, want := recorder.names(), []string{"r1.example.", "r2.example.", "r3.example."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("added sink received %v, want %v: earlier events must not be replayed", got, want)
	}
	if got, want := recorder.ids(), []uint64{id(total - 2), id(total - 1), id(total)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("added sink received ids %v, want %v", got, want)
	}

	// Another querylog change with the entry untouched: the sink keeps its
	// instance through every publication, and the privacy transform runs
	// before its queue.
	patch(map[string]any{"privacy": "hide_domains"})
	query("r4.example.")
	total++
	wait("the kept sink", func() bool { return len(recorder.names()) == 4 })
	if got := recorder.names()[3]; got != querylog.HiddenName {
		t.Errorf("the sink received %q under hide_domains", got)
	}
	if opens, closes, _ := recorder.counts(); opens != 1 || closes != 0 || doubles.builds("recorder") != 1 {
		t.Fatalf("an unchanged sink was restarted: opens %d closes %d builds %d", opens, closes, doubles.builds("recorder"))
	}

	// Replace it with a sink whose destination keeps answering "try again":
	// two attempts per batch, then the batch is dropped and counted.
	patch(map[string]any{"privacy": "full", "sinks": []map[string]any{{"name": "failing", "type": "slog", "batchSize": 1, "attempts": 2, "retryBackoff": "100ms"}}})
	wait("the removed sink to close", func() bool { _, closes, _ := recorder.counts(); return closes == 1 })
	query("f1.example.")
	query("f2.example.")
	total += 2
	wait("the bounded attempts", func() bool {
		dropped, _ := metric("kudurru_querylog_dropped_total", `sink="failing"`, `stage="sink"`)
		return dropped == 2
	})
	if _, _, calls := failing.counts(); calls != 4 {
		t.Errorf("the failing sink was written %d times, want 2 batches x 2 attempts", calls)
	}
	if got, _ := metric("kudurru_sink_errors_total", `sink="failing"`); got != 4 {
		t.Errorf("kudurru_sink_errors_total{sink=failing} = %v, want 4", got)
	}
	if got := len(recorder.names()); got != 4 {
		t.Errorf("the removed sink received %d events", got)
	}
	wait("rows behind a failing sink", func() bool { return rows() == total })

	// Settings the runtime could not serve are refused and change nothing.
	refused("unknown type", []map[string]any{{"name": "kafka", "type": "kafka"}})
	refused("two log-line sinks", []map[string]any{{"name": "one", "type": "slog"}, {"name": "two", "type": "slog"}})
	refused("reserved name", []map[string]any{{"name": "sqlite", "type": "slog"}})
	var settings api.SettingsBody
	mountedFactorRequest(t, client, base, http.MethodGet, "/settings", "", nil, http.StatusOK, &settings)
	configured, _ := settings.Settings["querylog"].(map[string]any)["sinks"].([]any)
	if len(configured) != 1 || configured[0].(map[string]any)["name"] != "failing" {
		t.Fatalf("refused settings changed querylog.sinks: %v", configured)
	}

	// Remove every sink: its series go with it, and DNS, the database and
	// the tail carry on.
	patch(map[string]any{"sinks": []map[string]any{}})
	wait("the removed sink's series to disappear", func() bool {
		_, sent := metric("kudurru_sink_sent_total", `sink="failing"`)
		_, dropped := metric("kudurru_querylog_dropped_total", `sink="failing"`)
		return !sent && !dropped
	})
	query("after-all-removed.example.")
	total++
	wait("the last row", func() bool { return rows() == total })
	wait("the last tail event", func() bool { return len(tailed()) == total })
	seen := tailed()
	for i, row := range seen {
		if row.ID != uint64(i+1) {
			t.Fatalf("tail event %d has id %d", i, row.ID)
		}
	}
	if seen[total-1].Domain != "after-all-removed.example" || seen[1].Domain != "stall-first.example" {
		t.Errorf("tail order: %q ... %q", seen[1].Domain, seen[total-1].Domain)
	}

	// The stalled sink is still inside its first write; let it go and stop.
	if _, closes, calls := stalled.counts(); closes != 0 || calls != 1 {
		t.Fatalf("stalled sink: closes %d calls %d", closes, calls)
	}
	unstall()
	wait("the released sink to close", func() bool { _, closes, _ := stalled.counts(); return closes == 1 })
	// What it wrote is the batch it held, plus its queue when the release
	// came inside the drain bound of a removed sink; never a later query.
	switch got := stalled.names(); {
	case len(got) == 1 && got[0] == "stall-first.example.":
	case len(got) == 101 && got[0] == "stall-first.example." && got[100] == "burst-099.example.":
	default:
		t.Errorf("the stalled sink wrote %d events after its removal: %v", len(got), got)
	}
	streamCancel()
	stop()
	opts, err := sqlitelog.OptionsFrom(cfg.Querylog, cfg.QuerylogDataDir(), cfg.Node.Name)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitelog.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	persisted, err := reopened.Reader().Queries(t.Context(), sqlitelog.QueryFilter{Limit: 1})
	if err != nil || int(persisted.RecordsTotal) != total || persisted.Queries[0].ID != id(total) {
		t.Fatalf("local history after shutdown: %v", err)
	}
}
