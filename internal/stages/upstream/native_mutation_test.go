package upstream

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	upstreamset "github.com/robertherbaugh/kudurru/internal/upstream"
)

type stageNativeSource struct{ captured, applied int }

func (*stageNativeSource) Domain() [32]byte { return [32]byte{} }
func (s *stageNativeSource) Capture(ctx context.Context) (recursor.CacheMutation, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing original deadline")
	}
	s.captured++
	return &stageNativeMutation{source: s}, nil
}

type stageNativeMutation struct {
	source *stageNativeSource
	used   bool
}

func (a *stageNativeMutation) Apply(fn func() error) error {
	if a.used {
		return errors.New("reused attempt")
	}
	a.used = true
	a.source.applied++
	return fn()
}

type stageNativeExchange struct{ before func() }

func (e stageNativeExchange) Exchange(_ context.Context, q *dns.Msg, _ netip.AddrPort, _ bool) (*dns.Msg, error) {
	e.before()
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	if q.Question[0].Qtype == dns.TypeNS {
		for _, name := range []string{"a.root.", "b.root."} {
			m.Answer = append(m.Answer, &dns.NS{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 60}, Ns: name})
			m.Extra = append(m.Extra, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(10, 0, 0, 1)})
		}
	} else {
		m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)}}
	}
	return m, nil
}
func TestNativeMutationStageOriginalSourceAndRefusal(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "capture", true: "refuse"}[refuse], func(t *testing.T) {
			source := &stageNativeSource{}
			issued := false
			wire := 0
			cfg := recursor.DefaultConfig()
			cfg.DNSSEC = false
			cfg.IPv6 = recursor.IPv6Off
			cfg.RootHints = []string{"10.0.0.1:53"}
			r, err := recursor.New(cfg, recursor.Options{RequireCacheMutations: true, Exchanger: stageNativeExchange{func() {
				wire++
				if !issued || source.captured == 0 {
					t.Error("acquisition preceded original stage/source capture")
				}
			}}})
			if err != nil {
				t.Fatal(err)
			}
			set, err := upstreamset.New(upstreamset.Config{Groups: map[string]upstreamset.GroupConfig{"default": {Recursive: true}}, Recursion: cfg}, upstreamset.Options{Recursor: r})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = set.Close() }()
			stage := NewStage(set)
			deadline := time.Now().Add(3 * time.Second)
			stage.CaptureNative = func(req *pipeline.Request) (recursor.CacheMutationSource, error) {
				got, ok := req.Ctx.Deadline()
				if !ok || got != deadline {
					t.Error("stage lost earlier original deadline", got, deadline)
				}
				issued = true
				if refuse {
					return nil, errors.New("retired source")
				}
				return source, nil
			}
			m := new(dns.Msg)
			m.SetQuestion("example.", dns.TypeA)
			response, err := stage.Serve(&pipeline.Request{Ctx: t.Context(), Msg: m, Deadline: deadline}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Close()
			if refuse {
				if wire != 0 || r.CacheLen() != 0 || response.Msg.Rcode != dns.RcodeServerFailure {
					t.Fatal("refused stage still acquired", wire, response.Msg)
				}
			} else {
				if wire == 0 || source.applied == 0 || r.CacheLen() == 0 || response.Msg.Rcode != dns.RcodeSuccess {
					t.Fatal("actual recursive cache path missing", wire, source.applied, response.Msg)
				}
			}
		})
	}
}

type contactFailingExchange struct{ asked *int }

func (e contactFailingExchange) Exchange(context.Context, *dns.Msg, netip.AddrPort, bool) (*dns.Msg, error) {
	*e.asked++
	return nil, errors.New("authority unreachable")
}

type refusedNativeSource struct{}

func (refusedNativeSource) Domain() [32]byte { return [32]byte{} }
func (refusedNativeSource) Capture(context.Context) (recursor.CacheMutation, error) {
	return nil, errors.New("retired source")
}

// TestFailureUpstreamContacted: only a failure of an exchange that asked a
// server carries UpstreamContacted (D377). Refusals the stage produces before
// any server is asked carry the same status without it.
func TestFailureUpstreamContacted(t *testing.T) {
	closed, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.LocalAddr().String()
	_ = closed.Close()
	recursive := func(t *testing.T, capture func(*pipeline.Request) (recursor.CacheMutationSource, error)) (*Stage, *int) {
		t.Helper()
		asked := new(int)
		cfg := recursor.DefaultConfig()
		cfg.DNSSEC = false
		cfg.IPv6 = recursor.IPv6Off
		cfg.RootHints = []string{"10.0.0.1:53"}
		r, err := recursor.New(cfg, recursor.Options{RequireCacheMutations: true, Exchanger: contactFailingExchange{asked}})
		if err != nil {
			t.Fatal(err)
		}
		set, err := upstreamset.New(upstreamset.Config{Groups: map[string]upstreamset.GroupConfig{"default": {Recursive: true}}, Recursion: cfg}, upstreamset.Options{Recursor: r})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = set.Close() })
		stage := NewStage(set)
		stage.CaptureNative = capture
		return stage, asked
	}
	for _, tc := range []struct {
		name      string
		stage     func(*testing.T) (*Stage, *int)
		deadline  time.Duration
		contacted bool
	}{
		{"forwarder failed", func(t *testing.T) (*Stage, *int) {
			s, err := New("default", []string{dead}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Set().Close() })
			return s, nil
		}, 3 * time.Second, true},
		{"query ended before any server", func(t *testing.T) (*Stage, *int) {
			s, err := New("default", []string{dead}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Set().Close() })
			return s, nil
		}, -time.Second, false},
		{"serving capture failed", func(t *testing.T) (*Stage, *int) {
			s, err := New("default", []string{dead}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Set().Close() })
			s.Capture = func(*pipeline.Request) (*View, error) { return nil, errors.New("retired generation") }
			return s, nil
		}, 3 * time.Second, false},
		{"native recursion refused", func(t *testing.T) (*Stage, *int) {
			return recursive(t, func(*pipeline.Request) (recursor.CacheMutationSource, error) {
				return nil, errors.New("retired source")
			})
		}, 3 * time.Second, false},
		{"native cache admission refused", func(t *testing.T) (*Stage, *int) {
			return recursive(t, func(*pipeline.Request) (recursor.CacheMutationSource, error) { return refusedNativeSource{}, nil })
		}, 3 * time.Second, false},
		{"native authorities failed", func(t *testing.T) (*Stage, *int) {
			return recursive(t, func(*pipeline.Request) (recursor.CacheMutationSource, error) { return &stageNativeSource{}, nil })
		}, 3 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage, asked := tc.stage(t)
			m := new(dns.Msg)
			m.SetQuestion("contact.example.", dns.TypeA)
			response, err := stage.Serve(&pipeline.Request{Ctx: t.Context(), Msg: m, Deadline: time.Now().Add(tc.deadline)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Close()
			if response.Msg.Rcode != dns.RcodeServerFailure || response.Status != pipeline.StatusServfailUpstream && response.Status != pipeline.StatusTimeoutUpstream || response.Origin != pipeline.OriginUnknown {
				t.Fatal("not an upstream-stage failure", response.Status, response.Origin, response.Msg)
			}
			if response.UpstreamContacted != tc.contacted {
				t.Fatal("wrong contact evidence", response.UpstreamContacted)
			}
			if asked != nil && (*asked != 0) != tc.contacted {
				t.Fatal("authorities asked", *asked)
			}
		})
	}
}
