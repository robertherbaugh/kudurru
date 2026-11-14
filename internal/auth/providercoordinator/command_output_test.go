package providercoordinator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestOperatorCommandOutputRetainsDependencyPins(t *testing.T) {
	f := newFixture(t)
	ctx, target := writer(t, f.store)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, token, err := f.store.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request, err := f.store.CaptureCommandOutput(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	type outcome struct {
		committed bool
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		committed, err := f.coordinator.WithCommandOutput(ctx, target, request, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			_, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, servingauth.Credential{Kind: servingauth.APIToken, Value: f.wire}, rbac.Attributes{Resource: "stats", Verb: "read"}, time.Now)
			if err != nil {
				return err
			}
			return tx.PutSection(ctx, "dedup", replication.Entry{Key: "operator-output", Data: []byte(`{}`)})
		}, func(output context.Context) error {
			if err := request.CheckOutput(output); err != nil {
				return err
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-output.Done():
				return output.Err()
			}
		})
		done <- outcome{committed, err}
	}()
	select {
	case <-entered:
	case out := <-done:
		t.Fatal("output not entered", out)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, pin := range []func(context.Context) error{
		func(ctx context.Context) error {
			_, release, err := f.publisher.Pin(ctx)
			if release != nil {
				release()
			}
			return err
		},
		func(ctx context.Context) error {
			return f.resources.WithReplacement(ctx, func(*providerresources.Replacement) error { return nil })
		},
	} {
		short, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		err := pin(short)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("dependency crossed output", err)
		}
	}
	swapped := make(chan struct{})
	go func() {
		f.host.install(&hostGeneration{id: "next", allowed: true, secret: "replacement"})
		close(swapped)
	}()
	select {
	case <-swapped:
		t.Fatal("host crossed output")
	case <-time.After(25 * time.Millisecond):
	}
	finish()
	select {
	case out := <-done:
		if !out.committed || out.err != nil {
			t.Fatal("completion lost", out)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-swapped:
	case <-ctx.Done():
		t.Fatal("host pin leaked")
	}
}
