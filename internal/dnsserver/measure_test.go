package dnsserver

// Spike measurements for docs/spikes/dnsserver-quic.md. They are skipped
// unless KUDURRU_SPIKE_MEASURE=1 because they take about a minute and print
// tables instead of asserting:
//
//	KUDURRU_SPIKE_MEASURE=1 go test -run TestSpikeMeasure -count=1 -v ./internal/dnsserver/
//
// Client and server share the process (and the CPUs); numbers are relative
// costs on loopback, not capacity claims.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func skipUnlessMeasuring(t *testing.T) {
	if os.Getenv("KUDURRU_SPIKE_MEASURE") != "1" {
		t.Skip("set KUDURRU_SPIKE_MEASURE=1 to run spike measurements")
	}
}

// measureClient is one connection of one protocol that can exchange
// queries sequentially (or concurrently when the protocol multiplexes).
type measureClient interface {
	exchange(m *dns.Msg) (*dns.Msg, error)
	close()
}

type miekgClient struct {
	c  *dns.Client
	co *dns.Conn
	mu sync.Mutex
}

func (m *miekgClient) exchange(q *dns.Msg) (*dns.Msg, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, _, err := m.c.ExchangeWithConn(q, m.co)
	return r, err
}
func (m *miekgClient) close() { _ = m.co.Close() }

type httpClient struct {
	url    string
	client *http.Client
	closer func()
}

func (h *httpClient) exchange(q *dns.Msg) (*dns.Msg, error) {
	q.Id = 0
	wire, err := q.Pack()
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Post(h.url, dohMIME, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	m := new(dns.Msg)
	return m, m.Unpack(body)
}
func (h *httpClient) close() { h.closer() }

type doqClient struct{ conn *quic.Conn }

func (d *doqClient) exchange(q *dns.Msg) (*dns.Msg, error) {
	q.Id = 0
	return doqExchange(d.conn, q)
}
func (d *doqClient) close() { closeQUIC(d.conn) }

// dial opens a fresh connection (full handshake, no session resumption)
// and returns it once it can carry a query.
func dialMeasure(p Protocol, addr string, pair *testcert.Pair) (measureClient, error) {
	tlsCfg := func(alpn ...string) *tls.Config {
		return &tls.Config{RootCAs: pair.Pool, ServerName: "localhost", NextProtos: alpn, MinVersion: tls.VersionTLS12}
	}
	switch p {
	case ProtoUDP, ProtoTCP, ProtoDoT:
		c := &dns.Client{Net: map[Protocol]string{ProtoUDP: "udp", ProtoTCP: "tcp", ProtoDoT: "tcp-tls"}[p], Timeout: 5 * time.Second}
		if p == ProtoDoT {
			c.TLSConfig = tlsCfg("dot")
		}
		co, err := c.Dial(addr)
		if err != nil {
			return nil, err
		}
		return &miekgClient{c: c, co: co}, nil
	case ProtoDoH:
		tr := &http.Transport{TLSClientConfig: tlsCfg(), ForceAttemptHTTP2: true, MaxConnsPerHost: 1}
		return &httpClient{url: "https://" + addr + DefaultDoHPath, client: &http.Client{Transport: tr, Timeout: 5 * time.Second}, closer: tr.CloseIdleConnections}, nil
	case ProtoDoH3:
		tr := &http3.Transport{TLSClientConfig: tlsCfg()}
		return &httpClient{url: "https://" + addr + DefaultDoHPath, client: &http.Client{Transport: tr, Timeout: 5 * time.Second}, closer: func() { _ = tr.Close() }}, nil
	case ProtoDoQ:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := quic.DialAddr(ctx, addr, tlsCfg("doq"), nil)
		if err != nil {
			return nil, err
		}
		return &doqClient{conn: conn}, nil
	}
	return nil, fmt.Errorf("unknown protocol %s", p)
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	i := int(float64(len(d)-1) * p)
	return d[i]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.3f", float64(d.Microseconds())/1000) }

func TestSpikeMeasure(t *testing.T) {
	skipUnlessMeasuring(t)
	f := startFixture(t, fixtureOpts{specs: allSpecs(), stages: []pipeline.Stage{localAnswer{}}})
	fmt.Printf("\nGo %s, GOOS/GOARCH %s/%s, NumCPU %d, GOMAXPROCS %d\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0))

	// Warm up every path once (lazy init in TLS and quic-go).
	for _, p := range Protocols() {
		c, err := dialMeasure(p, f.addr(p), f.cert)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.exchange(query("warm.example")); err != nil {
			t.Fatalf("%s warm-up: %v", p, err)
		}
		c.close()
	}

	const n = 200
	fmt.Printf("\n### Handshake + first query on loopback (new connection each time, n=%d, ms)\n\n", n)
	fmt.Println("| protocol | p50 | p90 | p99 | max | second query on same conn p50 |")
	fmt.Println("|---|---|---|---|---|---|")
	for _, p := range Protocols() {
		var first, second []time.Duration
		for range n {
			start := time.Now()
			c, err := dialMeasure(p, f.addr(p), f.cert)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := c.exchange(query("example.com"))
			first = append(first, time.Since(start))
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			checkA(t, resp)
			start = time.Now()
			if _, err := c.exchange(query("example.com")); err != nil {
				t.Fatalf("%s second: %v", p, err)
			}
			second = append(second, time.Since(start))
			c.close()
		}
		slices.Sort(first)
		slices.Sort(second)
		fmt.Printf("| %s | %s | %s | %s | %s | %s |\n", p, ms(pct(first, .5)), ms(pct(first, .9)), ms(pct(first, .99)), ms(first[len(first)-1]), ms(pct(second, .5)))
	}

	run := func(p Protocol, conns, inflight int, d time.Duration) (qps float64, errs int64) {
		clients := make([]measureClient, conns)
		for i := range clients {
			c, err := dialMeasure(p, f.addr(p), f.cert)
			if err != nil {
				t.Fatal(err)
			}
			clients[i] = c
		}
		defer func() {
			for _, c := range clients {
				c.close()
			}
		}()
		var ok, bad atomic.Int64
		stop := time.Now().Add(d)
		var wg sync.WaitGroup
		for _, c := range clients {
			for range inflight {
				wg.Go(func() {
					q := query("example.com")
					for time.Now().Before(stop) {
						if _, err := c.exchange(q.Copy()); err != nil {
							bad.Add(1)
							continue
						}
						ok.Add(1)
					}
				})
			}
		}
		wg.Wait()
		return float64(ok.Load()) / d.Seconds(), bad.Load()
	}
	const dur = 5 * time.Second
	fmt.Printf("\n### Queries/sec, 4 connections, %s per run, local answer stage\n\n", dur)
	fmt.Println("| protocol | 4 conns x 1 in flight | errors | 4 conns x 16 in flight | errors |")
	fmt.Println("|---|---|---|---|---|")
	for _, p := range Protocols() {
		q1, e1 := run(p, 4, 1, dur)
		multi, e16 := "n/a (no multiplexing in the client)", "-"
		if p == ProtoDoH || p == ProtoDoQ || p == ProtoDoH3 {
			q16, e := run(p, 4, 16, dur)
			multi, e16 = fmt.Sprintf("%.0f", q16), strconv.FormatInt(e, 10)
		}
		fmt.Printf("| %s | %.0f | %d | %s | %s |\n", p, q1, e1, multi, e16)
	}
}

// TestSpikeDoQGoroutines starts a DoQ-only server in a child process (so
// client goroutines do not pollute the count), parks 256 concurrent
// streams on one connection in the pipeline, and reads the server's
// goroutine count; then repeats with 4 connections x 256 streams, which
// meets the 1024-worker cap.
func TestSpikeDoQGoroutines(t *testing.T) {
	skipUnlessMeasuring(t)
	pair, err := testcert.Write(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, conns := range []int{1, 4} {
		target := conns * DefaultQUICMaxStreams
		cmd := exec.Command(os.Args[0], "-test.run=^TestSpikeDoQHelper$", "-test.count=1") //nolint:gosec // re-executes this test binary as the helper process
		cmd.Env = append(os.Environ(), "KUDURRU_SPIKE_HELPER=doq", "KUDURRU_SPIKE_TARGET="+strconv.Itoa(target),
			"KUDURRU_SPIKE_CERT="+pair.CertFile, "KUDURRU_SPIKE_KEY="+pair.KeyFile)
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := bufio.NewScanner(stdout)
		next := func(prefix string) string {
			for lines.Scan() {
				if l := lines.Text(); strings.HasPrefix(l, prefix) {
					return strings.TrimSpace(strings.TrimPrefix(l, prefix))
				}
			}
			t.Fatalf("helper ended before %q", prefix)
			return ""
		}
		addr := next("ADDR ")
		_, _ = io.WriteString(stdin, "SAMPLE\n")
		base := next("SAMPLE ")

		var cs []*quic.Conn
		for range conns {
			c, err := doqDial(t, addr, &tlsPool{pair})
			if err != nil {
				t.Fatal(err)
			}
			cs = append(cs, c)
		}
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(stdin, "SAMPLE\n")
		idle := next("SAMPLE ")

		var wg sync.WaitGroup
		var failed atomic.Int64
		for _, c := range cs {
			for range DefaultQUICMaxStreams {
				wg.Go(func() {
					q := query("example.com")
					q.Id = 0
					if _, err := doqExchange(c, q); err != nil {
						failed.Add(1)
					}
				})
			}
		}
		result := next("INFLIGHT ")
		wg.Wait()
		_, _ = io.WriteString(stdin, "QUIT\n")
		_ = cmd.Wait()
		for _, c := range cs {
			closeQUIC(c)
		}
		fmt.Printf("DoQ server, %d conn(s) x %d streams: goroutines before any connection %s, with %d idle connection(s) %s, at %s; failed exchanges %d\n",
			conns, DefaultQUICMaxStreams, base, conns, idle, result, failed.Load())
	}
}

// TestSpikeDoQHelper is the child-process side of TestSpikeDoQGoroutines.
func TestSpikeDoQHelper(t *testing.T) {
	if os.Getenv("KUDURRU_SPIKE_HELPER") != "doq" {
		t.Skip("helper process only")
	}
	target, _ := strconv.Atoi(os.Getenv("KUDURRU_SPIKE_TARGET"))
	g := newGate(target + 16)
	reg, err := NewRegistry(Options{Handler: &Handler{Chain: pipeline.MustNew(g)}, Logger: quietLogger})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(Spec{Protocol: ProtoDoQ, Address: "127.0.0.1:0", CertFile: os.Getenv("KUDURRU_SPIKE_CERT"), KeyFile: os.Getenv("KUDURRU_SPIKE_KEY")}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = reg.Run(ctx) }()
	for ok, _ := reg.Ready(); !ok; ok, _ = reg.Ready() {
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Printf("ADDR %s\n", reg.Addr(ProtoDoQ))

	go func() {
		for range target {
			<-g.arrived
		}
		time.Sleep(300 * time.Millisecond)
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		fmt.Printf("INFLIGHT %d in-flight queries: goroutines %d, heap in use %.1f MiB\n", g.inflight.Load(), runtime.NumGoroutine(), float64(m.HeapInuse)/(1<<20))
		close(g.release)
	}()
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		switch in.Text() {
		case "SAMPLE":
			fmt.Printf("SAMPLE %d\n", runtime.NumGoroutine())
		case "QUIT":
			return
		}
	}
}
