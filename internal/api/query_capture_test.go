package api

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	logstage "github.com/robertherbaugh/kudurru/internal/stages/log"
)

type queryLease struct {
	events *[]string
	value  any
}

func (l *queryLease) Value() any { return l.value }
func (l *queryLease) Clone() (pipeline.ServingLease, error) {
	return nil, errors.New("unexpected detached query")
}
func (l *queryLease) Release() { *l.events = append(*l.events, "lease") }

type queryStage func(*pipeline.Request) (*pipeline.Response, error)

func (queryStage) Name() string { return "upstream" }
func (f queryStage) Serve(r *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	return f(r)
}

type queryCollector struct{ events int }

func (c *queryCollector) Enqueue(*querylog.Event) bool { c.events++; return true }

func TestDNSQueryCaptureOwnsLeaseAfterResponse(t *testing.T) {
	for _, kind := range []string{"answer", "drop", "error", "nil", "stream", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			var events []string
			lease := &queryLease{events: &events, value: "original"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			chain := pipeline.MustNew(queryStage(func(req *pipeline.Request) (*pipeline.Response, error) {
				if req.Serving != lease || !req.Client.Synthetic || req.Client.TSIGKey != "" || req.Msg.IsTsig() != nil || req.Client.ClientID != "api-dig" || req.Client.Proto != pipeline.ProtoDoT || req.Client.Addr.Addr() != netip.MustParseAddr("192.0.2.8") {
					t.Fatal("lost original serving/client semantics", req)
				}
				deadline, ok := req.Ctx.Deadline()
				if !ok || deadline.After(time.Now().Add(5*time.Second)) {
					t.Fatal("missing bounded context")
				}
				if kind == "nil" {
					return nil, nil
				}
				resp, _ := answerStage{}.Serve(req, nil)
				resp.AddCleanup(func() {
					if len(events) != 0 {
						t.Error("lease released before response")
					}
					events = append(events, "response")
				})
				switch kind {
				case "drop":
					resp.Msg, resp.Status = nil, pipeline.StatusBlockedRPZ
					return resp, pipeline.ErrDrop
				case "error":
					return resp, errors.New("network failure")
				case "stream":
					resp.Transfer = func(func(*dns.Msg) bool) { t.Error("HTTP diagnostic iterated a stream") }
				case "cancel":
					cancel()
					return resp, req.Ctx.Err()
				}
				return resp, nil
			}))
			chain.SetCapture(func(*pipeline.Request) (pipeline.ServingLease, error) {
				t.Fatal("recaptured a successor")
				return nil, nil
			})
			collector := &queryCollector{}
			chain.Observe(&logstage.Observer{Collector: collector})
			h := newHarness(t, func(o *Options) {
				o.Chain, o.DNSQueryCapture = chain, func(*pipeline.Request) (pipeline.ServingLease, error) { return lease, nil }
			})
			out, err := h.srv.dnsQuery(ctx, &dnsQueryInput{Body: DNSQueryRequest{Name: "example.", Client: "192.0.2.8", Proto: "dot"}})
			if err != nil || out == nil {
				t.Fatal(out, err)
			}
			want := []string{"response", "lease"}
			if kind == "nil" {
				want = []string{"lease"}
			}
			if !reflect.DeepEqual(events, want) || collector.events != 0 {
				t.Fatal("cleanup order or synthetic log", events, collector.events)
			}
		})
	}
}

func TestDNSQueryCaptureUnavailableAndValidation(t *testing.T) {
	for _, kind := range []string{"error", "error-with-lease", "nil", "objects-only", "settings-only", "no-chain"} {
		t.Run(kind, func(t *testing.T) {
			var events []string
			chain := pipeline.MustNew(queryStage(func(*pipeline.Request) (*pipeline.Response, error) {
				t.Fatal("unavailable query reached pipeline")
				return nil, nil
			}))
			chain.SetCapture(func(*pipeline.Request) (pipeline.ServingLease, error) {
				t.Fatal("unavailable query used live capture")
				return nil, nil
			})
			h := newHarness(t, func(o *Options) {
				o.Chain = chain
				o.DNSQueryCapture = func(*pipeline.Request) (pipeline.ServingLease, error) {
					if kind == "error-with-lease" {
						return &queryLease{events: &events}, context.Canceled
					}
					if kind == "nil" {
						return nil, nil
					}
					return nil, context.Canceled
				}
				switch kind {
				case "objects-only":
					o.DNSQueryCapture, o.ObjectReads = nil, &objectReadFixture{}
				case "settings-only":
					o.DNSQueryCapture, o.CanonicalSettings = nil, policyReadSettings{}
				case "no-chain":
					o.Chain = nil
				}
			})
			h.do(http.MethodPost, "/api/v1/dns/query", "viewer", map[string]any{"name": "example."}).want(http.StatusServiceUnavailable)
			if (kind == "error-with-lease" && len(events) != 1) || (kind != "error-with-lease" && len(events) != 0) {
				t.Fatal("capture error leaked ownership", events)
			}
		})
	}
	h := newHarness(t, func(o *Options) {
		o.Chain = pipeline.MustNew(answerStage{})
		o.DNSQueryCapture = func(*pipeline.Request) (pipeline.ServingLease, error) {
			t.Fatal("invalid input acquired lease")
			return nil, nil
		}
	})
	for _, body := range []map[string]any{{"name": "example", "type": "NOPE"}, {"name": "a..example"}, {"name": "example", "client": "bad"}, {"name": "example", "proto": "bad"}} {
		h.do(http.MethodPost, "/api/v1/dns/query", "viewer", body).want(http.StatusBadRequest)
	}
	newHarness(t).do(http.MethodPost, "/api/v1/dns/query", "viewer", map[string]any{"name": "example"}).want(http.StatusNotImplemented)
}

type queryPanicObserver struct{}

func (queryPanicObserver) Observe(*pipeline.Request, *pipeline.Response, error, time.Duration) {
	panic("observer")
}

func TestDNSQueryCapturePanicClosesResponseBeforeLease(t *testing.T) {
	var events []string
	chain := pipeline.MustNew(queryStage(func(req *pipeline.Request) (*pipeline.Response, error) {
		resp, _ := answerStage{}.Serve(req, nil)
		resp.AddCleanup(func() { events = append(events, "response") })
		return resp, nil
	}))
	chain.Observe(queryPanicObserver{})
	h := newHarness(t, func(o *Options) {
		o.Chain = chain
		o.DNSQueryCapture = func(*pipeline.Request) (pipeline.ServingLease, error) {
			return &queryLease{events: &events}, nil
		}
	})
	defer func() {
		if recover() == nil || !reflect.DeepEqual(events, []string{"response", "lease"}) {
			t.Error("panic leaked ownership", events)
		}
	}()
	_, _ = h.srv.dnsQuery(t.Context(), &dnsQueryInput{Body: DNSQueryRequest{Name: "example"}})
}

func TestDNSQueryCaptureNilPreservesLegacyChainCapture(t *testing.T) {
	var events []string
	lease := &queryLease{events: &events, value: "legacy"}
	chain := pipeline.MustNew(queryStage(func(req *pipeline.Request) (*pipeline.Response, error) {
		if req.Serving != lease {
			t.Error("legacy chain capture was skipped")
		}
		return answerStage{}.Serve(req, nil)
	}))
	captures := 0
	chain.SetCapture(func(*pipeline.Request) (pipeline.ServingLease, error) {
		captures++
		return lease, nil
	})
	h := newHarness(t, func(o *Options) { o.Chain = chain })
	h.do(http.MethodPost, "/api/v1/dns/query", "viewer", map[string]any{"name": "example"}).want(http.StatusOK)
	if captures != 1 || !reflect.DeepEqual(events, []string{"lease"}) {
		t.Fatal("legacy ownership changed", captures, events)
	}
}
