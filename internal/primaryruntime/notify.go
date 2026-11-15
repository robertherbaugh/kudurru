package primaryruntime

import (
	"context"
	"errors"
	"sort"
	"time"

	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// NotifyOnce recovers persisted intent, delivers only for the currently
// published membership/head, then clears EXACT observed intent through CAS.
// Delivery succeeded + ack failed means a later attempt may duplicate delivery.
func (r *Runtime) NotifyOnce(ctx context.Context) error {
	if r.o.Notify == nil || r.o.Admit == nil {
		return ErrUnavailable
	}
	r.notifyMu.Lock()
	defer r.notifyMu.Unlock()
	cut := r.Capture(r.o.Now())
	apices := make([]string, 0, len(cut.entries))
	for apex := range cut.entries {
		apices = append(apices, apex)
	}
	sort.Strings(apices)
	start := sort.SearchStrings(apices, r.notifyAfter)
	if start < len(apices) && apices[start] == r.notifyAfter {
		start++
	}
	var failures []error
	count := 0
	for i := 0; i < len(apices) && count < 32; i++ {
		apex := apices[(start+i)%len(apices)]
		r.notifyAfter = apex
		e := cut.entries[apex]
		m, ok := cut.Member(apex)
		if !ok || !m.Published || e.view == nil || e.view.Head.Spec.NotifySequence == 0 {
			continue
		}
		count++
		if err := r.notifyOne(ctx, apex, e, m); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (r *Runtime) notifyOne(ctx context.Context, apex string, e *entry, m Member) error {
	l, err := r.ReadLifecycle(ctx, m.Zone)
	if err != nil {
		return err
	}
	if l.Spec.Incarnation != m.Incarnation || l.Spec.Sequence != m.Sequence || l.Spec.Phase != zk.PhaseActive {
		return nil
	}
	v, err := r.o.Repository.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		return err
	}
	if version(v) != m.Version || v.Head.Spec.NotifySequence == 0 {
		return nil
	}
	if binder, ok := r.o.Notify.(NotifyBinder); ok {
		bound, release, err := binder.Bind(ctx, m, e.model)
		if release != nil {
			defer release()
		}
		if err != nil {
			return err
		}
		if bound == nil || release == nil {
			return ErrUnavailable
		}
		// Preserve the original caller's cancellation even if an adapter returns
		// an independently derived context. The proof remains in bound's values.
		bound, cancel := context.WithCancel(bound)
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		if err := ctx.Err(); err != nil {
			return err
		}
		ctx = bound
	}
	if _, err := r.admit(ctx, Command{Lifecycle: l, Expected: v.Expected, Reason: "notify-delivery"}); err != nil {
		return err
	}
	current, _ := r.Capture(r.o.Now()).Member(apex)
	if !current.Published || current.Version != m.Version || current.Incarnation != m.Incarnation {
		return nil
	}
	sendCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err = r.o.Notify.Send(sendCtx, m, e.model)
	cancel()
	if err != nil {
		return err
	}
	p, err := zonestate.Prepare(v.Aggregate, v.Head, r.o.Limits)
	if err != nil {
		return err
	}
	p.Head.Spec.NotifySequence = 0
	p.Head.Spec.NotifySerial = 0
	committed, err := r.admit(ctx, Command{Lifecycle: l, Expected: v.Expected, Prepared: p, Reason: "notify-ack"})
	if err != nil {
		return err
	}
	next, err := r.build(ctx, l, committed, nil)
	if err != nil {
		return err
	}
	if err := r.install(next); err != nil {
		return err
	}
	if err := r.publish(ctx, next); err != nil {
		return err
	}
	return nil
}

// RunNotify is one bounded recovery worker with backoff; it creates no per-zone
// goroutines and keeps unsuccessful or unacknowledged intent durable.
func (r *Runtime) RunNotify(ctx context.Context) error {
	if r.o.Notify == nil {
		return ErrUnavailable
	}
	delay := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.NotifyOnce(ctx); err != nil {
			delay = min(delay*2, 32*time.Second)
		} else {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
