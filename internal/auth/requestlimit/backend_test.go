package requestlimit

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
)

type requestBackend struct {
	mu     sync.Mutex
	raw    []byte
	writes int
	after  func()
	lost   error
	omit   bool
}

func (b *requestBackend) UpdateRequestLimit(_ context.Context, change func([]byte) ([]byte, error)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.omit {
		return nil
	}
	next, err := change(bytes.Clone(b.raw))
	if err != nil {
		return err
	}
	b.raw = bytes.Clone(next)
	b.writes++
	if b.after != nil {
		b.after()
	}
	return b.lost
}

func TestRequestBackendSharedPolicyAndOriginalCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), Lifetime)
	defer cancel()
	b := new(requestBackend)
	a, err := New(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := New(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		l := a
		if i%2 == 1 {
			l = other
		}
		if err := l.Admit(ctx, Start, ClientKey("[2001:db8:1::a]:111")); err != nil {
			t.Fatal(err)
		}
	}
	recreated, err := New(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	var locked *loginlimit.Locked
	if err := recreated.Admit(ctx, Start, ClientKey("2001:db8:1::b")); !errors.As(err, &locked) || b.writes != 5 {
		t.Fatal("new adapter or IPv6 host bypassed prefix budget", err)
	}
	if err := recreated.Admit(ctx, Callback, ClientKey("2001:db8:1::b")); err != nil || b.writes != 6 {
		t.Fatal("independent callback lane locked", err)
	}
	if bytes.Contains(b.raw, []byte("2001:db8")) {
		t.Fatal("raw address stored")
	}
	if ClientKey("::ffff:192.0.2.1") != "192.0.2.1" || ClientKey("arbitrary-identity") != "unknown" {
		t.Fatal("unbounded or mapped address identity")
	}
}

func TestRequestBackendLateFailureRetainsChargeWithoutGrantOrRetry(t *testing.T) {
	for _, mode := range []string{"lost", "cancel", "clock", "expiry", "no-callback", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), Lifetime)
			defer cancel()
			now := time.Now().UTC()
			b := new(requestBackend)
			switch mode {
			case "lost":
				b.lost = errors.New("test: committed reply lost")
			case "cancel":
				b.after = cancel
			case "clock":
				b.after = func() { now = now.Add(-time.Second) }
			case "expiry":
				b.after = func() { now = now.Add(Lifetime) }
			case "no-callback":
				b.omit = true
			case "malformed":
				b.raw = []byte(`{"version":99}`)
			}
			l, err := New(b, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			err = l.Admit(ctx, Start, "192.0.2.1")
			if !errors.Is(err, ErrUnavailable) {
				t.Fatal("late error granted dispatch", err)
			}
			want := 1
			if mode == "no-callback" || mode == "malformed" {
				want = 0
			}
			if b.writes != want {
				t.Fatal("charge was retried or refunded", b.writes)
			}
			if want == 1 && Validate(b.raw) != nil {
				t.Fatal("actual charge not retained")
			}
		})
	}
}
