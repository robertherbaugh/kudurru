package license

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"time"
)

// ErrCapture rejects missing or expired captured management authority.
var ErrCapture = errors.New("license: captured authority unavailable")

type installation struct {
	identity string
	claims   *Claims
	err      error
	source   string
}

// Cut is one immutable management policy and its original validity interval.
// Its identity is local installation evidence, not durable entitlement history.
// Root must persist withdrawal/reactivation floors before external reactivation.
type Cut struct {
	manager *Manager
	value   *installation
	status  Status
	at      time.Time
	until   time.Time
	phase   string
}

func (m *Manager) clock() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

// refreshLocked runs only under the replacement mutex, or during construction.
// Repeated source polling with identical claims does not rotate authority.
func (m *Manager) refreshLocked() {
	old := m.current.Load()
	if old != nil && old.source == m.source && sameError(old.err, m.err) && reflect.DeepEqual(old.claims, m.claims) {
		return
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	next := &installation{identity: hex.EncodeToString(id[:]), err: m.err, source: m.source}
	if m.claims != nil {
		claims := *m.claims
		claims.Features = slices.Clone(claims.Features)
		next.claims = &claims
	}
	m.current.Store(next)
	if m.changed != nil {
		close(m.changed)
	}
	m.changed = make(chan struct{})
}

func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Error() == b.Error()
}

// Capture reads immutable memory only. A nil or uninitialized Manager has no
// captured authority; use New for an explicit community installation.
func (m *Manager) Capture() Cut {
	if m == nil {
		return Cut{}
	}
	v := m.current.Load()
	if v == nil {
		return Cut{}
	}
	now := m.clock()
	c := Cut{manager: m, value: v, at: now, status: statusAt(v.claims, v.err, v.source, now), phase: "denied"}
	if v.claims != nil && v.err == nil {
		nbf := time.Unix(v.claims.NotBefore, 0)
		end := v.claims.Expires().Add(Grace)
		switch {
		case now.Before(nbf):
			c.phase, c.until = "not-yet-valid", now.Add(nbf.Sub(now))
		case now.Before(end):
			c.phase, c.until = "granted", now.Add(end.Sub(now))
		default:
			c.phase = "expired"
		}
	}
	return c
}

// Identity distinguishes installations and policy intervals. Identical-byte
// reactivation after replacement cannot revive a held cut.
func (c Cut) Identity() string {
	if c.value == nil {
		return ""
	}
	return "license-local/" + c.value.identity + "/" + c.phase
}

// Status returns detached policy as evaluated at capture time. The caller must
// check Current before using it for new protected work and again at commit.
func (c Cut) Status() Status {
	s := c.status
	s.Features = slices.Clone(s.Features)
	if s.ExpiresAt != nil {
		v := *s.ExpiresAt
		s.ExpiresAt = &v
	}
	if s.GraceEndsAt != nil {
		v := *s.GraceEndsAt
		s.GraceEndsAt = &v
	}
	return s
}

// Current checks only installation memory and the original absolute deadline.
// It neither takes a lock nor reads credentials, files, SQLite or the network.
func (c Cut) Current() bool {
	if c.manager == nil || c.value == nil || c.manager.current.Load() != c.value || c.at.IsZero() {
		return false
	}
	now := c.manager.clock()
	return !now.Before(c.at) && (c.until.IsZero() || now.Before(c.until))
}

// Changes signals actual installations, not identical polling. Deadline expiry
// is checked by Cut.Current; subscribers must also arrange deadline/timer wakes.
func (m *Manager) Changes() <-chan struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.changed
}

// WithCurrent excludes Set/Load replacement through the entire callback. Use it
// outside a complete command/publication barrier, after resource exclusion and
// before SQLite. No network or nested Manager methods that lock may run in fn.
// The callback uses Cut.Status and Current; time expiry still needs a final check.
func (m *Manager) WithCurrent(ctx context.Context, fn func(Cut) error) error {
	if m == nil || ctx == nil || fn == nil {
		return ErrCapture
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.mu.TryRLock() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if m.mu.TryRLock() {
					goto locked
				}
			}
		}
	}
locked:
	defer m.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	cut := m.Capture()
	if !cut.Current() {
		return ErrCapture
	}
	return fn(cut)
}
