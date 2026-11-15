package blocking

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

type blockingLease struct{ v *View }

func (l blockingLease) Value() any                            { return l.v }
func (l blockingLease) Clone() (pipeline.ServingLease, error) { return l, nil }
func (blockingLease) Release()                                {}

func TestCapturedBlockingUnwindKeepsDefaultsAndKnownEmptyIdentity(t *testing.T) {
	s, err := New(Options{Context: func(*pipeline.Request) engine.Context {
		return engine.Context{Tags: []string{"child"}, ClientNames: []string{"live.lan"}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	old, err := NewView(Options{DefaultResponse: "customIP", CustomV4: netip.MustParseAddr("192.0.2.11"), BlockTTL: 11 * time.Second}, time.Time{}, catalogue.Embedded())
	if err != nil {
		t.Fatal(err)
	}
	nextView, err := NewView(Options{DefaultResponse: "nxdomain"}, Forever, catalogue.Embedded())
	if err != nil {
		t.Fatal(err)
	}
	current := old
	s.Capture = func(*pipeline.Request) (*View, error) { return current, nil }
	ix := modifierIndex(t, "||target.example^")
	r := modifierRequest(ix, "raw.example.", dns.TypeA, "192.0.2.1")
	r.Serving = blockingLease{old}
	r.Client.IdentityCaptured = true
	d := *policy.FromRequest(r)
	d.BlockTTL = 0
	r.Policy = &d
	terminal := &modifierTerminal{alias: true}
	resp, err := s.Serve(r, func(req *pipeline.Request) (*pipeline.Response, error) {
		current = nextView
		s.Reconfigure(Options{Disabled: true})
		_, _ = s.Toggle().Disable(0)
		return terminal.Serve(req, nil)
	})
	if err != nil || len(resp.Msg.Answer) != 1 || resp.Msg.Answer[0].(*dns.A).A.String() != "192.0.2.11" || resp.Msg.Answer[0].Header().Ttl != 11 {
		t.Fatalf("captured unwind changed %+v %v", resp, err)
	}
	fresh := modifierRequest(ix, "raw.example.", dns.TypeA, "192.0.2.1")
	fresh.Serving = blockingLease{nextView}
	resp, err = s.Serve(fresh, func(req *pipeline.Request) (*pipeline.Response, error) { return terminal.Serve(req, nil) })
	if err != nil || resp.Status != pipeline.StatusForwarded {
		t.Fatal("new disabled cut missing", err)
	}
	current = old
	tagIndex := modifierIndex(t, "||target.example^$ctag=child")
	empty := modifierRequest(tagIndex, "raw.example.", dns.TypeA, "192.0.2.1")
	empty.Serving = blockingLease{old}
	empty.Client.IdentityCaptured = true
	resp, err = s.Serve(empty, func(req *pipeline.Request) (*pipeline.Response, error) { return terminal.Serve(req, nil) })
	if err != nil || resp.Status != pipeline.StatusForwarded {
		t.Fatal("live context enriched captured-empty inventory", err)
	}
	s.Capture = nil
	fresh.Ctx = nil
	if _, err = s.Serve(fresh, nil); !errors.Is(err, pipeline.ErrServingCapture) {
		t.Fatal("live blocking fallback", err)
	}
}

func TestCapturedBlockingAbsoluteDeadline(t *testing.T) {
	now := time.Now()
	view, err := NewView(Options{}, now.Add(time.Minute), catalogue.Embedded())
	if err != nil {
		t.Fatal(err)
	}
	if view.enabled(now) || !view.enabled(now.Add(time.Minute)) {
		t.Fatal("absolute deadline changed semantics")
	}
	if view.disabledUntil != now.Add(time.Minute) {
		t.Fatal("deadline snapshot was mutated")
	}
}

func TestCapturedBlockingTargetInheritsLeaseIdentityAndSettings(t *testing.T) {
	ix := modifierIndex(t, "||alias.example^$dnsrewrite=target.example")
	old, err := NewView(Options{}, time.Time{}, catalogue.Embedded())
	if err != nil {
		t.Fatal(err)
	}
	newView, err := NewView(Options{Disabled: true}, Forever, catalogue.Embedded())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	current := old
	captures, closed := 0, 0
	s.Capture = func(*pipeline.Request) (*View, error) { captures++; return current, nil }
	r := modifierRequest(ix, "alias.example.", dns.TypeA, "192.0.2.1")
	r.Serving = blockingLease{old}
	r.Client.IdentityCaptured = true
	r.Client.Names = []string{"pinned.lan"}
	r.Client.Tags = []string{"child"}
	s.opts.ResolveTarget = func(sub *pipeline.Request) (*pipeline.Response, error) {
		current = newView
		if sub.Serving != r.Serving || sub.Policy != r.Policy || sub.Client.Names[0] != "pinned.lan" || s.view(sub) != old {
			t.Fatal("target lost borrowed cut")
		}
		return s.Serve(sub, func(target *pipeline.Request) (*pipeline.Response, error) {
			if target.Question().Name != "target.example." || s.view(target) != old {
				t.Fatal("target recaptured mutable settings")
			}
			out, e := (&modifierTerminal{}).Serve(target, nil)
			out.AddCleanup(func() { closed++ })
			return out, e
		})
	}
	out, err := s.Serve(r, func(*pipeline.Request) (*pipeline.Response, error) {
		t.Fatal("alias skipped target dispatch")
		return nil, nil
	})
	if err != nil || captures != 1 || len(out.Msg.Answer) != 2 || out.Msg.Answer[0].(*dns.CNAME).Target != "target.example." {
		t.Fatal("captured rewrite target", err)
	}
	out.Close()
	out.Close()
	if closed != 1 {
		t.Fatal("target cleanup lost or repeated")
	}
}
