package dnsdist

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	qlog "github.com/robertherbaugh/kudurru/internal/stages/log"
)

// liveLog is the query log of the kudurru stand-in: the real collector,
// the dedup index and a sink that keeps the rows.
type liveLog struct {
	mu   sync.Mutex
	rows []*querylog.Event
}

func (s *liveLog) Name() string { return "rows" }
func (s *liveLog) Open(context.Context, json.RawMessage, prometheus.Registerer) error {
	return nil
}
func (s *liveLog) Flush(context.Context) error { return nil }
func (s *liveLog) Close() error                { return nil }
func (s *liveLog) Write(batch []*querylog.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, batch...)
	return nil
}

func (s *liveLog) snapshot() []*querylog.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*querylog.Event(nil), s.rows...)
}

// TestDnstapLiveEdgeOneRowPerClientQuery runs the pinned dnsdist under the
// supervisor with the rendered dnstap output (D372) in front of a kudurru
// stand-in: a PROXYv2 backend that logs every query it answers through the
// real log observer and collector, and a real dnstap listener feeding the
// same collector. An operator rule added through the console makes dnsdist
// also log every response it relays from the backend, the duplicates the
// listener has to recognize. Every client query must be exactly one row:
// the forwarded ones from the backend's own log, the one dnsdist answers
// itself (a NOTIFY it refuses) from its dnstap message.
func TestDnstapLiveEdgeOneRowPerClientQuery(t *testing.T) {
	if os.Getenv("KUDURRU_F43_LIVE") != "1" {
		t.Skip("set KUDURRU_F43_LIVE=1 for owned dnsdist Docker acceptance")
	}
	parent := os.Getenv("KUDURRU_F43_WORKDIR")
	if parent == "" {
		t.Fatal("KUDURRU_F43_WORKDIR must name an owned scratch parent")
	}
	dir, err := os.MkdirTemp(parent, "dnstap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			if data, err := os.ReadFile(filepath.Join(dir, "child.private.log")); err == nil { //nolint:gosec // owned fixture
				t.Logf("child log tail: %.2000s", data[max(0, len(data)-2000):])
			}
		}
		_ = os.RemoveAll(dir) //nolint:gosec // the fresh owned fixture directory
	})
	t.Setenv("TMPDIR", dir)
	name := "kudurru-obs-dnstap-listen-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	fixture, _ := json.Marshal(map[string]string{"name": name, "image": consoleLabImage})
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), fixture, 0o600); err != nil { //nolint:gosec // owned fixture
		t.Fatal(err)
	}
	wrapper, err := os.ReadFile("testdata/console-client.py")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "dnsdist-client")
	if err := os.WriteFile(binary, wrapper, 0o700); err != nil { //nolint:gosec // fixture executable
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "--host", "unix:///tmp/kudurru-lab-docker.sock", "rm", "--force", name).Run() //nolint:gosec // the generated owned container only
	})

	// The kudurru stand-in's query log.
	rows := &liveLog{}
	async := querylog.NewAsyncWith(querylog.AsyncOptions{Sink: rows, BatchSize: 1, Interval: 10 * time.Millisecond})
	t.Cleanup(func() { _ = async.Close(context.Background()) })
	dedup := querylog.NewDedupIndex(0, 0)
	async.SetDedup(dedup)
	observer := &qlog.Observer{Node: "node-a", Collector: async}
	listenAddr := freePort(t)
	metrics := dnstap.NewListenMetrics(nil)
	listener, err := dnstap.Listen(dnstap.ListenOptions{Settings: config.DnstapListener{Address: "tcp:" + listenAddr, AllowFrom: []string{"127.0.0.1"}}, Collector: async, Dedup: dedup, Node: "node-a", Metrics: metrics, Self: func(id string) bool { return id == "node-a" }})
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- listener.Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = listener.Close()
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
	frames := func(result string) float64 {
		return testutil.ToFloat64(metrics.Frames.WithLabelValues(listener.Name(), result))
	}

	credentials, err := edge.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(credentials.Password)
	backend := startLoggingBackend(t, "node-a", secret, observer)
	ports := map[edge.Protocol]int{}
	_, port, _ := net.SplitHostPort(freePort(t))
	ports[edge.Do53], _ = strconv.Atoi(port)
	sup, err := New(Config{Renderer: liveRenderer(), Options: edge.Options{ServerID: "edge-live", PoolResolver: "firstAvailable", Listen: []string{"127.0.0.1"}, Protocols: []edge.Protocol{edge.Do53}, Ports: ports, API: freePort(t), Console: freePort(t), ProxyProtocolSecret: string(secret),
		Dnstap: &edge.Dnstap{Target: "tcp:" + listenAddr}},
		Source: &chanSource{initial: []cluster.Endpoint{backend}, ch: make(chan []cluster.Endpoint, 1)}, ConfigDir: filepath.Join(dir, "config"), Binary: binary, ChildEnv: []string{"KUDURRU_F43_LAB=" + dir},
		Debounce: 100 * time.Millisecond, StatusPoll: 100 * time.Millisecond, HealthTimeout: 5 * time.Second, StopTimeout: 10 * time.Second, ConsoleTimeout: 5 * time.Second,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), NewCredentials: func() (edge.Credentials, error) { return credentials, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("supervisor: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("supervisor shutdown timed out")
		}
	})
	labWait(t, "edge ready", func() bool { ok, _ := sup.Ready(); return ok })
	// The operator's rule: log every relayed response too.
	if err := sup.console(t.Context(), `addResponseAction(AllRule(), DnstapLogResponseAction("edge-live", newFrameStreamTcpLogger("`+listenAddr+`")))`); err != nil {
		t.Fatal("console rule:", err)
	}
	edgeAddr := net.JoinHostPort("127.0.0.1", port)
	const forwarded = 5
	for i := range forwarded {
		q := new(dns.Msg)
		q.SetQuestion("q"+strconv.Itoa(i)+".live.invalid.", dns.TypeA)
		r, _, err := (&dns.Client{Timeout: 3 * time.Second}).Exchange(q, edgeAddr)
		if err != nil || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
			t.Fatalf("query %d through the edge: %v %v", i, err, r)
		}
	}
	notify := new(dns.Msg)
	notify.SetNotify("live.invalid.")
	r, _, err := (&dns.Client{Timeout: 3 * time.Second}).Exchange(notify, edgeAddr)
	if err != nil || r.Rcode != dns.RcodeRefused {
		t.Fatalf("NOTIFY through the edge: %v %v", err, r)
	}
	labWait(t, "the edge's dnstap", func() bool {
		return frames(dnstap.ResultDuplicate) == forwarded && frames(dnstap.ResultRecorded) == 1
	})
	time.Sleep(3 * time.Second) // nothing more may arrive
	got := rows.snapshot()
	names := map[string]int{}
	for _, ev := range got {
		names[ev.Name]++
	}
	t.Logf("rows by name %v; frames recorded=%v duplicate=%v paired=%v self=%v type=%v malformed=%v", names, frames(dnstap.ResultRecorded), frames(dnstap.ResultDuplicate), frames(dnstap.ResultPaired), frames(dnstap.ResultSelf), frames(dnstap.ResultType), frames(dnstap.ResultMalformed))
	if len(got) != forwarded+1 {
		t.Fatalf("%d rows for %d client queries", len(got), forwarded+1)
	}
	for name, n := range names {
		if n != 1 {
			t.Fatalf("%s has %d rows", name, n)
		}
	}
	for _, ev := range got {
		if ev.Name == "live.invalid." {
			if ev.Origin != "edge-live" || ev.Rcode != dns.RcodeRefused || ev.Client != netip.MustParseAddr("127.0.0.1") {
				t.Fatalf("the edge's own answer: %+v", ev)
			}
		} else if ev.Origin != "" || ev.Status != pipeline.StatusForwarded {
			t.Fatalf("a forwarded query was not the backend's row: %+v", ev)
		}
	}
	t.Log("PASS pinned dnsdist: forwarded queries one row each from the backend log, relayed-response dnstap claimed as duplicates, the edge's own answer one row from dnstap")
}

func liveRenderer() Renderer {
	if os.Getenv("KUDURRU_F43_RENDERER") == "lua" {
		return LuaRenderer{}
	}
	return YAMLRenderer{}
}

// startLoggingBackend is a kudurru stand-in behind PROXYv2: it answers the
// health check and A queries and logs every client query through the
// observer with the client the PROXY header names.
func startLoggingBackend(t *testing.T, name string, secret []byte, observer *qlog.Observer) cluster.Endpoint {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		_ = tcp.Close()
		t.Fatal(err)
	}
	policy, err := proxyproto.NewPolicy([]string{"127.0.0.1"}, secret)
	if err != nil {
		t.Fatal(err)
	}
	health := &ingress.Stage{NodeID: name, Epoch: func() uint64 { return 1 }, Lameduck: func() bool { return false }}
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		received := time.Now()
		m := new(dns.Msg)
		m.SetReply(q)
		if len(q.Question) != 1 {
			_ = w.WriteMsg(m)
			return
		}
		question := q.Question[0]
		if question.Qtype == dns.TypeTXT {
			// The health check, answered as kudurru's ingress stage does.
			response, err := health.Serve(&pipeline.Request{Ctx: context.Background(), Msg: q}, func(req *pipeline.Request) (*pipeline.Response, error) {
				return pipeline.Reply(req, dns.RcodeNameError, pipeline.StatusForwarded), nil
			})
			if err == nil {
				m = response.Msg
			} else {
				m.SetRcode(q, dns.RcodeServerFailure)
			}
			_ = w.WriteMsg(m)
			return
		}
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(203, 0, 113, 1)}}
		_ = w.WriteMsg(m)
		addr, ok := w.RemoteAddr().(*proxyproto.Addr)
		if !ok {
			return
		}
		req := &pipeline.Request{Ctx: context.Background(), Msg: q, Received: received, Client: pipeline.Client{Addr: addr.Client, Proto: pipeline.ProtoUDP}}
		observer.Observe(req, &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded}, nil, time.Since(received))
	})
	tcpServer := &dns.Server{Listener: &proxyproto.Listener{Listener: tcp, Policy: policy}, Handler: handler}
	udpServer := &dns.Server{PacketConn: &proxyproto.PacketConn{PacketConn: udp, Policy: policy}, Handler: handler}
	go func() { _ = tcpServer.ActivateAndServe() }()
	go func() { _ = udpServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = tcpServer.Shutdown(); _ = udpServer.Shutdown() })
	return cluster.Endpoint{Name: name, Address: tcp.Addr().String(), Ready: true, Serving: true}
}
