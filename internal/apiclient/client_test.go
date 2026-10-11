package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.Handler, opts Options) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewValidatesTheServer(t *testing.T) {
	cases := []struct {
		server, want, err string
	}{
		{"https://dns.example", "https://dns.example", ""},
		{"dns.example:8443", "https://dns.example:8443", ""},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080", ""},
		{"https://proxy.example/kudurru/", "https://proxy.example/kudurru", ""},
		{"", "", "no server URL"},
		{"ftp://dns.example", "", "not http or https"},
		{"https://", "", "has no host"},
	}
	for _, tc := range cases {
		c, err := New(tc.server, Options{})
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("New(%q) err = %v, want %q", tc.server, err, tc.err)
			}
			continue
		}
		if err != nil || c.Server() != tc.want {
			t.Errorf("New(%q) = %v, %v; want %s", tc.server, c, err, tc.want)
		}
	}
	if _, err := New("https://dns.example", Options{CAFile: "/nonexistent.pem"}); err == nil {
		t.Error("a missing caFile must fail")
	}
}

func TestURL(t *testing.T) {
	c, _ := New("https://proxy.example/kudurru", Options{})
	cases := []struct {
		path  string
		query url.Values
		want  string
	}{
		{"/zones", nil, "https://proxy.example/kudurru/api/v1/zones"},
		{"zones", nil, "https://proxy.example/kudurru/api/v1/zones"},
		{"/api/v2/x", nil, "https://proxy.example/kudurru/api/v2/x"},
		{"/objects/dnszones/" + Seg("a/b c"), nil, "https://proxy.example/kudurru/api/v1/objects/dnszones/a%2Fb%20c"},
		{"/queries?limit=5", url.Values{"client": {"10.0.0.1"}}, "https://proxy.example/kudurru/api/v1/queries?client=10.0.0.1&limit=5"},
	}
	for _, tc := range cases {
		if got := c.URL(tc.path, tc.query); got != tc.want {
			t.Errorf("URL(%q) = %s, want %s", tc.path, got, tc.want)
		}
	}
	if got := ObjectPath("dnszones", "", "home"); got != "/objects/dnszones/home" {
		t.Errorf("cluster-scoped path %s", got)
	}
	if got := ObjectPath("policies", "default", "kids"); got != "/objects/policies/default/kids" {
		t.Errorf("namespaced path %s", got)
	}
	if got := ObjectPath("policies", "default", ""); got != "/objects/policies" {
		t.Errorf("kind path %s", got)
	}
}

func TestErrorDecoding(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		header  http.Header
		body    string
		want    Error
		wantStr string
	}{
		{"validation", 400, nil, `{"error":{"key":"invalid","message":"spec.zoneName: required","hint":""},"took_ms":0.4}`,
			Error{StatusCode: 400, Key: "invalid", Message: "spec.zoneName: required", TookMS: 0.4}, "invalid: spec.zoneName: required"},
		{"forbidden with missing object", 403, nil, `{"error":{"key":"forbidden","message":"not allowed","hint":{"missing":{"resource":"zones","verb":"write","scope":"zone:home"}}}}`,
			Error{StatusCode: 403, Key: "forbidden", Message: "not allowed", Missing: &Missing{"zones", "write", "zone:home"}}, "forbidden: not allowed (missing zones.write on zone:home)"},
		{"forbidden with missing as JSON string", 403, nil, `{"error":{"key":"forbidden","message":"no","hint":"{\"missing\":{\"resource\":\"cache\",\"verb\":\"flush\",\"scope\":\"\"}}"}}`,
			Error{StatusCode: 403, Key: "forbidden", Message: "no", Missing: &Missing{"cache", "flush", ""}}, "forbidden: no (missing cache.flush on cluster)"},
		{"managed by", 409, nil, `{"error":{"key":"managed_by","message":"owned by a file","hint":"file:objects.d/zones.yaml"}}`,
			Error{StatusCode: 409, Key: "managed_by", Message: "owned by a file", Hint: "file:objects.d/zones.yaml"}, "managed_by: owned by a file (managed by file:objects.d/zones.yaml)"},
		{"rate limited", 429, http.Header{"Retry-After": {"7"}}, `{"error":{"key":"rate_limited","message":"slow down"}}`,
			Error{StatusCode: 429, Key: "rate_limited", Message: "slow down", RetryAfter: 7 * time.Second}, "rate_limited: slow down (retry after 7s)"},
		{"read only", 503, nil, `{"error":{"key":"read_only","message":"follower","hint":"write to node-a"}}`,
			Error{StatusCode: 503, Key: "read_only", Message: "follower", Hint: "write to node-a"}, "read_only: follower (hint: write to node-a)"},
		{"not the envelope", 502, nil, "<html>Bad Gateway</html>\nmore",
			Error{StatusCode: 502, Message: "<html>Bad Gateway</html>"}, "bad_gateway: <html>Bad Gateway</html>"},
		{"empty body", 404, nil, "",
			Error{StatusCode: 404, Message: "404 Not Found"}, "not_found: 404 Not Found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header()[k] = v
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}), Options{})
			err := c.Get(context.Background(), "/x", nil, nil)
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if e.StatusCode != tc.want.StatusCode || e.Key != tc.want.Key || e.Message != tc.want.Message || e.Hint != tc.want.Hint ||
				e.RetryAfter != tc.want.RetryAfter || e.TookMS != tc.want.TookMS {
				t.Errorf("error = %+v, want %+v", *e, tc.want)
			}
			if (e.Missing == nil) != (tc.want.Missing == nil) || (e.Missing != nil && *e.Missing != *tc.want.Missing) {
				t.Errorf("missing = %+v, want %+v", e.Missing, tc.want.Missing)
			}
			if e.Error() != tc.wantStr {
				t.Errorf("Error() = %q, want %q", e.Error(), tc.wantStr)
			}
			if tc.want.Key != "" && !IsKey(err, tc.want.Key) {
				t.Error("IsKey false")
			}
			if !IsStatus(err, tc.status) {
				t.Error("IsStatus false")
			}
		})
	}
}

func TestAuthHeaders(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]http.Header{}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = r.Header.Clone()
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "kudurru_session", Value: "s1", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}) //nolint:gosec // G124: httptest serves plain HTTP
			_, _ = io.WriteString(w, `{}`)
		case "/api/v1/auth/me":
			_, _ = io.WriteString(w, `{"principal":{"kind":"User","id":"u1","name":"admin"},"roles":["admin"],"csrf":"c-1","permissions":{"zones":["read","write"]}}`)
		case "/api/v1/auth/tokens":
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"id":"abc","name":"cli","secret":"kdr_abc_def"}`)
		default:
			w.WriteHeader(204)
		}
	}), Options{UserAgent: "test-agent"})
	ctx := context.Background()
	me, err := c.Login(ctx, "admin", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !me.Can("zones", "write") || me.Can("zones", "delete") {
		t.Errorf("permissions %v", me.Permissions)
	}
	tok, err := c.CreateToken(ctx, CreateTokenRequest{Name: "cli"})
	if err != nil || tok.Secret != "kdr_abc_def" || tok.ID != "abc" {
		t.Fatalf("token %+v %v", tok, err)
	}
	h := seen["POST /api/v1/auth/tokens"]
	if h.Get(CSRFHeader) != "c-1" || !strings.Contains(h.Get("Cookie"), "kudurru_session=s1") || h.Get("Authorization") != "" {
		t.Errorf("token request headers %v", h)
	}
	if seen["GET /api/v1/auth/me"].Get(CSRFHeader) != "" {
		t.Error("CSRF sent on a GET")
	}
	if seen["POST /api/v1/auth/login"].Get("User-Agent") != "test-agent" {
		t.Error("user agent")
	}
	c.SetToken("kdr_abc_def")
	if err := c.Delete(ctx, "/objects/dnszones/x", nil); err != nil {
		t.Fatal(err)
	}
	if got := seen["DELETE /api/v1/objects/dnszones/x"].Get("Authorization"); got != "Bearer kdr_abc_def" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestTokenSecretFieldNames(t *testing.T) {
	for _, body := range []string{`{"id":"a","token":"kdr_x"}`, `{"id":"a","secret":"kdr_x"}`, `{"id":"a","value":"kdr_x"}`} {
		var tok Token
		if err := json.Unmarshal([]byte(body), &tok); err != nil || tok.Secret != "kdr_x" || tok.ID != "a" {
			t.Errorf("%s → %+v %v", body, tok, err)
		}
	}
}

func TestPagesFollowContinue(t *testing.T) {
	var calls []string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.RawQuery)
		switch r.URL.Query().Get("continue") {
		case "":
			_, _ = io.WriteString(w, `{"items":[{"n":1},{"n":2}],"continue":"t2","took_ms":1}`)
		case "t2":
			_, _ = io.WriteString(w, `{"items":[{"n":3}],"continue":"t3"}`)
		case "t3":
			_, _ = io.WriteString(w, `{"items":[]}`)
		}
	}), Options{})
	items, err := c.ListObjects(context.Background(), "policies", ListOptions{Namespace: "default", LabelSelector: "a=b", PageSize: 2})
	if err != nil || len(items) != 3 {
		t.Fatalf("items %d err %v", len(items), err)
	}
	if calls[0] != "labelSelector=a%3Db&limit=2&namespace=default" || calls[1] != "continue=t2&labelSelector=a%3Db&limit=2&namespace=default" || len(calls) != 3 {
		t.Errorf("calls %v", calls)
	}
}

func TestPagesStopOnRepeatedToken(t *testing.T) {
	n := 0
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		_, _ = io.WriteString(w, `{"items":[{}],"continue":"same"}`)
	}), Options{})
	items, err := c.ListAll(context.Background(), "/x", nil, 0)
	if err != nil || len(items) != 2 || n != 2 {
		t.Fatalf("items %d calls %d err %v", len(items), n, err)
	}
}

func TestApplyResultShapes(t *testing.T) {
	for _, body := range []string{
		`[{"kind":"DNSZone","name":"a","result":"created"}]`,
		`{"items":[{"kind":"DNSZone","name":"a","result":"created"}],"took_ms":1}`,
		`{"results":[{"kind":"DNSZone","name":"a","result":"created"}]}`,
	} {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			if string(b) != `[{"kind":"DNSZone"}]` || r.Header.Get("Content-Type") != "application/json" {
				w.WriteHeader(400)
				return
			}
			_, _ = io.WriteString(w, body)
		}), Options{})
		res, err := c.Apply(context.Background(), []json.RawMessage{json.RawMessage(`{"kind":"DNSZone"}`)})
		if err != nil || len(res) != 1 || res[0].Result != "created" {
			t.Errorf("%s → %+v %v", body, res, err)
		}
	}
}

func TestPutObjectSendsIfMatch(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.Header.Get("If-Match") != "41" {
			w.WriteHeader(412)
			_, _ = io.WriteString(w, `{"error":{"key":"conflict","message":"resourceVersion mismatch"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"42"}}`)
	}), Options{})
	out, err := c.PutObject(context.Background(), "dnszones", "", "home", "41", map[string]any{"kind": "DNSZone"})
	if err != nil || !strings.Contains(string(out), `"42"`) {
		t.Fatalf("put %s %v", out, err)
	}
	if _, err := c.PutObject(context.Background(), "dnszones", "", "home", "40", map[string]any{}); !IsStatus(err, 412) {
		t.Fatalf("stale put err = %v", err)
	}
}

func TestDoBodies(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(b)
	}), Options{})
	var raw []byte
	if err := c.Post(context.Background(), "/echo", nil, []byte(`{"a":1}`), &raw); err != nil || string(raw) != `{"a":1}` {
		t.Fatalf("[]byte body: %s %v", raw, err)
	}
	var sb strings.Builder
	if err := c.Post(context.Background(), "/echo", nil, strings.NewReader("plain"), &sb); err != nil || sb.String() != "plain" {
		t.Fatalf("reader body: %s %v", sb.String(), err)
	}
	var m map[string]int
	if err := c.Post(context.Background(), "/echo", nil, map[string]int{"b": 2}, &m); err != nil || m["b"] != 2 {
		t.Fatalf("struct body: %v %v", m, err)
	}
	// An empty 200 body decodes to nothing without an error.
	if err := c.Post(context.Background(), "/echo", nil, nil, &m); err != nil {
		t.Fatalf("empty body: %v", err)
	}
}

func TestParseSSE(t *testing.T) {
	stream := ": comment\r\nretry: 250\r\n\r\nid: 1\nevent: query\ndata: {\"a\":1}\n\n" +
		"data: line one\ndata: line two\n\n" +
		"id: 3\ndata:no-space\n\n" +
		"data: unterminated"
	type got struct {
		ev    Event
		retry time.Duration
	}
	var events []got
	err := ParseSSE(strings.NewReader(stream), func(ev Event, r time.Duration) error {
		events = append(events, got{ev, r})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []got{
		{Event{}, 250 * time.Millisecond},
		{Event{ID: "1", Event: "query", Data: []byte(`{"a":1}`)}, 250 * time.Millisecond},
		{Event{Data: []byte("line one\nline two")}, 250 * time.Millisecond},
		{Event{ID: "3", Data: []byte("no-space")}, 250 * time.Millisecond},
	}
	if len(events) != len(want) {
		t.Fatalf("events %+v", events)
	}
	for i := range want {
		if events[i].ev.ID != want[i].ev.ID || events[i].ev.Event != want[i].ev.Event || string(events[i].ev.Data) != string(want[i].ev.Data) || events[i].retry != want[i].retry {
			t.Errorf("event %d = %+v, want %+v", i, events[i], want[i])
		}
	}
}

func TestSubscribeReconnectsWithLastEventID(t *testing.T) {
	var (
		mu      sync.Mutex
		lastIDs []string
		conns   int
	)
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		n := conns
		lastIDs = append(lastIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		if r.Header.Get("Accept") != "text/event-stream" {
			w.WriteHeader(406)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			_, _ = fmt.Fprint(w, "retry: 5\n\nid: 10\ndata: a\n\nid: 11\ndata: b\n\n")
		case 2:
			w.WriteHeader(503) // transient: retried
		default:
			_, _ = fmt.Fprint(w, "id: 12\ndata: c\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}), Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var data []string
	err := c.Subscribe(ctx, "/queries/stream", nil, SubscribeOptions{LastEventID: "9", Reconnect: true, RetryDelay: time.Millisecond}, func(ev Event) error {
		data = append(data, string(ev.Data))
		if ev.ID == "12" {
			return ErrStopSubscription
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(data, ",") != "a,b,c" {
		t.Errorf("data %v", data)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(lastIDs, ",") != "9,11,11" {
		t.Errorf("Last-Event-ID per connection %v", lastIDs)
	}
}

func TestSubscribeErrors(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/denied":
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"key":"unauthenticated","message":"no"}}`)
		case "/api/v1/json":
			_, _ = io.WriteString(w, `{}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: x\n\n")
		}
	}), Options{})
	ctx := context.Background()
	if err := c.Subscribe(ctx, "/denied", nil, SubscribeOptions{Reconnect: true}, func(Event) error { return nil }); !IsKey(err, "unauthenticated") {
		t.Errorf("401 err = %v", err)
	}
	if err := c.Subscribe(ctx, "/json", nil, SubscribeOptions{}, func(Event) error { return nil }); err == nil || !strings.Contains(err.Error(), "not text/event-stream") {
		t.Errorf("wrong content type err = %v", err)
	}
	// Without Reconnect a clean end of the stream is not an error.
	n := 0
	if err := c.Subscribe(ctx, "/ok", nil, SubscribeOptions{}, func(Event) error { n++; return nil }); err != nil || n != 1 {
		t.Errorf("clean end: n=%d err=%v", n, err)
	}
	boom := errors.New("boom")
	if err := c.Subscribe(ctx, "/ok", nil, SubscribeOptions{Reconnect: true}, func(Event) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("handler error = %v", err)
	}
}
