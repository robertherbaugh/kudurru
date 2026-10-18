package blocking

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Forever is what DisabledUntil reports for a disable without a timer.
var Forever = time.Unix(0, math.MaxInt64).UTC()

// Publisher is the replication seam of the temporary disable
// (dev-docs/design-record.md §7.1 step 7): a local Disable or Enable is handed
// to it so that every replica follows. The cluster-wide implementations
// are wired later and are not part of this package:
//
//   - Kubernetes v0.1: server-side apply of the annotation
//     kudurru.io/blocking-disabled-until on the kudurru-runtime ConfigMap
//     (Kudurru.status.blocking.disabledUntil once Kudurru is a CRD).
//   - Docker: a row in state.db through the Store.
//   - ha-static twins: none; the toggle stays node-local (badged in the UI).
//
// Every replica, including the one that published, watches that field and
// calls Toggle.Apply with the new value. PublishDisabledUntil runs on the
// caller's goroutine (an API request) and must not block the query path;
// a zero time means enabled, Forever means disabled without a timer.
type Publisher interface {
	PublishDisabledUntil(until time.Time) error
}

// Toggle is the in-process temporary blocking switch. Enabled is one
// atomic load while blocking is on. It is safe for concurrent use.
type Toggle struct {
	until atomic.Int64 // unix nanoseconds; 0 = enabled
	now   func() time.Time

	mu       sync.Mutex
	pub      Publisher
	onChange func(until time.Time)
}

// NewToggle returns an enabled toggle. now is the clock; nil means
// time.Now.
func NewToggle(now func() time.Time) *Toggle {
	if now == nil {
		now = time.Now
	}
	return &Toggle{now: now}
}

// SetPublisher installs the replication seam; nil keeps changes local.
func (t *Toggle) SetPublisher(p Publisher) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pub = p
}

// SetOnChange installs an observer Apply calls, on the caller's goroutine,
// whenever the disabled-until value actually changes (a local Disable or
// Enable as well as a replicated Apply). The observer must not block: the
// edge cache flusher uses it to expunge dnsdist's resolver cache. nil
// removes it.
func (t *Toggle) SetOnChange(fn func(until time.Time)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onChange = fn
}

// Disable turns blocking off for d (d <= 0: until Enable) and publishes the
// change. It returns the time blocking comes back (Forever without a
// timer) and the publisher's error; the local change applies either way.
func (t *Toggle) Disable(d time.Duration) (time.Time, error) {
	until := Forever
	if d > 0 {
		until = t.now().Add(d)
	}
	t.Apply(until)
	return until, t.publish(until)
}

// Enable turns blocking back on and publishes the change.
func (t *Toggle) Enable() error {
	t.Apply(time.Time{})
	return t.publish(time.Time{})
}

// Apply sets the state without publishing it: the zero time enables, any
// other time disables until then (Forever: until Enable). Replication
// watchers call it with the replicated value.
func (t *Toggle) Apply(until time.Time) {
	var v int64
	switch {
	case until.IsZero():
		v = 0
	case !until.Before(Forever):
		v = math.MaxInt64
	default:
		v = until.UnixNano()
	}
	if t.until.Swap(v) == v {
		return
	}
	t.mu.Lock()
	fn := t.onChange
	t.mu.Unlock()
	if fn != nil {
		fn(until)
	}
}

// Enabled reports whether blocking is on; an expired disable re-enables
// it. A nil Toggle is always enabled.
func (t *Toggle) Enabled() bool {
	if t == nil {
		return true
	}
	u := t.until.Load()
	if u == 0 {
		return true
	}
	if u != math.MaxInt64 && t.now().UnixNano() >= u {
		t.until.CompareAndSwap(u, 0)
		return true
	}
	return false
}

// DisabledUntil returns when blocking comes back: the zero time while
// enabled, Forever for a disable without a timer.
func (t *Toggle) DisabledUntil() time.Time {
	if t.Enabled() {
		return time.Time{}
	}
	u := t.until.Load()
	switch u {
	case 0:
		return time.Time{}
	case math.MaxInt64:
		return Forever
	}
	return time.Unix(0, u)
}

func (t *Toggle) publish(until time.Time) error {
	t.mu.Lock()
	p := t.pub
	t.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.PublishDisabledUntil(until)
}
