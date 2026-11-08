package main

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// These routes use the production command on the actual fixture's store,
// coordinator, publisher and elected writer. Only its existing clock dependency
// is local to the request; the running runtime and all workers remain unchanged.
func TestServingTSIGFinalCommandLifetimeRollback(t *testing.T) {
	for _, operation := range []string{"create", "replace", "revoke", "reseal"} {
		for _, failure := range []string{"expiry", "regression", "cancellation", "earlier-deadline"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				f := newTSIGCommandFixture(t)
				setup := tsigCommandHTTP(t, f.servingCommandFixture, nil)
				path := "/tsigkeys"
				body := tsigCreateBody("final-clock.", 0x61)
				if operation != "create" {
					first := tsigReadReceipt(t, tsigRouteRequest(t.Context(), t, setup, f.token, http.MethodPost, path, body), http.StatusCreated)
					path += "/final-clock./" + operation
					body = map[string]any{"uid": first.Key.UID, "resourceVersion": first.Key.ResourceVersion}
					if operation == "replace" {
						body = tsigReplaceBody(first.Key, 0x62)
					}
				}
				before, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil {
					t.Fatal("read original final-admission image")
				}
				audit := tsigABQAudit(t.Context(), t, f.s.opts.Store, 0)
				bound := 3 * time.Second
				if failure == "earlier-deadline" {
					bound = time.Second
				}
				ctx, cancel := context.WithTimeout(t.Context(), bound)
				defer cancel()
				var samples atomic.Int32
				command := &servingRuntime{opts: f.s.opts, publisher: f.s.publisher, providers: f.s.providers, primary: f.s.primary}
				command.opts.Now = func() time.Time {
					// Capture, initial target, current reauthorization and pre-Stage
					// target use four samples. The fifth is final authentication;
					// the sixth is the final TSIG check after Stage/audit/fencing.
					if samples.Add(1) < 6 {
						return f.now
					}
					switch failure {
					case "expiry":
						return f.expires
					case "regression":
						return f.now.Add(-time.Nanosecond)
					case "cancellation":
						cancel()
					case "earlier-deadline":
						<-ctx.Done()
					}
					return f.now
				}
				server, err := api.New(api.Options{
					Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config,
					TSIGKeyCommand: command.TSIGKeyCommand, Authenticate: f.http.Middleware,
					Authorizer: f.http, Now: f.s.opts.Now, Logger: slog.New(slog.DiscardHandler),
				})
				if err != nil {
					t.Fatal("mount original final-admission route")
				}
				t.Cleanup(server.Close)
				response := tsigRouteRequest(ctx, t, server, f.token, http.MethodPost, path, body)
				want := http.StatusUnauthorized
				if failure == "cancellation" || failure == "earlier-deadline" {
					want = http.StatusServiceUnavailable
				}
				if samples.Load() != 6 || response.Code != want || strings.Contains(response.Body.String(), `"committed":true`) {
					t.Fatalf("final original lifetime refusal: samples=%d status=%d want=%d", samples.Load(), response.Code, want)
				}
				after, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil || !tsigABQSameImage(before, after) || !reflect.DeepEqual(audit, tsigABQAudit(t.Context(), t, f.s.opts.Store, 0)) || f.s.opts.Store.VerifyAudit(t.Context()) != nil {
					t.Fatal("final lifetime refusal changed durable image or audit")
				}
			})
		}
	}
}
