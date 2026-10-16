package recursor

import (
	"context"
	"errors"
	"math/rand/v2"
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

type netExchanger struct{}

func (netExchanger) Exchange(ctx context.Context, m *dns.Msg, server netip.AddrPort, tcp bool) (*dns.Msg, error) {
	c := &dns.Client{Net: "udp", UDPSize: ednsSize}
	if tcp {
		c.Net = "tcp"
	}
	r, _, err := c.ExchangeContext(ctx, m, server.String())
	return r, err
}

var (
	errMismatch = errors.New("recursor: answer does not match the question")
	errBudget   = errors.New("recursor: query budget exhausted")
	errDepth    = errors.New("recursor: resolution nested too deeply")
)

// send asks server for name/qtype once: UDP, then TCP when the answer is
// truncated. The answer must echo the question, including the random
// letter case; a server that does not is retried once in lower case and,
// after a few such answers, asked without 0x20.
func (q *query) send(server netip.AddrPort, name string, qtype uint16) (*dns.Msg, error) {
	q.st.budget--
	if q.st.budget < 0 {
		return nil, errBudget
	}
	r := q.r
	use0x20 := r.infra.supports0x20(server.Addr())
	resp, rtt, err := q.exchangeOnce(server, name, qtype, use0x20)
	if errors.Is(err, errCase) {
		r.infra.caseMismatch(server.Addr())
		resp, rtt, err = q.exchangeOnce(server, name, qtype, false)
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
	r.metrics.outgoing(false, nil)
	return resp, nil
}

var errCase = errors.New("recursor: answer changed the case of the query name")

func (q *query) exchangeOnce(server netip.AddrPort, name string, qtype uint16, use0x20 bool) (*dns.Msg, time.Duration, error) {
	r := q.r
	qname := name
	if use0x20 && q.s.cfg.CaseRandomization {
		qname = randomizeCase(name, rand.Uint64)
	}
	m := &dns.Msg{MsgHdr: dns.MsgHdr{Id: dns.Id(), Opcode: dns.OpcodeQuery}}
	m.Question = []dns.Question{{Name: qname, Qtype: qtype, Qclass: dns.ClassINET}}
	m.SetEdns0(ednsSize, q.s.cfg.DNSSEC)

	ctx, cancel := context.WithTimeout(q.ctx, r.infra.rto(server.Addr()))
	defer cancel()
	start := time.Now()
	resp, err := r.ex.Exchange(ctx, m, server, false)
	if err == nil && resp != nil && resp.Truncated {
		tctx, tcancel := context.WithTimeout(q.ctx, maxRTO)
		resp, err = r.ex.Exchange(tctx, m, server, true)
		tcancel()
	}
	rtt := time.Since(start)
	if err != nil {
		return nil, rtt, err
	}
	if resp == nil || !resp.Response || resp.Id != m.Id || resp.Opcode != dns.OpcodeQuery || len(resp.Question) != 1 {
		return nil, rtt, errMismatch
	}
	rq := resp.Question[0]
	if rq.Qtype != qtype || rq.Qclass != dns.ClassINET || !equalName(rq.Name, qname) {
		return nil, rtt, errMismatch
	}
	if rq.Name != qname {
		return nil, rtt, errCase
	}
	return resp, rtt, nil
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}
