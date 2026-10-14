package pipeline

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type countedLease struct {
	refs *atomic.Int64
	once sync.Once
}

func newCountedLease() *countedLease {
	refs := &atomic.Int64{}
	refs.Store(1)
	return &countedLease{refs: refs}
}
func (l *countedLease) Value() any { return l.refs.Load() }
func (l *countedLease) Clone() (ServingLease, error) {
	l.refs.Add(1)
	return &countedLease{refs: l.refs}, nil
}
func (l *countedLease) Release() { l.once.Do(func() { l.refs.Add(-1) }) }

type captureObserver struct{ check func(*Request) }

func (o captureObserver) Observe(r *Request, _ *Response, _ error, _ time.Duration) { o.check(r) }

func TestServingCaptureOwnsWholeUnwindAndInheritedQueries(t *testing.T) {
	lease := newCountedLease()
	captures := 0
	child := MustNew(named{name: "upstream", fn: func(r *Request, _ Next) (*Response, error) {
		if r.Serving != lease || lease.refs.Load() != 1 {
			t.Fatal("child lost outer ownership")
		}
		return Reply(r, dns.RcodeSuccess, StatusForwarded), nil
	}})
	child.SetCapture(func(*Request) (ServingLease, error) { t.Fatal("child recaptured"); return nil, nil })
	chain := MustNew(named{name: "authoritative", fn: func(r *Request, _ Next) (*Response, error) {
		sub := *r
		resp, err := child.Serve(&sub)
		if lease.refs.Load() != 1 || sub.Serving != lease {
			t.Fatal("child released borrowed lease")
		}
		return resp, err
	}})
	chain.SetCapture(func(*Request) (ServingLease, error) { captures++; return lease, nil })
	chain.Observe(captureObserver{check: func(r *Request) {
		if r.Serving != lease || lease.refs.Load() != 1 {
			t.Fatal("released before observer")
		}
	}})
	q := new(dns.Msg)
	q.SetQuestion("example.", dns.TypeA)
	req := &Request{Msg: q}
	resp, err := chain.Serve(req)
	if err != nil || captures != 1 || lease.refs.Load() != 0 || req.Serving != nil {
		t.Fatalf("capture lifecycle %d %d %v", captures, lease.refs.Load(), err)
	}
	resp.Close()
}

func TestServingCaptureReleaseFailuresAndStreamCleanup(t *testing.T) {
	for _, mode := range []string{"error", "drop", "panic", "observer-panic", "no-answer", "stream", "capture-error", "nil-capture"} {
		t.Run(mode, func(t *testing.T) {
			lease := newCountedLease()
			called := false
			var responseClosed atomic.Int64
			chain := MustNew(named{name: "upstream", fn: func(r *Request, _ Next) (*Response, error) {
				called = true
				switch mode {
				case "error":
					return nil, errors.New("failure")
				case "drop":
					return nil, ErrDrop
				case "panic":
					panic("stage")
				case "no-answer":
					return nil, ErrNoAnswer
				}
				rsp := Reply(r, dns.RcodeSuccess, StatusForwarded)
				rsp.AddCleanup(func() { responseClosed.Add(1) })
				if mode == "stream" {
					rsp.Transfer = func(yield func(*dns.Msg) bool) { yield(rsp.Msg) }
				}
				return rsp, nil
			}})
			chain.SetCapture(func(*Request) (ServingLease, error) {
				if mode == "capture-error" {
					return lease, errors.New("prepare")
				}
				if mode == "nil-capture" {
					lease.Release()
					return nil, nil
				}
				return lease, nil
			})
			if mode == "observer-panic" {
				chain.Observe(captureObserver{check: func(*Request) { panic("observer") }})
			}
			q := new(dns.Msg)
			q.SetQuestion("example.", dns.TypeA)
			var rsp *Response
			func() {
				defer func() {
					if r := recover(); r != nil && mode != "panic" && mode != "observer-panic" {
						t.Fatalf("unexpected panic %v", r)
					}
				}()
				rsp, _ = chain.Serve(&Request{Msg: q})
			}()
			if mode == "stream" {
				if lease.refs.Load() != 1 {
					t.Fatal("released before stream use")
				}
				// Never iterate. A shallow copy and concurrent repeated Close own it.
				copyResponse := *rsp
				var wg sync.WaitGroup
				for range 8 {
					wg.Go(func() { copyResponse.Close(); rsp.Close() })
				}
				wg.Wait()
			}
			if lease.refs.Load() != 0 {
				t.Fatalf("leaked lease: %d", lease.refs.Load())
			}
			if mode == "observer-panic" && responseClosed.Load() != 1 {
				t.Fatal("observer panic leaked response resources")
			}
			if (mode == "capture-error" || mode == "nil-capture") && called {
				t.Fatal("capture failure ran stages")
			}
		})
	}
}

func TestResponseCleanupPanicCannotStrandOtherOwners(t *testing.T) {
	var r Response
	var n atomic.Int64
	r.AddCleanup(func() { panic("owner") })
	r.AddCleanup(func() { n.Add(1) })
	copyResponse := r
	copyResponse.Close()
	r.Close()
	r.AddCleanup(func() { n.Add(1) })
	if n.Load() != 2 {
		t.Fatalf("cleanup calls %d", n.Load())
	}
}
