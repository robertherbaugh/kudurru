package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type queryEmissionFunc func(func() error) error

func (f queryEmissionFunc) Emit(fn func() error) error { return f(fn) }

type queryOutputLease struct{ released atomic.Bool }

func (l *queryOutputLease) Value() any { return l }
func (l *queryOutputLease) Clone() (pipeline.ServingLease, error) {
	return nil, errors.New("unexpected query recapture")
}
func (l *queryOutputLease) Release() { l.released.Store(true) }

func queryOutputPOST(ctx context.Context, t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, url+BasePath+"/dns/query", strings.NewReader(`{"name":"example."}`))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", apitest.Bearer("viewer"))
	return client.Do(r)
}

func TestDNSQueryOutputActualHTTPFlushBeforeRelease(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "http1", true: "http2"}[http2], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			lease := &queryOutputLease{}
			var captured, closed atomic.Bool
			flushed, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var original context.Context
			var cancelRequest context.CancelFunc
			chain := pipeline.MustNew(queryStage(func(req *pipeline.Request) (*pipeline.Response, error) {
				if !captured.Load() || req.Serving != lease || req.Ctx != original {
					return nil, errors.New("pipeline preceded original output capture")
				}
				out, err := answerStage{}.Serve(req, nil)
				out.AddCleanup(func() {
					if lease.released.Load() {
						t.Error("lease released before response")
					}
					closed.Store(true)
				})
				return out, err
			}))
			h := newHarness(t, func(o *Options) {
				o.Chain = chain
				o.DNSQueryCapture = func(*pipeline.Request) (pipeline.ServingLease, error) { return lease, nil }
				o.DNSQueryOutput = func(req *pipeline.Request) (DNSQueryEmission, error) {
					original = req.Ctx
					captured.Store(true)
					return queryEmissionFunc(func(fn func() error) error {
						if lease.released.Load() || closed.Load() || original.Err() != nil {
							return errors.New("resources expired before emission")
						}
						if err := fn(); err != nil {
							return err
						}
						// Cancellation after actual flush must not reset delivered output.
						cancelRequest()
						close(flushed)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
						return errors.New("later scope exit error after actual flush")
					}), nil
				}
			})
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				requestCtx, stop := context.WithCancel(r.Context())
				cancelRequest = stop
				defer stop()
				h.srv.ServeHTTP(w, r.WithContext(requestCtx))
			}))
			srv.EnableHTTP2 = http2
			srv.StartTLS()
			defer srv.Close()
			resp, err := queryOutputPOST(ctx, t, srv.Client(), srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			wantVersion := 1
			if http2 {
				wantVersion = 2
			}
			if resp.StatusCode != http.StatusOK || resp.ProtoMajor != wantVersion || resp.ContentLength <= 0 || resp.ContentLength > maxDNSQueryOutputBytes {
				t.Fatal("actual response headers", resp.StatusCode, resp.Proto, resp.ContentLength)
			}
			wire := make([]byte, int(resp.ContentLength))
			if _, err := io.ReadFull(resp.Body, wire); err != nil {
				t.Fatal("unflushed body", err)
			}
			var out DNSQueryResult
			if err := json.Unmarshal(wire, &out); err != nil || out.Rcode != "NOERROR" || len(out.Answer) == 0 {
				t.Fatal("actual DNS diagnostic body", err, string(wire))
			}
			select {
			case <-flushed:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if closed.Load() || lease.released.Load() {
				t.Fatal("resources released before emission returned")
			}
			unblock()
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if !closed.Load() || !lease.released.Load() || original.Err() == nil {
				t.Fatal("response/lease/lifetime not closed after completed flush")
			}
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatal("late error reset completed response", err)
			}
		})
	}
}

func TestDNSQueryOutputRefusalNeverExportsAnswer(t *testing.T) {
	for _, kind := range []string{"capture", "admission", "expired-proof", "oversized", "missing-capture", "missing-emission"} {
		t.Run(kind, func(t *testing.T) {
			lease := &queryOutputLease{}
			var calls atomic.Int32
			chain := pipeline.MustNew(queryStage(func(req *pipeline.Request) (*pipeline.Response, error) {
				calls.Add(1)
				out, err := answerStage{}.Serve(req, nil)
				if kind == "expired-proof" {
					out.Security = pipeline.Security{State: pipeline.ValidationSecure, ValidUntil: time.Now().Add(-time.Second)}
				}
				if kind == "oversized" {
					out.Rule = strings.Repeat("x", maxDNSQueryOutputBytes)
				}
				return out, err
			}))
			h := newHarness(t, func(o *Options) {
				o.Chain = chain
				if kind != "missing-capture" {
					o.DNSQueryCapture = func(*pipeline.Request) (pipeline.ServingLease, error) { return lease, nil }
				}
				o.DNSQueryOutput = func(*pipeline.Request) (DNSQueryEmission, error) {
					if kind == "capture" {
						return nil, errors.New("retired source")
					}
					if kind == "missing-emission" {
						return nil, nil
					}
					return queryEmissionFunc(func(fn func() error) error {
						if kind == "admission" {
							return errors.New("retired attempt")
						}
						return fn()
					}), nil
				}
			})
			srv := httptest.NewServer(h.srv)
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			resp, err := queryOutputPOST(ctx, t, srv.Client(), srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(wire), `"answer"`) {
				t.Fatal("refusal leaked DNS answer", resp.StatusCode, string(wire))
			}
			if (kind == "capture" || kind == "missing-capture" || kind == "missing-emission") && calls.Load() != 0 {
				t.Fatal("capture refusal still acquired answer")
			}
			if kind != "missing-capture" && !lease.released.Load() {
				t.Fatal("refused lease leaked")
			}
		})
	}
}

func TestDNSQueryOutputRequiresHTTPHandoff(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(o *Options) {
		o.DNSQueryOutput = func(*pipeline.Request) (DNSQueryEmission, error) { calls.Add(1); return nil, nil }
	})
	if out, err := h.srv.dnsQuery(t.Context(), &dnsQueryInput{Body: DNSQueryRequest{Name: "example."}}); err == nil || out != nil || calls.Load() != 0 {
		t.Fatal("direct call exported guarded data", out, err)
	}
}

type failingQueryFlush struct{ http.ResponseWriter }

func (w failingQueryFlush) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w failingQueryFlush) FlushError() error           { return errors.New("injected actual flush failure") }

func TestDNSQueryOutputPartialFlushAborts(t *testing.T) {
	lease := &queryOutputLease{}
	h := newHarness(t, func(o *Options) {
		o.Chain = pipeline.MustNew(answerStage{})
		o.DNSQueryCapture = func(*pipeline.Request) (pipeline.ServingLease, error) { return lease, nil }
		o.DNSQueryOutput = func(*pipeline.Request) (DNSQueryEmission, error) {
			return queryEmissionFunc(func(fn func() error) error { return fn() }), nil
		}
	})
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h.srv.ServeHTTP(failingQueryFlush{w}, r)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resp, err := queryOutputPOST(ctx, t, srv.Client(), srv.URL)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("failed flush delivered a successful response")
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !lease.released.Load() {
		t.Fatal("partial handoff leaked lease")
	}
}

type queryDeadlineWriter struct {
	http.ResponseWriter
	observe func(time.Time)
}

func (w queryDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w queryDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.observe(deadline)
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}

func TestDNSQueryOutputOriginalDeadlines(t *testing.T) {
	for _, proof := range []bool{false, true} {
		t.Run(map[bool]string{false: "earlier-request", true: "earlier-proof"}[proof], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			original := time.Now().Add(3 * time.Second)
			bound := original
			if proof {
				bound = original.Add(-time.Second)
			}
			observed := make(chan time.Time, 2)
			h := newHarness(t, func(o *Options) {
				o.Chain = pipeline.MustNew(queryStage(func(req *pipeline.Request) (*pipeline.Response, error) {
					if req.Deadline != original {
						t.Error("pipeline extended original request deadline", req.Deadline, original)
					}
					out, err := answerStage{}.Serve(req, nil)
					if proof {
						out.Security = pipeline.Security{State: pipeline.ValidationSecure, ValidUntil: bound}
					}
					return out, err
				}))
				o.DNSQueryCapture = func(*pipeline.Request) (pipeline.ServingLease, error) { return &queryOutputLease{}, nil }
				o.DNSQueryOutput = func(*pipeline.Request) (DNSQueryEmission, error) {
					return queryEmissionFunc(func(fn func() error) error { return fn() }), nil
				}
			})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCtx, stop := context.WithDeadline(r.Context(), original)
				defer stop()
				h.srv.ServeHTTP(queryDeadlineWriter{w, func(d time.Time) { observed <- d }}, r.WithContext(requestCtx))
			}))
			defer srv.Close()
			resp, err := queryOutputPOST(ctx, t, srv.Client(), srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatal(resp.StatusCode, err)
			}
			select {
			case got := <-observed:
				if got != bound {
					t.Fatal("actual write extended original/proof deadline", got, bound)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestDNSQueryOutputCancellationDuringFlush(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	lease := &queryOutputLease{}
	entered, finished := make(chan struct{}), make(chan struct{})
	deadlines := make(chan time.Time, 2)
	interrupted := make(chan struct{})
	var interruptOnce sync.Once
	var requestCancel context.CancelFunc
	h := newHarness(t, func(o *Options) {
		o.Chain = pipeline.MustNew(answerStage{})
		o.DNSQueryCapture = func(*pipeline.Request) (pipeline.ServingLease, error) { return lease, nil }
		o.DNSQueryOutput = func(*pipeline.Request) (DNSQueryEmission, error) {
			return queryEmissionFunc(func(fn func() error) error { return fn() }), nil
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		requestCtx, stop := context.WithCancel(r.Context())
		requestCancel = stop
		defer stop()
		dw := queryDeadlineWriter{w, func(d time.Time) {
			deadlines <- d
			if !d.After(time.Now()) {
				interruptOnce.Do(func() { close(interrupted) })
			}
		}}
		h.srv.ServeHTTP(queryCancelFlush{dw, func() {
			close(entered)
			// Keep the simulated transport write pending until the actual
			// cancellation callback interrupts it. Returning on requestCtx.Done
			// can legally stop that callback before it ever sets the deadline.
			select {
			case <-interrupted:
			case <-ctx.Done():
				t.Error("cancellation did not interrupt pending flush")
			}
		}}, r.WithContext(requestCtx))
	}))
	defer srv.Close()
	outcome := make(chan error, 1)
	go func() {
		resp, err := queryOutputPOST(ctx, t, srv.Client(), srv.URL)
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		outcome <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	requestCancel()
	select {
	case err := <-outcome:
		if err == nil {
			t.Fatal("canceled pending flush succeeded")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	readDeadline := func() time.Time {
		t.Helper()
		select {
		case d := <-deadlines:
			return d
		case <-ctx.Done():
			t.Fatal("missing original or cancellation write deadline")
			return time.Time{}
		}
	}
	first, second := readDeadline(), readDeadline()
	if !second.Before(first) || second.After(time.Now()) || !lease.released.Load() {
		t.Fatal("cancellation did not tighten and join original write bound", first, second)
	}
}

type queryCancelFlush struct {
	http.ResponseWriter
	cancelWait func()
}

func (w queryCancelFlush) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w queryCancelFlush) FlushError() error           { w.cancelWait(); return context.Canceled }
