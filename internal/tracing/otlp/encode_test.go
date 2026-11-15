package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/tracing"
)

// finished builds a finished, kept trace with a root, a stage and a failed
// upstream span.
func finished(t *testing.T) *tracing.Trace {
	t.Helper()
	tr, err := tracing.New(tracing.Config{Ratio: 1, AcceptIncoming: true, Exporter: &tracing.Memory{}})
	if err != nil {
		t.Fatal(err)
	}
	m := new(dns.Msg)
	m.SetQuestion("www.example.org.", dns.TypeA)
	m.SetEdns0(1232, false)
	parentTrace := tracing.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	parentSpan := tracing.SpanID{0xa, 0xb, 0xc, 0xd, 0xe, 0xf, 1, 2}
	opt := m.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_LOCAL{Code: tracing.DefaultOptionCode, Data: tracing.AppendOption(nil, parentTrace, parentSpan, 1)})
	start := time.Unix(1_700_000_000, 123)
	trace, _ := tr.Begin(m, true, start)
	stage := trace.Start(tracing.Root, "kudurru.upstream", tracing.KindInternal, start.Add(time.Millisecond))
	up := trace.Start(stage, "kudurru.upstream.exchange", tracing.KindClient, start.Add(2*time.Millisecond))
	trace.SetStr(up, "kudurru.upstream", "default/one")
	trace.SetInt(up, "kudurru.traceparent_sent", 1)
	trace.End(up, start.Add(3*time.Millisecond), "timeout")
	trace.SetStr(tracing.Root, "dns.question.name", "www.example.org.")
	trace.Decide(0, false)
	trace.Finish(start.Add(4 * time.Millisecond))
	return trace
}

// TestEncodingDecodesWithTheGeneratedTypes checks the hand-written
// protobuf against the OTLP Go package (test-only: the binary never links
// it).
func TestEncodingDecodesWithTheGeneratedTypes(t *testing.T) {
	trace := finished(t)
	b := AppendRequest(nil, Resource{ServiceName: "kudurru", Version: "v0.2.0", Instance: "dns1"}, []*tracing.Trace{trace, trace})
	var got tracepb.TracesData // same wire form as ExportTraceServiceRequest
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.ResourceSpans) != 1 {
		t.Fatalf("%d resource spans", len(got.ResourceSpans))
	}
	rs := got.ResourceSpans[0]
	res := map[string]string{}
	for _, kv := range rs.Resource.Attributes {
		res[kv.Key] = kv.Value.GetStringValue()
	}
	if res["service.name"] != "kudurru" || res["service.version"] != "v0.2.0" || res["service.instance.id"] != "dns1" {
		t.Fatalf("resource %v", res)
	}
	scope := rs.ScopeSpans[0]
	if scope.Scope.Name != scopeName || scope.Scope.Version != "v0.2.0" || len(scope.Spans) != 6 {
		t.Fatalf("scope %v with %d spans", scope.Scope, len(scope.Spans))
	}
	want := trace.Spans()
	for i, s := range scope.Spans[:3] {
		w := want[i]
		id := trace.ID()
		if !bytes.Equal(s.TraceId, id[:]) || !bytes.Equal(s.SpanId, w.ID[:]) || !bytes.Equal(s.ParentSpanId, w.Parent[:]) {
			t.Fatalf("span %d ids", i)
		}
		if s.Name != w.Name || int(s.Kind) != int(w.Kind) || s.StartTimeUnixNano != uint64(w.Start.UnixNano()) || s.EndTimeUnixNano != uint64(w.End.UnixNano()) {
			t.Fatalf("span %d: %v", i, s)
		}
	}
	root, exchange := scope.Spans[0], scope.Spans[2]
	if root.Kind != tracepb.Span_SPAN_KIND_SERVER || root.Status.Code != tracepb.Status_STATUS_CODE_OK || root.Attributes[0].Key != "dns.question.name" {
		t.Fatalf("root %v", root)
	}
	if exchange.Status.Code != tracepb.Status_STATUS_CODE_ERROR || exchange.Status.Message != "timeout" {
		t.Fatalf("exchange status %v", exchange.Status)
	}
	if exchange.Attributes[0].Value.GetStringValue() != "default/one" || exchange.Attributes[1].Value.GetIntValue() != 1 {
		t.Fatalf("exchange attributes %v", exchange.Attributes)
	}
	if scope.Spans[1].Status.Code != tracepb.Status_STATUS_CODE_UNSET {
		t.Fatal("an internal span without error must leave the status unset")
	}
}

// collector is an OTLP/HTTP double that decodes every request.
type collector struct {
	mu       sync.Mutex
	requests []*tracepb.TracesData
	headers  []http.Header
	status   int
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		z, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "gzip", http.StatusBadRequest)
			return
		}
		body = z
	}
	raw, _ := io.ReadAll(body)
	var td tracepb.TracesData
	if err := proto.Unmarshal(raw, &td); err != nil {
		http.Error(w, "decode", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.requests = append(c.requests, &td)
	c.headers = append(c.headers, r.Header.Clone())
	status := c.status
	c.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
}

func TestExporterPostsOTLPHTTP(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/v1/traces")
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("s3cret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolve := func(_ context.Context, ref config.CredentialRef) ([]byte, error) { return os.ReadFile(ref.File) }
	e, err := New(Options{URL: u, Resource: Resource{Version: "test"}, Headers: map[string]string{"X-Scope-OrgID": "home"}, Bearer: &config.CredentialRef{File: token}, Gzip: true, Resolve: resolve})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.Export(t.Context(), []*tracing.Trace{finished(t)}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	if len(c.requests) != 1 || len(c.requests[0].ResourceSpans[0].ScopeSpans[0].Spans) != 3 {
		t.Fatalf("collector got %v", c.requests)
	}
	h := c.headers[0]
	if h.Get("Content-Type") != "application/x-protobuf" || h.Get("X-Scope-Orgid") != "home" || h.Get("Authorization") != "Bearer s3cret-token" {
		t.Fatalf("headers %v", h)
	}
	c.status = http.StatusServiceUnavailable
	c.mu.Unlock()
	err = e.Export(t.Context(), []*tracing.Trace{finished(t)})
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("a 503 must fail without the credential: %v", err)
	}
	if _, err := New(Options{URL: u, Headers: map[string]string{"authorization": "x"}}); err == nil {
		t.Fatal("an Authorization header setting was accepted")
	}
}
