package clickhouse_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/clickhouse"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/record"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/sinktest"
)

func spec(t *testing.T, settings config.QuerylogClickHouse) querylog.SinkSpec {
	t.Helper()
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	return querylog.SinkSpec{Name: "warehouse", Type: config.QuerylogSinkClickHouse, Config: raw}
}

func files(values map[string]string) func(context.Context, config.CredentialRef) ([]byte, error) {
	return func(_ context.Context, ref config.CredentialRef) ([]byte, error) {
		value, ok := values[ref.File]
		if !ok {
			return nil, errors.New("credential reference is not allowed")
		}
		return []byte(value), nil
	}
}

func events() []*querylog.Event {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	return []*querylog.Event{
		{ID: 7, Time: at, Latency: 1500 * time.Microsecond, Node: "node-a", Proto: pipeline.ProtoUDP, Client: netip.MustParseAddr("192.0.2.7"), ClientName: "laptop",
			Name: "Example.COM.", QType: 1, QClass: 1, Status: pipeline.StatusForwarded, Rcode: 0, Upstream: "1.1.1.1:53", Answers: 1, Answer: "192.0.2.80",
			ReplyType: querylog.ReplyIP, DNSSEC: querylog.DNSSECInsecure, Flags: querylog.Flags{RD: true}},
		{ID: 8, Time: at.Add(250 * time.Millisecond), Node: "node-a", Proto: pipeline.ProtoDoT, Client: netip.MustParseAddr("2001:db8::7"),
			Name: "ads.example.net.", QType: 28, QClass: 1, Status: pipeline.StatusBlockedList, Rcode: 0, List: "default/ads", Rule: `||ads.example.net^$dnstype=AAAA "quoted" <tag>`,
			ReplyType: querylog.ReplyNoData, DNSSEC: querylog.DNSSECUnknown},
	}
}

// TestInsertIsOneJSONEachRowRequest: the request a real ClickHouse HTTP
// interface expects, and rows that are exactly the documented record.
func TestInsertIsOneJSONEachRowRequest(t *testing.T) {
	double := sinktest.New(t, http.StatusOK, false)
	sink, err := clickhouse.New(spec(t, config.QuerylogClickHouse{
		URL: double.URL, Database: "dns", Table: "query_log", Username: "kudurru", PasswordRef: &config.CredentialRef{File: "/run/secrets/ch"},
	}), clickhouse.Options{Resolve: files(map[string]string{"/run/secrets/ch": "insert-only-password\n"}), Logger: slog.New(slog.DiscardHandler), UserAgent: "kudurru/test"})
	if err != nil {
		t.Fatal(err)
	}
	if sink.Name() != "clickhouse" {
		t.Fatalf("name %q", sink.Name())
	}
	if err := sink.Open(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(double.Requests()); got != 0 {
		t.Fatalf("Open sent %d requests: it must not contact ClickHouse", got)
	}
	batch := events()
	if err := sink.WriteBatch(t.Context(), batch, querylog.PrivacyFull); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteBatch(t.Context(), nil, querylog.PrivacyFull); err != nil {
		t.Fatal(err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	requests := double.Requests()
	if len(requests) != 1 {
		t.Fatalf("%d requests for one batch and one empty batch", len(requests))
	}
	r := requests[0]
	if r.Method != http.MethodPost || r.Path != "/" {
		t.Fatalf("request line: %s %s", r.Method, r.Path)
	}
	wantQuery := map[string][]string{
		"query":                            {"INSERT INTO `dns`.`query_log` FORMAT JSONEachRow"},
		"date_time_input_format":           {"best_effort"},
		"input_format_skip_unknown_fields": {"1"},
		"wait_end_of_query":                {"1"},
	}
	if !reflect.DeepEqual(map[string][]string(r.Query), wantQuery) {
		t.Fatalf("query parameters: %v", r.Query)
	}
	if got := r.Header.Get("Authorization"); got != sinktest.Basic("kudurru", "insert-only-password") {
		t.Fatal("the user and password were not sent as basic authentication")
	}
	if r.Header.Get("Content-Type") != "application/x-ndjson" || r.Header.Get("User-Agent") != "kudurru/test" || !r.Compressed {
		t.Fatalf("headers: %v, compressed %v", r.Header, r.Compressed)
	}
	if !strings.HasSuffix(string(r.Body), "\n") || strings.Count(string(r.Body), "\n") != 2 {
		t.Fatalf("body is not one line per row: %q", r.Body)
	}
	rows := sinktest.ClickHouseRows(t, r)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	for i, row := range rows {
		raw, err := json.Marshal(record.From(batch[i]))
		if err != nil {
			t.Fatal(err)
		}
		want := sinktest.ClickHouseRows(t, sinktest.Request{Body: raw})[0]
		if !reflect.DeepEqual(row, want) {
			t.Fatalf("row %d:\n got %v\nwant %v", i, row, want)
		}
	}
	if rows[0]["domain"] != "example.com" || rows[0]["ts"] != "2026-11-10T12:00:00.000000Z" || rows[0]["id"] != json.Number("7") || rows[0]["latency_us"] != json.Number("1500") {
		t.Fatalf("first row: %v", rows[0])
	}
	if rows[1]["rule"] != `||ads.example.net^$dnstype=AAAA "quoted" <tag>` || rows[1]["client"] != "2001:db8::7" {
		t.Fatalf("second row: %v", rows[1])
	}
	if _, present := rows[1]["upstream"]; present {
		t.Fatal("an empty optional field was sent")
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestInsertURL: defaults, a path prefix of a proxy, and no way for the
// settings to add a parameter.
func TestInsertURL(t *testing.T) {
	for raw, wantPrefix := range map[string]string{
		"http://clickhouse:8123":                "http://clickhouse:8123/?",
		"http://clickhouse:8123/":               "http://clickhouse:8123/?",
		"https://gateway.example/clickhouse":    "https://gateway.example/clickhouse?",
		"https://[2001:db8::1]:8443/ch/":        "https://[2001:db8::1]:8443/ch/?",
		"https://clickhouse.example:8443/a/b/c": "https://clickhouse.example:8443/a/b/c?",
	} {
		u, err := clickhouse.InsertURL(config.QuerylogClickHouse{URL: raw})
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !strings.HasPrefix(u.String(), wantPrefix) || u.Query().Get("query") != "INSERT INTO `kudurru`.`queries` FORMAT JSONEachRow" || len(u.Query()) != 4 {
			t.Errorf("%s: %s", raw, u)
		}
	}
}

// TestNewRefusesWhatTheSettingsValidatorRefuses: an entry that reached the
// factory around the validator is still not built, and nothing is contacted.
func TestNewRefusesWhatTheSettingsValidatorRefuses(t *testing.T) {
	opts := clickhouse.Options{Resolve: files(nil), Logger: slog.New(slog.DiscardHandler)}
	for name, tc := range map[string]struct {
		spec querylog.SinkSpec
		want string
	}{
		"no settings":        {querylog.SinkSpec{Name: "a", Type: "clickhouse"}, "the clickhouse settings are missing"},
		"malformed settings": {querylog.SinkSpec{Name: "a", Type: "clickhouse", Config: json.RawMessage(`[1]`)}, "the clickhouse settings are malformed"},
		"no URL":             {spec(t, config.QuerylogClickHouse{}), "clickhouse.url is required"},
		"URL with password":  {spec(t, config.QuerylogClickHouse{URL: "https://u:p@clickhouse.example"}), "must not carry a username or password"},
		"statement in table": {spec(t, config.QuerylogClickHouse{URL: "https://c.example", Table: "q` FORMAT Null; DROP TABLE x --"}), "clickhouse.table:"},
		"database":           {spec(t, config.QuerylogClickHouse{URL: "https://c.example", Database: "a.b"}), "clickhouse.database:"},
		"compression":        {spec(t, config.QuerylogClickHouse{URL: "https://c.example", Compression: "zstd"}), "clickhouse.compression is not gzip or none"},
		"password, no user":  {spec(t, config.QuerylogClickHouse{URL: "https://c.example", PasswordRef: &config.CredentialRef{File: "/p"}}), "clickhouse.username is required with passwordRef"},
		"two reference forms": {spec(t, config.QuerylogClickHouse{URL: "https://c.example", Username: "u", PasswordRef: &config.CredentialRef{File: "/p", Env: "P"}}),
			"clickhouse.passwordRef: set exactly one of secretRef, file or env"},
	} {
		sink, err := clickhouse.New(tc.spec, opts)
		if err == nil || sink != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	// A reference this node may not read keeps the sink from opening; the
	// host tries again.
	sink, err := clickhouse.New(spec(t, config.QuerylogClickHouse{URL: "https://c.example", Username: "u", PasswordRef: &config.CredentialRef{File: "/etc/shadow"}}), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Open(t.Context(), nil, nil); err == nil || !strings.Contains(err.Error(), "ClickHouse: password: credential reference is not allowed") {
		t.Fatalf("a reference outside the node's grants: %v", err)
	}
}

// TestRefusalReportsTheExceptionCodeAndNeverTheMessage: ClickHouse's error
// text quotes the rows it could not read, so only its numeric code and the
// status are reported.
func TestRefusalReportsTheExceptionCodeAndNeverTheMessage(t *testing.T) {
	double := sinktest.New(t, http.StatusOK, false)
	sink, err := clickhouse.New(spec(t, config.QuerylogClickHouse{URL: double.URL, Compression: config.QuerylogCompressionNone}), clickhouse.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Open(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status int
		code   string
		want   string
		again  bool
	}{
		{404, "60", "ClickHouse answered HTTP 404 (ClickHouse exception code 60)", false},
		{400, "27", "ClickHouse answered HTTP 400 (ClickHouse exception code 27)", false},
		{500, "241", "ClickHouse answered HTTP 500 (ClickHouse exception code 241)", true},
		{503, "", "ClickHouse answered HTTP 503", true},
		{400, "60; DROP", "ClickHouse answered HTTP 400", false},
		{400, "1234567", "ClickHouse answered HTTP 400", false},
	} {
		double.Answer(func(int, sinktest.Request) sinktest.Answer {
			return sinktest.Answer{Status: tc.status, Header: http.Header{clickhouse.ExceptionCodeHeader: {tc.code}},
				Body: "Code: 27. DB::Exception: Cannot parse input: expected '\"' before: 'example.com from 192.0.2.7'"}
		})
		err := sink.WriteBatch(t.Context(), events(), querylog.PrivacyFull)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%d/%q: %v, want %q", tc.status, tc.code, err, tc.want)
		}
		if _, again := querylog.RetryableWait(err); again != tc.again {
			t.Errorf("%d: retryable %v, want %v", tc.status, again, tc.again)
		}
	}
	if r := double.Requests()[0]; r.Compressed || r.Header.Get("Authorization") != "" || r.Header.Get("Content-Encoding") != "" {
		t.Fatalf("compression none without a user: %v", r.Header)
	}
}

var (
	columnLine = regexp.MustCompile("^    ([a-z_]+) +([A-Za-z0-9_(), ']+?),?$")
	fieldRow   = regexp.MustCompile("^\\| `([a-z_]+)` \\| ([a-z ,]+) \\|")
)

// TestDocumentedTableFitsTheRecord keeps the operator page in step with the
// exported record: every field has a column of a type that reads its JSON
// form, the table has no column the record never fills, and the field table
// lists the same names with the same optionality.
func TestDocumentedTableFitsTheRecord(t *testing.T) {
	page, err := os.ReadFile("../../../../docs/deploy/query-log-sinks.md")
	if err != nil {
		t.Fatal(err)
	}
	columns := map[string]string{}
	documented := map[string]string{}
	inTable, inFields := false, false
	for line := range strings.Lines(string(page)) {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "## "):
			inFields = line == "## What one query looks like"
		case strings.HasPrefix(line, "CREATE TABLE IF NOT EXISTS kudurru.queries"):
			inTable = true
		case inTable && strings.HasPrefix(line, ")"):
			inTable = false
		case inTable:
			if m := columnLine.FindStringSubmatch(line); m != nil {
				columns[m[1]] = m[2]
			} else if line != "(" {
				t.Fatalf("column line not understood: %q", line)
			}
		case inFields:
			if m := fieldRow.FindStringSubmatch(line); m != nil {
				documented[m[1]] = m[2]
			}
		}
	}
	if len(columns) == 0 {
		t.Fatal("the page has no CREATE TABLE for kudurru.queries")
	}
	// The JSON forms the record produces, from a full event.
	full := record.From(&querylog.Event{
		ID: 1, Time: time.Unix(1, 0), Node: "n", Client: netip.MustParseAddr("192.0.2.1"), ClientName: "c", ClientGroup: "g", Name: "a.example.", QType: 1,
		Upstream: "u", List: "l", Rule: "r", Policy: "p", Zone: "z.", CNAME: "c.", HasEDE: true, EDE: 1, ECS: netip.MustParsePrefix("192.0.2.0/24"),
		TraceID: [16]byte{1}, Answer: "a", Flags: querylog.Flags{RD: true},
	})
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	optional := map[string]bool{}
	rt := reflect.TypeOf(full)
	for i := range rt.NumField() {
		name, opts, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		optional[name] = strings.Contains(opts, "omitempty")
		if _, present := fields[name]; !present {
			t.Fatalf("the probe event leaves %s unset", name)
		}
	}
	var names []string
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		column, ok := columns[name]
		if !ok {
			t.Errorf("field %s has no column in the documented table", name)
			continue
		}
		var fits bool
		switch fields[name].(type) {
		case string:
			fits = column == "String" || column == "LowCardinality(String)" || (name == "ts" && strings.HasPrefix(column, "DateTime64("))
		case float64:
			fits = strings.HasPrefix(column, "UInt") || strings.HasPrefix(column, "Nullable(UInt")
		case []any:
			fits = strings.HasPrefix(column, "Array(")
		}
		if !fits {
			t.Errorf("field %s (%T) does not fit column type %s", name, fields[name], column)
		}
		doc, ok := documented[name]
		switch {
		case !ok:
			t.Errorf("field %s is missing from the field table of the page", name)
		case strings.Contains(doc, "optional") != optional[name]:
			t.Errorf("field %s: the page says %q, the record's omitempty is %v", name, doc, optional[name])
		}
	}
	for name := range columns {
		if _, ok := fields[name]; !ok {
			t.Errorf("column %s of the documented table is never filled", name)
		}
	}
	for name := range documented {
		if _, ok := fields[name]; !ok {
			t.Errorf("the field table of the page lists %s, which the record does not have", name)
		}
	}
	// The statement and the settings the page describes are the ones sent.
	u, err := clickhouse.InsertURL(config.QuerylogClickHouse{URL: "http://clickhouse:8123"})
	if err != nil {
		t.Fatal(err)
	}
	for parameter, values := range u.Query() {
		if parameter == "query" {
			continue
		}
		if !strings.Contains(string(page), "`"+parameter+"="+values[0]+"`") {
			t.Errorf("the page does not mention the input setting %s=%s", parameter, values[0])
		}
	}
	if !strings.Contains(string(page), "INSERT INTO <database>.<table> FORMAT JSONEachRow") {
		t.Error("the page does not name the statement")
	}
}
