package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// A deferred physical constraint reaches the real driver's Commit after all
// command checks. This covers an actual commit-error boundary, not an invented
// backend error or a committed-but-lost acknowledgment. The API must preserve
// uncertainty even though this fixture independently knows its SQL fault.
func TestServingTSIGActualSQLCommitFailureHasNoReceiptOrReplay(t *testing.T) {
	f := newTSIGCommandFixture(t)
	db, err := sql.Open("sqlite", f.s.opts.Store.Path())
	if err != nil {
		t.Fatal("open physical completion fixture")
	}
	defer func() { _ = db.Close() }()
	for _, statement := range []string{
		`CREATE TABLE tsig_completion_parent (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE tsig_completion_child (id INTEGER REFERENCES tsig_completion_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER tsig_completion_failure AFTER INSERT ON objects WHEN NEW.kind='TSIGKey' BEGIN INSERT INTO tsig_completion_child VALUES (1); END`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal("prepare physical deferred completion refusal")
		}
	}
	var samples, invocations atomic.Int32
	command := &servingRuntime{opts: f.s.opts, publisher: f.s.publisher, providers: f.s.providers, primary: f.s.primary}
	command.opts.Now = func() time.Time { samples.Add(1); return f.now }
	server, err := api.New(api.Options{
		Store: f.s.opts.Store, Validator: f.s.opts.Validator, Config: f.s.opts.Config,
		TSIGKeyCommand: func(ctx context.Context, change api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
			invocations.Add(1)
			return command.TSIGKeyCommand(ctx, change)
		},
		Authenticate: f.http.Middleware, Authorizer: f.http, Now: f.currentClock(), Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal("mount actual completion route")
	}
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	response := tsigRouteRequest(ctx, t, server, f.token, http.MethodPost, "/tsigkeys", tsigCreateBody("completion-failure.", 0x64))
	if invocations.Load() != 1 || samples.Load() != 6 || response.Code != http.StatusServiceUnavailable {
		t.Fatalf("actual completion refusal: calls=%d samples=%d status=%d", invocations.Load(), samples.Load(), response.Code)
	}
	for _, forbidden := range []string{"committed", "published", "rollback", "rolled back", "retry", "constraint", "tsig_completion", "sealed", "secret"} {
		if strings.Contains(strings.ToLower(response.Body.String()), forbidden) {
			t.Fatal("uncertain completion response disclosed a private error or claimed an outcome")
		}
	}
}
