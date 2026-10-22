package pipeline

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
)

type named struct {
	name string
	fn   func(*Request, Next) (*Response, error)
}

func TestResponseTransformOrderWrapsTerminalAnswers(t *testing.T) {
	for _, terminal := range []string{"local", "authoritative", "blocking", "rewrite", "upstream"} {
		t.Run(terminal, func(t *testing.T) {
			var events []string
			stages := []Stage{named{name: "postprocess", fn: func(r *Request, next Next) (*Response, error) {
				events = append(events, "postprocess-enter")
				resp, err := next(r)
				events = append(events, "postprocess-exit")
				return resp, err
			}}}
			for _, name := range []string{"local", "authoritative", "blocking", "rewrite", "cache", "dnssec", "upstream"} {
				stages = append(stages, named{name: name, fn: func(r *Request, next Next) (*Response, error) {
					events = append(events, name+"-enter")
					if name == terminal {
						return Reply(r, dns.RcodeSuccess, StatusForwarded), nil
					}
					resp, err := next(r)
					events = append(events, name+"-exit")
					return resp, err
				}})
			}
			q := new(dns.Msg)
			q.SetQuestion("example.", dns.TypeA)
			if _, err := MustNew(stages...).Serve(&Request{Msg: q}); err != nil {
				t.Fatal(err)
			}
			want := []string{"postprocess-enter"}
			var entered []string
			for _, name := range []string{"local", "authoritative", "blocking", "rewrite", "cache", "dnssec", "upstream"} {
				want = append(want, name+"-enter")
				if name == terminal {
					break
				}
				entered = append(entered, name)
			}
			for i := len(entered) - 1; i >= 0; i-- {
				want = append(want, entered[i]+"-exit")
			}
			want = append(want, "postprocess-exit")
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("unwind: %v, want %v", events, want)
			}
		})
	}
	if _, err := New(named{name: "upstream"}, named{name: "postprocess"}); err == nil {
		t.Fatal("postprocess accepted after terminal upstream")
	}
	var response Response
	if response.Origin != OriginUnknown || response.Origin == OriginForwarded {
		t.Fatal("zero provenance implies forwarding")
	}
}

func (n named) Name() string { return n.name }
func (n named) Serve(r *Request, next Next) (*Response, error) {
	if n.fn == nil {
		return next(r)
	}
	return n.fn(r, next)
}

func TestNewEnforcesOrder(t *testing.T) {
	if _, err := New(named{name: "ingress"}, named{name: "upstream"}); err != nil {
		t.Fatalf("subset in order should be accepted: %v", err)
	}
	if _, err := New(named{name: "upstream"}, named{name: "ingress"}); err == nil {
		t.Fatal("out-of-order stages must be rejected")
	}
	if _, err := New(named{name: "ingress"}, named{name: "ingress"}); err == nil {
		t.Fatal("duplicate stage must be rejected")
	}
	if _, err := New(named{name: "bogus"}); err == nil {
		t.Fatal("unknown stage must be rejected")
	}
}

type recorder struct {
	resp *Response
	err  error
	lat  time.Duration
	n    int
}

func (r *recorder) Observe(_ *Request, resp *Response, err error, lat time.Duration) {
	r.resp, r.err, r.lat, r.n = resp, err, lat, r.n+1
}

func TestChainServeAndObserve(t *testing.T) {
	var order []string
	c := MustNew(
		named{name: "ingress", fn: func(r *Request, next Next) (*Response, error) {
			order = append(order, "ingress-pre")
			resp, err := next(r)
			order = append(order, "ingress-post")
			return resp, err
		}},
		named{name: "upstream", fn: func(r *Request, _ Next) (*Response, error) {
			order = append(order, "upstream")
			return Reply(r, dns.RcodeSuccess, StatusForwarded), nil
		}},
	)
	rec := &recorder{}
	c.Observe(rec)

	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	resp, err := c.Serve(&Request{Msg: q})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != StatusForwarded || resp.Msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("unexpected response %+v", resp)
	}
	if want := []string{"ingress-pre", "upstream", "ingress-post"}; len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("middleware order wrong: %v", order)
	}
	if rec.n != 1 || rec.resp != resp {
		t.Fatalf("observer not called with final response: %+v", rec)
	}
}

func TestChainNoAnswer(t *testing.T) {
	c := MustNew(named{name: "ingress"})
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	if _, err := c.Serve(&Request{Msg: q}); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("expected ErrNoAnswer, got %v", err)
	}
}

func TestStatusClasses(t *testing.T) {
	cases := map[Status]Class{
		StatusForwarded:        ClassPermitted,
		StatusCached:           ClassCached,
		StatusBlockedList:      ClassBlocked,
		StatusBlockedRateLimit: ClassBlocked,
		StatusRefusedACL:       ClassError,
		StatusDNSSECBogus:      ClassError,
	}
	for s, want := range cases {
		if got := s.Class(); got != want {
			t.Errorf("%s: class %s, want %s", s, got, want)
		}
	}
	if StatusBlockedCNAME.String() != "blocked_cname" {
		t.Errorf("name mapping broken: %s", StatusBlockedCNAME)
	}
}
