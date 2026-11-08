// Package loginlimit reserves bounded password-attempt capacity in a shared
// durable document. Admission is not authentication or permission to issue a
// session. A backend must atomically retain the original read through its write.
package loginlimit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sync/atomic"
	"time"
)

const (
	// MaxBytes bounds the complete serialized document before decoding.
	MaxBytes = 512 << 10
	// MaxKeys bounds live IP-prefix and username entries together.
	MaxKeys = 4096
	// Lifetime bounds an original reservation and each backend operation.
	Lifetime = 30 * time.Second
)

var (
	// ErrUnavailable refuses authentication rather than using local counters.
	ErrUnavailable = errors.New("loginlimit: shared admission unavailable")
	// ErrState covers invalid documents, rollback and stale reservations.
	ErrState = errors.New("loginlimit: invalid or stale state")
	// ErrCapacity never permits eviction of live lockouts or reservations.
	ErrCapacity = errors.New("loginlimit: capacity exhausted")
)

// Backend invokes change once against a current observation, then writes once
// with that original identity. A conflict or unknown completion is never retried.
// A nil observation means confirmed absence, not a failed read.
type Backend interface {
	UpdateLoginLimit(context.Context, func([]byte) ([]byte, error)) error
}

// Policy is immutable for the lifetime of a durable document.
type Policy struct {
	Failures int           `json:"failures"`
	Window   time.Duration `json:"window"`
	Lockout  time.Duration `json:"lockout"`
}

// Locked reports either a durable lockout or fully reserved attempt capacity.
type Locked struct{ RetryAfter time.Duration }

func (*Locked) Error() string { return "loginlimit: too many password attempts" }

// Limiter coordinates one replica with the shared document owner.
type Limiter struct {
	backend  Backend
	now      func() time.Time
	policies [2]Policy
}

// New creates an adapter without performing I/O. The default clock is time.Now.
func New(backend Backend, ip, user Policy, now func() time.Time) (*Limiter, error) {
	for _, p := range []Policy{ip, user} {
		if p.Failures < 1 || p.Failures > 100 || p.Window <= 0 || p.Window > time.Hour || p.Lockout <= 0 || p.Lockout > time.Hour {
			return nil, ErrState
		}
	}
	if backend == nil {
		return nil, ErrState
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{backend: backend, now: now, policies: [2]Policy{ip, user}}, nil
}

// Validate checks a stored document without applying current-time expiry or
// changing it. Replica validation must not reinterpret source time locally.
func Validate(raw []byte, ip, user Policy) error {
	if len(raw) == 0 || len(raw) > MaxBytes {
		return ErrState
	}
	var header struct{ At time.Time }
	if err := json.Unmarshal(raw, &header); err != nil {
		return ErrState
	}
	l := &Limiter{policies: [2]Policy{ip, user}}
	_, err := l.decode(raw, header.At)
	return err
}

type pending struct {
	ID       string    `json:"id"`
	Sequence uint64    `json:"sequence"`
	Issued   time.Time `json:"issued"`
	Expires  time.Time `json:"expires"`
}
type entry struct {
	Kind            int       `json:"kind"`
	Failures        int       `json:"failures"`
	FailureSequence uint64    `json:"failureSequence"`
	Last            time.Time `json:"last"`
	LockedUntil     time.Time `json:"lockedUntil"`
	Pending         []pending `json:"pending"`
}
type document struct {
	Version  int               `json:"version"`
	Sequence uint64            `json:"sequence"`
	Salt     string            `json:"salt"`
	At       time.Time         `json:"at"`
	Policies [2]Policy         `json:"policies"`
	Entries  map[string]*entry `json:"entries"`
}

// Ticket is an opaque one-use reservation. Copies share consumption; neither a
// failed completion nor an expired reservation can be reused for another login.
type Ticket struct{ state *ticket }
type ticket struct {
	owner    *Limiter
	original context.Context
	keys     [2]string
	pending  pending
	used     atomic.Bool
}

func (Ticket) String() string               { return "private login reservation" }
func (t Ticket) GoString() string           { return t.String() }
func (Ticket) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

func token() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
func validToken(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(b) == s
}
func key(salt, kind, raw string) string {
	h := hmac.New(sha256.New, []byte(salt))
	_, _ = h.Write([]byte(kind + "\x00" + raw))
	return hex.EncodeToString(h.Sum(nil))
}

func (l *Limiter) decode(raw []byte, now time.Time) (*document, error) {
	if now.IsZero() {
		return nil, ErrState
	}
	if len(raw) == 0 {
		return &document{Version: 1, Sequence: 1, Salt: token(), At: now, Policies: l.policies, Entries: map[string]*entry{}}, nil
	}
	if len(raw) > MaxBytes {
		return nil, ErrCapacity
	}
	var d document
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return nil, ErrState
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, ErrState
	}
	if d.Version != 1 || d.Sequence == 0 || d.Sequence == math.MaxUint64 || !validToken(d.Salt) || d.At.IsZero() || now.Before(d.At) || d.Policies != l.policies || d.Entries == nil || len(d.Entries) > MaxKeys {
		return nil, ErrState
	}
	for k, e := range d.Entries {
		if !validToken(k) || e == nil || e.Kind < 0 || e.Kind > 1 {
			return nil, ErrState
		}
		p := l.policies[e.Kind]
		if e.Failures < 0 || e.Failures >= p.Failures || e.Last.After(d.At) || e.FailureSequence > d.Sequence || (e.Failures > 0 && (e.Last.IsZero() || e.FailureSequence == 0)) || len(e.Pending)+e.Failures > p.Failures || e.LockedUntil.After(d.At.Add(p.Lockout)) {
			return nil, ErrState
		}
		seen := map[string]bool{}
		for _, q := range e.Pending {
			if !validToken(q.ID) || seen[q.ID] || q.Sequence == 0 || q.Sequence > d.Sequence || q.Issued.IsZero() || q.Issued.After(d.At) || !q.Expires.After(q.Issued) || q.Expires.Sub(q.Issued) > Lifetime {
				return nil, ErrState
			}
			seen[q.ID] = true
		}
	}
	// Only validated, expired state is reclaimed. Live keys are never evicted
	// to admit a flood of new identities. Expiry charges abandoned work.
	d.Sequence++
	for k, e := range d.Entries {
		p := l.policies[e.Kind]
		kept := e.Pending[:0]
		for _, q := range e.Pending {
			if !now.Before(q.Expires) {
				fail(e, p, q.Expires, d.Sequence)
			} else {
				kept = append(kept, q)
			}
		}
		e.Pending = kept
		if now.Sub(e.Last) > p.Window {
			e.Failures = 0
		}
		if len(e.Pending) == 0 && e.Failures == 0 && !now.Before(e.LockedUntil) {
			delete(d.Entries, k)
		}
	}
	d.At = now
	return &d, nil
}
func fail(e *entry, p Policy, at time.Time, sequence uint64) {
	if at.Before(e.Last) {
		at = e.Last
	}
	if at.Sub(e.Last) > p.Window {
		e.Failures = 0
	}
	e.Last = at
	e.FailureSequence = sequence
	if at.Before(e.LockedUntil) {
		return
	}
	e.Failures++
	if e.Failures >= p.Failures {
		e.Failures = 0
		e.LockedUntil = at.Add(p.Lockout)
	}
}
func encode(d *document) ([]byte, error) {
	b, err := json.Marshal(d)
	if len(b) > MaxBytes {
		return nil, ErrCapacity
	}
	return b, err
}
func (l *Limiter) update(ctx context.Context, change func(*document, time.Time) error) error {
	if ctx == nil {
		return ErrUnavailable
	}
	if _, ok := ctx.Deadline(); !ok {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, Lifetime)
	defer cancel()
	err := l.backend.UpdateLoginLimit(ctx, func(raw []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := l.now().UTC()
		d, err := l.decode(raw, now)
		if err != nil {
			return nil, err
		}
		if err = change(d, now); err != nil {
			return nil, err
		}
		return encode(d)
	})
	// A committed update is not admission after the original operation ended.
	// Keep its durable charge; do not retry or return a usable ticket/result.
	if err == nil {
		err = ctx.Err()
	}
	var locked *Locked
	if err != nil && !errors.As(err, &locked) {
		return errors.Join(ErrUnavailable, err)
	}
	return err
}

// Begin reserves IP and username capacity before any password verification.
// Caller supplies normalized bounded identities and one original finite context.
func (l *Limiter) Begin(ctx context.Context, ip, user string) (Ticket, error) {
	if len(ip) == 0 || len(ip) > 128 || len(user) == 0 || len(user) > 253 {
		return Ticket{}, ErrUnavailable
	}
	if ctx == nil {
		return Ticket{}, ErrUnavailable
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return Ticket{}, ErrUnavailable
	}
	t := &ticket{owner: l, original: ctx}
	id := token()
	err := l.update(ctx, func(d *document, now time.Time) error {
		// Durable equality uses UTC wall time. An original context deadline
		// may carry monotonic metadata that JSON cannot preserve.
		expires := minTime(deadline, now.Add(Lifetime)).UTC()
		if !expires.After(now) {
			return ErrState
		}
		t.pending = pending{ID: id, Sequence: d.Sequence, Issued: now, Expires: expires}
		t.keys = [2]string{key(d.Salt, "ip", ip), key(d.Salt, "user", user)}
		for i, k := range t.keys {
			e := d.Entries[k]
			if e == nil {
				if len(d.Entries) >= MaxKeys {
					return ErrCapacity
				}
				e = &entry{Kind: i}
				d.Entries[k] = e
			}
			if e.Kind != i {
				return ErrState
			}
			if now.Before(e.LockedUntil) {
				return &Locked{RetryAfter: e.LockedUntil.Sub(now)}
			}
			if e.Failures+len(e.Pending) >= l.policies[i].Failures {
				until := now.Add(Lifetime)
				for _, q := range e.Pending {
					until = minTime(until, q.Expires)
				}
				return &Locked{RetryAfter: until.Sub(now)}
			}
			e.Pending = append(e.Pending, t.pending)
		}
		return nil
	})
	if err != nil {
		return Ticket{}, err
	}
	return Ticket{state: t}, nil
}

// Finish records this one authentication result. Success never clears IP
// failures or another in-flight attempt; later username failures survive too.
// Unknown completion cannot issue a session and is not retried.
func (t Ticket) Finish(ctx context.Context, success bool) error {
	s := t.state
	if s == nil || ctx == nil || !s.used.CompareAndSwap(false, true) {
		return ErrUnavailable
	}
	if err := s.original.Err(); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	original, cancel := context.WithCancel(s.original)
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	return s.owner.update(original, func(d *document, now time.Time) error {
		if err := s.original.Err(); err != nil {
			return err
		}
		if now.Before(s.pending.Issued) || !now.Before(s.pending.Expires) {
			return ErrState
		}
		for i, k := range s.keys {
			e := d.Entries[k]
			if e == nil || e.Kind != i {
				return ErrState
			}
			found := -1
			for n, q := range e.Pending {
				if q == s.pending {
					found = n
					break
				}
			}
			if found < 0 {
				return ErrState
			}
			e.Pending = append(e.Pending[:found], e.Pending[found+1:]...)
			if !success {
				fail(e, s.owner.policies[i], now, d.Sequence)
			} else if i == 1 && e.FailureSequence <= s.pending.Sequence {
				e.Failures = 0
				e.LockedUntil = time.Time{}
			}
		}
		return nil
	})
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
