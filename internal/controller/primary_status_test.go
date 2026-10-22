package controller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type primaryStatusProbe struct {
	mu            sync.Mutex
	changed       chan struct{}
	calls         map[string]int
	subscriptions int
	reconcile     func(context.Context, *v1alpha1.DNSZone) (bool, time.Duration, error)
}

func newStatusProbe() *primaryStatusProbe {
	return &primaryStatusProbe{changed: make(chan struct{}), calls: map[string]int{}}
}
func (p *primaryStatusProbe) Subscribe() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subscriptions++
	return p.changed
}
func (p *primaryStatusProbe) wake() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.changed)
	p.changed = make(chan struct{})
}
func (p *primaryStatusProbe) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[name]
}
func (p *primaryStatusProbe) Reconcile(ctx context.Context, z *v1alpha1.DNSZone) (bool, time.Duration, error) {
	p.mu.Lock()
	p.calls[z.Name]++
	fn := p.reconcile
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, z)
	}
	return false, 0, nil
}

func TestCommittedPrimaryStatusKeepsConflictDiagnosticsWithoutLegacyStamp(t *testing.T) {
	p := newStatusProbe()
	e := newEnv(t, func(o *Options) { o.CommittedPrimaryStatus = p })
	e.apply(zoneObj("primary", "example.com."))
	e.apply(rrsetObj("a", "first", "primary", 0, ep("x.example.com", "A", "192.0.2.1")))
	e.apply(rrsetObj("b", "cname", "primary", time.Second, ep("x.example.com", "CNAME", "elsewhere.example.net")))
	e.start()
	e.eventually("delegated primary", func() bool { return p.count("primary") > 0 })
	e.eventually("record conflict diagnostics", func() bool {
		return cond(e.rrset("b", "cname").Status.Conditions, v1alpha1.ConditionDegraded) == "True/Duplicated"
	})
	if z := e.zone("primary"); z.Status.Serial != 0 || z.Status.ContentHash != "" || z.Status.RecordCount != 0 || e.st.Writes(KindDNSZone) != 0 || e.eventCount(EventSerialBumped, key(KindDNSZone, "", "primary")) != 0 {
		t.Fatal("legacy primary stamper wrote status", z.Status)
	}
	e.del(e.rrset("a", "first"))
	e.eventually("conflict removal fanout", func() bool {
		return cond(e.rrset("b", "cname").Status.Conditions, v1alpha1.ConditionDegraded) == "False/AsExpected"
	})
}

func TestCommittedPrimaryStatusBroadcastRetryAndTermJoin(t *testing.T) {
	p := newStatusProbe()
	var retry atomic.Bool
	retry.Store(true)
	p.reconcile = func(_ context.Context, z *v1alpha1.DNSZone) (bool, time.Duration, error) {
		z.Spec.ZoneName = "mutated.example." // callback receives a detached cached object.
		if retry.Swap(false) {
			return false, 15 * time.Millisecond, nil
		}
		return false, 0, nil
	}
	e := newEnv(t, func(o *Options) { o.CommittedPrimaryStatus = p })
	e.apply(zoneObj("primary", "example.com."))
	secondary := zoneObj("secondary", "secondary.example.")
	secondary.Spec.Type = v1alpha1.ZoneTypeSecondary
	e.apply(secondary)
	forward := zoneObj("forward", "forward.example.")
	forward.Spec.Type = v1alpha1.ZoneTypeForward
	e.apply(forward)
	e.start()
	e.eventually("bounded retry", func() bool { return p.count("primary") >= 2 })
	if e.zone("primary").Spec.ZoneName != "example.com." {
		t.Fatal("callback mutated cached source")
	}
	for range 3 {
		before := p.count("primary")
		p.wake()
		e.eventually("broadcast primary requeue", func() bool { return p.count("primary") > before })
	}
	if p.count("secondary") != 0 || p.count("forward") != 0 {
		t.Fatal("nonprimary delegated")
	}
	e.eventually("legacy secondary status", func() bool { return e.zone("secondary").Status.ObservedGeneration != 0 })
	entered := make(chan struct{}, 1)
	exited := make(chan struct{}, 1)
	p.mu.Lock()
	p.reconcile = func(ctx context.Context, _ *v1alpha1.DNSZone) (bool, time.Duration, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		select {
		case exited <- struct{}{}:
		default:
		}
		return false, 0, ctx.Err()
	}
	p.mu.Unlock()
	p.wake()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no in-flight reconcile")
	}
	e.stop()
	select {
	case <-exited:
	default:
		t.Fatal("Run returned before canceled callback joined")
	}
	p.mu.Lock()
	subscriptions := p.subscriptions
	p.mu.Unlock()
	p.wake()
	time.Sleep(25 * time.Millisecond)
	p.mu.Lock()
	after := p.subscriptions
	p.mu.Unlock()
	if after != subscriptions {
		t.Fatal("broadcast watcher survived term")
	}
	// A fresh controller term resubscribes and observes the latest installation.
	p.mu.Lock()
	p.reconcile = nil
	p.mu.Unlock()
	before := p.count("primary")
	e.start()
	e.eventually("next term observation", func() bool { return p.count("primary") > before })
}

func TestCommittedPrimaryStatusErrorAndWroteOutcome(t *testing.T) {
	p := newStatusProbe()
	sentinel := errors.New("test status admission denied")
	var attempts atomic.Int64
	p.reconcile = func(context.Context, *v1alpha1.DNSZone) (bool, time.Duration, error) {
		if attempts.Add(1) == 1 {
			return false, 0, sentinel
		}
		return true, 0, nil
	}
	observed := make(chan struct{}, 1)
	e := newEnv(t, func(o *Options) {
		o.CommittedPrimaryStatus = p
		o.ReconcileHook = func(k store.Key, _ time.Duration, wrote bool, err error) {
			if k.Kind == KindDNSZone && wrote && err == nil {
				select {
				case observed <- struct{}{}:
				default:
				}
			}
		}
	})
	e.apply(zoneObj("primary", "example.com."))
	e.start()
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("capability error was not retried/wrote not reported")
	}
	if e.st.Writes(KindDNSZone) != 0 {
		t.Fatal("controller wrote after capability outcome")
	}
}
