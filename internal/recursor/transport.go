package recursor

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

// ednsSize is the UDP payload size advertised to authoritative servers
// (DNS flag day 2020).
const ednsSize = 1232

// Exchanger sends one query to one authoritative server. The default uses
// a new UDP socket per query (a random source port) and TCP when asked.
// Tests replace it with an in-memory network.
type Exchanger interface {
	Exchange(ctx context.Context, m *dns.Msg, server netip.AddrPort, tcp bool) (*dns.Msg, error)
}

type netExchanger struct{ cookies *cookieJar }

func (n netExchanger) Exchange(ctx context.Context, m *dns.Msg, server netip.AddrPort, tcp bool) (*dns.Msg, error) {
	c := &dns.Client{Net: "udp", UDPSize: ednsSize}
	if tcp {
		c.Net = "tcp"
	}
	co, err := c.DialContext(ctx, server.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = co.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = co.Close() })
	defer stop()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	local, err := netip.ParseAddrPort(co.LocalAddr().String())
	if err != nil {
		return nil, errMismatch
	}
	remote, err := netip.ParseAddrPort(co.RemoteAddr().String())
	if err != nil {
		return nil, errMismatch
	}
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	if remote != netip.AddrPortFrom(server.Addr().Unmap(), server.Port()) {
		return nil, errMismatch
	}
	wire := m.Copy()
	ticket := n.cookies.prepare(wire, cookiePeer{local: local.Addr().Unmap(), server: remote})
	r, _, err := c.ExchangeWithConnContext(ctx, wire, co)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if err := matchResponse(r, wire); err != nil {
		return nil, err
	}
	if err := n.cookies.accept(r, ticket); err != nil {
		return nil, err
	}
	stripCookies(r)
	return r, nil
}

var (
	errMismatch = errors.New("recursor: answer does not match the question")
	errBudget   = errors.New("recursor: query budget exhausted")
	errDepth    = errors.New("recursor: resolution nested too deeply")
)

// send asks server for name/qtype once: UDP, then TCP when the answer is
// truncated or repeated BADCOOKIE requires transport recovery. Each wire
// attempt consumes the shared query budget. The answer must echo the
// question, including the random letter case; a server that does not is retried once in lower case and,
// after a few such answers, asked without 0x20.
func (q *query) send(server netip.AddrPort, name string, qtype uint16) (*dns.Msg, error) {
	r := q.r
	use0x20 := r.infra.supports0x20(server.Addr())
	retry := &transportRetry{}
	resp, rtt, err := q.exchangeOnce(server, name, qtype, use0x20, retry)
	if errors.Is(err, errCase) {
		r.infra.caseMismatch(server.Addr())
		resp, rtt, err = q.exchangeOnce(server, name, qtype, false, retry)
	}
	if errors.Is(err, errBudget) {
		// Local work exhaustion is not evidence that this authority failed.
		return nil, err
	}
	if err != nil {
		timeout := isTimeout(err)
		if q.ctx.Err() == nil {
			r.infra.fail(server.Addr(), timeout)
		}
		r.metrics.outgoing(timeout, err)
		return nil, err
	}
	r.infra.ok(server.Addr(), rtt)
	r.lastAnswer.Store(r.now().UnixNano())
	r.metrics.outgoing(false, nil)
	return resp, nil
}

var errCase = errors.New("recursor: answer changed the case of the query name")

// cryptoUint64 supplies the 0x20 case bits. Like the query ID (dns.Id),
// they are what a spoofed answer has to guess, so they come from
// crypto/rand, whose Read never fails.
func cryptoUint64() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

type transportRetry struct{ tcp, badCookie bool }

func (q *query) exchangeOnce(server netip.AddrPort, name string, qtype uint16, use0x20 bool, retry *transportRetry) (*dns.Msg, time.Duration, error) {
	r := q.r
	qname := name
	if use0x20 && q.s.cfg.CaseRandomization {
		qname = randomizeCase(name, cryptoUint64)
	}
	m := &dns.Msg{MsgHdr: dns.MsgHdr{Id: dns.Id(), Opcode: dns.OpcodeQuery}}
	m.Question = []dns.Question{{Name: qname, Qtype: qtype, Qclass: dns.ClassINET}}
	m.SetEdns0(ednsSize, q.s.cfg.DNSSEC)

	ctx, cancel := context.WithTimeout(q.ctx, r.infra.rto(server.Addr()))
	defer cancel()
	start := time.Now()
	for {
		wireCtx := ctx
		if retry.tcp {
			var tcancel context.CancelFunc
			wireCtx, tcancel = context.WithTimeout(q.ctx, maxRTO)
			defer tcancel()
		}
		if err := wireCtx.Err(); err != nil {
			return nil, time.Since(start), err
		}
		q.st.budget--
		if q.st.budget < 0 {
			return nil, time.Since(start), errBudget
		}
		resp, err := r.ex.Exchange(wireCtx, m, server, retry.tcp)
		if err == nil {
			err = matchResponse(resp, m)
		}
		if err != nil {
			return nil, time.Since(start), err
		}
		if resp.Rcode == dns.RcodeBadCookie {
			if retry.tcp {
				return nil, time.Since(start), errCookie
			}
			if !retry.badCookie {
				retry.badCookie = true
			} else {
				retry.tcp = true
			}
			continue
		}
		if resp.Truncated {
			if retry.tcp {
				return nil, time.Since(start), errMismatch
			}
			retry.tcp = true
			continue
		}
		return resp, time.Since(start), nil
	}
}

func matchResponse(resp, req *dns.Msg) error {
	if resp == nil || !resp.Response || resp.Id != req.Id || resp.Opcode != dns.OpcodeQuery || len(resp.Question) != 1 {
		return errMismatch
	}
	rq, qq := resp.Question[0], req.Question[0]
	if rq.Qtype != qq.Qtype || rq.Qclass != dns.ClassINET || !equalName(rq.Name, qq.Name) {
		return errMismatch
	}
	if rq.Name != qq.Name {
		return errCase
	}
	return nil
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}
