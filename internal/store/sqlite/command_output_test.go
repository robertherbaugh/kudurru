package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func outputRequest(t *testing.T, s *Store) (context.Context, context.CancelFunc, *CommandOutputRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	writer, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithWriterToken(ctx, writer)
	_, token, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CaptureCommandOutput(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, cancel, r
}

func TestCommandOutputActualCompletionAndOneUse(t *testing.T) {
	for _, mode := range []string{"write", "noop", "transport", "cancel-output", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			s := openTemp(t)
			ctx, cancel, request := outputRequest(t, s)
			copyRequest := *request
			before := imageOf(t, s).Generation()
			failure := errors.New("fixture refusal")
			var escaped context.Context
			committed, err := s.CommandWithOutput(ctx, request, func(ctx context.Context, tx *CommandTx) error {
				if mode == "noop" {
					return nil
				}
				if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "output", Data: []byte(`{}`)}); err != nil {
					return err
				}
				if mode == "rollback" {
					return failure
				}
				return nil
			}, func(scope context.Context) error {
				escaped = scope
				if err := copyRequest.CheckOutput(scope); err != nil {
					return err
				}
				if request.CheckOutput(ctx) == nil {
					t.Error("ordinary context gained output authority")
				}
				if mode == "cancel-output" {
					cancel()
					<-scope.Done()
					return scope.Err()
				}
				if mode == "transport" {
					return failure
				}
				return nil
			})
			if committed != (mode != "rollback") || (escaped != nil) != committed {
				t.Fatal("proposal replaced completion", committed, err)
			}
			if (err == nil) != (mode == "write" || mode == "noop") {
				t.Fatal("wrong disposition", err)
			}
			if request.CheckOutput(escaped) == nil {
				t.Fatal("escaped output context remained valid")
			}
			after := imageOf(t, s).Generation()
			if after.Same(before) != (mode == "rollback" || mode == "noop") {
				t.Fatal("durable result confused with output")
			}
			if ok, err := s.CommandWithOutput(ctx, &copyRequest, func(context.Context, *CommandTx) error { t.Error("replayed command"); return nil }, func(context.Context) error { t.Error("replayed output"); return nil }); ok || err == nil {
				t.Fatal("request copy reused")
			}
		})
	}
}

func TestCommandOutputOriginalCaptureRefusals(t *testing.T) {
	s := openTemp(t)
	ctx, _, request := outputRequest(t, s)
	_, token, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []GenerationToken{{}, {source: openTemp(t), generation: token.generation, authority: token.authority}} {
		if _, err := s.CaptureCommandOutput(ctx, value); err == nil {
			t.Fatal("foreign or zero source accepted")
		}
	}
	if _, err := s.CaptureCommandOutput(t.Context(), token); err == nil {
		t.Fatal("unbounded capture")
	}
	long, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := s.CaptureCommandOutput(long, token); err == nil {
		t.Fatal("expanded original bound")
	}
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "interposition", Data: []byte(`{}`)})
	}); err != nil {
		t.Fatal(err)
	}
	committed, err := s.CommandWithOutput(ctx, request, func(context.Context, *CommandTx) error { t.Error("stale callback entered"); return nil }, func(context.Context) error { t.Error("stale output entered"); return nil })
	if committed || !errors.Is(err, replication.ErrCAS) {
		t.Fatal("stale original rescued", committed, err)
	}
}

func TestCommandOutputRetainsWriterThroughEmission(t *testing.T) {
	s := openTemp(t)
	ctx, _, request := outputRequest(t, s)
	_, _, contender := outputRequest(t, s)
	entered, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	done := make(chan error, 1)
	go func() {
		_, err := s.CommandWithOutput(ctx, request, func(context.Context, *CommandTx) error { return nil }, func(scope context.Context) error {
			close(entered)
			select {
			case <-release:
				return request.CheckOutput(scope)
			case <-scope.Done():
				return scope.Err()
			}
		})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal("output missing", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	short, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	committed, err := s.CommandWithOutput(short, contender, func(context.Context, *CommandTx) error { t.Error("writer crossed active emission"); return nil }, func(context.Context) error { return nil })
	if committed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("writer exclusion lost", err)
	}
	finish()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("output did not release", ctx.Err())
	}
}

func TestCommandOutputCannotDetachOriginalCancellation(t *testing.T) {
	s := openTemp(t)
	_, cancel, request := outputRequest(t, s)
	cancel()
	ctx, _, _ := outputRequest(t, s)
	committed, err := s.CommandWithOutput(ctx, request, func(context.Context, *CommandTx) error { t.Error("canceled original entered"); return nil }, func(context.Context) error { t.Error("canceled original emitted"); return nil })
	if committed || !errors.Is(err, context.Canceled) {
		t.Fatal("later context rescued canceled original", err)
	}
}

func TestCommandOutputSQLFailuresNeverEmit(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "unknown"}[unknown], func(t *testing.T) {
			s := openTemp(t)
			statements := []string{`CREATE TEMP TRIGGER output_abort BEFORE INSERT ON main.replication_sections WHEN NEW.section='dedup' BEGIN SELECT RAISE(ABORT,'fixture rollback'); END`}
			if unknown {
				statements = []string{
					`CREATE TEMP TABLE output_parent(id INTEGER PRIMARY KEY)`,
					`CREATE TEMP TABLE output_child(parent INTEGER REFERENCES output_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
					`CREATE TEMP TRIGGER output_unknown AFTER INSERT ON main.replication_sections WHEN NEW.section='dedup' BEGIN INSERT INTO output_child(parent) VALUES (1); END`,
				}
			}
			for _, statement := range statements {
				if _, err := s.w.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			ctx, _, request := outputRequest(t, s)
			reached := false
			committed, err := s.CommandWithOutput(ctx, request, func(ctx context.Context, tx *CommandTx) error {
				reached = true
				return tx.PutSection(ctx, "dedup", replication.Entry{Key: "actual", Data: []byte(`{}`)})
			}, func(context.Context) error { t.Error("failed SQL emitted output"); return nil })
			var failure interface{ Code() int }
			if committed || !reached || !errors.As(err, &failure) {
				t.Fatal("actual SQL failure not reached", committed, err)
			}
			if unknown {
				if failure.Code() != 787 || !errors.Is(err, sql.ErrTxDone) || !errors.Is(err, ErrCommandCompletionUnknown) {
					t.Fatal("unknown completion relabeled", err)
				}
			} else if failure.Code() != 1811 || errors.Is(err, ErrCommandCompletionUnknown) {
				t.Fatal("rollback relabeled", err)
			}
		})
	}
}
