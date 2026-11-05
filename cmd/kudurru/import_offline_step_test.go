package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

type offlineStepEvent struct {
	Program, Ordinal, Total int
	Phase, Outcome          string
	Status                  int
	Key                     string
	Canceled, Deadline      bool
}
type offlineStepProbe struct {
	mu             sync.Mutex
	serial, events int
	recent         []offlineStepEvent
	changed        chan struct{}
}

func newOfflineStepProbe() *offlineStepProbe {
	return &offlineStepProbe{changed: make(chan struct{}, 1)}
}
func (p *offlineStepProbe) record(e offlineStepEvent) {
	p.mu.Lock()
	p.events++
	if len(p.recent) == 16 {
		copy(p.recent, p.recent[1:])
		p.recent = p.recent[:15]
	}
	p.recent = append(p.recent, e)
	p.mu.Unlock()
	select {
	case p.changed <- struct{}{}:
	default:
	}
}
func (p *offlineStepProbe) dump(t *testing.T, phase string) {
	t.Helper()
	p.mu.Lock()
	raw, _ := json.Marshal(struct {
		Phase  string
		Events int
		Recent []offlineStepEvent
	}{phase, p.events, p.recent})
	p.mu.Unlock()
	t.Logf("MIGRATION_STEP_PROBE %s", raw)
}
func offlineStepOutcome(ctx context.Context, err error, e offlineStepEvent) offlineStepEvent {
	e.Canceled = errors.Is(ctx.Err(), context.Canceled)
	e.Deadline = errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
	e.Outcome = "success"
	if err == nil {
		return e
	}
	e.Outcome = "unknown_refusal"
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case 400, 401, 403, 404, 409, 422, 429, 500, 503:
			e.Status = apiErr.Status
		}
		switch apiErr.Body.Key {
		case "invalid", "unauthorized", "forbidden", "not_found", "conflict", "unavailable", "rate_limited":
			e.Key = apiErr.Body.Key
			e.Outcome = "api_refusal"
		}
	}
	return e
}
func (p *offlineStepProbe) wrap(original api.MigrationCommand) api.MigrationCommand {
	return func(ctx context.Context, program *api.MigrationProgram) (api.MigrationNext, error) {
		p.mu.Lock()
		p.serial++
		serial := p.serial
		p.mu.Unlock()
		event := offlineStepEvent{Program: serial, Total: program.Len(), Phase: "admission_enter"}
		p.record(event)
		next, err := original(ctx, program)
		event.Phase = "admission_return"
		p.record(offlineStepOutcome(ctx, err, event))
		if err != nil {
			return next, err
		}
		ordinal := 0
		return func(stepCtx context.Context) error {
			ordinal++
			e := offlineStepEvent{Program: serial, Ordinal: ordinal, Total: program.Len(), Phase: "step_enter"}
			p.record(e)
			returned := false
			defer func() {
				if !returned {
					e.Phase = "step_did_not_return"
					e.Outcome = "unwind"
					p.record(e)
				}
			}()
			err := next(stepCtx)
			returned = true
			e.Phase = "step_return"
			p.record(offlineStepOutcome(stepCtx, err, e))
			return err
		}, nil
	}
}
func offlineProbeWaitRun(t *testing.T, f *servingCommandFixture, id, source string, probe *offlineStepProbe) {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for {
		changed := f.s.opts.Store.GenerationChanges()
		run := offlineAcceptanceJournal(t, f, id, source)
		if run.Spec.Status != "running" {
			probe.dump(t, "journal_terminal")
			break
		}
		select {
		case <-changed:
		case <-probe.changed:
		case <-t.Context().Done():
			probe.dump(t, "journal_context_end")
			t.Fatal("journal wait context ended")
		case <-timer.C:
			probe.dump(t, "journal_deadline_running")
			t.Fatal("journal did not reach terminal status within original 15s bound")
		}
	}
	probe.dump(t, "adoption_enter")
	adoptionWait(t, f)
	probe.dump(t, "adoption_complete")
}
