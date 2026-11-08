package loginlimit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type memoryBackend struct {
	mu            sync.Mutex
	raw           []byte
	before, after error
	committed     func()
	calls         int
}

func (b *memoryBackend) UpdateLoginLimit(ctx context.Context, fn func([]byte) ([]byte, error)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.before != nil {
		return b.before
	}
	next, err := fn(bytes.Clone(b.raw))
	if err != nil {
		return err
	}
	b.raw = bytes.Clone(next)
	if b.committed != nil {
		b.committed()
	}
	return b.after
}
func limits(t *testing.T, b Backend, clock *time.Time) *Limiter {
	t.Helper()
	l, err := New(b, Policy{5, 5 * time.Minute, 5 * time.Minute}, Policy{10, 15 * time.Minute, 15 * time.Minute}, func() time.Time { return *clock })
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func begin(t *testing.T, l *Limiter, ip, user string) Ticket {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), Lifetime)
	t.Cleanup(cancel)
	ticket, err := l.Begin(ctx, ip, user)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}
func finish(t *testing.T, ticket Ticket, success bool) {
	t.Helper()
	if err := ticket.Finish(t.Context(), success); err != nil {
		t.Fatal(err)
	}
}
func locked(t *testing.T, l *Limiter, ip, user string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), Lifetime)
	defer cancel()
	_, err := l.Begin(ctx, ip, user)
	var e *Locked
	if !errors.As(err, &e) || e.RetryAfter <= 0 {
		t.Fatal("expected shared lockout", err)
	}
}
func TestAlternatingReplicasAndRestart(t *testing.T) {
	b := &memoryBackend{}
	now := time.Now().UTC()
	a, c := limits(t, b, &now), limits(t, b, &now)
	for i := range 5 {
		l := a
		if i%2 == 1 {
			l = c
		}
		finish(t, begin(t, l, "192.0.2.1", "alice"), false)
	}
	locked(t, limits(t, b, &now), "192.0.2.1", "other")
	for i := range 5 {
		finish(t, begin(t, c, fmt.Sprint("other-", i), "alice"), false)
	}
	locked(t, a, "fresh", "alice")
	if bytes.Contains(b.raw, []byte("alice")) || bytes.Contains(b.raw, []byte("192.0.2.1")) {
		t.Fatal("raw identity persisted")
	}
}

func TestOriginalEarlierDeadlineSurvivesPersistence(t *testing.T) {
	b := &memoryBackend{}
	l, err := New(b, Policy{5, 5 * time.Minute, 5 * time.Minute}, Policy{10, 15 * time.Minute, 15 * time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	q, err := l.Begin(ctx, "ip", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !q.state.pending.Expires.Equal(deadline) {
		t.Fatal("original deadline extended")
	}
	if err := q.Finish(ctx, true); err != nil {
		t.Fatal("durable reservation lost original identity", err)
	}
}
func TestPendingReservationsAndSuccessIsolation(t *testing.T) {
	b := &memoryBackend{}
	now := time.Now().UTC()
	a, c := limits(t, b, &now), limits(t, b, &now)
	var holds []Ticket
	for range 5 {
		holds = append(holds, begin(t, a, "same-ip", "user"))
	}
	locked(t, c, "same-ip", "user")
	finish(t, holds[0], true)
	for _, q := range holds[1:] {
		finish(t, q, false)
	}
	// One successful attempt did not erase four other IP reservations.
	finish(t, begin(t, c, "same-ip", "user"), false)
	locked(t, a, "same-ip", "different")
	// Equal wall times still have distinct sequences: later failure must
	// survive successful completion of an earlier reservation.
	old := begin(t, a, "one", "isolation")
	finish(t, begin(t, c, "two", "isolation"), false)
	finish(t, old, true)
	for i := range 9 {
		finish(t, begin(t, a, fmt.Sprint("ip-", i), "isolation"), false)
	}
	locked(t, c, "fresh", "isolation")
}

func TestSimultaneousReplicasCannotOverreserve(t *testing.T) {
	b := &memoryBackend{}
	now := time.Now().UTC()
	a, c := limits(t, b, &now), limits(t, b, &now)
	ctx, cancel := context.WithTimeout(t.Context(), Lifetime)
	defer cancel()
	start := make(chan struct{})
	results := make(chan Ticket, 20)
	failures := make(chan error, 20)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l := a
			if i%2 == 1 {
				l = c
			}
			q, err := l.Begin(ctx, "same", "user")
			if err != nil {
				failures <- err
			} else {
				results <- q
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)
	if len(results) != 5 || len(failures) != 15 {
		t.Fatal("shared capacity exceeded", len(results), len(failures))
	}
	for err := range failures {
		var refused *Locked
		if !errors.As(err, &refused) {
			t.Fatal(err)
		}
	}
	for q := range results {
		finish(t, q, false)
	}
	locked(t, a, "same", "other")
}

func TestExpiredLockoutAndIdleCapacityReclaim(t *testing.T) {
	b := &memoryBackend{}
	now := time.Now().UTC()
	l := limits(t, b, &now)
	for range 5 {
		finish(t, begin(t, l, "ip", "user"), false)
	}
	locked(t, l, "ip", "user")
	now = now.Add(16 * time.Minute)
	ctx, cancel := context.WithDeadline(t.Context(), now.Add(Lifetime))
	defer cancel()
	q, err := l.Begin(ctx, "ip", "user")
	if err != nil {
		t.Fatal("expired limits did not reopen", err)
	}
	if err := q.Finish(ctx, true); err != nil {
		t.Fatal(err)
	}
	var d document
	if err := json.Unmarshal(b.raw, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Entries) != 2 {
		t.Fatal("unexpected retained entries", len(d.Entries))
	}
}
func TestSuccessfulUsernameResetAndIPFailures(t *testing.T) {
	b := &memoryBackend{}
	now := time.Now().UTC()
	l := limits(t, b, &now)
	for i := range 9 {
		finish(t, begin(t, l, fmt.Sprint("ip-", i), "user"), false)
	}
	finish(t, begin(t, l, "valid", "user"), true)
	finish(t, begin(t, l, "new", "user"), false)
	finish(t, begin(t, l, "newer", "user"), true)
}
func TestAbandonedExpiryClockRollbackAndOriginalContext(t *testing.T) {
	b := &memoryBackend{}
	now := time.Now().UTC()
	l := limits(t, b, &now)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	q, err := l.Begin(ctx, "a", "user")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := q.Finish(t.Context(), true); !errors.Is(err, ErrUnavailable) {
		t.Fatal("new context rescued canceled attempt", err)
	}
	for i := range 4 {
		c, stop := context.WithTimeout(t.Context(), 5*time.Millisecond)
		_, err := l.Begin(c, "a", fmt.Sprint("user-", i))
		stop()
		if err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Second)
	locked(t, l, "a", "fresh")
	// A successful unrelated mutation persists the clock watermark.
	_, err = l.decode(b.raw, now)
	if err != nil {
		t.Fatal(err)
	}
	c, stop := context.WithTimeout(t.Context(), Lifetime)
	defer stop()
	_, err = l.Begin(c, "unrelated", "person")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(-time.Second)
	if _, err = l.Begin(c, "new", "person"); !errors.Is(err, ErrState) {
		t.Fatal("clock rollback reset state", err)
	}
}
func TestUnknownCompletionAndTicketCopiesNeverReplay(t *testing.T) {
	b := &memoryBackend{after: errors.New("lost reply")}
	now := time.Now().UTC()
	l := limits(t, b, &now)
	ctx, cancel := context.WithTimeout(t.Context(), Lifetime)
	defer cancel()
	if q, err := l.Begin(ctx, "a", "user"); !errors.Is(err, ErrUnavailable) || q.state != nil || b.calls != 1 {
		t.Fatal("unknown reserve granted or retried", err, b.calls)
	}
	b.after = nil
	q := begin(t, l, "a", "user")
	copy := q
	b.after = errors.New("lost completion reply")
	before := b.calls
	if err := q.Finish(ctx, true); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := copy.Finish(ctx, true); !errors.Is(err, ErrUnavailable) || b.calls != before+1 {
		t.Fatal("ticket replay", err, b.calls)
	}
	b.before = errors.New("outage")
	if _, err := l.Begin(ctx, "new", "user"); !errors.Is(err, ErrUnavailable) {
		t.Fatal("outage fell back", err)
	}
}

func TestPostcommitCancellationNeverGrantsAdmission(t *testing.T) {
	for _, phase := range []string{"reserve", "complete"} {
		t.Run(phase, func(t *testing.T) {
			b := &memoryBackend{}
			now := time.Now().UTC()
			l := limits(t, b, &now)
			ctx, cancel := context.WithTimeout(t.Context(), Lifetime)
			defer cancel()
			if phase == "reserve" {
				b.committed = cancel
				q, err := l.Begin(ctx, "ip", "user")
				if !errors.Is(err, context.Canceled) || q.state != nil || len(b.raw) == 0 || b.calls != 1 {
					t.Fatal("canceled committed reserve granted or replayed", err)
				}
			} else {
				q, err := l.Begin(ctx, "ip", "user")
				if err != nil {
					t.Fatal(err)
				}
				b.committed = cancel
				if err := q.Finish(ctx, true); !errors.Is(err, context.Canceled) || b.calls != 2 {
					t.Fatal("canceled committed completion granted or replayed", err)
				}
			}
		})
	}
}
func TestCapacityAndMalformedStateFailClosed(t *testing.T) {
	b := &memoryBackend{}
	now := time.Now().UTC()
	l := limits(t, b, &now)
	d, err := l.decode(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := range MaxKeys {
		d.Entries[key(d.Salt, "user", fmt.Sprint(i))] = &entry{Kind: 1, Failures: 1, Last: now, FailureSequence: 1}
	}
	b.raw, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), Lifetime)
	defer cancel()
	original := bytes.Clone(b.raw)
	if _, err = l.Begin(ctx, "fresh", "fresh"); !errors.Is(err, ErrCapacity) {
		t.Fatal("live capacity evicted", err)
	}
	if !bytes.Equal(original, b.raw) {
		t.Fatal("refusal mutated state")
	}
	for _, raw := range [][]byte{[]byte(`{}`), []byte(`{"version":9}`), []byte(`null`), bytes.Repeat([]byte(" "), MaxBytes+1)} {
		b.raw = raw
		if _, err = l.Begin(ctx, "fresh", "fresh"); !errors.Is(err, ErrUnavailable) {
			t.Fatal("invalid state admitted", err)
		}
	}
}
