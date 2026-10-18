package externaldns

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestServeShutdownResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "clean"},
		{name: "serve error", err: errors.New("accept failed during shutdown")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			ln := &shutdownListener{ready: make(chan struct{}), closed: make(chan struct{}), acceptErr: tc.err}
			defer func() { _ = ln.Close() }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- s.Serve(ctx, ln) }()

			select {
			case <-ln.ready:
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not enter the accept loop")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, tc.err) {
					t.Fatalf("Serve after shutdown: got %v, want %v", err, tc.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve did not stop after cancellation")
			}
		})
	}
}

type shutdownListener struct {
	ready, closed chan struct{}
	closeOnce     sync.Once
	acceptErr     error
}

func (l *shutdownListener) Accept() (net.Conn, error) {
	if l.acceptErr != nil {
		return nil, &shutdownAcceptError{error: l.acceptErr, ready: l.ready, closed: l.closed}
	}
	close(l.ready)
	<-l.closed
	return nil, net.ErrClosed
}

func (l *shutdownListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (*shutdownListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

type shutdownAcceptError struct {
	error
	ready  chan struct{}
	closed <-chan struct{}
}

func (e *shutdownAcceptError) Unwrap() error { return e.error }
func (*shutdownAcceptError) Timeout() bool   { return false }

func (e *shutdownAcceptError) Temporary() bool {
	// net/http has already checked its shutdown state before calling this.
	// Hold the failure until Shutdown closes the listener so that Serve's
	// cancellation branch must receive and report it while draining errCh.
	close(e.ready)
	<-e.closed
	return false
}
