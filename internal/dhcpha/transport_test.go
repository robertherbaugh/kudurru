package dhcpha

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
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
		if err := <-reading; err != nil {
			t.Fatal(err)
		}
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

// Raft's CloseStreams closes only idle pooled sockets, so the partition hook
// must also cut a socket an in-flight RPC already holds, including bytes the
// peer sent before the partition began.
func TestPartitionSeversEstablishedConnection(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = b.Close() }()
	var isolated atomic.Bool
	s := &peerStream{}
	s.blocked.Store(&partitionRule{blocked: func(peer string) bool { return peer == "node2" && isolated.Load() }})
	var released atomic.Int32
	gate := &gatedConn{Conn: a, entered: make(chan struct{}), proceed: make(chan struct{})}
	c := &trackedConn{Conn: gate, stream: s, close: func() { released.Add(1) }}
	c.startIdle(time.Minute)
	c.bindPeer("node2")
	defer func() { _ = c.Close() }()
	go func() { _, _ = io.Copy(io.Discard, b) }()
	if _, err := c.Write([]byte("heartbeat")); err != nil {
		t.Fatalf("connected peer rejected a write: %v", err)
	}
	sent := make(chan error, 1)
	reading := make(chan error, 1)
	buf := make([]byte, 16)
	go func() {
		_, err := c.Read(buf)
		reading <- err
	}()
	// The read passes the partition check and blocks before the partition
	// begins; the reply arrives only after it.
	<-gate.entered
	isolated.Store(true)
	go func() { _, err := b.Write([]byte("reply")); sent <- err }()
	close(gate.proceed)
	if err := <-reading; err == nil {
		t.Fatalf("partitioned connection delivered %q", buf)
	}
	if _, err := c.Write([]byte("append")); err == nil {
		t.Fatal("partitioned connection accepted a write")
	}
	if released.Load() != 1 {
		t.Fatal("partitioned connection was not closed")
	}
	// The reply reached the socket; only the partition check withheld it.
	if err := <-sent; err != nil {
		t.Fatalf("peer reply did not reach the socket: %v", err)
	}
}

// gatedConn holds the first read inside the underlying socket until released.
type gatedConn struct {
	net.Conn
	once             sync.Once
	entered, proceed chan struct{}
}

func (g *gatedConn) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered); <-g.proceed })
	return g.Conn.Read(p)
}

// The partition must cut sockets made by the real dial and serve paths: both
// bind the authenticated voter ID, including the accepted Raft socket a
// follower's handler is already blocked reading when the partition starts.
func TestPartitionSeversDialedAndAcceptedRaftStreams(t *testing.T) {
	c := clusterFixture(t)
	open := func(i int) *peerStream {
		s, err := newPeerStream(c.configs[i], "transport-test")
		if err != nil {
			t.Fatal(err)
		}
		s.start()
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	// Each side gets its own one-way rule, so one side cuts while the other
	// still sends: the cut must come from the side under test.
	var dialerCut, acceptorCut atomic.Bool
	dialer, acceptor := open(0), open(1)
	dialer.blocked.Store(&partitionRule{blocked: func(peer string) bool { return peer == "node2" && dialerCut.Load() }})
	acceptor.blocked.Store(&partitionRule{blocked: func(peer string) bool { return peer == "node1" && acceptorCut.Load() }})
	pair := func() (net.Conn, net.Conn) {
		t.Helper()
		out, err := dialer.dial(context.Background(), c.configs[1].ListenAddress, "raft", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = out.Close() })
		in, err := acceptor.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = in.Close() })
		for conn, want := range map[net.Conn]string{out: "node2", in: "node1"} {
			tracked, ok := conn.(*tls.Conn).NetConn().(*trackedConn)
			if !ok {
				t.Fatalf("peer socket is not tracked: %T", conn.(*tls.Conn).NetConn())
			}
			if peer := tracked.peer.Load(); peer == nil || *peer != want {
				t.Fatalf("peer socket bound to %v, want %s", peer, want)
			}
		}
		if _, err := out.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(in, make([]byte, 4)); err != nil {
			t.Fatalf("connected peers could not exchange data: %v", err)
		}
		return out, in
	}
	cut := func(name string, isolate *atomic.Bool, reader, writer net.Conn) {
		t.Helper()
		reading := make(chan error, 1)
		go func() { _, err := reader.Read(make([]byte, 16)); reading <- err }()
		isolate.Store(true)
		defer isolate.Store(false)
		// The other side is not partitioned, so its write reaches the socket.
		if _, err := writer.Write([]byte("append")); err != nil {
			t.Fatalf("%s: unpartitioned side could not write: %v", name, err)
		}
		select {
		case err := <-reading:
			if !errors.Is(err, errVoterUnreachable) {
				t.Fatalf("%s: partitioned read returned %v", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: partitioned read stayed open", name)
		}
		if _, err := reader.Write([]byte("reply")); err == nil {
			t.Fatalf("%s: partitioned socket accepted a write", name)
		}
	}
	out, in := pair()
	cut("accepted", &acceptorCut, in, out)
	out, in = pair()
	cut("dialed", &dialerCut, out, in)
}
