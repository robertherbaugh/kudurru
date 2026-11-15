package main

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// initializeSQLiteRuntime binds one original startup lifetime and writer before
// any owner command. Followers only bind committed state; they never initialize
// canonical owners or private denial history from a received proposal.
func initializeSQLiteRuntime(ctx context.Context, s *sqlite.Store, cfg *config.Config) error {
	original, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.Authority().Writable() {
		writer, err := s.WriterToken()
		if err != nil {
			return err
		}
		original = sqlite.WithWriterToken(original, writer)
		if err := s.Command(original, writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
			if err := runtimeowners.Initialize(ctx, tx, time.Now().UTC()); err != nil {
				return err
			}
			return initializeServingSettings(ctx, tx, cfg)
		}); err != nil {
			return err
		}
	}
	_, err := s.BindAnchorLifecycle(original)
	return err
}
