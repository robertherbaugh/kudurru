package dnsserver

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type observedDoTCloseListener struct {
	net.Listener
	closed chan<- struct{}
}

func (l observedDoTCloseListener) Accept() (net.Conn, error) {
	co, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &observedDoTCloseConn{Conn: co, closed: l.closed}, nil
}

type observedDoTCloseConn struct {
	net.Conn
	once   sync.Once
	closed chan<- struct{}
}

func (c *observedDoTCloseConn) ConnectionState() tls.ConnectionState {
	return c.Conn.(*tls.Conn).ConnectionState()
}
func (c *observedDoTCloseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.closed <- struct{}{} })
	return err
}

// Observe the precise server Close instead of sleeping past a guessed idle
// duration. The original sequential fixture loses query2 if client scheduling
// resumes only after that Close. Prequeued frames have both replies buffered
// even if the client does not read until the server has closed at max2.
func TestDoTMultiSocketQueuedAndSequentialScheduling(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "original-sequential-idle-refusal"
		if queued {
			name = "queued-max2-before-client-reads"
		}
		t.Run(name, func(t *testing.T) {
			closed := make(chan struct{}, 1)
			reg, cert, counts, _ := runObservedDoTSockets(t, 4, Limits{ReadTimeout: time.Second, IdleTimeout: 150 * time.Millisecond, MaxTCPQueries: 2, ShutdownTimeout: time.Second}, func(l *dnsListener) {
				for _, s := range l.srvs {
					s.Listener = observedDoTCloseListener{Listener: s.Listener, closed: closed}
				}
			})
			client := dotSocketClient(cert)
			co, err := client.Dial(reg.Addr(ProtoDoT).String())
			if err != nil {
				t.Fatal("controlled verified TLS dial failed")
			}
			defer func() { _ = co.Close() }()
			queries := []*dns.Msg{query("original-first.example."), query("original-second.example.")}
			if queued {
				if err := queueDoTQueries(co, queries...); err != nil {
					t.Fatal("controlled two-query write failed")
				}
			} else {
				r, _, err := client.ExchangeWithConn(queries[0], co)
				if err != nil {
					t.Fatal("controlled original first exchange failed")
				}
				checkA(t, r)
			}
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("server did not close at its original idle/query limit")
			}
			if queued {
				for _, q := range queries {
					r, err := co.ReadMsg()
					if err != nil || r.Id != q.Id || len(r.Question) != 1 || r.Question[0] != q.Question[0] {
						t.Fatal("queued original reply lost after observed server close")
					}
					checkA(t, r)
				}
				if _, _, err := client.ExchangeWithConn(query("over-limit.example."), co); err == nil {
					t.Fatal("server accepted a third query beyond max2")
				}
				t.Log("both original queued replies survived observed max2 close; single third request refused")
			} else {
				_, _, err := client.ExchangeWithConn(queries[1], co)
				if !errors.Is(err, io.EOF) {
					t.Fatal("controlled original sequential second query did not reproduce EOF")
				}
				t.Log("original sequential second request failed EOF after observed 150ms idle close; no retry")
			}
			var handled int64
			for i := range counts {
				handled += counts[i].Load()
			}
			want := int64(1)
			if queued {
				want = 2
			}
			if handled != want {
				t.Fatal("server handler count differs from original admitted requests")
			}
		})
	}
}
