package dhcpha

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"
)

type hello struct {
	Version                              int
	ClusterID, NodeID, Fingerprint, Kind string
}

// peerStream multiplexes authenticated Raft streams and bounded management
// requests on the same TCP endpoint. Handshakes verify both CA trust and the
// certificate subject's binding to a configured voter ID.
type peerStream struct {
	net.Listener
	cfg          Config
	fingerprint  string
	address      net.Addr
	raftConns    chan net.Conn
	done         chan struct{}
	limit        chan struct{}
	onManagement func(net.Conn, string)
	mu           sync.Mutex
	connections  map[net.Conn]struct{}
	wg           sync.WaitGroup
	once         sync.Once
	// Tests partition existing connections and new dials through this hook.
	blocked atomic.Pointer[partitionRule]
}

type partitionRule struct{ blocked func(string) bool }

const (
	maxPeerConnections = 32
	peerIdleTimeout    = time.Minute
)

func (s *peerStream) isBlocked(id string) bool {
	rule := s.blocked.Load()
	return rule != nil && rule.blocked(id)
}

type advertisedAddr string

func (a advertisedAddr) Network() string { return "tcp" }
func (a advertisedAddr) String() string  { return string(a) }

func newPeerStream(cfg Config, fp string) (*peerStream, error) {
	l, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return nil, err
	}
	var address string
	for _, p := range cfg.Peers {
		if p.ID == cfg.NodeID {
			address = p.Address
		}
	}
	s := &peerStream{Listener: l, cfg: cfg, fingerprint: fp, address: advertisedAddr(address), raftConns: make(chan net.Conn, 16), done: make(chan struct{}), limit: make(chan struct{}, maxPeerConnections), connections: map[net.Conn]struct{}{}}
	return s, nil
}

func (s *peerStream) start() { s.wg.Add(1); go s.serve() }
func (s *peerStream) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.Listener.Accept()
		if err != nil {
			return
		}
		if err := s.reserve(); err != nil {
			_ = conn.Close()
			continue
		}
		owned, err := s.own(conn)
		if err != nil {
			continue
		}
		conn = owned
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			tc := tls.Server(conn, s.cfg.TLS)
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			if err := tc.Handshake(); err != nil {
				s.discard(conn)
				return
			}
			var h hello
			if readFrame(tc, &h, 4096) != nil || !s.validHello(tc, h) {
				s.discard(conn)
				return
			}
			if writeFrame(tc, s.hello(h.Kind)) != nil {
				s.discard(conn)
				return
			}
			if h.Kind == "management" {
				defer s.discard(conn)
				_ = conn.SetDeadline(time.Now().Add(s.cfg.RequestTimeout))
				if s.onManagement != nil {
					s.onManagement(tc, h.NodeID)
				}
				return
			}
			if h.Kind != "raft" {
				s.discard(conn)
				return
			}
			_ = conn.SetDeadline(time.Time{})
			select {
			case s.raftConns <- tc:
			case <-owned.done:
				_ = tc.Close()
			case <-s.done:
				_ = tc.Close()
			}
		}()
	}
}

func (s *peerStream) hello(kind string) hello {
	return hello{1, s.cfg.ClusterID, s.cfg.NodeID, s.fingerprint, kind}
}
func (s *peerStream) validHello(tc *tls.Conn, h hello) bool {
	if h.Version != 1 || h.ClusterID != s.cfg.ClusterID || h.Fingerprint != s.fingerprint || len(tc.ConnectionState().VerifiedChains) == 0 {
		return false
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 || certs[0].Subject.CommonName != h.NodeID {
		return false
	}
	if s.isBlocked(h.NodeID) {
		return false
	}
	for _, p := range s.cfg.Peers {
		if p.ID == h.NodeID {
			return true
		}
	}
	return false
}
func (s *peerStream) reserve() error {
	select {
	case <-s.done:
		return net.ErrClosed
	default:
	}
	select {
	case s.limit <- struct{}{}:
		return nil
	default:
		return errors.New("dhcp HA: peer connection limit reached")
	}
}

// own consumes an already reserved slot until the socket closes. Tracking the
// underlying socket also lets shutdown interrupt TLS handshakes and idle pools.
func (s *peerStream) own(raw net.Conn) (*trackedConn, error) {
	c := &trackedConn{Conn: raw, done: make(chan struct{})}
	c.close = func() {
		s.mu.Lock()
		delete(s.connections, c)
		s.mu.Unlock()
		<-s.limit
	}
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		_ = c.Close()
		return nil, net.ErrClosed
	default:
	}
	s.connections[c] = struct{}{}
	c.startIdle(peerIdleTimeout)
	s.mu.Unlock()
	return c, nil
}
func (s *peerStream) discard(c net.Conn) { _ = c.Close() }

func (s *peerStream) Accept() (net.Conn, error) {
	select {
	case c := <-s.raftConns:
		return c, nil
	case <-s.done:
		return nil, net.ErrClosed
	}
}
func (s *peerStream) Addr() net.Addr { return s.address }
func (s *peerStream) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		err = s.Listener.Close()
		s.mu.Lock()
		connections := make([]net.Conn, 0, len(s.connections))
		for c := range s.connections {
			connections = append(connections, c)
		}
		s.mu.Unlock()
		// Close invokes ownership callbacks, which take mu to remove tracking.
		for _, c := range connections {
			_ = c.Close()
		}
		s.wg.Wait()
	})
	return err
}
func (s *peerStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	return s.dial(context.Background(), string(address), "raft", timeout)
}
func (s *peerStream) dial(ctx context.Context, address, kind string, timeout time.Duration) (net.Conn, error) {
	var peer string
	for _, p := range s.cfg.Peers {
		if p.Address == address {
			peer = p.ID
		}
	}
	if peer == "" || s.isBlocked(peer) {
		return nil, errors.New("dhcp HA: voter is unreachable")
	}
	if err := s.reserve(); err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		<-s.limit
		return nil, err
	}
	conn, err = s.own(conn)
	if err != nil {
		return nil, err
	}
	cfg := s.cfg.TLS.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName, _, _ = net.SplitHostPort(address)
	}
	tc := tls.Client(conn, cfg)
	deadline := time.Now().Add(timeout)
	if v, ok := ctx.Deadline(); ok && v.Before(deadline) {
		deadline = v
	}
	_ = conn.SetDeadline(deadline)
	if err = tc.HandshakeContext(ctx); err != nil {
		s.discard(conn)
		return nil, err
	}
	if err = writeFrame(tc, s.hello(kind)); err != nil {
		s.discard(conn)
		return nil, err
	}
	var h hello
	if err = readFrame(tc, &h, 4096); err != nil || !s.validHello(tc, h) || h.NodeID != peer || h.Kind != kind {
		s.discard(conn)
		return nil, fmt.Errorf("dhcp HA: peer authentication rejected: %w", errors.Join(err, ErrUnavailable))
	}
	_ = conn.SetDeadline(time.Time{})
	return tc, nil
}

type trackedConn struct {
	net.Conn
	once  sync.Once
	close func()
	done  chan struct{}

	idleMu       sync.Mutex
	idleTimer    *time.Timer
	idleTimeout  time.Duration
	lastActivity time.Time
	idleExpired  bool
}

func (c *trackedConn) startIdle(timeout time.Duration) {
	c.idleMu.Lock()
	defer c.idleMu.Unlock()
	c.idleTimeout, c.lastActivity = timeout, time.Now()
	c.idleTimer = time.AfterFunc(timeout, c.expireIdle)
}

func (c *trackedConn) activity() {
	c.idleMu.Lock()
	if !c.idleExpired {
		c.lastActivity = time.Now()
	}
	c.idleMu.Unlock()
}

func (c *trackedConn) expireIdle() {
	c.idleMu.Lock()
	if c.idleExpired {
		c.idleMu.Unlock()
		return
	}
	if remaining := time.Until(c.lastActivity.Add(c.idleTimeout)); remaining > 0 {
		c.idleTimer.Reset(remaining)
		c.idleMu.Unlock()
		return
	}
	c.idleExpired = true
	c.idleMu.Unlock()
	_ = c.Close()
}

// Activity extends the idle lifetime without replacing caller-supplied Raft,
// handshake or management deadlines. Active large snapshots may run longer
// than the idle timeout while completely idle pooled sockets are reclaimed.
func (c *trackedConn) Read(p []byte) (int, error) {
	c.activity()
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.activity()
	}
	if err != nil {
		_ = c.Close()
	}
	return n, err
}
func (c *trackedConn) Write(p []byte) (int, error) {
	c.activity()
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.activity()
	}
	if err != nil {
		_ = c.Close()
	}
	return n, err
}
func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.idleMu.Lock()
		c.idleExpired = true
		if c.idleTimer != nil {
			c.idleTimer.Stop()
		}
		c.idleMu.Unlock()
		if c.done != nil {
			close(c.done)
		}
		c.close()
	})
	return err
}

func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxSnapshotBytes {
		return errors.New("dhcp HA: outgoing frame exceeds limit")
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b))) // #nosec G115 -- bounded by maxSnapshotBytes above.
	if _, err = w.Write(size[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}
func readFrame(r io.Reader, v any, limit uint32) error {
	var size [4]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > limit {
		return errors.New("dhcp HA: frame exceeds limit")
	}
	b := make([]byte, int(n))
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
