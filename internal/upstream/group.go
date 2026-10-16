package upstream

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/recursor"
)

// ErrTimeout is wrapped by errors from exchanges that timed out.
var ErrTimeout = errors.New("upstream: timeout")

// ErrUnknownGroup is returned for a group name that is not configured.
var ErrUnknownGroup = errors.New("upstream: unknown group")

// Result is a usable answer from one server.
type Result struct {
	Msg *dns.Msg
	// Group and Server name where the answer came from; Upstream is
	// "<group>/<server>", the value for pipeline.Response.Upstream.
	Group, Server, Upstream string
	RTT                     time.Duration
	// DNSSEC is the validation state from a recursive group; unknown for
	// forwarded answers, which kudurru does not validate.
	DNSSEC recursor.Security
}

// Error is returned when no server of a group produced a usable answer.
type Error struct {
	Group string
	Errs  []error
	// Msg is the last SERVFAIL or REFUSED response received, if any, and
	// Upstream the server that sent it; callers may relay it to keep the
	// upstream's extended error.
	Msg      *dns.Msg
	Upstream string
}

func (e *Error) Error() string {
	parts := make([]string, len(e.Errs))
	for i, err := range e.Errs {
		parts[i] = err.Error()
	}
	return fmt.Sprintf("upstream group %s: every server failed: %s", e.Group, strings.Join(parts, "; "))
}

// Unwrap returns the per-server errors.
func (e *Error) Unwrap() []error { return e.Errs }

// Timeout reports whether every server failed by timing out.
func (e *Error) Timeout() bool {
	if len(e.Errs) == 0 {
		return false
	}
	for _, err := range e.Errs {
		if !errors.Is(err, ErrTimeout) && !isTimeout(err) {
			return false
		}
	}
	return true
}

// IsTimeout reports whether err means the upstreams timed out.
func IsTimeout(err error) bool {
	var ue *Error
	if errors.As(err, &ue) {
		return ue.Timeout()
	}
	return errors.Is(err, ErrTimeout) || isTimeout(err)
}

// Group is a named set of servers with a strategy.
type Group struct {
	name     string
	strategy Strategy
	timeout  time.Duration
	servers  []*server
	now      func() time.Time
}

// Name returns the group name.
func (g *Group) Name() string { return g.name }

// Strategy returns the group's strategy.
func (g *Group) Strategy() Strategy { return g.strategy }

// plan orders the servers for one query: the first batch is raced in
// parallel (a batch of one is a plain exchange), the remaining batches are
// tried in order after it fails.
func (g *Group) plan() [][]*server {
	now := g.now()
	healthy := make([]*server, 0, len(g.servers))
	var down []*server
	for _, s := range g.servers {
		if s.healthy(now) {
			healthy = append(healthy, s)
		} else {
			down = append(down, s)
		}
	}
	// Servers in backoff are still tried last, earliest recovery first, so a
	// group never fails without asking anyone.
	slices.SortStableFunc(down, func(a, b *server) int { return a.downSince().Compare(b.downSince()) })

	var order []*server
	switch g.strategy {
	case Strict:
		order = append(healthy, down...)
	case Random:
		shuffle(healthy)
		order = append(healthy, down...)
	case RTTWeighted:
		order = append(rttOrder(healthy), down...)
	default: // ParallelBest
		shuffle(healthy)
		order = append(healthy, down...)
		if len(order) <= 2 {
			return [][]*server{order}
		}
		return [][]*server{order[:2], order[2:]}
	}
	batches := make([][]*server, len(order))
	for i, s := range order {
		batches[i] = []*server{s}
	}
	return batches
}

// shuffle randomises the order of servers in place.
func shuffle(servers []*server) {
	rand.Shuffle(len(servers), func(i, j int) { servers[i], servers[j] = servers[j], servers[i] }) //nolint:gosec // load spreading, not security
}

// rttOrder picks the first server at random weighted by 1/RTT and orders
// the rest by RTT. Servers without a sample count as the fastest known one
// so they get measured.
func rttOrder(servers []*server) []*server {
	if len(servers) <= 1 {
		return servers
	}
	rtts := make([]float64, len(servers))
	best := 0.0
	for i, s := range servers {
		rtts[i] = s.smoothedRTT()
		if rtts[i] > 0 && (best == 0 || rtts[i] < best) {
			best = rtts[i]
		}
	}
	if best == 0 {
		best = 0.01
	}
	weights := make([]float64, len(servers))
	total := 0.0
	for i := range rtts {
		if rtts[i] == 0 {
			rtts[i] = best
		}
		weights[i] = 1 / max(rtts[i], 1e-4)
		total += weights[i]
	}
	pick := rand.Float64() * total //nolint:gosec // load spreading, not security
	first := len(servers) - 1
	for i, w := range weights {
		if pick < w {
			first = i
			break
		}
		pick -= w
	}
	idx := make([]int, 0, len(servers))
	for i := range servers {
		if i != first {
			idx = append(idx, i)
		}
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		switch {
		case rtts[a] < rtts[b]:
			return -1
		case rtts[a] > rtts[b]:
			return 1
		}
		return 0
	})
	out := make([]*server, 0, len(servers))
	out = append(out, servers[first])
	for _, i := range idx {
		out = append(out, servers[i])
	}
	return out
}

// exchange runs the plan until a server answers usably.
func (g *Group) exchange(ctx context.Context, msg *dns.Msg) (*Result, error) {
	failure := &Error{Group: g.name}
	for _, batch := range g.plan() {
		if ctx.Err() != nil {
			break
		}
		a := g.race(ctx, batch, msg, failure)
		if a != nil {
			return &Result{Msg: a.msg, Group: g.name, Server: a.srv.name, Upstream: a.srv.label, RTT: a.rtt, DNSSEC: a.sec}, nil
		}
	}
	if len(failure.Errs) == 0 {
		err := ctx.Err()
		if err == nil {
			err = errors.New("no servers")
		}
		failure.Errs = append(failure.Errs, err)
	}
	return nil, failure
}

// race asks every server of batch concurrently and returns the first usable
// answer, cancelling the others. Failures are appended to failure.
func (g *Group) race(ctx context.Context, batch []*server, msg *dns.Msg, failure *Error) *attempt {
	if len(batch) == 1 {
		a := batch[0].exchange(ctx, msg, g.timeout)
		return g.collect(a, failure)
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan attempt, len(batch))
	for _, s := range batch {
		go func() { ch <- s.exchange(rctx, msg, g.timeout) }()
	}
	for range batch {
		if a := g.collect(<-ch, failure); a != nil {
			return a
		}
	}
	return nil
}

// collect returns a when it is usable and records it in failure otherwise.
func (g *Group) collect(a attempt, failure *Error) *attempt {
	if a.err == nil {
		return &a
	}
	failure.Errs = append(failure.Errs, a.err)
	var rc errRcode
	if errors.As(a.err, &rc) {
		failure.Msg, failure.Upstream = a.msg, a.srv.label
	}
	return nil
}
