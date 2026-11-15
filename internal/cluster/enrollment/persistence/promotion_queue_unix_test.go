//go:build unix

package persistence_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestPromotionQueuedBehindActualAcquisitionHonorsOriginalLifetime(t *testing.T) {
	entered := make(chan struct{})
	f := newAcquisitionFixture(t, func(http.Handler) http.Handler {
		return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
		})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	acquired := make(chan error, 1)
	go func() { _, err := f.source.Fetch(ctx, ""); acquired <- err }()
	defer func() {
		cancel()
		select {
		case err := <-acquired:
			if !errors.Is(err, context.Canceled) {
				t.Error("held acquisition lost its cancellation", err)
			}
		case <-time.After(time.Second):
			t.Error("held acquisition did not join")
		}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("actual acquisition did not reach its original peer")
	}
	before := f.mirror.Authority()
	for _, kind := range []string{"deadline", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			lifetime := time.Second
			if kind == "deadline" {
				lifetime = 30 * time.Millisecond
			}
			original, stop := context.WithTimeout(t.Context(), lifetime)
			defer stop()
			checked := make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() {
				_, err := f.mirror.PromoteChecked(original, replication.PromoteRequest{
					ExpectedTerm: before.Term, ExpectedInstalled: before.Installed, RequestID: "queued-" + kind,
				}, func(context.Context) error { checked <- struct{}{}; return nil })
				done <- err
			}()
			select {
			case <-checked:
			case <-ctx.Done():
				t.Fatal("promotion did not reach original admission")
			}
			want := context.DeadlineExceeded
			if kind == "cancel" {
				stop()
				want = context.Canceled
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatal("queued promotion lost original lifetime", err)
				}
			case <-time.After(300 * time.Millisecond):
				t.Error("queued promotion outwaited its original lifetime behind network acquisition")
				// Join the refused attempt after releasing only our own holder.
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Error("released promotion lost original lifetime", err)
					}
				case <-time.After(time.Second):
					t.Fatal("queued promotion did not join after release")
				}
			}
			if !reflect.DeepEqual(before, f.mirror.Authority()) {
				t.Fatal("refused queued promotion changed authority")
			}
		})
		if ctx.Err() != nil {
			break
		}
	}
}
