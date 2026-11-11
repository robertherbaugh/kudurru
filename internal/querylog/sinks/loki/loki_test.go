package loki_test

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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/loki"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/record"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/sinktest"
)

func spec(t *testing.T, settings config.QuerylogLoki) querylog.SinkSpec {
	t.Helper()
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	return querylog.SinkSpec{Name: "logs", Type: config.QuerylogSinkLoki, Config: raw}
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

var codeTerm = regexp.MustCompile("`([a-z_]+)`")

var at = time.Date(2026, 11, 10, 12, 0, 0, 123456789, time.UTC)

// events is a batch of two nodes and three status classes, out of class
// order, so that the grouping is visible.
func events() []*querylog.Event {
	return []*querylog.Event{
		{ID: 1, Time: at, Node: "node-a", Client: netip.MustParseAddr("192.0.2.7"), ClientName: "laptop", Name: "Example.COM.", QType: 1, Status: pipeline.StatusForwarded,
			ReplyType: querylog.ReplyIP, TraceID: [16]byte{0xab, 0xcd}},
		{ID: 2, Time: at.Add(time.Millisecond), Node: "node-a", Client: netip.MustParseAddr("192.0.2.8"), Name: "ads.example.net.", QType: 28, Status: pipeline.StatusBlockedList,
			List: "default/ads", Rule: `||ads.example.net^ "quoted" <b>&`},
		{ID: 3, Time: at.Add(2 * time.Millisecond), Node: "node-a", Client: netip.MustParseAddr("192.0.2.7"), Name: "second.example.com.", QType: 65, Status: pipeline.StatusRetried},
		{ID: 4, Time: at.Add(3 * time.Millisecond), Node: "node-b", Client: netip.MustParseAddr("2001:db8::9"), Name: "example.com.", QType: 1, Status: pipeline.StatusCachedStale},
		{ID: 5, Time: at.Add(4 * time.Millisecond), Node: "node-a", Name: "broken.example.", QType: 1, Status: pipeline.StatusServfailUpstream, Rcode: 2},
	}
}

// TestPushIsLokiJSONWithLowCardinalityStreams: the request Loki's push API
// expects. Streams are keyed by the fixed labels, the node and the status
// class only; what differs per query is in the line and in the structured
// metadata.
func TestPushIsLokiJSONWithLowCardinalityStreams(t *testing.T) {
	double := sinktest.New(t, http.StatusNoContent, false)
	sink, err := loki.New(spec(t, config.QuerylogLoki{
		URL: double.URL + "/gateway/", Tenant: "home", BearerTokenRef: &config.CredentialRef{File: "/run/secrets/loki"},
		Labels: map[string]string{"job": "dns", "site": "home lab"},
	}), loki.Options{Resolve: files(map[string]string{"/run/secrets/loki": "glc_token-123=\n"}), Logger: slog.New(slog.DiscardHandler), UserAgent: "kudurru/test"}) //nolint:gosec // G101: a made-up token
	if err != nil {
		t.Fatal(err)
	}
	if sink.Name() != "loki" {
		t.Fatalf("name %q", sink.Name())
	}
	if err := sink.Open(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(double.Requests()); got != 0 {
		t.Fatalf("Open sent %d requests: it must not contact Loki", got)
	}
	batch := events()
	if err := sink.WriteBatch(t.Context(), batch, querylog.PrivacyFull); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteBatch(t.Context(), nil, querylog.PrivacyFull); err != nil {
		t.Fatal(err)
	}
	requests := double.Requests()
	if len(requests) != 1 {
		t.Fatalf("%d requests for one batch and one empty batch", len(requests))
	}
	r := requests[0]
	if r.Method != http.MethodPost || r.Path != "/gateway/loki/api/v1/push" || len(r.Query) != 0 {
		t.Fatalf("request line: %s %s %v", r.Method, r.Path, r.Query)
	}
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get(loki.TenantHeader) != "home" || r.Header.Get("User-Agent") != "kudurru/test" || !r.Compressed {
		t.Fatalf("headers: %v, compressed %v", r.Header, r.Compressed)
	}
	if r.Header.Get("Authorization") != "Bearer glc_token-123=" {
		t.Fatal("the token was not sent as a bearer token")
	}
	streams := sinktest.LokiStreams(t, r)
	type key struct{ node, class string }
	wantIDs := map[key][]uint64{
		{"node-a", "permitted"}: {1, 3}, {"node-a", "blocked"}: {2}, {"node-b", "cached"}: {4}, {"node-a", "error"}: {5},
	}
	if len(streams) != len(wantIDs) {
		t.Fatalf("%d streams, want %d: %+v", len(streams), len(wantIDs), streams)
	}
	byID := map[uint64]*querylog.Event{}
	for _, ev := range batch {
		byID[ev.ID] = ev
	}
	for _, st := range streams {
		k := key{st.Labels["node"], st.Labels["status_class"]}
		want, ok := wantIDs[k]
		if !ok {
			t.Fatalf("unexpected stream %v", st.Labels)
		}
		delete(wantIDs, k)
		if !reflect.DeepEqual(st.Labels, map[string]string{"job": "dns", "site": "home lab", "node": k.node, "status_class": k.class}) {
			t.Fatalf("stream labels %v: a label beyond the fixed ones, node and status_class makes streams per query", st.Labels)
		}
		if len(st.Entries) != len(want) {
			t.Fatalf("stream %v has %d entries, want %d", st.Labels, len(st.Entries), len(want))
		}
		for i, entry := range st.Entries {
			ev := byID[want[i]]
			if !entry.Time.Equal(ev.Time) {
				t.Fatalf("entry %d: time %s, want %s to the nanosecond", ev.ID, entry.Time, ev.Time)
			}
			// The line is exactly the documented record.
			raw, err := json.Marshal(record.From(ev))
			if err != nil {
				t.Fatal(err)
			}
			var wantLine map[string]any
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.UseNumber()
			if err := dec.Decode(&wantLine); err != nil {
				t.Fatal(err)
			}
			if got := entry.LineJSON(t); !reflect.DeepEqual(got, wantLine) {
				t.Fatalf("entry %d line:\n got %v\nwant %v", ev.ID, got, wantLine)
			}
			if strings.ContainsAny(entry.Line, "\n\r") {
				t.Fatalf("entry %d line spans lines: %q", ev.ID, entry.Line)
			}
			rec := record.From(ev)
			wantMeta := map[string]string{"domain": rec.Domain, "qtype": rec.QType, "status": rec.Status}
			if rec.Client != "" {
				wantMeta["client"] = rec.Client
			}
			if rec.ClientName != "" {
				wantMeta["client_name"] = rec.ClientName
			}
			if rec.TraceID != "" {
				wantMeta["trace_id"] = rec.TraceID
			}
			if !reflect.DeepEqual(entry.Metadata, wantMeta) {
				t.Fatalf("entry %d structured metadata %v, want %v", ev.ID, entry.Metadata, wantMeta)
			}
		}
	}
	// Spot checks against literal values.
	first := streams[0]
	if first.Labels["status_class"] != "permitted" || first.Entries[0].Metadata["trace_id"] != "abcd0000000000000000000000000000" ||
		first.Entries[0].Metadata["client"] != "192.0.2.7" || first.Entries[0].Metadata["client_name"] != "laptop" || first.Entries[0].Metadata["domain"] != "example.com" {
		t.Fatalf("first stream: %+v", first)
	}
	if !strings.Contains(string(r.Body), `"1794312000123456789"`) {
		t.Fatal("the timestamp is not a string of Unix nanoseconds")
	}
	if line := streams[1].Entries[0].Line; !strings.Contains(line, `"rule":"||ads.example.net^ \"quoted\" <b>&"`) {
		t.Fatalf("the line escapes more than JSON needs: %s", line)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestPushWithoutStructuredMetadata: for a Loki that does not accept it,
// entries are [timestamp, line] and the line still carries every field.
func TestPushWithoutStructuredMetadata(t *testing.T) {
	double := sinktest.New(t, http.StatusNoContent, false)
	off := false
	sink, err := loki.New(spec(t, config.QuerylogLoki{
		URL: double.URL, Username: "1234", PasswordRef: &config.CredentialRef{File: "/p"}, StructuredMetadata: &off, Compression: config.QuerylogCompressionNone,
	}), loki.Options{Resolve: files(map[string]string{"/p": "cloud-key"}), Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Open(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteBatch(t.Context(), events()[:2], querylog.PrivacyFull); err != nil {
		t.Fatal(err)
	}
	r := double.Requests()[0]
	if r.Compressed || r.Header.Get("Authorization") != sinktest.Basic("1234", "cloud-key") || r.Header.Get(loki.TenantHeader) != "" || r.Path != loki.PushPath {
		t.Fatalf("request: %s %v", r.Path, r.Header)
	}
	streams := sinktest.LokiStreams(t, r)
	if len(streams) != 2 {
		t.Fatalf("%d streams", len(streams))
	}
	for _, st := range streams {
		// Without labels in the settings the job label names kudurru.
		if st.Labels["job"] != "kudurru" || len(st.Labels) != 3 {
			t.Fatalf("default labels: %v", st.Labels)
		}
		for _, entry := range st.Entries {
			if entry.Metadata != nil {
				t.Fatalf("structured metadata was sent although it is turned off: %v", entry.Metadata)
			}
			if line := entry.LineJSON(t); line["domain"] == nil || line["client"] == nil || line["status"] == nil {
				t.Fatalf("the line lost fields: %v", line)
			}
		}
	}
	if strings.Count(string(r.Body), `["`) != 2 {
		t.Fatalf("entries are not two-element arrays: %s", r.Body)
	}
}

// TestPushCarriesOnlyWhatThePrivacyModeKeeps: labels and structured
// metadata are part of the export, so the transform reaches them too.
func TestPushCarriesOnlyWhatThePrivacyModeKeeps(t *testing.T) {
	double := sinktest.New(t, http.StatusNoContent, false)
	sink, err := loki.New(spec(t, config.QuerylogLoki{URL: double.URL, Compression: config.QuerylogCompressionNone}), loki.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Open(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	privacy, err := querylog.NewPrivacy(querylog.PrivacyOptions{Mode: querylog.PrivacyHideDomainsClients})
	if err != nil {
		t.Fatal(err)
	}
	batch := events()
	for _, ev := range batch {
		if !privacy.Apply(ev) {
			t.Fatal("event excluded")
		}
	}
	if err := sink.WriteBatch(t.Context(), batch, querylog.PrivacyHideDomainsClients); err != nil {
		t.Fatal(err)
	}
	body := string(double.Requests()[0].Body)
	for _, hidden := range []string{"example.com", "example.net", "192.0.2.", "2001:db8", "laptop"} {
		if strings.Contains(body, hidden) {
			t.Errorf("the push still carries %q", hidden)
		}
	}
	for _, st := range sinktest.LokiStreams(t, double.Requests()[0]) {
		for _, entry := range st.Entries {
			if entry.Metadata["domain"] != querylog.HiddenName || entry.Metadata["client"] != "0.0.0.0" || entry.Metadata["client_name"] != "" {
				t.Fatalf("structured metadata under hide_domains_clients: %v", entry.Metadata)
			}
		}
	}
}

// TestPushURL: the push path is appended to a base URL with or without a
// prefix, and not doubled when the settings already name the endpoint.
func TestPushURL(t *testing.T) {
	for raw, want := range map[string]string{
		"http://loki:3100":                                "http://loki:3100/loki/api/v1/push",
		"http://loki:3100/":                               "http://loki:3100/loki/api/v1/push",
		"https://logs.example/tenant-a":                   "https://logs.example/tenant-a/loki/api/v1/push",
		"https://logs.example/tenant-a/":                  "https://logs.example/tenant-a/loki/api/v1/push",
		"https://logs.example/loki/api/v1/push":           "https://logs.example/loki/api/v1/push",
		"https://logs.example/prefix/loki/api/v1/push/":   "https://logs.example/prefix/loki/api/v1/push",
		"https://logs-prod-012.grafana.net":               "https://logs-prod-012.grafana.net/loki/api/v1/push",
		"https://[2001:db8::1]:3100":                      "https://[2001:db8::1]:3100/loki/api/v1/push",
		"https://logs.example/loki":                       "https://logs.example/loki/loki/api/v1/push",
		"https://logs.example/api/prom/loki/api/v1/push/": "https://logs.example/api/prom/loki/api/v1/push",
	} {
		u, err := loki.PushURL(config.QuerylogLoki{URL: raw})
		if err != nil || u.String() != want {
			t.Errorf("%s: %v %v, want %s", raw, u, err, want)
		}
	}
}

// TestNewRefusesWhatTheSettingsValidatorRefuses: an entry that reached the
// factory around the validator is still not built.
func TestNewRefusesWhatTheSettingsValidatorRefuses(t *testing.T) {
	opts := loki.Options{Resolve: files(nil), Logger: slog.New(slog.DiscardHandler)}
	for name, tc := range map[string]struct {
		spec querylog.SinkSpec
		want string
	}{
		"no settings":        {querylog.SinkSpec{Name: "a", Type: "loki"}, "the loki settings are missing"},
		"malformed settings": {querylog.SinkSpec{Name: "a", Type: "loki", Config: json.RawMessage(`"x"`)}, "the loki settings are malformed"},
		"no URL":             {spec(t, config.QuerylogLoki{}), "loki.url is required"},
		"URL with query":     {spec(t, config.QuerylogLoki{URL: "https://loki.example/?token=x"}), "must not carry a query or fragment"},
		"label per query":    {spec(t, config.QuerylogLoki{URL: "https://loki.example", Labels: map[string]string{"node": "x"}}), `"node" is set by the sink for every query`},
		"tenant":             {spec(t, config.QuerylogLoki{URL: "https://loki.example", Tenant: "a b"}), "loki.tenant:"},
		"compression":        {spec(t, config.QuerylogLoki{URL: "https://loki.example", Compression: "br"}), "loki.compression is not gzip or none"},
		"basic and bearer": {spec(t, config.QuerylogLoki{URL: "https://loki.example", Username: "u", PasswordRef: &config.CredentialRef{File: "/p"}, BearerTokenRef: &config.CredentialRef{File: "/t"}}),
			"loki: use either username with passwordRef or bearerTokenRef, not both"},
	} {
		sink, err := loki.New(tc.spec, opts)
		if err == nil || sink != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	sink, err := loki.New(spec(t, config.QuerylogLoki{URL: "https://loki.example", BearerTokenRef: &config.CredentialRef{File: "/var/lib/kudurru/encryption.key"}}), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Open(t.Context(), nil, nil); err == nil || !strings.Contains(err.Error(), "Loki: bearer token: credential reference is not allowed") {
		t.Fatalf("a reference outside the node's grants: %v", err)
	}
}

// TestRefusalsAreSortedForTheHost: Loki's rate limit and server errors may
// be sent again, a rejected push may not, and its message is never logged.
func TestRefusalsAreSortedForTheHost(t *testing.T) {
	double := sinktest.New(t, http.StatusNoContent, false)
	sink, err := loki.New(spec(t, config.QuerylogLoki{URL: double.URL}), loki.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Open(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status int
		header http.Header
		again  bool
		wait   time.Duration
	}{
		{http.StatusTooManyRequests, http.Header{"Retry-After": {"3"}}, true, 3 * time.Second},
		{http.StatusInternalServerError, nil, true, 0},
		{http.StatusServiceUnavailable, nil, true, 0},
		{http.StatusBadRequest, nil, false, 0},
		{http.StatusRequestEntityTooLarge, nil, false, 0},
	} {
		double.Answer(func(int, sinktest.Request) sinktest.Answer {
			return sinktest.Answer{Status: tc.status, Header: tc.header, Body: `entry for stream '{job="kudurru"}' has timestamp too old: example.com`}
		})
		err := sink.WriteBatch(t.Context(), events(), querylog.PrivacyFull)
		if err == nil || err.Error() != "Loki answered HTTP "+strconv.Itoa(tc.status) {
			t.Errorf("%d: %v", tc.status, err)
		}
		if wait, again := querylog.RetryableWait(err); again != tc.again || wait != tc.wait {
			t.Errorf("%d: retryable %v after %s, want %v after %s", tc.status, again, wait, tc.again, tc.wait)
		}
	}
}

// TestDocumentedMappingMatchesThePush keeps the operator page in step with
// what is sent: the labels the sink adds, the structured metadata keys and
// the push path.
func TestDocumentedMappingMatchesThePush(t *testing.T) {
	page, err := os.ReadFile("../../../../docs/deploy/query-log-sinks.md")
	if err != nil {
		t.Fatal(err)
	}
	double := sinktest.New(t, http.StatusNoContent, false)
	sink, err := loki.New(spec(t, config.QuerylogLoki{URL: double.URL, Compression: config.QuerylogCompressionNone}), loki.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Open(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	full := &querylog.Event{ID: 1, Time: at, Node: "node-a", Client: netip.MustParseAddr("192.0.2.7"), ClientName: "laptop", Name: "example.com.", QType: 1, TraceID: [16]byte{1}}
	if err := sink.WriteBatch(t.Context(), []*querylog.Event{full}, querylog.PrivacyFull); err != nil {
		t.Fatal(err)
	}
	st := sinktest.LokiStreams(t, double.Requests()[0])[0]
	// The code terms of the two mapping rows of the page.
	terms := func(row string) []string {
		var out []string
		for line := range strings.Lines(string(page)) {
			if strings.HasPrefix(line, row) {
				for _, m := range codeTerm.FindAllStringSubmatch(line, -1) {
					out = append(out, m[1])
				}
			}
		}
		sort.Strings(out)
		return out
	}
	var sent []string
	for name := range st.Entries[0].Metadata {
		sent = append(sent, name)
	}
	sort.Strings(sent)
	if got := terms("| Structured metadata |"); !reflect.DeepEqual(got, sent) {
		t.Errorf("the page documents the structured metadata %v, the sink sends %v", got, sent)
	}
	// The row names the setting, the two labels the sink adds and the four
	// values of status_class.
	wantLabels := []string{"blocked", "cached", "error", "labels", "node", "permitted", "status_class"}
	if got := terms("| Stream labels |"); !reflect.DeepEqual(got, wantLabels) {
		t.Errorf("the page documents the stream labels as %v, want %v", got, wantLabels)
	}
	for label := range st.Labels {
		if label != "job" && label != config.QuerylogLokiLabelNode && label != config.QuerylogLokiLabelStatusClass {
			t.Errorf("the sink adds the stream label %s, which the page does not document", label)
		}
	}
	for _, class := range []pipeline.Status{pipeline.StatusForwarded, pipeline.StatusCached, pipeline.StatusBlockedList, pipeline.StatusServfailUpstream} {
		if !strings.Contains(strings.Join(wantLabels, " "), string(class.Class())) {
			t.Errorf("status class %s is not in the documented set", class.Class())
		}
	}
	if !strings.Contains(string(page), "`POST <url>"+loki.PushPath+"`") || !strings.Contains(string(page), "`"+loki.TenantHeader+"`") {
		t.Error("the page does not name the push path or the tenant header")
	}
}
