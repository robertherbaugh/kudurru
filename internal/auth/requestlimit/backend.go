package requestlimit

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
)

// Backend invokes change once, then writes once with the original observed
// identity. Confirmed absence may create; conflicts and uncertain writes must
// never retry. It must retain ctx across the read and write.
type Backend interface {
	UpdateRequestLimit(context.Context, func([]byte) ([]byte, error)) error
}

// ErrUnavailable refuses request dispatch when shared admission is uncertain.
var ErrUnavailable = errors.New("requestlimit: shared admission unavailable")

// Limiter charges the same fixed document through a shared backend.
type Limiter struct {
	backend Backend
	now     func() time.Time
}

// New performs no I/O and creates no local fallback counters.
func New(backend Backend, now func() time.Time) (*Limiter, error) {
	if backend == nil {
		return nil, ErrState
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{backend: backend, now: now}, nil
}

// Admit counts one request regardless of its later outcome. A successful return
// permits dispatch only; it grants no credential or authentication authority.
// Late cancellation/clock failure retains any real charge and never refunds it.
func (l *Limiter) Admit(ctx context.Context, lane Lane, client string) error {
	if l == nil || ctx == nil {
		return ErrUnavailable
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > Lifetime {
		return errors.Join(ErrUnavailable, ErrState)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	start := l.now().UTC()
	bound := start.Add(Lifetime)
	if deadline.Before(bound) {
		bound = deadline
	}
	if start.IsZero() || !bound.After(start) {
		return errors.Join(ErrUnavailable, ErrState)
	}
	called := false
	var observed time.Time
	err := l.backend.UpdateRequestLimit(ctx, func(raw []byte) ([]byte, error) {
		if called {
			return nil, ErrState
		}
		called = true
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		observed = l.now().UTC()
		if observed.Before(start) || !observed.Before(bound) {
			return nil, ErrState
		}
		return charge(raw, lane, client, observed)
	})
	var locked *loginlimit.Locked
	if errors.As(err, &locked) {
		return locked
	}
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	now := l.now().UTC()
	if !called || now.Before(observed) || !now.Before(bound) {
		return errors.Join(ErrUnavailable, ErrState)
	}
	return nil
}

// Validate checks a stored document without reinterpreting source time.
func Validate(raw []byte) error { _, err := decode(raw); return err }

// ClientKey normalizes IPv4 and IPv6 /64 prefixes. Invalid input shares one
// bounded unknown key rather than creating attacker-controlled identity keys.
func ClientKey(raw string) string {
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	ip, err := netip.ParseAddr(raw)
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		return netip.PrefixFrom(ip, 64).Masked().String()
	}
	return ip.String()
}
