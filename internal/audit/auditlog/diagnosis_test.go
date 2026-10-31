package auditlog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/robertherbaugh/kudurru/internal/audit"
)

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type measuredBody struct {
	io.ReadCloser
	read *atomic.Int64
}

func (b measuredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func httpProbe(t *testing.T, handler http.HandlerFunc) (*Probe, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var calls, read atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/prefix/apis/audit.kudurru.io/v1alpha1/namespaces/release/auditlogs" || r.Header.Get("Accept") != "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1" || r.Header.Get("X-Test-Guard") != "inherited" {
			t.Error("probe changed scope, metadata negotiation or provided guard")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	probe, err := NewProbe(&rest.Config{Host: srv.URL + "/prefix", WrapTransport: func(rt http.RoundTripper) http.RoundTripper {
		return diagnosticTransport(func(r *http.Request) (*http.Response, error) {
			guarded := r.Clone(r.Context())
			guarded.Header.Set("X-Test-Guard", "inherited")
			response, err := rt.RoundTrip(guarded)
			if err == nil {
				response.Body = measuredBody{response.Body, &read}
			}
			return response, err
		})
	}}, "release")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("constructor initiated acquisition")
	}
	return probe, &calls, &read
}

func diagnosticJSON(count int, continuation, rv string, padding int) string {
	item := `{"metadata":{}}`
	if padding > 0 {
		item = `{"metadata":{"annotations":{"test":"` + strings.Repeat("x", padding) + `"}}}`
	}
	items := ""
	if count > 0 {
		items = strings.Repeat(item+",", count-1) + item
	}
	return fmt.Sprintf(`{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1","metadata":{"resourceVersion":%q,"continue":%q},"items":[%s]}`, rv, continuation, items)
}

func TestAuditLogProbeAvailabilityAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		status int
		state  string
	}{
		{404, DiagnosticMissing}, {401, DiagnosticForbidden}, {403, DiagnosticForbidden},
		{503, DiagnosticUnavailable}, {406, DiagnosticUnavailable}, {410, DiagnosticIndeterminate},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			var recovered atomic.Bool
			probe, calls, read := httpProbe(t, func(w http.ResponseWriter, _ *http.Request) {
				if !recovered.Load() {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, strings.Repeat("private-error-body", 4096))
					return
				}
				_, _ = io.WriteString(w, diagnosticJSON(0, "", "1", 0))
			})
			got := probe.Diagnose(t.Context())
			if got.State != tc.state || got.Exact || got.Count != 0 || calls.Load() != 1 || read.Load() != 0 {
				t.Fatalf("incorrect bounded failure: %+v, requests=%d, bytes=%d", got, calls.Load(), read.Load())
			}
			recovered.Store(true)
			got = probe.Diagnose(t.Context())
			if got.State != DiagnosticAvailable || !got.Exact || got.Namespace != "release" || calls.Load() != 2 {
				t.Fatalf("probe did not recover: %+v", got)
			}
		})
	}
}

func TestAuditLogProbeCountBoundaries(t *testing.T) {
	for _, objects := range []int{0, 1, 49_999, 50_000, 50_001, 100_000} {
		t.Run(strconv.Itoa(objects), func(t *testing.T) {
			observed := 0
			probe, calls, _ := httpProbe(t, func(w http.ResponseWriter, r *http.Request) {
				limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
				if limit <= 0 || limit > probePageSize || limit > RecommendedObjects+1-observed {
					t.Error("unbounded request limit")
				}
				if r.URL.Query().Get("continue") != continuationFor(observed) {
					t.Error("incorrect continuation")
				}
				n := min(limit, objects-observed)
				observed += n
				next := ""
				if observed < objects {
					next = continuationFor(observed)
				}
				_, _ = io.WriteString(w, diagnosticJSON(n, next, "100", 0))
			})
			got := probe.Diagnose(t.Context())
			if got.Count != min(objects, RecommendedObjects+1) || calls.Load() > probeMaxPages {
				t.Fatalf("count/cap: %+v, requests=%d", got, calls.Load())
			}
			if objects > RecommendedObjects {
				if got.State != DiagnosticOverLimit || got.Exact {
					t.Fatalf("capped count was not a lower bound: %+v", got)
				}
			} else if got.State != DiagnosticAvailable || !got.Exact {
				t.Fatalf("complete count: %+v", got)
			}
		})
	}
}

func continuationFor(count int) string {
	if count == 0 {
		return ""
	}
	return strconv.Itoa(count)
}

func TestAuditLogProbeIncompleteNeverHealthy(t *testing.T) {
	for _, mode := range []string{"repeated-token", "changed-snapshot", "expired", "malformed", "full-object-fallback", "missing-items", "too-many-items", "large-token", "large-annotations", "byte-budget", "page-budget", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			requests := 0
			probe, calls, read := httpProbe(t, func(w http.ResponseWriter, _ *http.Request) {
				requests++
				body := diagnosticJSON(1, "next", "1", 0)
				switch mode {
				case "changed-snapshot":
					if requests > 1 {
						body = diagnosticJSON(1, "", "2", 0)
					}
				case "expired":
					if requests > 1 {
						w.WriteHeader(http.StatusGone)
						return
					}
				case "malformed":
					body = `{"items":`
				case "full-object-fallback":
					body = strings.Replace(diagnosticJSON(1, "", "1", 0), "PartialObjectMetadataList", "AuditLogList", 1)
				case "missing-items":
					body = `{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1","metadata":{"resourceVersion":"1"}}`
				case "too-many-items":
					body = diagnosticJSON(probePageSize+1, "", "1", 0)
				case "large-token":
					body = diagnosticJSON(1, strings.Repeat("t", probeTokenBytes+1), "1", 0)
				case "large-annotations":
					body = diagnosticJSON(1, "", "1", probePageBytes)
				case "byte-budget":
					body = diagnosticJSON(probePageSize, strconv.Itoa(requests), "1", 2048)
				case "page-budget":
					body = diagnosticJSON(0, strconv.Itoa(requests), "1", 0)
				case "redirect":
					w.Header().Set("Location", "/private-fallback")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				_, _ = io.WriteString(w, body)
			})
			byteBudget := probeTotalBytes
			if mode == "byte-budget" {
				byteBudget = 3 * probePageBytes
			}
			got := probe.diagnose(t.Context(), byteBudget)
			wantState := DiagnosticIndeterminate
			if mode == "redirect" {
				wantState = DiagnosticUnavailable
			}
			if got.State != wantState || got.Exact || calls.Load() > probeMaxPages || read.Load() > int64(byteBudget)+1 {
				t.Fatalf("incomplete scan was not bounded/explicit: %+v, requests=%d, bytes=%d", got, calls.Load(), read.Load())
			}
			if mode == "page-budget" && calls.Load() != probeMaxPages {
				t.Fatal("did not exercise page cap")
			}
			if mode == "byte-budget" && read.Load() != int64(byteBudget)+1 {
				t.Fatal("did not exercise total byte cap")
			}
			if mode == "large-annotations" && read.Load() != probePageBytes+1 {
				t.Fatal("did not exercise pre-decode annotation cap")
			}
			if mode == "redirect" && calls.Load() != 1 {
				t.Fatal("redirect was followed")
			}
		})
	}
}

func TestAuditLogProbeCancellationAndDeadline(t *testing.T) {
	started := make(chan struct{})
	probe, calls, _ := httpProbe(t, func(_ http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result := probe.Diagnose(ctx); result.Exact || calls.Load() != 0 {
		t.Fatal("canceled request initiated acquisition")
	}
	ctx, cancel = context.WithCancel(t.Context())
	done := make(chan Diagnostic, 1)
	go func() { done <- probe.Diagnose(ctx) }()
	<-started
	cancel()
	select {
	case result := <-done:
		if result.State != DiagnosticUnavailable || result.Exact {
			t.Fatal("canceled scan became healthy")
		}
	case <-time.After(time.Second):
		t.Fatal("probe ignored cancellation")
	}
	// Verify the fixed whole-scan deadline without waiting for it to expire.
	observed := false
	cfg := &rest.Config{Host: "http://127.0.0.1", WrapTransport: func(http.RoundTripper) http.RoundTripper {
		return diagnosticTransport(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) > probeTimeout {
				t.Error("scan lacks fixed deadline")
			}
			observed = true
			return nil, context.DeadlineExceeded
		})
	}}
	bounded, err := NewProbe(cfg, "release")
	if err != nil {
		t.Fatal(err)
	}
	if result := bounded.Diagnose(t.Context()); result.Exact || !observed {
		t.Fatal("deadline path was not exercised")
	}
}

func TestAuditLogProbeRealMetadataScope(t *testing.T) {
	ns, other := namespace(t), namespace(t)
	for _, scope := range []string{ns, other} {
		sp := openSpool(t, "probe")
		if err := sp.Record(t.Context(), audit.Event{Action: "test", Result: audit.ResultSuccess}); err != nil {
			t.Fatal(err)
		}
		ship(t, &Target{Client: env.client, Namespace: scope}, sp)
	}
	probe, err := NewProbe(env.cfg, ns)
	if err != nil {
		t.Fatal(err)
	}
	got := probe.Diagnose(t.Context())
	if got.State != DiagnosticAvailable || !got.Exact || got.Count != 1 || got.Namespace != ns {
		t.Fatalf("real namespaced metadata count: %+v", got)
	}
}

func TestAuditLogProbeConcurrentWorkIsBounded(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	probe, calls, _ := httpProbe(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, diagnosticJSON(0, "", "1", 0))
	})
	done := make(chan Diagnostic, 1)
	go func() { done <- probe.Diagnose(t.Context()) }()
	<-started
	second := probe.Diagnose(t.Context())
	if second.State != DiagnosticUnavailable || second.Exact || calls.Load() != 1 {
		t.Fatal("concurrent observation started another scan")
	}
	close(release)
	if first := <-done; first.State != DiagnosticAvailable || !first.Exact {
		t.Fatal("original observation did not complete")
	}
}

func TestAuditLogProbeExplicitEmptyLists(t *testing.T) {
	for _, items := range []string{"[]", "null"} {
		t.Run(items, func(t *testing.T) {
			probe, _, _ := httpProbe(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"kind":"PartialObjectMetadataList","apiVersion":"meta.k8s.io/v1","metadata":{"resourceVersion":"1"},"items":%s}`, items)
			})
			got := probe.Diagnose(t.Context())
			if got.State != DiagnosticAvailable || !got.Exact || got.Count != 0 {
				t.Fatalf("explicit empty metadata list: %+v", got)
			}
		})
	}
}

func TestAuditLogProbeInvalidConfigurationDoesNotAcquire(t *testing.T) {
	for _, tc := range []struct {
		name, host, namespace string
	}{
		{"no-host", "", "release"},
		{"no-namespace", "http://127.0.0.1", ""},
		{"invalid-namespace", "http://127.0.0.1", "../../private"},
		{"invalid-endpoint", "http://[", "release"},
		{"endpoint-userinfo", "http://private@127.0.0.1", "release"},
		{"endpoint-query", "http://127.0.0.1?private", "release"},
		{"endpoint-fragment", "http://127.0.0.1#private", "release"},
		{"endpoint-scheme", "file:///private", "release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &rest.Config{Host: tc.host, WrapTransport: func(http.RoundTripper) http.RoundTripper {
				t.Error("invalid configuration constructed transport")
				return nil
			}}
			if probe, err := NewProbe(cfg, tc.namespace); err == nil || probe != nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if probe, err := NewProbe(nil, "release"); err == nil || probe != nil {
		t.Fatal("nil configuration accepted")
	}
}
