package dnstap

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

type discard struct{}

func (discard) Enqueue(*querylog.Event) bool { return true }

// TestListenPairingFIFOIsBoundedByCount: a query that is never answered
// sits at the head of the pairing FIFO for PairWait; the paired queries
// behind it must not pile up there without bound. With Queue 4 the FIFO
// holds at most 8 entries and at most 4 queries wait, however many pairs
// arrive.
func TestListenPairingFIFOIsBoundedByCount(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const queue = 4
	l, err := Listen(ListenOptions{Settings: config.DnstapListener{Address: "tcp:" + ln.Addr().String(), AllowFrom: []string{"127.0.0.1"}}, Listener: ln, Collector: discard{}, Queue: queue, PairWait: time.Minute, Hold: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan [3]int, 1024)
	l.observe = func(queries, rows, waiting int) { seen <- [3]int{queries, rows, waiting} }
	done := make(chan error, 1)
	go func() { done <- l.Serve(context.Background()) }()
	t.Cleanup(func() {
		_ = l.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	client := netip.MustParseAddr("192.168.1.20")
	now := time.Now()
	send := func(id uint16, response bool) [3]int {
		t.Helper()
		q := new(dns.Msg)
		q.SetQuestion("fifo.example.", dns.TypeA)
		q.Id = id
		m := Message{Type: ClientQuery, SocketFamily: FamilyINET, SocketProtocol: ProtocolUDP, QueryAddress: client.AsSlice(), QueryPort: 1000 + id, QueryTime: now}
		packedQ, _ := q.Pack()
		m.QueryMessage = packedQ
		if response {
			r := new(dns.Msg)
			r.SetReply(q)
			packedR, _ := r.Pack()
			m.Type, m.QueryMessage, m.ResponseMessage, m.ResponseTime = ClientResponse, nil, packedR, now.Add(time.Millisecond)
		}
		payload, _ := AppendFrame(nil, nil, &Frame{Identity: []byte("edge-1"), Message: m})
		l.receive(payload)
		select {
		case s := <-seen:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("the ingest loop did not take the message")
			return [3]int{}
		}
	}
	send(1, false) // never answered: the head for a minute
	worst := [3]int{}
	for id := uint16(2); id < 200; id++ {
		for _, response := range []bool{false, true} {
			s := send(id, response)
			for i := range s {
				worst[i] = max(worst[i], s[i])
			}
		}
	}
	t.Logf("largest pairing FIFO %d, row FIFO %d, waiting %d", worst[0], worst[1], worst[2])
	if worst[0] > 2*queue || worst[1] > queue || worst[2] > queue {
		t.Fatalf("the stages hold more than their bound: FIFO %d (bound %d), rows %d, waiting %d (bound %d)", worst[0], 2*queue, worst[1], worst[2], queue)
	}
}

// TestListenAcceptLogCarriesTheSuppressedCount: after the first line, a
// line about failed Accept calls comes at most once per acceptLogEvery and
// reports how many failures it stands for, so none is lost from the log.
func TestListenAcceptLogCarriesTheSuppressedCount(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	failing := &failingListener{Listener: ln}
	var (
		mu     sync.Mutex
		counts []int64
	)
	logger := slog.New(handlerFunc(func(r slog.Record) {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "failures" {
				mu.Lock()
				counts = append(counts, a.Value.Int64())
				mu.Unlock()
			}
			return true
		})
	}))
	l, err := Listen(ListenOptions{Settings: config.DnstapListener{Address: "tcp:" + ln.Addr().String(), AllowFrom: []string{"127.0.0.1"}}, Listener: failing, Collector: discard{}, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	l.acceptLogEvery = 700 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- l.Serve(context.Background()) }()
	time.Sleep(2500 * time.Millisecond)
	_ = l.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	calls := failing.calls.Load() - 1 // the last call is the one Close ended
	var logged int64
	for _, n := range counts {
		logged += n
	}
	t.Logf("%d failed calls, log lines %v", calls, counts)
	if len(counts) < 2 || len(counts) > 4 || counts[0] != 1 {
		t.Fatalf("log lines %v: want the first failure at once, then one line per 700 ms at most", counts)
	}
	if logged > int64(calls) || int64(calls)-logged > int64(calls)/2+1 {
		t.Fatalf("the lines report %d of %d failures", logged, calls)
	}
}

type failingListener struct {
	net.Listener
	calls atomic.Int64
}

func (f *failingListener) Accept() (net.Conn, error) {
	f.calls.Add(1)
	return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", syscall.EMFILE)}
}

type handlerFunc func(slog.Record)

func (h handlerFunc) Enabled(context.Context, slog.Level) bool      { return true }
func (h handlerFunc) Handle(_ context.Context, r slog.Record) error { h(r); return nil }
func (h handlerFunc) WithAttrs([]slog.Attr) slog.Handler            { return h }
func (h handlerFunc) WithGroup(string) slog.Handler                 { return h }
