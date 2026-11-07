package snapshotowner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sample struct{ number int }

func sameSample(a, b *sample) bool { return *a == *b }

func publishSample(t *testing.T, o *Owner[sample], n int) Cut[sample] {
	t.Helper()
	if err := o.Publish(t.Context(), &sample{n}, sameSample); err != nil {
		t.Fatal(err)
	}
	return o.Capture()
}

func TestOwnerExactCaptureBroadcastNoChurnAndABA(t *testing.T) {
	var o Owner[sample]
	first := publishSample(t, &o, 1)
	a, b := o.Changes(), o.Changes()
	if first.Value().number != 1 || first.Identity() == "" || !first.Current() || a != b {
		t.Fatal("incoherent initial installation")
	}
	publishSample(t, &o, 1)
	if o.Capture().Value() != first.Value() || o.Capture().Identity() != first.Identity() || !first.Current() {
		t.Fatal("equal publication replaced actual immutable installation")
	}
	select {
	case <-a:
		t.Fatal("equal publication broadcast a change")
	default:
	}
	middle := publishSample(t, &o, 2)
	for _, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		default:
			t.Fatal("replacement not broadcast to every subscriber")
		}
	}
	if o.Changes() == a || first.Current() || first.Value().number != 1 {
		t.Fatal("replacement lost retained snapshot or subscription")
	}
	if err := o.Publish(t.Context(), first.Value(), sameSample); err != nil {
		t.Fatal(err)
	}
	last := o.Capture()
	if first.Current() || middle.Current() || !last.Current() || last.Value() != first.Value() || last.Identity() == first.Identity() {
		t.Fatal("A-B-A revived old installation")
	}
	var other Owner[sample]
	if publishSample(t, &other, 1).Identity() == first.Identity() {
		t.Fatal("independent owners share identity")
	}
}

func TestOwnerPinSpansFullCallbackAndCanceledWaiters(t *testing.T) {
	var o Owner[sample]
	cut := publishSample(t, &o, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	done := make(chan error, 1)
	var calls atomic.Int32
	go func() {
		done <- o.WithCurrent(t.Context(), cut, func() error {
			calls.Add(1)
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	poll, stopPoll := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stopPoll()
	if err := o.Publish(poll, &sample{1}, sameSample); err != nil {
		t.Fatal("identical polling blocked behind installation pin", err)
	}
	changed := o.Changes()
	installed := make(chan error, 1)
	go func() { installed <- o.Publish(t.Context(), &sample{2}, sameSample) }()
	wait, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := o.WithCurrent(wait, cut, func() error { calls.Add(1); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("canceled waiter entered pin", err)
	}
	rotate, stopRotate := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stopRotate()
	if err := o.Publish(rotate, &sample{3}, sameSample); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("canceled replacement crossed held callback", err)
	}
	if !cut.Current() || o.Capture().Value() != cut.Value() {
		t.Fatal("lock-free capture/current failed while gated")
	}
	select {
	case err := <-installed:
		t.Fatal("replacement crossed held callback", err)
	case <-changed:
		t.Fatal("change signaled before installation")
	default:
	}
	unblock()
	if err := <-done; err != nil || calls.Load() != 1 {
		t.Fatal("callback not synchronous exactly once", err, calls.Load())
	}
	if err := <-installed; err != nil || cut.Current() {
		t.Fatal("replacement did not finish", err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("replacement omitted wake")
	}
}

func TestOwnerInvalidObsoleteCanceledAndCallbackFailure(t *testing.T) {
	var o, other Owner[sample]
	cut := publishSample(t, &o, 1)
	foreign := publishSample(t, &other, 1)
	var nilOwner *Owner[sample]
	if nilOwner.Capture().Current() || nilOwner.Changes() != nil || (Cut[sample]{}).Value() != nil {
		t.Fatal("nil/zero capture usable")
	}
	called := 0
	fn := func() error { called++; return nil }
	for _, candidate := range []Cut[sample]{{}, foreign} {
		if err := o.WithCurrent(t.Context(), candidate, fn); !errors.Is(err, ErrObsolete) {
			t.Fatal("unknown cut entered callback", err)
		}
	}
	if err := nilOwner.WithCurrent(t.Context(), cut, fn); !errors.Is(err, ErrObsolete) {
		t.Fatal(err)
	}
	if err := o.WithCurrent(t.Context(), cut, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := o.WithCurrent(canceled, cut, fn); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := o.Publish(canceled, &sample{2}, sameSample); !errors.Is(err, context.Canceled) || !cut.Current() {
		t.Fatal("canceled replacement mutated state", err)
	}
	if called != 0 {
		t.Fatal("rejected capability called callback")
	}
	want := errors.New("callback failed")
	if err := o.WithCurrent(t.Context(), cut, func() error { return want }); !errors.Is(err, want) {
		t.Fatal("callback error replaced", err)
	}
	func() {
		defer func() {
			if recover() != "test panic" {
				t.Fatal("callback panic not propagated")
			}
		}()
		_ = o.WithCurrent(t.Context(), cut, func() error { panic("test panic") })
	}()
	publishSample(t, &o, 2)
	if err := o.WithCurrent(t.Context(), cut, fn); !errors.Is(err, ErrObsolete) || called != 0 {
		t.Fatal("stale cut entered callback", err)
	}
}

func TestOwnerCounterExhaustionCannotWrapOrInvalidateLastGood(t *testing.T) {
	var o Owner[sample]
	cut := publishSample(t, &o, 1)
	ch := o.Changes()
	o.sequence = math.MaxUint64
	if err := o.Publish(t.Context(), &sample{2}, sameSample); !errors.Is(err, ErrExhausted) || !cut.Current() {
		t.Fatal("counter wrapped or last-good state changed", err)
	}
	if err := o.Publish(t.Context(), &sample{1}, sameSample); err != nil {
		t.Fatal("equal no-op consumed identity", err)
	}
	select {
	case <-ch:
		t.Fatal("failed replacement woke subscribers")
	default:
	}
}

func TestOwnerConcurrentCapturesAlwaysBindOneState(t *testing.T) {
	var o Owner[sample]
	publishSample(t, &o, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		for n := 2; n <= 300; n++ {
			if err := o.Publish(t.Context(), &sample{n}, sameSample); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				c := o.Capture()
				if !strings.HasSuffix(c.Identity(), fmt.Sprintf("/%d", c.Value().number)) {
					t.Error("mixed value/identity capture")
					return
				}
			}
		})
	}
	wg.Wait()
}
