package catalogue

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func catalogStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestHolderCapturePinsWholeSQLitePublication(t *testing.T) {
	s := catalogStore(t)
	h := NewHolder(captureDocument(t, "A", "source-A"))
	c, changed := h.Capture(), h.Changes()
	prepared := c.Catalogue() // prepare BEFORE either holder or SQLite gate
	_, token, err := s.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	var published *Catalogue
	go func() {
		done <- h.WithCurrent(t.Context(), c, func() error {
			return s.PublishIfCurrent(t.Context(), token, sqlite.PublicationOptions{}, func() error {
				close(entered)
				<-release
				if !c.Current() {
					return snapshotowner.ErrObsolete
				}
				published = prepared
				return nil
			})
		})
	}()
	<-entered
	base, imp := captureDocument(t, "B", "source-B"), captureImport(t, "Gamma", "import-B")
	installed := make(chan bool, 2)
	go func() { installed <- h.Set(base) }()
	go func() { installed <- h.SetImport(imp) }()
	select {
	case <-installed:
		t.Fatal("replacement crossed the actual SQLite publication callback")
	case <-time.After(20 * time.Millisecond):
	}
	if !c.Current() || c.Sources().BaseOrigin != "source-A" || prepared.Version != "A" {
		t.Fatal("held source publication changed dependency")
	}
	expectOpen(t, changed)
	unblock()
	if err := <-done; err != nil || published != prepared {
		t.Fatal("whole callback did not install the captured preparation", err)
	}
	baseChanged, importChanged := <-installed, <-installed
	if !baseChanged || !importChanged || c.Current() {
		t.Fatal("queued replacement failed after publication release")
	}
	expectClosed(t, changed)
	if current := h.Capture(); current.Sources().BaseOrigin != "source-B" || current.Sources().ImportOrigin != "import-B" || current.Catalogue().Version != "B" {
		t.Fatal("queued writers lost their complete installation")
	}
	// A fresh holder pin never repairs the older prepared dependency, even when
	// the SQLite source token itself is unchanged and would still admit it.
	called := false
	err = h.WithCurrent(t.Context(), c, func() error {
		return s.PublishIfCurrent(t.Context(), token, sqlite.PublicationOptions{}, func() error { called = true; return nil })
	})
	if !errors.Is(err, snapshotowner.ErrObsolete) || called {
		t.Fatal("stale prepared source admitted via fresh pin", err)
	}
}

func TestHolderCapturePinsFullSQLiteCommandAndFinalCommitCheck(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "cancel"}[abort], func(t *testing.T) {
			s := catalogStore(t)
			h := NewHolder(captureDocument(t, "A", "A"))
			c, changed := h.Capture(), h.Changes()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			done := make(chan error, 1)
			go func() {
				done <- h.WithCurrent(ctx, c, func() error {
					return s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "catalog-pin", Data: json.RawMessage(`{"captured":"A"}`)}); err != nil {
							return err
						}
						return tx.RequireCommitCheck(func(context.Context) error {
							close(entered)
							<-release
							if !c.Current() {
								return snapshotowner.ErrObsolete
							}
							return nil
						})
					})
				})
			}()
			<-entered
			next := captureDocument(t, "B", "B")
			installed := make(chan bool, 1)
			go func() { installed <- h.Set(next) }()
			select {
			case <-installed:
				t.Fatal("installation crossed actual final transaction check")
			case <-time.After(20 * time.Millisecond):
			}
			if abort {
				cancel()
				select {
				case <-installed:
					t.Fatal("context cancellation released pin before callback completion")
				case <-time.After(20 * time.Millisecond):
				}
			}
			if !c.Current() {
				t.Fatal("pinned command cursor changed")
			}
			expectOpen(t, changed)
			unblock()
			err := <-done
			if abort && !errors.Is(err, context.Canceled) || !abort && err != nil {
				t.Fatal("command result", err)
			}
			if !<-installed || c.Current() {
				t.Fatal("replacement failed after whole command returned")
			}
			expectClosed(t, changed)
			image, _, err := s.ReadServingCandidate(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			wantRows := 1
			if abort {
				wantRows = 0
			}
			if len(image.Payload.Runtime) != wantRows {
				t.Fatal("actual commit/rollback disagrees with callback result", len(image.Payload.Runtime))
			}
		})
	}
}
