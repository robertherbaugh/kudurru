package dhcpha

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthenticatedConnectionCapReleaseAndShutdown(t *testing.T) {
	c := clusterFixture(t)
	cfg := c.configs[0]
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	s, err := newPeerStream(cfg, "transport-test")
	if err != nil {
		t.Fatal(err)
	}
	s.start()
	t.Cleanup(func() { _ = s.Close() })
	dial := func() (net.Conn, error) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", cfg.ListenAddress, c.configs[1].TLS)
		if err != nil {
			return nil, err
		}
		if err = conn.SetDeadline(time.Now().Add(time.Second)); err == nil {
			err = writeFrame(conn, hello{1, cfg.ClusterID, "node2", "transport-test", "raft"})
		}
		if err == nil {
			var h hello
			err = readFrame(conn, &h, 4096)
		}
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		t.Cleanup(func() { _ = conn.Close() })
		return conn, nil
	}
	accepted := make([]net.Conn, 0, maxPeerConnections)
	for range maxPeerConnections {
		if _, err := dial(); err != nil {
			t.Fatal(err)
		}
		conn, err := s.Accept()
		if err != nil {
			t.Fatal(err)
		}
		accepted = append(accepted, conn)
	}
	if len(s.limit) != maxPeerConnections {
		t.Fatalf("authenticated sockets released lifetime slots: %d", len(s.limit))
	}
	if conn, err := dial(); err == nil {
		_ = conn.Close()
		t.Fatal("accepted an authenticated socket beyond the connection cap")
	}
	// The same bound applies before outgoing TCP dials, preventing another
	// unbounded population outside the inbound handshake and Raft pools.
	if conn, err := s.dial(context.Background(), cfg.Peers[1].Address, "raft", time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("outgoing dial bypassed the shared connection cap")
	}
	_ = accepted[0].Close()
	if len(s.limit) != maxPeerConnections-1 {
		t.Fatal("closing an accepted TLS socket did not release its slot")
	}
	if _, err := dial(); err != nil {
		t.Fatalf("released slot could not be reused: %v", err)
	}
	if _, err := s.Accept(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown deadlocked while releasing owned connection slots")
	}
	s.mu.Lock()
	remaining := len(s.connections)
	s.mu.Unlock()
	if remaining != 0 || len(s.limit) != 0 {
		t.Fatalf("shutdown leaked %d sockets and %d slots", remaining, len(s.limit))
	}
}

func TestTrackedConnectionIdleLifetimeAndRequestDeadline(t *testing.T) {
	t.Run("idle closes without a pending read", func(t *testing.T) {
		a, b := net.Pipe()
		defer func() { _ = b.Close() }()
		var released atomic.Int32
		c := &trackedConn{Conn: a, close: func() { released.Add(1) }}
		c.startIdle(30 * time.Millisecond)
		defer func() { _ = c.Close() }()
		eventually(t, time.Second, func() bool { return released.Load() == 1 }, "idle connection release")
		_ = c.Close()
		if released.Load() != 1 {
			t.Fatal("close released an idle slot more than once")
		}
	})
	t.Run("active transfer outlives idle interval", func(t *testing.T) {
		a, b := net.Pipe()
		defer func() { _ = b.Close() }()
		var released atomic.Int32
		c := &trackedConn{Conn: a, close: func() { released.Add(1) }}
		c.startIdle(200 * time.Millisecond)
		defer func() { _ = c.Close() }()
		reading := make(chan error, 1)
		go func() { _, err := io.Copy(io.Discard, b); reading <- err }()
		for range 15 {
			if _, err := c.Write([]byte("snapshot data")); err != nil {
				t.Fatalf("idle timeout interrupted active transfer: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if released.Load() != 0 {
			t.Fatal("active transfer lost its connection slot")
		}
		_ = c.Close()
		<-reading
	})
	t.Run("caller deadline stays shorter", func(t *testing.T) {
		a, b := net.Pipe()
		defer func() { _ = b.Close() }()
		var released atomic.Int32
		c := &trackedConn{Conn: a, close: func() { released.Add(1) }}
		c.startIdle(time.Minute)
		defer func() { _ = c.Close() }()
		if err := c.SetDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err := c.Read(make([]byte, 1))
		if err == nil || time.Since(start) > time.Second || released.Load() != 1 {
			t.Fatalf("caller deadline was replaced or failed to release: %v", err)
		}
	})
}
