package sqlite

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// A writer that sees its token stop being current must find its elected term
// already canceled. Writers (the primary driver, the serving writer) return as
// soon as ServingAuthority no longer matches their token; the cluster runner
// then tells an expected end of term from a failure only by the term context.
// The store used to publish the new authority and leave the cancellation to
// the elector goroutine, so a writer could report a failure for the term that
// was ending ("primaryruntime: required adapter/publication unavailable").
func TestElectorTermEndsBeforeAuthorityChangeIsVisible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *Store) error
	}{
		{"higher-term-fence", func(_ *testing.T, s *Store) error {
			term := s.Authority().Term
			term.Epoch++
			term.PrimaryID, term.PromotionID = "successor", "successor-promotion"
			_, err := s.ObserveAuthority(context.Background(), term)
			return err
		}},
		{"promotion-conflict", func(t *testing.T, s *Store) error {
			term := s.Authority().Term
			term.PrimaryID, term.PromotionID = "competing", "competing-promotion"
			if _, err := s.ObserveAuthority(context.Background(), term); !errors.Is(err, replication.ErrPromotionConflict) {
				t.Fatalf("same-epoch observation: %v", err)
			}
			return nil
		}},
		{"enable-primary", func(t *testing.T, s *Store) error {
			_, err := s.EnablePrimary(context.Background(), imageOf(t, s).Generation())
			return err
		}},
		{"close", func(_ *testing.T, s *Store) error { return s.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			terms := make(chan context.Context, 4)
			observed := make(chan error, 4)
			done := make(chan error, 1)
			go func() {
				done <- s.Elector("order").Run(ctx, func(term context.Context) {
					token, ok := WriterTokenFromContext(term)
					if !ok {
						observed <- errors.New("term without writer token")
						return
					}
					terms <- term
					// The same check the primary driver and writerCurrent make.
					current := func() bool {
						a, blocked := s.ServingAuthority()
						return !blocked && a.Writable() && a.Term.Same(token.Term()) && a.Incarnation == token.Incarnation()
					}
					for {
						changed := s.AuthorityChanges()
						if !current() {
							if term.Err() == nil {
								observed <- errors.New("writer saw its token retired while its term was alive")
							} else {
								observed <- nil
							}
							return
						}
						select {
						case <-changed:
						case <-term.Done():
							observed <- nil
							return
						}
					}
				}, nil)
			}()
			var term context.Context
			select {
			case term = <-terms:
			case <-time.After(5 * time.Second):
				t.Fatal("elector did not start a term")
			}
			// Keep the elector goroutine from running between the change and the
			// check below, so the old order fails on every run.
			defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
			if err := tc.change(t, s); err != nil {
				t.Fatal(err)
			}
			if term.Err() == nil {
				t.Fatal("authority change returned while the retired term was still alive")
			}
			runtime.GOMAXPROCS(2)
			select {
			case err := <-observed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("writer did not observe the change")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, store.ErrClosed) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("elector did not stop")
			}
		})
	}
}
