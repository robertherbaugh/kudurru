//go:build linux

package replica

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// TestWriteWhoseDialRunsIntoTheDeadlineStaysUnknown pins the documented limit
// of the "not sent" answer (D368, docs/deploy/sqlite-join.md): a connection
// attempt that is still pending when the exchange's deadline ends surfaces as
// the context's deadline, not as a dial error, and stays an unknown outcome.
// A listener with a full accept queue drops the next SYN, so the dial hangs
// like one to a host that silently drops packets.
func TestWriteWhoseDialRunsIntoTheDeadlineStaysUnknown(t *testing.T) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = syscall.Close(fd)
		t.Fatal(err)
	}
	// Backlog 0 leaves room for exactly one completed connection that is
	// never accepted.
	if err := syscall.Listen(fd, 0); err != nil {
		_ = syscall.Close(fd)
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "full-queue")
	full, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = full.Close() })
	address := full.Addr().String()
	var fillers []net.Conn
	t.Cleanup(func() {
		for _, c := range fillers {
			_ = c.Close()
		}
	})
	// Fill the queue until a dial no longer completes.
	for {
		c, err := net.DialTimeout("tcp", address, 300*time.Millisecond)
		if err != nil {
			break
		}
		fillers = append(fillers, c)
		if len(fillers) > 16 {
			t.Fatal("the kernel keeps completing connections to a full accept queue")
		}
	}

	p := openStore(t, replication.Primary)
	mirror := mirrorOf(t, p)
	creds := credentials(t, 1)
	peer := Peer{MemberID: p.Authority().MemberID, ClusterID: p.Authority().Term.ClusterID, Address: address}
	client := clientFor(t, mirror, creds, func(o *ClientOptions) { o.Timeout = 2 * time.Second })
	// The dialer's timeout and the client's are the same exchange timeout,
	// so either timer may end the pending dial (hosted CI saw the dialer's
	// "i/o timeout" first). A shorter caller deadline ends it through the
	// context instead. Every way stays unknown.
	for _, c := range []struct {
		name    string
		timeout time.Duration
	}{{"exchange timeout", 0}, {"caller deadline", 500 * time.Millisecond}} {
		ctx := t.Context()
		if c.timeout > 0 {
			var cancel func()
			ctx, cancel = context.WithTimeout(ctx, c.timeout)
			defer cancel()
		}
		_, e := client.Write(ctx, peer, commandFor(p, "dial-timed-out"))
		if !errors.Is(e, ErrUnavailable) && !errors.Is(e, context.DeadlineExceeded) || !errors.Is(e, ErrOutcomeUnknown) {
			t.Fatalf("dial that ran into the deadline (%s): %v", c.name, e)
		}
	}
}

// A dial that the dialer's own timer ended is reported like one the
// exchange's deadline ended, whichever fires first; only a dial that failed
// outright (refused, unreachable) was certainly not sent.
func TestDialTimeoutIsNotANotSentDial(t *testing.T) {
	timeout := errors.Join(ErrUnavailable, &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}})
	if notDispatched(timeout) {
		t.Fatal("a timed-out dial was reported as not sent")
	}
	refused := errors.Join(ErrUnavailable, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})
	if !notDispatched(refused) {
		t.Fatal("a refused dial was not reported as not sent")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
