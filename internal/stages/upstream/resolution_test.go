package upstream

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnssec"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

func TestResolutionPlanPinsRouteAcrossReplacement(t *testing.T) {
	a, b := startFake(t, 1), startFake(t, 2)
	s := newStage(t, upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{a.addr}}}})
	old := s.Set()
	t.Cleanup(func() { _ = old.Close() })
	r := request("www.example.", true)
	p, err := s.Bind(r)
	if err != nil {
		t.Fatal(err)
	}
	next, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{b.addr}}}}, upstream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := dnssec.New(dnssec.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(next, v, true); err != nil {
		t.Fatal(err)
	}
	if p.Current() {
		t.Fatal("retired plan can still publish")
	}
	answer, err := p.Exchange(context.Background(), r.Msg)
	if err != nil || answer.Msg.Answer[0].(*dns.A).A[3] != 1 {
		t.Fatalf("pinned route lost: %v", err)
	}
	fresh, err := s.Bind(request("www.example.", true))
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Validates() || fresh.Validator() != v || fresh.Scope().Epoch == p.Scope().Epoch || fresh.Scope().ID == p.Scope().ID {
		t.Fatal("replacement trust/route scope not published")
	}
	answer, err = fresh.Exchange(context.Background(), r.Msg)
	if err != nil || answer.Msg.Answer[0].(*dns.A).A[3] != 2 {
		t.Fatalf("new route lost: %v", err)
	}
	// A-B-A cannot accept A's old worker, even when stable settings return.
	if _, err := s.Replace(old, nil, false); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Bind(request("www.example.", true))
	if again.Scope().ID != p.Scope().ID || again.Scope().Epoch == p.Scope().Epoch || p.Current() {
		t.Fatal("A-B-A epoch fence failed")
	}
	_ = next.Close()
}

func TestReplacePublicationIsAtomic(t *testing.T) {
	a := newStage(t, upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{"127.0.0.1:10001"}}}})
	setA := a.Set()
	t.Cleanup(func() { _ = setA.Close() })
	setB, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{"127.0.0.1:10002"}}}, ECS: upstream.ECSConfig{Mode: upstream.ECSAdd}}, upstream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer setB.Close()
	vA, _ := dnssec.New(dnssec.Options{})
	vB, _ := dnssec.New(dnssec.Options{})
	if _, err := a.Replace(setA, vA, false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_, _ = a.Replace(setB, vB, true)
			_, _ = a.Replace(setA, vA, false)
		}
	}()
	for i := 0; i < 2000; i++ {
		r := request("www.example.", true)
		p, err := a.Bind(r)
		if err != nil {
			t.Fatal(err)
		}
		isB := p.Validator() == vB
		if p.Validates() != isB || p.ECSSubnet(r.Msg, r.Client.Addr.Addr()).IsValid() != isB {
			t.Fatal("mixed route/trust publication")
		}
	}
	wg.Wait()
	before := a.Set()
	if _, err := a.Replace(setB, nil, true); err == nil || a.Set() != before {
		t.Fatal("invalid replacement changed publication")
	}
}

func TestForwardValidationOverrideAndExactNTAExpiry(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	s := newStage(t, upstream.Config{Groups: map[string]upstream.GroupConfig{"default": {Servers: []string{"127.0.0.1:10001"}}}})
	v, err := dnssec.New(dnssec.Options{NTAs: []dnssec.NTA{{Domain: "example.", Reason: "recovery", ExpiresAt: now.Add(time.Hour)}}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetValidation(v, false); err != nil {
		t.Fatal(err)
	}
	r := request("www.example.", true)
	on := true
	r.DNSSECValidation = &on
	p, _ := s.Bind(r)
	if !p.Validates() {
		t.Fatal("forward override ignored")
	}
	now = now.Add(time.Hour)
	fresh := request("www.example.", true)
	fresh.DNSSECValidation = &on
	q, _ := s.Bind(fresh)
	if p.Current() || p.Scope().ID == q.Scope().ID {
		t.Fatal("NTA expiry did not invalidate scope")
	}
	off := false
	fresh.Resolution = nil
	fresh.DNSSECValidation = &off
	q, _ = s.Bind(fresh)
	if q.Validates() {
		t.Fatal("explicit validation disable ignored")
	}
}
