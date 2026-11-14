package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
)

type anchorQueryStage struct{ before func() error }

func (anchorQueryStage) Name() string { return "upstream" }
func (s anchorQueryStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	if s.before != nil {
		if err := s.before(); err != nil {
			return nil, err
		}
	}
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	rr, err := dns.NewRR("example. 60 IN A 192.0.2.1")
	if err != nil {
		return nil, err
	}
	m.Answer = []dns.RR{rr}
	return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded}, nil
}

type anchorQueryFlush struct {
	http.ResponseWriter
	hold func() error
}

func (w anchorQueryFlush) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w anchorQueryFlush) FlushError() error {
	if err := w.hold(); err != nil {
		return err
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func TestRootAnchorQueryActualHTTPRetirement(t *testing.T) {
	for _, where := range []string{"acquisition", "flush"} {
		t.Run(where, func(t *testing.T) {
			f := newServingCommandFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			bootstrap := captureOutputBootstrap(ctx, t, f)
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			hold := func() error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			stage := anchorQueryStage{}
			if where == "acquisition" {
				stage.before = hold
			}
			srv, err := api.New(api.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config,
				Authenticate: f.http.Middleware, Authorizer: f.http, Logger: slog.New(slog.DiscardHandler),
				Chain: pipeline.MustNew(stage), DNSQueryOutput: captureRootAnchorQueryOutput,
				DNSQueryCapture: func(req *pipeline.Request) (pipeline.ServingLease, error) {
					lease, err := servinghttp.CloneLease(req.Ctx)
					return servingdns.Wrap(lease), err
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				if where == "flush" {
					w = anchorQueryFlush{w, hold}
				}
				srv.ServeHTTP(w, r)
			}))
			defer peer.Close()
			defer unblock()
			type result struct {
				code int
				body string
				err  error
			}
			response := make(chan result, 1)
			go func() {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, peer.URL+api.BasePath+"/dns/query", strings.NewReader(`{"name":"example."}`))
				if err != nil {
					response <- result{err: err}
					return
				}
				req.Header.Set("Authorization", "Bearer "+f.token)
				req.Header.Set("Content-Type", "application/json")
				res, err := peer.Client().Do(req)
				if err != nil {
					response <- result{err: err}
					return
				}
				body, err := io.ReadAll(res.Body)
				_ = res.Body.Close()
				response <- result{res.StatusCode, string(body), err}
			}()
			select {
			case <-entered:
			case out := <-response:
				t.Fatal("request missed original boundary", out)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			committed := make(chan error, 1)
			go func() { committed <- bootstrap() }()
			if where == "flush" {
				select {
				case err := <-committed:
					t.Fatal("managed acceptance crossed actual HTTP flush", err)
				case <-time.After(40 * time.Millisecond):
				}
			} else {
				select {
				case err := <-committed:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			unblock()
			select {
			case out := <-response:
				if out.err != nil {
					t.Fatal(out.err)
				}
				if where == "acquisition" {
					if out.code != http.StatusServiceUnavailable || strings.Contains(out.body, `"answer"`) {
						t.Fatal("retired original answer exported", out)
					}
				} else {
					if out.code != http.StatusOK || !strings.Contains(out.body, "192.0.2.1") {
						t.Fatal("admitted output missing", out)
					}
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if where == "flush" {
				select {
				case err := <-committed:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			select {
			case <-finished:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func TestRootAnchorQueryMissingOriginalComponent(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if out, err := captureRootAnchorQueryOutput(&pipeline.Request{Ctx: ctx}); err == nil || out != nil {
		t.Fatal("query output accepted missing original component", out, err)
	}
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	out, err := captureRootAnchorQueryOutput(&pipeline.Request{Ctx: ctx, Serving: servingdns.Wrap(lease)})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := out.Emit(func() error { t.Fatal("canceled query entered emission"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
