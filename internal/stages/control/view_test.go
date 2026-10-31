package control

import (
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestControlImmutableAtomicView(t *testing.T) {
	opts := Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("192.0.2.123/24")}, BlockedQTypes: []uint16{dns.TypeA}}
	a, err := NewView(opts)
	if err != nil {
		t.Fatal("control view construction failed")
	}
	opts.AllowedNetworks[0] = netip.MustParsePrefix("203.0.113.0/24")
	opts.BlockedQTypes[0] = dns.TypeTXT
	b, err := ViewFromConfig(config.Control{AllowedNetworks: []string{"198.51.100.0/24"}, BlockedQTypes: []string{"AAAA"}})
	if err != nil {
		t.Fatal("control config construction failed")
	}
	s := New(Options{})
	if s.Replace(a) != nil {
		t.Fatal("view adoption failed")
	}
	req := func(typ uint16) *pipeline.Request {
		return &pipeline.Request{Msg: msg("example.", typ, nil), Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:12345")}}
	}
	if a.Decide(req(dns.TypeA)) != Refuse || a.Decide(req(dns.TypeAAAA)) != Allow {
		t.Fatal("view retained mutable caller input")
	}
	if _, err := NewView(Options{AllowedNetworks: []netip.Prefix{{}}}); err == nil {
		t.Fatal("strict view accepted invalid prefix")
	}
	if s.Replace(nil) == nil || s.Decide(req(dns.TypeAAAA)) != Allow {
		t.Fatal("failed replacement changed the old view")
	}
	var failed atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 2000 {
			if s.Replace(a) != nil || s.Replace(b) != nil {
				failed.Store(true)
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for range 2000 {
				r, err := s.Serve(req(dns.TypeA), func(*pipeline.Request) (*pipeline.Response, error) { failed.Store(true); return nil, nil })
				if err != nil || r == nil || r.Msg.Rcode != dns.RcodeRefused || r.Status != pipeline.StatusRefusedACL {
					failed.Store(true)
				}
			}
		})
	}
	wg.Wait()
	if failed.Load() {
		t.Fatal("reload mixed rules from different immutable views")
	}
}

func TestControlCaptureNeverFallsBack(t *testing.T) {
	request := &pipeline.Request{Msg: msg("example.", dns.TypeA, nil)}
	s := New(Options{})
	for _, capture := range []func(*pipeline.Request) (*View, error){func(*pipeline.Request) (*View, error) { return nil, nil }, func(*pipeline.Request) (*View, error) { return nil, pipeline.ErrServingCapture }} {
		s.Capture = capture
		resp, err := s.Serve(request, func(*pipeline.Request) (*pipeline.Response, error) {
			t.Fatal("missing control owner reached answer path")
			return nil, nil
		})
		if resp != nil || !errors.Is(err, pipeline.ErrServingCapture) {
			t.Fatal("missing original control owner fell back to live empty rules")
		}
	}
	s.Capture = nil
	request.Serving = missingControlLease{}
	resp, err := s.Serve(request, func(*pipeline.Request) (*pipeline.Response, error) {
		t.Fatal("captured request without control binding reached answer path")
		return nil, nil
	})
	if resp != nil || !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal("unbound captured control stage used live rules")
	}
}

type missingControlLease struct{}

func (missingControlLease) Value() any                              { return nil }
func (l missingControlLease) Clone() (pipeline.ServingLease, error) { return l, nil }
func (missingControlLease) Release()                                {}
