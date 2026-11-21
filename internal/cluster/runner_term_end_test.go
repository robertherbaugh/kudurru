package cluster

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestRunnableLastErrorSurvivesTermEnd(t *testing.T) {
	var calls atomic.Int32
	const failure = "writer failed while term was active"
	h := startHarness(t, Options{}, func(h *harness) {
		mustAdd(t, h.r, "writer", true, func(term context.Context) error {
			if calls.Add(1) == 1 {
				return errors.New(failure)
			}
			<-term.Done()
			return errors.New("term expired while writing")
		})
	})
	send(t, h.e.grant, "grant")
	eventually(t, "writer restarted after recording its failure", func() bool {
		status := h.r.Status()[0]
		return calls.Load() == 2 && status.Active && status.LastError == failure && status.Restarts == 1
	})
	send(t, h.e.revoke, "revoke")
	eventually(t, "writer joined", func() bool { return !h.r.Status()[0].Active })
	if status := h.r.Status()[0]; status.LastError != failure || status.Restarts != 1 {
		t.Fatalf("term end replaced the recorded failure: %+v", status)
	}
}

func TestRunnableLastErrorIgnoresErrorAtCancellation(t *testing.T) {
	h := startHarness(t, Options{}, func(h *harness) {
		mustAdd(t, h.r, "writer", true, func(term context.Context) error {
			<-term.Done()
			return errors.New("writer returned an error after cancellation")
		})
	})
	send(t, h.e.grant, "grant")
	eventually(t, "writer active", func() bool { return h.r.Status()[0].Active })
	send(t, h.e.revoke, "revoke")
	eventually(t, "writer joined", func() bool { return !h.r.Status()[0].Active })
	if status := h.r.Status()[0]; status.LastError != "" || status.Restarts != 0 {
		t.Fatalf("term cancellation recorded a failure: %+v", status)
	}
}

// A leader-only writer that returns an error because its token was retired
// (the primary driver's ErrUnavailable, the serving writer's ErrFenced) after
// its term ended is an end of term, not a failure: no restart, no ERROR log
// and no LastError in the runner status. The SQLite store cancels the term
// before the new authority is visible, so the writer always finds the term
// over when it sees its token retired.
func TestLeaderOnlyWriterEndOfTermIsNotAFailure(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), sqlite.FileName), sqlite.Options{Identity: "runner-node"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	started := make(chan struct{}, 1)
	returned := make(chan bool, 1)
	h := startHarness(t, Options{Elector: st.Elector("writer-order")}, func(h *harness) {
		mustAdd(t, h.r, "writer", true, func(term context.Context) error {
			token, ok := sqlite.WriterTokenFromContext(term)
			if !ok {
				return errors.New("term without writer token")
			}
			started <- struct{}{}
			for {
				changed := st.AuthorityChanges()
				a, blocked := st.ServingAuthority()
				if blocked || !a.Writable() || !a.Term.Same(token.Term()) || a.Incarnation != token.Incarnation() {
					returned <- term.Err() != nil
					return errors.New("required adapter/publication unavailable")
				}
				select {
				case <-changed:
				case <-term.Done():
					// The primary driver also answers ErrUnavailable here.
					returned <- true
					return errors.New("required adapter/publication unavailable")
				}
			}
		})
	})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("writer did not start a term")
	}
	term := st.Authority().Term
	term.Epoch++
	term.PrimaryID, term.PromotionID = "successor", "successor-promotion"
	if _, err := st.ObserveAuthority(context.Background(), term); err != nil {
		t.Fatal(err)
	}
	select {
	case ended := <-returned:
		if !ended {
			t.Fatal("writer saw its token retired while its term was alive")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writer did not observe the fence")
	}
	eventually(t, "writer inactive", func() bool { return !h.r.Status()[0].Active })
	if s := h.r.Status()[0]; s.Restarts != 0 || s.LastError != "" {
		t.Fatalf("end of term reported as a failure: %+v", s)
	}
}
