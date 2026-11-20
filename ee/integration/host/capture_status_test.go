package host

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
)

func TestCaptureFailureDiagnosticsAndRecovery(t *testing.T) {
	st := memory.New()
	definition := webhook("capture")
	definition.Spec.Suspend = true
	input := captureInput(t, definition)
	repo, err := ledger.New(ledger.Options{Reader: st, Write: func(ctx context.Context, _ ledger.Guard, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
		return st.Apply(ctx, obj, opts)
	}})
	if err != nil {
		t.Fatal(err)
	}
	const bound = 10 * time.Millisecond
	var logs bytes.Buffer
	registry := prometheus.NewRegistry()
	capture := func(context.Context) (Input, error) { return input, nil }
	h, err := New(Options{
		Capture: func(ctx context.Context) (Input, error) { return capture(ctx) }, CaptureTimeout: bound,
		Elector: st.Elector, Ledger: repo, Reader: st, Fence: func(context.Context, ExecutionGuard) error { return nil },
		Allowed: func() bool { return true }, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Metrics: registry,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHealthy := func() {
		t.Helper()
		h.refresh(t.Context())
		if statuses, reason := h.StatusSnapshot(); len(statuses) != 1 || reason != "" {
			t.Fatalf("healthy capture: statuses=%v reason=%q", statuses, reason)
		}
	}
	assertHealthy()
	key := instanceKey(InstanceID{Namespace: definition.Namespace, Name: definition.Name})
	jobCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.jobs[key] = &job{ctx: jobCtx, cancel: cancel, done: make(chan struct{})}
	capture = func(ctx context.Context) (Input, error) {
		<-ctx.Done()
		return input, nil
	}
	for range 2 {
		h.refresh(t.Context())
		if statuses, reason := h.StatusSnapshot(); len(statuses) != 0 || reason != "CaptureDeadlineExceeded" || jobCtx.Err() == nil {
			t.Fatalf("late capture: statuses=%v reason=%q job=%v", statuses, reason, jobCtx.Err())
		}
	}
	close(h.jobs[key].done)
	capture = func(context.Context) (Input, error) { return Input{}, errors.New("sensitive adapter detail") }
	h.refresh(t.Context())
	if _, reason := h.StatusSnapshot(); reason != "CaptureFailed" {
		t.Fatalf("unclassified capture reason: %q", reason)
	}
	capture = func(context.Context) (Input, error) { return input, nil }
	assertHealthy()
	if output := logs.String(); strings.Count(output, "reason=CaptureDeadlineExceeded") != 1 ||
		!strings.Contains(output, "reason=CaptureFailed") || !strings.Contains(output, "duration=") ||
		!strings.Contains(output, "bound=10ms") || strings.Contains(output, "sensitive adapter detail") {
		t.Fatalf("unexpected capture log: %s", output)
	}
	if err := testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP kudurru_integration_captures_total Integration host refresh captures by result.
# TYPE kudurru_integration_captures_total counter
kudurru_integration_captures_total{result="CaptureDeadlineExceeded"} 2
kudurru_integration_captures_total{result="CaptureFailed"} 1
kudurru_integration_captures_total{result="Success"} 2
`), "kudurru_integration_captures_total"); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "kudurru_integration_capture_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			if metric.GetLabel()[0].GetValue() == "CaptureDeadlineExceeded" &&
				metric.GetHistogram().GetSampleCount() == 2 && metric.GetHistogram().GetSampleSum() >= (2*bound).Seconds() {
				return
			}
		}
	}
	t.Fatal("deadline capture duration samples missing")
}
