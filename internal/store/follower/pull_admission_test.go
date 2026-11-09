package follower

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestQueuedPullRetainsCancellationBeforeAcquisition(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			primary, mirror := open(t, replication.Primary), open(t, replication.Follower)
			if err := mirror.Enroll(t.Context(), primary.Authority().Term); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			var calls atomic.Int32
			puller := &Puller{Mirror: mirror, Source: sourceFunc(func(ctx context.Context, etag string) (Response, error) {
				if calls.Add(1) == 1 {
					close(entered)
					select {
					case <-ctx.Done():
						return Response{}, ctx.Err()
					case <-release:
					}
				}
				return (directSource{primary}).Fetch(ctx, etag)
			})}
			first, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			firstDone := make(chan struct{})
			var firstErr error
			go func() { defer close(firstDone); firstErr = puller.Pull(first) }()
			defer func() { unblock(); <-firstDone }()
			select {
			case <-entered:
			case <-firstDone:
				t.Fatal("original pull did not reach source acquisition", firstErr)
			case <-first.Done():
				t.Fatal("original pull did not start")
			}

			queued, cancel := context.WithCancel(t.Context())
			want := error(context.Canceled)
			if deadline {
				cancel()
				queued, cancel = context.WithTimeout(t.Context(), 30*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			secondDone := make(chan struct{})
			var secondErr error
			go func() { defer close(secondDone); secondErr = puller.Pull(queued) }()
			defer func() { unblock(); <-secondDone }()
			if !deadline {
				cancel()
			}
			select {
			case <-secondDone:
				if !errors.Is(secondErr, want) {
					t.Fatal("queued pull lost original refusal", secondErr)
				}
			case <-time.After(time.Second):
				t.Fatal("queued pull outwaited cancellation behind the original acquisition")
			}
			if calls.Load() != 1 || !puller.Status().LastContact.IsZero() || mirror.Authority().Installed != (replication.Generation{}) {
				t.Fatal("canceled waiter acquired a source, installed data or acknowledged contact")
			}
			unblock()
			<-firstDone
			if firstErr != nil || calls.Load() != 1 || puller.Status().LastContact.IsZero() || puller.Status().Error != nil {
				t.Fatal("canceled waiter disturbed the original admitted pull", firstErr, puller.Status())
			}
		})
	}
}
