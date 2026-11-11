package sinks_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/record"
)

// The tests in this file run the sinks against real servers. They are
// optional: without the variables below they skip, and with
// KUDURRU_REQUIRE_SINK_SERVERS set a missing server is a failure (the
// convention of KUDURRU_TEST_POSTGRES_DSN). The local doubles of the other
// tests are the proof of the sinks' behavior; these check the two things a
// double cannot: that ClickHouse reads the rows into the documented table,
// and that Loki accepts the push and serves the documented mapping.
//
//	KUDURRU_TEST_CLICKHOUSE_URL       http://127.0.0.1:8123
//	KUDURRU_TEST_CLICKHOUSE_USER      user that may create a database (default "default")
//	KUDURRU_TEST_CLICKHOUSE_PASSWORD  its password (default empty)
//	KUDURRU_TEST_LOKI_URL             http://127.0.0.1:3100
const (
	envClickHouseURL      = "KUDURRU_TEST_CLICKHOUSE_URL"
	envClickHouseUser     = "KUDURRU_TEST_CLICKHOUSE_USER"
	envClickHousePassword = "KUDURRU_TEST_CLICKHOUSE_PASSWORD" //nolint:gosec // G101: the name of an environment variable, not a value
	envLokiURL            = "KUDURRU_TEST_LOKI_URL"
	envRequireServers     = "KUDURRU_REQUIRE_SINK_SERVERS"
)

func liveServer(t *testing.T, variable string) string {
	t.Helper()
	target := os.Getenv(variable)
	if target == "" {
		if os.Getenv(envRequireServers) != "" {
			t.Fatalf("%s is set but %s is empty", envRequireServers, variable)
		}
		t.Skipf("no server: set %s", variable)
	}
	return strings.TrimSuffix(target, "/")
}

func suffix(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

// documentedStatements returns the CREATE DATABASE and CREATE TABLE
// statements of the operator page, with the database renamed.
func documentedStatements(t *testing.T, database string) []string {
	t.Helper()
	page, err := os.ReadFile("../../../docs/deploy/query-log-sinks.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, found := strings.Cut(string(page), "```sql\n")
	if !found {
		t.Fatal("the page has no SQL block")
	}
	block, _, _ := strings.Cut(rest, "```")
	var out []string
	for _, statement := range strings.Split(block, ";") {
		statement = strings.TrimSpace(statement)
		switch {
		case statement == "CREATE DATABASE IF NOT EXISTS kudurru":
			out = append(out, "CREATE DATABASE IF NOT EXISTS "+database)
		case strings.HasPrefix(statement, "CREATE TABLE IF NOT EXISTS kudurru.queries"):
			out = append(out, strings.Replace(statement, "kudurru.queries", database+".queries", 1))
		}
	}
	if len(out) != 2 {
		t.Fatalf("the page's SQL block has %d CREATE DATABASE/TABLE statements", len(out))
	}
	return out
}

func liveEvents(marker string) []*querylog.Event {
	at := time.Now().UTC().Truncate(time.Microsecond)
	return []*querylog.Event{
		{Time: at, Latency: 1500 * time.Microsecond, Node: "node-a", Proto: pipeline.ProtoDoH3, Client: netip.MustParseAddr("192.0.2.7"), ClientName: "laptop", ClientGroup: "kids",
			Name: "One." + marker + ".Example.", QType: 65, QClass: 1, Status: pipeline.StatusBlockedCNAME, Rcode: 3, Upstream: "tls://dns.example", List: "default/ads",
			Rule: `||ads.example^ "quoted" <b>&`, Answers: 2, Answer: "tracker.example.", Policy: "default/strict", Zone: "example.", CNAME: "tracker.example.",
			ReplyType: querylog.ReplyNXDomain, DNSSEC: querylog.DNSSECSecure, EDE: 15, HasEDE: true, ECS: netip.MustParsePrefix("198.51.100.0/24"),
			TraceID: [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
			Flags:   querylog.Flags{RD: true, DO: true, Cached: true}},
		{Time: at.Add(time.Millisecond), Node: "node-a", Proto: pipeline.ProtoUDP, Client: netip.MustParseAddr("2001:db8::7"),
			Name: "two." + marker + ".example.", QType: 1, QClass: 1, Status: pipeline.StatusForwarded, ReplyType: querylog.ReplyIP, Answers: 1},
		{Time: at.Add(2 * time.Millisecond), Node: "node-b", Proto: pipeline.ProtoTCP,
			Name: "three." + marker + ".example.", QType: 65280, QClass: 1, Status: pipeline.StatusServfailUpstream, Rcode: 2, ReplyType: querylog.ReplyServfail},
	}
}

// TestLiveClickHouseReadsTheDocumentedTable creates the documented table in
// a real ClickHouse, lets the sink insert into it through the host, and
// reads the rows back.
func TestLiveClickHouseReadsTheDocumentedTable(t *testing.T) {
	base := liveServer(t, envClickHouseURL)
	user, password := os.Getenv(envClickHouseUser), os.Getenv(envClickHousePassword)
	if user == "" {
		user = "default"
	}
	database := "kudurru_test_" + suffix(t)
	statement := func(query string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/?wait_end_of_query=1", strings.NewReader(query))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(user, password)
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("ClickHouse refused %q: HTTP %d %s", query, response.StatusCode, body)
		}
		return string(body)
	}
	for _, create := range documentedStatements(t, database) {
		statement(create)
	}
	t.Cleanup(func() {
		req, err := http.NewRequest(http.MethodPost, base+"/", strings.NewReader("DROP DATABASE IF EXISTS "+database))
		if err == nil {
			req.SetBasicAuth(user, password)
			if response, err := (&http.Client{Timeout: 30 * time.Second}).Do(req); err == nil {
				_ = response.Body.Close()
			}
		}
	})

	credentials := map[string]string{"/run/secrets/export": password}
	if password == "" {
		credentials = nil
	}
	f := newFixture(t, credentials)
	route := f.route("full", nil, entry(config.QuerylogSinkClickHouse, "export", base, func(s *config.QuerylogSink) {
		s.BatchSize = 10
		s.ClickHouse.Database, s.ClickHouse.Username = database, user
		if password == "" {
			s.ClickHouse.PasswordRef = nil
		}
	}))
	marker := suffix(t)
	events := liveEvents(marker)
	for _, ev := range events {
		if !route.Enqueue(ev) {
			t.Fatal("event refused")
		}
	}
	waitFor(t, "ClickHouse to store the batch", func() bool { st := f.status("export"); return st.Sent == 3 || st.Dropped > 0 })
	if st := f.status("export"); st.Sent != 3 || st.Dropped != 0 || st.Errors != 0 {
		t.Fatalf("status: %+v\n%s", st, f.logged())
	}
	// Read the rows back in the record's own format and compare field by
	// field; ClickHouse renders the column defaults for what a row left out.
	out := statement("SELECT * FROM " + database + ".queries ORDER BY id FORMAT JSONEachRow SETTINGS output_format_json_quote_64bit_integers = 0, date_time_output_format = 'iso'")
	var stored []map[string]any
	for line := range strings.Lines(out) {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("row %q: %v", line, err)
		}
		stored = append(stored, row)
	}
	if len(stored) != len(events) {
		t.Fatalf("%d rows stored, want %d", len(stored), len(events))
	}
	for i, ev := range events {
		ev.ID = uint64(i + 1) // #nosec G115 -- the collector stamped 1..3
		raw, err := json.Marshal(record.From(ev))
		if err != nil {
			t.Fatal(err)
		}
		var want map[string]any
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		got := stored[i]
		for name, value := range want {
			if name == "ts" {
				at, err := time.Parse(time.RFC3339Nano, got["ts"].(string))
				if err != nil || !at.Equal(ev.Time) {
					t.Errorf("row %d ts: stored %v, sent %s (%v)", i, got["ts"], ev.Time, err)
				}
				continue
			}
			if !reflect.DeepEqual(got[name], value) {
				t.Errorf("row %d %s: stored %#v, sent %#v", i, name, got[name], value)
			}
		}
		// What the record left out took the column default.
		for name, value := range got {
			if _, sent := want[name]; sent {
				continue
			}
			empty := value == nil || value == "" || value == float64(0) || reflect.DeepEqual(value, []any{})
			if !empty {
				t.Errorf("row %d %s: stored %#v for a field the record left out", i, name, value)
			}
		}
	}

	// The table ignores a batch that arrives a second time unchanged, which
	// is what a repeated attempt sends.
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, ev := range events {
		if err := enc.Encode(record.From(ev)); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(table string, rows []byte) (int, string) {
		t.Helper()
		query := url.Values{"query": {"INSERT INTO `" + database + "`.`" + table + "` FORMAT JSONEachRow"}, "date_time_input_format": {"best_effort"}, "input_format_skip_unknown_fields": {"1"}, "wait_end_of_query": {"1"}}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/?"+query.Encode(), bytes.NewReader(rows))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(user, password)
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return response.StatusCode, response.Header.Get("X-ClickHouse-Exception-Code") + " " + string(answer)
	}
	for attempt := range 2 {
		if status, answer := insert("queries", body.Bytes()); status != http.StatusOK {
			t.Fatalf("repeated insert %d: HTTP %d %s", attempt, status, answer)
		}
	}
	if count := strings.TrimSpace(statement("SELECT count() FROM " + database + ".queries")); count != "3" {
		t.Errorf("%s rows after the same batch arrived three times, want 3: the documented table must deduplicate a repeated batch", count)
	}

	// A table without some of the columns keeps accepting the rows, and a
	// missing table answers with the status and the code the sink reports.
	statement("CREATE TABLE " + database + ".narrow (ts DateTime64(6, 'UTC'), id UInt64, domain String) ENGINE = MergeTree ORDER BY (ts, id)")
	if status, answer := insert("narrow", body.Bytes()); status != http.StatusOK {
		t.Errorf("insert into a table with fewer columns: HTTP %d %s", status, answer)
	}
	if count := strings.TrimSpace(statement("SELECT count() FROM " + database + ".narrow WHERE domain = 'one." + marker + ".example'")); count != "1" {
		t.Errorf("narrow table: %s matching rows", count)
	}
	if status, answer := insert("absent", body.Bytes()); status != http.StatusNotFound || !strings.HasPrefix(answer, "60 ") {
		t.Errorf("insert into a missing table: HTTP %d, exception code and text %q; the operator page says 404 with code 60", status, answer)
	}
}

// TestLiveLokiServesTheDocumentedMapping pushes through the sink to a real
// Loki and reads the entries back with the queries the operator page shows:
// by stream labels and by structured metadata, without parsing the line.
func TestLiveLokiServesTheDocumentedMapping(t *testing.T) {
	base := liveServer(t, envLokiURL)
	job := "kudurru-test-" + suffix(t)
	f := newFixture(t, nil)
	route := f.route("full", nil, entry(config.QuerylogSinkLoki, "export", base, func(s *config.QuerylogSink) {
		s.BatchSize = 10
		s.Loki.BearerTokenRef = nil
		s.Loki.Labels = map[string]string{"job": job}
	}))
	marker := suffix(t)
	events := liveEvents(marker)
	for _, ev := range events {
		if !route.Enqueue(ev) {
			t.Fatal("event refused")
		}
	}
	waitFor(t, "Loki to accept the push", func() bool { st := f.status("export"); return st.Sent == 3 || st.Dropped > 0 })
	if st := f.status("export"); st.Sent != 3 || st.Dropped != 0 || st.Errors != 0 {
		t.Fatalf("status: %+v\n%s", st, f.logged())
	}
	type result struct {
		Stream map[string]string `json:"stream"`
		Values [][]string        `json:"values"`
	}
	search := func(logql string) []result {
		t.Helper()
		query := url.Values{"query": {logql}, "limit": {"100"}, "direction": {"forward"},
			"start": {fmt.Sprint(events[0].Time.Add(-time.Minute).UnixNano())}, "end": {fmt.Sprint(events[0].Time.Add(time.Minute).UnixNano())}}
		var found []result
		deadline := time.Now().Add(30 * time.Second)
		for {
			response, err := (&http.Client{Timeout: 10 * time.Second}).Get(base + "/loki/api/v1/query_range?" + query.Encode())
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("Loki refused %s: HTTP %d %s", logql, response.StatusCode, raw)
			}
			var answer struct {
				Data struct {
					Result []result `json:"result"`
				} `json:"data"`
			}
			if err := json.Unmarshal(raw, &answer); err != nil {
				t.Fatalf("Loki answer: %v", err)
			}
			found = answer.Data.Result
			if len(found) > 0 || time.Now().After(deadline) {
				return found
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	lines := func(results []result) []string {
		var out []string
		for _, r := range results {
			for _, v := range r.Values {
				var line map[string]any
				if err := json.Unmarshal([]byte(v[1]), &line); err != nil {
					t.Fatalf("stored line %q is not JSON: %v", v[1], err)
				}
				out = append(out, line["domain"].(string))
			}
		}
		sort.Strings(out)
		return out
	}
	one, two, three := "one."+marker+".example", "two."+marker+".example", "three."+marker+".example"

	// Every entry, by the fixed label.
	all := search(`{job="` + job + `"}`)
	if got := lines(all); !reflect.DeepEqual(got, []string{one, three, two}) {
		t.Fatalf("Loki holds %v", got)
	}
	// Stream labels: node and status_class select streams.
	if got := lines(search(`{job="` + job + `", status_class="blocked"}`)); !reflect.DeepEqual(got, []string{one}) {
		t.Errorf(`status_class="blocked": %v`, got)
	}
	if got := lines(search(`{job="` + job + `", node="node-b"}`)); !reflect.DeepEqual(got, []string{three}) {
		t.Errorf(`node="node-b": %v`, got)
	}
	// Structured metadata: filtered without parsing the line.
	for filter, want := range map[string]string{
		`domain="` + two + `"`: two, `client="192.0.2.7"`: one, `client_name="laptop"`: one, `status="servfail_upstream"`: three,
		`qtype="TYPE65280"`: three, `trace_id="4bf92f3577b34da6a3ce929d0e0e4736"`: one,
	} {
		if got := lines(search(`{job="` + job + `"} | ` + filter)); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("| %s: %v, want %s", filter, got, want)
		}
	}
	// The line itself is the record: | json reaches every other field.
	if got := lines(search(`{job="` + job + `"} | json | latency_us > 1000`)); !reflect.DeepEqual(got, []string{one}) {
		t.Errorf("| json | latency_us > 1000: %v", got)
	}
	// The stored line of the first event is exactly the record.
	for _, r := range search(`{job="` + job + `"} | domain="` + one + `"`) {
		events[0].ID = 1
		want, err := json.Marshal(record.From(events[0]))
		if err != nil {
			t.Fatal(err)
		}
		var gotLine, wantLine map[string]any
		if json.Unmarshal([]byte(r.Values[0][1]), &gotLine) != nil || json.Unmarshal(want, &wantLine) != nil || !reflect.DeepEqual(gotLine, wantLine) {
			t.Errorf("stored line %s, want %s", r.Values[0][1], want)
		}
		if r.Values[0][0] != fmt.Sprint(events[0].Time.UnixNano()) {
			t.Errorf("stored timestamp %s, want %d", r.Values[0][0], events[0].Time.UnixNano())
		}
	}
}
