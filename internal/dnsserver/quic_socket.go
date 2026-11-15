package dnsserver

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/quic-go/quic-go"
)

const maxQUICSockets = 256

// shardConnectionIDs reserve one routing byte; the other 120 bits are random.
// The byte identifies a socket shared by many unrelated connections, not a
// client. Every newly issued CID retains its socket through path migration.
type shardConnectionIDs struct{ index byte }

func (shardConnectionIDs) ConnectionIDLen() int { return 16 }
func (g shardConnectionIDs) GenerateConnectionID() (quic.ConnectionID, error) {
	var id [16]byte
	_, _ = rand.Read(id[:])
	id[0] = g.index
	return quic.ConnectionIDFromBytes(id[:]), nil
}

type quicAcceptor interface {
	Accept(context.Context) (*quic.Conn, error)
	Addr() net.Addr
	Close() error
}

type quicAccepted struct {
	conn *quic.Conn
	err  error
}

// quicSockets has immutable kernel indices for its entire lifetime. An accept
// failure is fatal to the whole listener; removing one socket would reindex the
// kernel group and strand connections. There is at most one pending accepted
// connection per socket, and no unbounded dispatch queue.
type quicSockets struct {
	packets    []net.PacketConn
	transports []*quic.Transport
	listeners  []quicAcceptor
	accepted   chan quicAccepted
	ctx        context.Context
	cancel     context.CancelFunc
	pumps      sync.WaitGroup
	stop       sync.Once
	dispose    sync.Once
	closeErr   error
}

func listenQUICSockets(address string, count int, listen func(*quic.Transport) (quicAcceptor, error)) (*quicSockets, error) {
	if count < 1 || count > maxQUICSockets {
		return nil, fmt.Errorf("dnsserver: QUIC socket count must be between 1 and %d", maxQUICSockets)
	}
	if listen == nil {
		return nil, errors.New("dnsserver: QUIC acceptor required")
	}
	packets, err := listenQUICPackets(address, count)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &quicSockets{packets: packets, accepted: make(chan quicAccepted), ctx: ctx, cancel: cancel}
	failed := true
	defer func() {
		if failed {
			_ = g.closeSockets()
		}
	}()
	if count > 1 {
		if err := attachQUICRouting(packets[0], count); err != nil {
			return nil, fmt.Errorf("dnsserver: install QUIC connection-ID routing: %w", err)
		}
	}
	// Resumption can reach a different shard on a fresh client connection.
	var tokenKey quic.TokenGeneratorKey
	_, _ = rand.Read(tokenKey[:])
	for i, pc := range packets {
		tr := &quic.Transport{Conn: pc, TokenGeneratorKey: &tokenKey}
		if count > 1 {
			tr.ConnectionIDGenerator = shardConnectionIDs{index: byte(i)} // #nosec G115 -- bounded by maxQUICSockets
		}
		g.transports = append(g.transports, tr)
		ln, err := listen(tr)
		if err != nil {
			return nil, err
		}
		g.listeners = append(g.listeners, ln)
	}
	for _, ln := range g.listeners {
		g.pumps.Go(func() {
			for {
				conn, err := ln.Accept(ctx)
				select {
				case g.accepted <- quicAccepted{conn, err}:
				case <-ctx.Done():
					if conn != nil {
						_ = conn.CloseWithError(0, "listener closed")
					}
					return
				}
				if err != nil {
					return
				}
			}
		})
	}
	failed = false
	return g, nil
}

// Keep the exclusive first bind while enabling reuse-port. Releasing a probe
// and rebinding would allow two simultaneous startups to join the same group,
// where either process's CID selector could strand the other's connections.
func listenQUICPackets(address string, count int) ([]net.PacketConn, error) {
	first, err := net.ListenPacket("udp", address)
	if err != nil {
		return nil, err
	}
	out := []net.PacketConn{first}
	if count == 1 {
		return out, nil
	}
	raw, err := first.(*net.UDPConn).SyscallConn()
	if err == nil {
		err = reuseportControl("udp", address, raw)
	}
	if err != nil {
		_ = first.Close()
		return nil, err
	}
	bound := rebindAddress(address, first.LocalAddr())
	lc := net.ListenConfig{Control: reuseportControl}
	for i := 1; i < count; i++ {
		pc, err := lc.ListenPacket(context.Background(), "udp", bound)
		if err != nil {
			_ = closeAllFunc(out)()
			return nil, err
		}
		out = append(out, pc)
	}
	return out, nil
}

func (g *quicSockets) Addr() net.Addr { return g.packets[0].LocalAddr() }
func (g *quicSockets) Accept(ctx context.Context) (*quic.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-g.ctx.Done():
		return nil, quic.ErrServerClosed
	case a := <-g.accepted:
		err := ctx.Err()
		if err == nil && g.ctx.Err() != nil {
			err = quic.ErrServerClosed
		}
		if err != nil {
			if a.conn != nil {
				_ = a.conn.CloseWithError(0, "listener canceled")
			}
			return nil, err
		}
		return a.conn, a.err
	}
}

// Close stops accepting and joins all accept pumps. Existing connections remain
// owned by the protocol's bounded graceful shutdown until closeSockets.
func (g *quicSockets) Close() error {
	g.stop.Do(func() {
		g.cancel()
		for _, ln := range g.listeners {
			_ = ln.Close()
		}
		g.pumps.Wait()
	})
	return nil
}

func (g *quicSockets) closeSockets() error {
	g.dispose.Do(func() {
		_ = g.Close()
		for _, tr := range g.transports {
			_ = tr.Close()
		}
		g.closeErr = closeAllFunc(g.packets)()
	})
	return g.closeErr
}

func normalQUICClose(err error) bool {
	return err == nil || errors.Is(err, quic.ErrServerClosed) || errors.Is(err, context.Canceled)
}
