package host

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type scheduledObserver struct {
	integration.Integration
	interval time.Duration
	err      error
	calls    atomic.Int32
}

func (p *scheduledObserver) ObservationSchedule() (time.Duration, error) { return p.interval, p.err }
func (p *scheduledObserver) Observe(context.Context) (SourceSnapshot, error) {
	p.calls.Add(1)
	return SourceSnapshot{Complete: true}, nil
}

func TestObservationScheduleBoundedAndLegacyCadence(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		interval, override, want time.Duration
		enabled, invalid         bool
	}{
		{name: "disabled", override: time.Second},
		{name: "default", interval: time.Minute, want: time.Minute, enabled: true},
		{name: "stored_CRD_default", interval: time.Minute, override: 10 * time.Minute, want: time.Minute, enabled: true},
		{name: "shorter_explicit", interval: time.Minute, override: 5 * time.Second, want: 5 * time.Second, enabled: true},
		{name: "minimum", interval: time.Minute, override: time.Millisecond, want: time.Second, enabled: true},
		{name: "negative", interval: -time.Second, invalid: true},
		{name: "too_short", interval: time.Millisecond, invalid: true},
		{name: "too_long", interval: 25 * time.Hour, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			definition := webhook("schedule")
			definition.Spec.Resync.Duration = tc.override
			p := &scheduledObserver{Integration: &fakePublisher{}, interval: tc.interval}
			enabled, interval, err := syncSchedule(p, definition)
			if (err != nil) != tc.invalid || enabled != tc.enabled || interval != tc.want {
				t.Fatalf("schedule: enabled=%t interval=%s error=%t", enabled, interval, err != nil)
			}
		})
	}
	definition := webhook("legacy")
	enabled, interval, err := syncSchedule(&fakePublisher{}, definition)
	if err != nil || !enabled || interval != 10*time.Minute {
		t.Fatal("legacy default changed")
	}
	definition.Spec.Resync.Duration = 5 * time.Minute
	definition.Spec.UniFi = &public.UniFiIntegration{Poll: metav1.Duration{Duration: 9 * time.Second}}
	_, interval, err = syncSchedule(&fakePublisher{}, definition)
	if err != nil || interval != 9*time.Second {
		t.Fatal("existing UniFi poll precedence changed")
	}
}

func TestObservationDisabledLifecycleNeverPublishesEmptyInventory(t *testing.T) {
	started := make(chan struct{})
	p := &scheduledObserver{Integration: &fakePublisher{start: func(ctx context.Context) error { close(started); <-ctx.Done(); return nil }}}
	h, _, _, _ := lifecycleFixture(t, func(string) (integration.Integration, error) { return p, nil }, nil)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider did not start")
	}
	statuses := h.Statuses()
	if len(statuses) != 1 {
		t.Fatal("missing active owner")
	}
	id := statuses[0].Owner
	// Reconcile and explicit commands exercise the real worker while disabled.
	// Sync must refuse before any source acquisition or successful empty result.
	for range 4 {
		h.mu.RLock()
		j := h.jobs[instanceKey(id)]
		h.mu.RUnlock()
		select {
		case j.reconcile <- struct{}{}:
		default:
		}
		if _, err := h.Check(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Sync(t.Context(), id, ""); !errors.Is(err, ErrUnsupported) {
			t.Fatal("disabled inventory sync was accepted", err)
		}
	}
	statuses = h.Statuses()
	if p.calls.Load() != 0 || len(statuses) != 1 || statuses[0].Phase != "Running" || !statuses[0].LastSync.IsZero() {
		t.Fatal("disabled source was polled, degraded or published a sync result")
	}
}

func TestObservationScheduleOriginalRefusal(t *testing.T) {
	h, j, p, _, _ := engineFixture(t)
	source := &scheduledObserver{Integration: p, interval: time.Minute, err: ErrFenced}
	j.provider = source
	answer := h.execute(t.Context(), j, command{kind: "sync"})
	if !errors.Is(answer.err, ErrFenced) || source.calls.Load() != 0 {
		t.Fatal("changed schedule acquired inventory")
	}
}
