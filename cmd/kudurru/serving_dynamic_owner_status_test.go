package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/controller"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestServingRuntimeRecordSetAtDynamicNameShowsDuplicated runs the complete
// SQLite serving runtime with the leader controller mounted on its committed
// primary status, as serve does. After a signed dynamic update (RFC 2136)
// adds dyn.example. and dyn2.example., a record set with one endpoint at
// dyn.example. and one elsewhere is Ready and Degraded/Duplicated in its own
// conditions, naming the left-out endpoint, and a record set whose only
// endpoint is at dyn2.example. is not Ready
// (D373). Before, both said Ready/AsExpected while the committed zone left
// the endpoint out, and the reason appeared only in the zone's source
// diagnostics.
func TestServingRuntimeRecordSetAtDynamicNameShowsDuplicated(t *testing.T) {
	s := newServingRuntimeFixtureProviders(t, false)
	runServingFixture(t, s)
	ctrl, err := controller.New(s.opts.Store, controller.Options{CommittedPrimaryStatus: s.status, ZoneCoalesce: -1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector("dynamic-owner-status").Run(ctx, func(ctx context.Context) {
			child, stop := context.WithCancel(ctx)
			controllerDone := make(chan error, 1)
			go func() { controllerDone <- ctrl.Run(child) }()
			if err := s.RunWriter(child); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
			stop()
			if err := <-controllerDone; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	awaitServingRuntime(t, s, func() bool {
		lease, err := s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		owner := value.(*servingbuild.RuntimeOwner)
		return s.primary.driver.Status().Ready && owner.Primary().Admitted("example.", time.Now())
	})
	awaitServingRuntime(t, s, func() bool { ready, _ := s.ready(); return ready })
	f := servingRuntimeDNS(t, s)
	// One UPDATE adds both names. An UPDATE that overlaps a publication is
	// refused with SERVFAIL and no effect (G21); a client sends it again,
	// as this one does.
	update := primaryUpdateMessage("example.", "dyn.example.", true)
	second, err := dns.NewRR("dyn2.example. 60 IN A 192.0.2.34")
	if err != nil {
		t.Fatal(err)
	}
	update.Insert([]dns.RR{second})
	// The controller's first status write of the zone moves its resource
	// version, which the primary driver then aligns; wait for both.
	awaitServingRuntime(t, s, func() bool {
		var z public.DNSZone
		if err := s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
			return false
		}
		c := meta.FindStatusCondition(z.Status.Conditions, public.ConditionReady)
		return c != nil && c.Status == metav1.ConditionTrue && s.primary.driver.Status().Ready
	})
	var answer *dns.Msg
	for attempt := range 10 {
		awaitServingRuntime(t, s, func() bool { st := s.publisher.Status(); return st.ProtectedReady && !st.Preparing })
		answer, err = f.exchange(update.Copy(), false)
		if err == nil && answer != nil && answer.Rcode != dns.RcodeServerFailure {
			break
		}
		t.Logf("UPDATE attempt %d: %v %v", attempt, answer, err)
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil || answer == nil || answer.Rcode != dns.RcodeSuccess {
		t.Fatal("signed UPDATE failed", answer, err)
	}
	set := func(name string, endpoints ...public.Endpoint) *public.DNSRecordSet {
		return &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: endpoints}}
	}
	a := func(host, address string) public.Endpoint {
		return public.Endpoint{DNSName: host, RecordType: "A", Targets: []string{address}}
	}
	if err := s.opts.Store.Command(t.Context(), s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, obj := range []store.Object{set("mixed", a("kept.example.", "192.0.2.42"), a("dyn.example.", "192.0.2.43")), set("collides", a("dyn2.example.", "192.0.2.40"))} {
			if _, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	conditions := func(name string) []metav1.Condition {
		var rs public.DNSRecordSet
		if err := s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: name}, &rs); err != nil {
			t.Fatal(err)
		}
		return rs.Status.Conditions
	}
	is := func(conds []metav1.Condition, typ, status, reason string) bool {
		c := meta.FindStatusCondition(conds, typ)
		return c != nil && string(c.Status) == status && c.Reason == reason
	}
	until := time.Now().Add(30 * time.Second)
	for {
		mixed, collides := conditions("mixed"), conditions("collides")
		if is(mixed, public.ConditionReady, "True", public.ReasonServing) && is(mixed, public.ConditionDegraded, "True", public.ReasonDuplicated) &&
			is(collides, public.ConditionReady, "False", public.ReasonDuplicated) {
			msg := meta.FindStatusCondition(mixed, public.ConditionDegraded).Message
			if !strings.Contains(msg, "dyn.example. A (a record a dynamic update (RFC 2136) added holds this name") || strings.Contains(msg, "kept.example.") {
				t.Fatalf("degraded message %q does not name exactly the left-out endpoint", msg)
			}
			break
		}
		if time.Now().After(until) {
			t.Fatalf("record set conditions did not report the dynamic owner: mixed=%+v collides=%+v", mixed, collides)
		}
		time.Sleep(20 * time.Millisecond)
	}
	query := new(dns.Msg)
	query.SetQuestion("dyn.example.", dns.TypeA)
	answer, err = f.exchange(query, true)
	if err != nil || len(answer.Answer) != 1 || answer.Answer[0].(*dns.A).A.String() != "192.0.2.33" {
		t.Fatal("the dynamic record is not answered as the update stored it", answer, err)
	}
}

// servingRuntimeDNS serves the runtime's published bundle on loopback UDP
// and TCP listeners with the runtime's own primary UPDATE handler, as the
// native update test does.
func servingRuntimeDNS(t *testing.T, s *servingRuntime) *primaryNetworkFixture {
	t.Helper()
	f := &primaryNetworkFixture{store: s.opts.Store, publisher: s.publisher, keys: s.opts.Keyrings.Capture().Keys(), keyrings: s.opts.Keyrings, runtime: s.primary}
	i, a, u := identify.New(nil, nil), authoritative.New(nil), upstream.NewStage(nil)
	a.Update = s.primary.Update
	b, err := blocking.New(blocking.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(cachedata.New(cachedata.Options{}), cache.Settings{}, u)
	r := &postprocess.Stage{}
	ctl := control.New(control.Options{})
	f.chain, err = pipeline.New(i, ctl, r, a, b, c, u)
	if err != nil {
		t.Fatal(err)
	}
	if err := servingbuild.BindDNS(s.publisher, servingbuild.DNSStages{Chain: f.chain, Control: ctl, Identify: i, Authority: a, Blocking: b, Cache: c, Response: r, Resolution: u}, nil); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	f.listeners, err = dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: f.chain, Logger: logger}, Logger: logger, TSIG: f.keys, Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.listeners.Add(dnsserver.Spec{Protocol: dnsserver.ProtoUDP, Address: "127.0.0.1:0"}, dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := f.listeners.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.listeners.Run(ctx) }()
	t.Cleanup(func() {
		stop()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("shutdown: %v", err)
		}
	})
	awaitServingRuntime(t, s, func() bool { ready, _ := f.listeners.Ready(); return ready })
	return f
}
