package kudurrutest

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// Upstream is a fake recursive resolver on loopback (UDP and TCP on one
// port) for tests that need forwarding without leaving the host. It
// answers A queries from Records (lowercase FQDN → IPv4) with TTL 3600,
// NXDOMAIN otherwise, after Delay for names in Slow.
type Upstream struct {
	Addr string

	mu      sync.Mutex
	records map[string]string
	slow    map[string]time.Duration
	queries atomic.Int64
	servers []*dns.Server
}

// StartUpstream starts the fake resolver and stops it at cleanup.
func StartUpstream(t testing.TB, records map[string]string) *Upstream {
	t.Helper()
	u := &Upstream{records: map[string]string{}, slow: map[string]time.Duration{}}
	for k, v := range records {
		u.records[strings.ToLower(dns.Fqdn(k))] = v
	}
	port := FreePort(t)
	u.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for _, network := range []string{"udp", "tcp"} {
		started := make(chan error, 1)
		srv := &dns.Server{Addr: u.Addr, Net: network, Handler: dns.HandlerFunc(u.serve), NotifyStartedFunc: func() { started <- nil }}
		go func() {
			if err := srv.ListenAndServe(); err != nil {
				select {
				case started <- err:
				default:
				}
			}
		}()
		select {
		case err := <-started:
			if err != nil {
				t.Fatalf("fake upstream %s: %v", network, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("fake upstream %s did not start", network)
		}
		u.servers = append(u.servers, srv)
	}
	t.Cleanup(u.Stop)
	return u
}

// SetSlow makes answers for name wait d.
func (u *Upstream) SetSlow(name string, d time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.slow[strings.ToLower(dns.Fqdn(name))] = d
}

// Queries returns how many queries arrived.
func (u *Upstream) Queries() int64 { return u.queries.Load() }

// Stop shuts the servers down; it is safe to call twice.
func (u *Upstream) Stop() {
	u.mu.Lock()
	servers := u.servers
	u.servers = nil
	u.mu.Unlock()
	for _, s := range servers {
		_ = s.Shutdown()
	}
}

func (u *Upstream) serve(w dns.ResponseWriter, r *dns.Msg) {
	u.queries.Add(1)
	m := new(dns.Msg)
	m.SetReply(r)
	m.RecursionAvailable = true
	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		_ = w.WriteMsg(m)
		return
	}
	q := r.Question[0]
	name := strings.ToLower(q.Name)
	u.mu.Lock()
	ip, ok := u.records[name]
	delay := u.slow[name]
	u.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	switch {
	case !ok:
		m.Rcode = dns.RcodeNameError
	case q.Qtype == dns.TypeA:
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
			A:   net.ParseIP(ip).To4(),
		})
	}
	_ = w.WriteMsg(m)
}
