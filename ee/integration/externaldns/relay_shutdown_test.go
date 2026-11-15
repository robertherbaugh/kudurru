package externaldns

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// A TCP connection accepted by net/http but still waiting for its first request
// is owned by this relay too. Stop must close it without waiting for an HTTP
// request or consuming the entire graceful-handler shutdown budget.
func TestRelayShutdownClosesAcceptedPreRequestSocket(t *testing.T) {
	relay, err := NewRelay(RelayOptions{Upstream: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	read := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx, &relayReadListener{Listener: ln, read: read}) }()
	peer, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	select {
	case <-read: // actual server read, after it owns this accepted connection
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not begin reading accepted connection")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("idle accepted socket caused shutdown refusal: deadline=%t canceled=%t", errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay failed to join canceled listener")
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if _, err := peer.Read(data[:]); err == nil {
		t.Fatal("stopped relay still accepts socket traffic")
	} else {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("accepted connection remained open after joined shutdown")
		}
	}
}

type relayReadListener struct {
	net.Listener
	read chan struct{}
}

func (l *relayReadListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &relayReadConn{Conn: c, read: l.read}, nil
}

type relayReadConn struct {
	net.Conn
	once sync.Once
	read chan struct{}
}

func (c *relayReadConn) Read(b []byte) (int, error) {
	c.once.Do(func() { close(c.read) })
	return c.Conn.Read(b)
}

// A completed OS accept can still be handed to net/http after Shutdown closes
// the listener. Exercise that ordering explicitly, without a timing delay.
func TestRelayShutdownClosesConnectionReturnedDuringStop(t *testing.T) {
	relay, err := NewRelay(RelayOptions{Upstream: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	held := &relayHeldAccept{Listener: ln, accepted: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- relay.Serve(ctx, held) }()
	peer, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	select {
	case <-held.accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("listener did not accept peer")
	}
	cancel()
	select {
	case <-held.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not close listener")
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("late accepted socket caused shutdown refusal")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not join late accept")
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var data [1]byte
	if _, err := peer.Read(data[:]); err == nil {
		t.Fatal("late accepted socket stayed open")
	} else {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("late accepted socket was not closed")
		}
	}
}

type relayHeldAccept struct {
	net.Listener
	accepted, release, closed chan struct{}
	once                      sync.Once
}

func (l *relayHeldAccept) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	close(l.accepted)
	<-l.release
	return conn, nil
}

func (l *relayHeldAccept) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}
