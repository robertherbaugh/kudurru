package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	proxy "github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
)

// server is one configured upstream with its health state.
type server struct {
	name     string // display name or configured address
	label    string // "<group>/<name>", the metrics and Response.Upstream value
	u        proxy.Upstream
	hc       HealthConfig
	now      func() time.Time // backoff clock
	onChange func(label string, healthy bool)

	// Pre-resolved metric children; nil when metrics are off.
	mHealthy  prometheus.Gauge
	mRequests prometheus.Counter
	mDuration prometheus.Observer
	mErrors   map[string]prometheus.Counter

	mu        sync.Mutex
	fails     int           // consecutive failures
	downUntil time.Time     // backoff end while unhealthy
	backoff   time.Duration // current backoff
	rtt       float64       // smoothed RTT in seconds; 0 = no sample yet
}

// splitName separates an optional "name=" prefix from an address.
func splitName(entry string) (name, addr string) {
	entry = strings.TrimSpace(entry)
	if i := strings.IndexByte(entry, '='); i > 0 && !strings.ContainsAny(entry[:i], ":/[]") {
		return entry[:i], strings.TrimSpace(entry[i+1:])
	}
	return "", entry
}

// displayName is the name used in labels: the explicit name, else the
// address with credentials and query removed for DoH URLs.
func displayName(name, addr string, u proxy.Upstream) string {
	if name != "" {
		return name
	}
	if strings.HasPrefix(addr, "https://") || strings.HasPrefix(addr, "h3://") {
		if strings.ContainsAny(addr, "@?") {
			return u.Address()
		}
	}
	return addr
}

func (s *server) setMetrics(m *Metrics) {
	if m == nil {
		return
	}
	s.mHealthy = m.Healthy.WithLabelValues(s.label)
	s.mRequests = m.Requests.WithLabelValues(s.label)
	s.mDuration = m.Duration.WithLabelValues(s.label)
	s.mErrors = map[string]prometheus.Counter{}
	for _, r := range []string{reasonTimeout, reasonNetwork, reasonRefused, reasonServfail} {
		s.mErrors[r] = m.Errors.WithLabelValues(s.label, r)
	}
	s.mHealthy.Set(1)
}

// healthy reports whether the server may be asked now. A server in backoff
// becomes eligible again (one probe) when its backoff ends.
func (s *server) healthy(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fails < s.hc.Failures || !now.Before(s.downUntil)
}

// down reports the backoff end, zero when healthy.
func (s *server) downSince() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails < s.hc.Failures {
		return time.Time{}
	}
	return s.downUntil
}

// smoothedRTT returns the EWMA round-trip time (0 when unknown).
func (s *server) smoothedRTT() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rtt
}

// attempt is the outcome of one exchange with one server.
type attempt struct {
	srv *server
	msg *dns.Msg
	err error
	rtt time.Duration
}

// errRcode marks a response whose rcode makes it unusable (SERVFAIL,
// REFUSED); the message is kept so it can be returned when nothing better
// arrives.
type errRcode struct{ rcode int }

func (e errRcode) Error() string { return "upstream answered " + dns.RcodeToString[e.rcode] }

// exchange sends m (which it does not modify) with the per-server timeout
// and records the outcome. Failures caused by the caller (parent context
// cancelled because another server won the race, or the query deadline
// passing) are not held against the server.
func (s *server) exchange(parent context.Context, m *dns.Msg, timeout time.Duration) attempt {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if s.mRequests != nil {
		s.mRequests.Inc()
	}
	start := time.Now()
	resp, err := s.do(ctx, m)
	rtt := time.Since(start)
	a := attempt{srv: s, msg: resp, err: err, rtt: rtt}
	if err == nil && resp == nil {
		a.err = errors.New("upstream: empty response")
	}
	if a.err != nil {
		a.msg = nil
		if parent.Err() != nil {
			// The race was decided or the client gave up: not the server's fault.
			a.err = fmt.Errorf("%s: %w", s.name, parent.Err())
			return a
		}
		reason := reasonNetwork
		if isTimeout(a.err) || ctx.Err() != nil {
			reason = reasonTimeout
			a.err = fmt.Errorf("%s: %w: %w", s.name, ErrTimeout, a.err)
		} else {
			a.err = fmt.Errorf("%s: %w", s.name, a.err)
		}
		s.record(false, reason, timeout)
		return a
	}
	if s.mDuration != nil {
		s.mDuration.Observe(rtt.Seconds())
	}
	switch resp.Rcode {
	case dns.RcodeServerFailure:
		// The server is reachable; SERVFAIL is often about the name
		// (DNSSEC bogus, lame delegation), so health is not penalised.
		a.err = fmt.Errorf("%s: %w", s.name, errRcode{resp.Rcode})
		s.record(true, reasonServfail, rtt)
		return a
	case dns.RcodeRefused:
		a.err = fmt.Errorf("%s: %w", s.name, errRcode{resp.Rcode})
		s.record(false, reasonRefused, rtt)
		return a
	}
	s.record(true, "", rtt)
	return a
}

// do runs the dnsproxy exchange and returns as soon as ctx is done: the
// plain transport only honours its own timeout, so without this a race
// loser or a query past its deadline would hold the caller. The abandoned
// exchange finishes in the background within the per-server timeout.
func (s *server) do(ctx context.Context, m *dns.Msg) (*dns.Msg, error) {
	type result struct {
		msg *dns.Msg
		err error
	}
	ch := make(chan result, 1)
	q := m.Copy() // dnsproxy may modify the request (dns.Client does)
	go func() {
		resp, err := s.u.Exchange(ctx, q)
		ch <- result{resp, err}
	}()
	select {
	case r := <-ch:
		return r.msg, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// record updates health, RTT and metrics. ok with a non-empty reason counts
// an error without a health penalty.
func (s *server) record(ok bool, reason string, sample time.Duration) {
	if reason != "" && s.mErrors != nil {
		s.mErrors[reason].Inc()
	}
	s.mu.Lock()
	const alpha = 0.3
	v := sample.Seconds()
	if s.rtt == 0 {
		s.rtt = v
	} else {
		s.rtt = (1-alpha)*s.rtt + alpha*v
	}
	wasDown := s.fails >= s.hc.Failures
	var nowDown bool
	if ok {
		s.fails = 0
		s.backoff = 0
		s.downUntil = time.Time{}
	} else {
		s.fails++
		if s.fails >= s.hc.Failures {
			if s.backoff == 0 {
				s.backoff = s.hc.BackoffMin
			} else {
				s.backoff = min(2*s.backoff, s.hc.BackoffMax)
			}
			s.downUntil = s.now().Add(s.backoff)
			nowDown = true
		}
	}
	s.mu.Unlock()
	if wasDown != nowDown {
		if s.mHealthy != nil {
			v := 1.0
			if nowDown {
				v = 0
			}
			s.mHealthy.Set(v)
		}
		if s.onChange != nil {
			s.onChange(s.label, !nowDown)
		}
	}
}

// isTimeout reports whether err is a timeout.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
