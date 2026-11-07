package host

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration"
)

func TestCommandCaptureQueuedOriginalResourceAndSingleSubmission(t *testing.T) {
	h, j, p, _, _ := engineFixture(t)
	original, newer := new(int), new(int)
	j.frame.input.Resource = original
	j.commands = make(chan command, 1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	capture, err := h.CaptureCommand(ctx, j.guard.Owner, j.guard.IntegrationResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	p.check = func(ctx context.Context) (integration.Status, error) {
		calls++
		resource, err := h.RequestResource(ctx, j.guard)
		if err != nil || resource != original {
			t.Error("queued command replaced its original resource")
		}
		return integration.Status{Reachable: true, Authenticated: true}, err
	}
	done := make(chan error, 1)
	go func() { _, err := capture.Check(ctx); done <- err }()
	var request command
	select {
	case request = <-j.commands:
	case <-ctx.Done():
		t.Fatal("command did not reach its bounded queue")
	}
	h.mu.Lock()
	j.frame.input.Resource = newer
	h.mu.Unlock()
	request.reply <- h.execute(request.ctx, j, request)
	if err := <-done; err != nil || calls != 1 {
		t.Fatalf("original queued command failed: calls=%d error=%v", calls, err)
	}
	if _, err := capture.Check(ctx); !errors.Is(err, ErrInvalid) || calls != 1 {
		t.Fatal("a consumed capture submitted another command")
	}
}

func TestCommandCaptureRejectsQueuedSuccessor(t *testing.T) {
	for _, change := range []string{"instance", "lease", "job", "revision", "source"} {
		t.Run(change, func(t *testing.T) {
			h, j, p, _, _ := engineFixture(t)
			j.commands = make(chan command, 1)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			capture, err := h.CaptureCommand(ctx, j.guard.Owner, j.guard.IntegrationResourceVersion)
			if err != nil {
				t.Fatal(err)
			}
			p.check = func(context.Context) (integration.Status, error) {
				t.Error("successor dispatched the original command")
				return integration.Status{}, nil
			}
			done := make(chan error, 1)
			go func() { _, err := capture.Check(ctx); done <- err }()
			var request command
			select {
			case request = <-j.commands:
			case <-ctx.Done():
				t.Fatal("command did not reach its bounded queue")
			}
			h.mu.Lock()
			switch change {
			case "instance":
				replacement := *j.frame.instance
				j.frame.instance = &replacement
			case "lease":
				next, stop := context.WithCancel(t.Context())
				t.Cleanup(stop)
				j.leaseCtx = next
			case "job":
				replacement := *j
				h.jobs[j.key] = &replacement
			case "revision":
				j.guard.IntegrationResourceVersion = "replacement"
			case "source":
				j.frame.input.BundleID = "replacement"
			}
			h.mu.Unlock()
			request.reply <- h.execute(request.ctx, j, request)
			if err := <-done; !errors.Is(err, ErrFenced) {
				t.Fatalf("queued successor was not refused: %v", err)
			}
		})
	}
}

func TestCommandCaptureOriginalLifetimeAndReviewedRevision(t *testing.T) {
	h, j, _, _, _ := engineFixture(t)
	if _, err := h.CaptureCommand(t.Context(), j.guard.Owner, "other"); !errors.Is(err, ErrFenced) {
		t.Fatal("unreviewed revision was accepted")
	}
	original, cancel := context.WithCancel(t.Context())
	capture, err := h.CaptureCommand(original, j.guard.Owner, j.guard.IntegrationResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := capture.Check(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal("fresh caller context repaired the original canceled request")
	}
	if _, err := capture.Check(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatal("refused submission did not consume its capture")
	}
}

func TestCommandCaptureManualPublisherRequiresReviewedPreview(t *testing.T) {
	h, j, p, _, _ := engineFixture(t)
	capture, err := h.CaptureCommand(t.Context(), j.guard.Owner, j.guard.IntegrationResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	got := h.execute(t.Context(), j, command{kind: "sync", original: capture})
	if !errors.Is(got.err, ErrStalePreview) || p.calls != 0 {
		t.Fatal("manual publisher sync bypassed review")
	}
}
