package dnsserver

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

type cleanupAnswer struct {
	closed, iterated atomic.Int64
	ctx              context.Context
}

func (*cleanupAnswer) Name() string { return "local" }
func (s *cleanupAnswer) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	s.ctx = req.Ctx
	r, _ := localAnswer{}.Serve(req, nil)
	r.AddCleanup(func() { s.closed.Add(1) })
	r.Transfer = func(yield func(*dns.Msg) bool) {
		s.iterated.Add(1)
		if req.Ctx.Err() != nil {
			panic("stream context cancelled before consumption")
		}
		yield(r.Msg)
	}
	return r, nil
}

func TestResponseCleanupEveryListener(t *testing.T) {
	for _, p := range []Protocol{ProtoUDP, ProtoTCP, ProtoDoT, ProtoDoH, ProtoDoQ, ProtoDoH3} {
		t.Run(string(p), func(t *testing.T) {
			stage := &cleanupAnswer{}
			f := startFixture(t, fixtureOpts{specs: []Spec{{Protocol: p}}, stages: []pipeline.Stage{stage}})
			u := f.dnsproxyClient(t, p)
			r, err := u.Exchange(t.Context(), query("cleanup.example."))
			if err != nil {
				t.Fatal(err)
			}
			checkA(t, r)
			deadline := time.Now().Add(2 * time.Second)
			for stage.closed.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if stage.closed.Load() != 1 {
				t.Fatalf("cleanup count %d", stage.closed.Load())
			}
			want := int64(0)
			if p == ProtoTCP || p == ProtoDoT {
				want = 1
			}
			if stage.iterated.Load() != want {
				t.Fatalf("stream count %d want %d", stage.iterated.Load(), want)
			}
		})
	}
}

func TestResponseCleanupResolveAndNeverIterated(t *testing.T) {
	s := &cleanupAnswer{}
	h := &Handler{Chain: pipeline.MustNew(s), Logger: quietLogger}
	checkA(t, h.Resolve(t.Context(), query("ignored.example."), pipeline.Client{Proto: pipeline.ProtoTCP}))
	if s.closed.Load() != 1 || s.iterated.Load() != 0 {
		t.Fatal("Resolve ignored transfer without closing")
	}
	_, stream, closeResponse := h.resolve(t.Context(), query("never.example."), pipeline.Client{Proto: pipeline.ProtoTCP})
	if stream == nil || s.ctx.Err() != nil {
		t.Fatal("stream context must remain usable until close")
	}
	closeResponse()
	closeResponse()
	if s.closed.Load() != 2 || s.ctx.Err() == nil {
		t.Fatal("explicit close failed cancellation/cleanup")
	}
}

type failingWriter struct{ writes int }

func (*failingWriter) LocalAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53} }
func (*failingWriter) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000}
}
func (w *failingWriter) WriteMsg(*dns.Msg) error { w.writes++; return errors.New("write failed") }
func (*failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }
func (*failingWriter) Close() error              { return nil }
func (*failingWriter) TsigStatus() error         { return nil }
func (*failingWriter) TsigTimersOnly(bool)       {}
func (*failingWriter) Hijack()                   {}

func TestResponseCleanupWriteFailure(t *testing.T) {
	for _, p := range []pipeline.Proto{pipeline.ProtoUDP, pipeline.ProtoTCP, pipeline.ProtoDoT} {
		s := &cleanupAnswer{}
		w := &failingWriter{}
		h := &Handler{Chain: pipeline.MustNew(s), Logger: quietLogger}
		serveMiekg(h, &env{}, p, w, query("failure.example."))
		if w.writes != 1 || s.closed.Load() != 1 {
			t.Fatalf("%s writes=%d cleanup=%d", p, w.writes, s.closed.Load())
		}
	}
}
