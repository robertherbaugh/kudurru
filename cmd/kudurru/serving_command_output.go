package main

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// currentAuthCommandOutput retains the original HTTP source and elected writer
// through the command, then keeps the same writer/dependency exclusions through
// the actual response. A command's own effect may supersede its input source;
// that does not permit a replacement writer or detached credential lifetime.
func (s *servingRuntime) currentAuthCommandOutput(ctx context.Context, request *sqlite.CommandOutputRequest, fn func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error, emit func(context.Context) error) (bool, error) {
	if s == nil || ctx == nil || request == nil || fn == nil || emit == nil {
		return false, serving.ErrComponents
	}
	// Refused before the command was entered only because another commit
	// superseded the request's publication: capture again and dispatch once
	// (D399). The output request is single-use, so entering the transaction
	// callback already counts as dispatch here.
	return s.recaptureCommand(ctx, func(began *bool) (bool, error) {
		return s.currentAuthCommandOutputOnce(ctx, request, began, fn, emit)
	})
}

// currentAuthCommandOutputOnce is one dispatch attempt of
// currentAuthCommandOutput.
func (s *servingRuntime) currentAuthCommandOutputOnce(ctx context.Context, request *sqlite.CommandOutputRequest, began *bool, fn func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error, emit func(context.Context) error) (bool, error) {
	status := s.primary.driver.Status()
	if !status.Running {
		if err := s.primaryRequired(ctx); err != nil {
			return false, err
		}
		return false, replication.ErrFenced
	}
	original := s.primary.writer.active.Load()
	ctx, release, err := s.primary.writer.bind(ctx, status.Writer)
	if err != nil {
		return false, err
	}
	defer release()
	target := serving.TermActivation{Term: status.Writer.Term, Incarnation: status.Writer.Incarnation}
	checkWriter := func(ctx context.Context) error {
		if !s.primary.writer.current(original) {
			return replication.ErrFenced
		}
		return s.writerCurrent(ctx, target)
	}
	check := func(ctx context.Context) error {
		if err := checkWriter(ctx); err != nil {
			return err
		}
		return servinghttp.CheckWriter(ctx, s.publisher, target)
	}
	if hooks := s.commandHooks.Load(); hooks != nil && hooks.beforeCheck != nil {
		hooks.beforeCheck(ctx)
	}
	if err := check(ctx); err != nil {
		return false, err
	}
	committed, err := s.providers.WithCommandOutput(ctx, target, request, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		*began = true
		s.dispatchHook(ctx)
		if err := check(ctx); err != nil {
			return err
		}
		if err := tx.RequireCommitCheck(check); err != nil {
			return err
		}
		return fn(ctx, tx, deps)
	}, func(output context.Context) error {
		if err := checkWriter(output); err != nil {
			return err
		}
		if err := emit(output); err != nil {
			return err
		}
		return checkWriter(output)
	})
	if committed {
		s.publisher.Wake()
	}
	return committed, err
}
