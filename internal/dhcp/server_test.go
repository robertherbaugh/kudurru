package dhcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

type serverDatagram struct {
	wire   []byte
	source net.Addr
	err    error
}

type serverConn struct {
	input       chan serverDatagram
	writes      chan serverDatagram
	closed      chan struct{}
	once        sync.Once
	writeErr    error
	deadlineErr error
	closeErr    error
	shortWrite  bool
}

func newServerConn() *serverConn {
	return &serverConn{input: make(chan serverDatagram, 128), writes: make(chan serverDatagram, 128), closed: make(chan struct{})}
}
func (c *serverConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	select {
	case <-c.closed:
		return 0, nil, net.ErrClosed
	case packet := <-c.input:
		return copy(buffer, packet.wire), packet.source, packet.err
	}
}
func (c *serverConn) WriteTo(wire []byte, destination net.Addr) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	if c.shortWrite {
		return len(wire) - 1, nil
	}
	c.writes <- serverDatagram{wire: append([]byte(nil), wire...), source: destination}
	return len(wire), nil
}
func (c *serverConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.closeErr
}
func (*serverConn) LocalAddr() net.Addr                { return &net.UDPAddr{Port: serverPort} }
func (*serverConn) SetDeadline(time.Time) error        { return nil }
func (*serverConn) SetReadDeadline(time.Time) error    { return nil }
func (c *serverConn) SetWriteDeadline(time.Time) error { return c.deadlineErr }

func waitServerResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
		return nil
	}
}

func TestServerListenAndClose(t *testing.T) {
	calls := 0
	conn := newServerConn()
	server := &Server{cfg: protocolConfig(), listen: func(cfg Config) (net.PacketConn, error) {
		calls++
		if cfg.Interface != "eth0" {
			t.Fatal("configuration not passed to socket factory")
		}
		return conn, nil
	}}
	if err := server.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded without Listen")
	}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	if err := server.Listen(); err != nil || calls != 1 {
		t.Fatalf("Listen not idempotent: %v, calls %d", err, calls)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Listen(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Listen after Close = %v", err)
	}
	if err := server.Run(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Run after Close = %v", err)
	}
	bindErr := errors.New("address in use")
	server = &Server{listen: func(Config) (net.PacketConn, error) { return nil, bindErr }}
	if err := server.Listen(); !errors.Is(err, bindErr) || server.conn != nil {
		t.Fatalf("bind failure lost: %v", err)
	}
}

func TestServerCancellationAndExplicitClose(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		conn := newServerConn()
		server := &Server{conn: conn}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- server.Run(ctx) }()
		if cancelContext {
			cancel()
		} else {
			// Wait until Run owns the socket before explicitly closing it.
			deadline := time.Now().Add(time.Second)
			for {
				server.mu.Lock()
				running := server.running
				server.mu.Unlock()
				if running {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("Run did not start")
				}
				time.Sleep(time.Millisecond)
			}
			if err := server.Run(ctx); err == nil {
				t.Fatal("concurrent Run accepted")
			}
			_ = server.Close()
		}
		if err := waitServerResult(t, result); err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case <-conn.closed:
		default:
			t.Fatal("socket remains open")
		}
	}
}

type cancelLeases struct {
	protocolLeases
	started chan struct{}
}

type deadlineLeases struct {
	protocolLeases
	t *testing.T
}

func (s *deadlineLeases) Offer(ctx context.Context, client Client, address netip.Addr) (Lease, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > packetTimeout || time.Until(deadline) <= 0 {
		s.t.Error("packet context has no bounded processing deadline")
	}
	s.record("offer", client, address)
	if s.calls == 1 {
		return Lease{}, context.DeadlineExceeded
	}
	return s.lease, nil
}

func TestServerPacketDeadlineDoesNotStopService(t *testing.T) {
	conn := newServerConn()
	backend := &deadlineLeases{protocolLeases: protocolLeases{lease: Lease{Address: netip.MustParseAddr("192.0.2.10")}}, t: t}
	server := &Server{cfg: protocolConfig(), conn: conn, leases: backend}
	packet := clientPacket(dhcpv4.MessageTypeDiscover)
	for range 2 {
		conn.input <- serverDatagram{wire: packet.ToBytes(), source: packetSource(packet)}
	}
	fatal := errors.New("end of test")
	conn.input <- serverDatagram{err: fatal}
	if err := server.Run(context.Background()); !errors.Is(err, fatal) || backend.calls != 2 || len(conn.writes) != 1 {
		t.Fatalf("deadline stopped service: %v, calls=%d replies=%d", err, backend.calls, len(conn.writes))
	}
}

func (s *cancelLeases) Offer(ctx context.Context, _ Client, _ netip.Addr) (Lease, error) {
	close(s.started)
	<-ctx.Done()
	return Lease{}, ctx.Err()
}

func TestServerCancelsStorageWork(t *testing.T) {
	conn := newServerConn()
	backend := &cancelLeases{started: make(chan struct{})}
	server := &Server{cfg: protocolConfig(), conn: conn, leases: backend}
	packet := clientPacket(dhcpv4.MessageTypeDiscover)
	conn.input <- serverDatagram{wire: packet.ToBytes(), source: packetSource(packet)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- server.Run(ctx) }()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("storage call did not start")
	}
	cancel()
	if err := waitServerResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestServerTransportFailures(t *testing.T) {
	fatal := errors.New("injected failure")
	for _, kind := range []string{"read", "write", "deadline", "short write"} {
		t.Run(kind, func(t *testing.T) {
			conn := newServerConn()
			backend := &protocolLeases{lease: Lease{Address: netip.MustParseAddr("192.0.2.10")}}
			packet := clientPacket(dhcpv4.MessageTypeDiscover)
			input := serverDatagram{wire: packet.ToBytes(), source: packetSource(packet)}
			switch kind {
			case "read":
				input.err = fatal
			case "write":
				conn.writeErr = fatal
			case "deadline":
				conn.deadlineErr = fatal
			case "short write":
				conn.shortWrite = true
			}
			conn.input <- input
			server := &Server{cfg: protocolConfig(), conn: conn, leases: backend}
			if err := server.Run(context.Background()); err == nil || kind != "short write" && !errors.Is(err, fatal) {
				t.Fatalf("failure not propagated: %v", err)
			}
			select {
			case <-conn.closed:
			default:
				t.Fatal("failed server left socket open")
			}
		})
	}
}

type failOnceLeases struct {
	protocolLeases
	failure error
}

func (s *failOnceLeases) Offer(_ context.Context, client Client, address netip.Addr) (Lease, error) {
	s.record("offer", client, address)
	if s.calls == 1 {
		return Lease{}, s.failure
	}
	return s.lease, nil
}

func (s *failOnceLeases) Acknowledge(_ context.Context, client Client, address netip.Addr, mode RequestMode) (Lease, error) {
	s.record("ack", client, address)
	s.mode = mode
	if s.calls == 1 {
		return Lease{}, s.failure
	}
	return s.lease, nil
}

func TestServiceStorageFailureDoesNotStopService(t *testing.T) {
	for _, tc := range []struct {
		name      string
		message   dhcpv4.MessageType
		response  dhcpv4.MessageType
		operation string
	}{
		{"discover", dhcpv4.MessageTypeDiscover, dhcpv4.MessageTypeOffer, "offer"},
		{"request", dhcpv4.MessageTypeRequest, dhcpv4.MessageTypeAck, "ack"},
	} {
		for _, failure := range []error{errors.New("lease storage unavailable"), fmt.Errorf("lease storage: %w", syscall.EACCES)} {
			t.Run(tc.name+"/"+failure.Error(), func(t *testing.T) {
				cfg := protocolConfig()
				svc := serviceForTest(t, cfg)
				backend := &failOnceLeases{
					protocolLeases: protocolLeases{lease: Lease{Address: cfg.RangeStart}},
					failure:        failure,
				}
				svc.scopes["default"].leases = backend
				conn := newServerConn()
				svc.listeners[0].listen = func(Config) (net.PacketConn, error) { return conn, nil }
				var logs bytes.Buffer
				svc.listeners[0].logger = slog.New(slog.NewJSONHandler(&logs, nil))
				if err := svc.Listen(); err != nil {
					t.Fatal(err)
				}

				packet := clientPacket(tc.message)
				if tc.message == dhcpv4.MessageTypeRequest {
					packet.UpdateOption(dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
					packet.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IP(cfg.RangeStart.AsSlice())))
				}
				conn.input <- serverDatagram{wire: packet.ToBytes(), source: packetSource(packet)}
				packet.TransactionID[3]++
				conn.input <- serverDatagram{wire: packet.ToBytes(), source: packetSource(packet)}
				socketFailure := errors.New("listening socket failed")
				conn.input <- serverDatagram{err: socketFailure}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := svc.Run(ctx); !errors.Is(err, socketFailure) || errors.Is(err, failure) {
					t.Fatalf("want terminal socket failure after storage recovery, got %v", err)
				}
				if backend.calls != 2 || backend.operation != tc.operation {
					t.Fatalf("later packet did not reach storage: calls=%d operation=%s", backend.calls, backend.operation)
				}
				if tc.message == dhcpv4.MessageTypeRequest && backend.mode != Selecting {
					t.Fatalf("request mode = %v, want Selecting", backend.mode)
				}
				if len(conn.writes) != 1 {
					t.Fatalf("want only recovered packet's reply, got %d replies", len(conn.writes))
				}
				written := <-conn.writes
				reply, err := dhcpv4.FromBytes(written.wire)
				if err != nil {
					t.Fatal(err)
				}
				if reply.MessageType() != tc.response || reply.TransactionID != packet.TransactionID ||
					!reply.YourIPAddr.Equal(net.IP(cfg.RangeStart.AsSlice())) {
					t.Fatalf("unexpected recovery reply: %v", reply)
				}
				destination, ok := written.source.(*net.UDPAddr)
				if !ok || destination.Port != clientPort || !destination.IP.Equal(net.IPv4bcast) {
					t.Fatalf("unexpected reply destination: %v", written.source)
				}
				var record struct {
					Level     string `json:"level"`
					Error     string `json:"error"`
					Interface string `json:"interface"`
				}
				if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
					t.Fatalf("want one storage failure log: %v; logs=%s", err, logs.String())
				}
				if record.Level != "ERROR" || record.Error != failure.Error() || record.Interface != cfg.Interface {
					t.Fatalf("storage failure was not visible at error level: %+v", record)
				}
				select {
				case <-conn.closed:
				default:
					t.Fatal("terminal socket failure left listener open")
				}
			})
		}
	}
}

func TestServerMalformedTrafficDoesNotStopService(t *testing.T) {
	conn := newServerConn()
	backend := &protocolLeases{lease: Lease{Address: netip.MustParseAddr("192.0.2.10")}}
	server := &Server{cfg: protocolConfig(), conn: conn, leases: backend}
	packet := clientPacket(dhcpv4.MessageTypeDiscover)
	conn.input <- serverDatagram{wire: nil, source: packetSource(packet)}
	conn.input <- serverDatagram{wire: make([]byte, maxPacketSize+1), source: packetSource(packet)}
	conn.input <- serverDatagram{wire: []byte("malformed"), source: packetSource(packet)}
	conn.input <- serverDatagram{wire: packet.ToBytes(), source: &net.IPAddr{IP: net.IPv4zero}}
	conn.input <- serverDatagram{err: &net.OpError{Op: "read", Net: "udp", Err: syscall.ECONNREFUSED}}
	packet.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, []byte{1}))
	conn.input <- serverDatagram{wire: packet.ToBytes(), source: packetSource(packet)}
	packet.DeleteOption(dhcpv4.OptionClientIdentifier)
	conn.input <- serverDatagram{wire: packet.ToBytes(), source: packetSource(packet)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- server.Run(ctx) }()
	select {
	case <-conn.writes:
	case err := <-result:
		t.Fatalf("packet input stopped server: %v", err)
	case <-time.After(time.Second):
		t.Fatal("valid packet after malformed traffic was not answered")
	}
	cancel()
	if err := waitServerResult(t, result); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 {
		t.Fatalf("malformed traffic reached storage: %d calls", backend.calls)
	}
}

func TestServerPacketNetworkErrorsAreNonfatal(t *testing.T) {
	for _, packetErr := range []error{syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ECONNREFUSED, syscall.EMSGSIZE, syscall.ENOBUFS, syscall.EACCES, syscall.EPROTO, context.DeadlineExceeded} {
		conn := newServerConn()
		conn.writeErr = packetErr
		backend := &protocolLeases{lease: Lease{Address: netip.MustParseAddr("192.0.2.10")}}
		server := &Server{cfg: protocolConfig(), conn: conn, leases: backend}
		packet := clientPacket(dhcpv4.MessageTypeDiscover)
		conn.input <- serverDatagram{wire: packet.ToBytes(), source: packetSource(packet)}
		fatal := errors.New("end of test")
		conn.input <- serverDatagram{err: fatal}
		if err := server.Run(context.Background()); !errors.Is(err, fatal) {
			t.Fatalf("packet network error %v stopped server: %v", packetErr, err)
		}
	}
}

func TestNewServerValidation(t *testing.T) {
	if _, err := NewServer(protocolConfig(), nil, nil); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewServer(Config{}, &LeaseStore{}, nil); err == nil {
		t.Fatal("invalid config accepted")
	}
}
