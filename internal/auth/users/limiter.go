package users

import (
	"sync"
	"time"
)

// LimitPolicy locks a key out for Lockout once it has collected Failures
// failed logins, each within Window of the previous one.
type LimitPolicy struct {
	Failures int
	Window   time.Duration
	Lockout  time.Duration
}

// Default login limits of §8.7: per client IP 5 failures → 5 min, per user
// 10 failures → 15 min.
var (
	DefaultIPLimit   = LimitPolicy{Failures: 5, Window: 5 * time.Minute, Lockout: 5 * time.Minute}
	DefaultUserLimit = LimitPolicy{Failures: 10, Window: 15 * time.Minute, Lockout: 15 * time.Minute}
)

// maxLimiterKeys bounds the memory of a limiter: a flood of distinct
// usernames or addresses evicts idle entries first, then the oldest.
const maxLimiterKeys = 100_000

// Limiter counts failures per key (a client IP or a username). It is safe
// for concurrent use and lives in memory: every replica limits on its own,
// which still caps a single attacker's rate per replica.
type Limiter struct {
	policy LimitPolicy
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]*limitEntry
}

type limitEntry struct {
	failures    int
	last        time.Time
	lockedUntil time.Time
}

// NewLimiter returns a limiter with policy; now is the clock (nil:
// time.Now).
func NewLimiter(policy LimitPolicy, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{policy: policy, now: now, entries: map[string]*limitEntry{}}
}

// Locked returns how long key stays locked out, or 0.
func (l *Limiter) Locked(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return 0
	}
	if d := e.lockedUntil.Sub(l.now()); d > 0 {
		return d
	}
	return 0
}

// Fail records a failure and returns the lockout it triggered, or 0.
func (l *Limiter) Fail(key string) time.Duration {
	if l.policy.Failures <= 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil {
		if len(l.entries) >= maxLimiterKeys {
			l.evict(now)
		}
		e = &limitEntry{}
		l.entries[key] = e
	}
	if now.Sub(e.last) > l.policy.Window {
		e.failures = 0
	}
	e.failures++
	e.last = now
	if e.failures >= l.policy.Failures {
		e.failures = 0
		e.lockedUntil = now.Add(l.policy.Lockout)
		return l.policy.Lockout
	}
	return 0
}

// Reset forgets key (a successful login).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// Len returns the number of tracked keys.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// evict drops idle entries; if none are idle it drops the least recently
// failed one. Called with mu held.
func (l *Limiter) evict(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for k, e := range l.entries {
		if now.After(e.lockedUntil) && now.Sub(e.last) > l.policy.Window {
			delete(l.entries, k)
			continue
		}
		if oldestKey == "" || e.last.Before(oldest) {
			oldestKey, oldest = k, e.last
		}
	}
	if len(l.entries) >= maxLimiterKeys && oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}
