package secondary

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestNotifyFullChainAuthorizationAndRefresh(t *testing.T) {
	clock := newManagerClock()
	receiver := &managerReceiver{serial: 1}
	m, st, dz := managerFor(t, clock, receiver, managerDB(t))
	runManager(t, m)
	initial := awaitManager(t, m, dz, func(s Status) bool { return s.Available })
	n := NewNotifyReceiver(m, nil)
	n.Lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.53")}, nil
	}
	n.resolve(t.Context())
	var accepted atomic.Int32
	n.Accepted = func(id string) {
		if id != zone.SecondaryIdentity(dz) {
			t.Error("wrong generation")
		}
		accepted.Add(1)
	}
	auth := authoritative.New(nil)
	auth.Notify = n.Handle
	chain := pipeline.MustNew(&ingress.Stage{}, identify.New(nil, nil), control.New(control.Options{}), auth)
	send := func(msg *dns.Msg, addr, key string, proto pipeline.Proto, synthetic bool) int {
		t.Helper()
		resp, err := chain.Serve(&pipeline.Request{Ctx: t.Context(), Msg: msg, Client: pipeline.Client{Addr: netip.MustParseAddrPort(addr), TSIGKey: key, Proto: proto, Synthetic: synthetic}})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.Rcode
	}
	notify := func() *dns.Msg { return new(dns.Msg).SetNotify("example.") }
	receiver.set(2, nil)
	clock.advance(time.Second)
	if got := send(notify(), "192.0.2.53:5300", "", pipeline.ProtoUDP, false); got != dns.RcodeSuccess {
		t.Fatalf("NOTIFY = %d", got)
	}
	refreshed := awaitManager(t, m, dz, func(s Status) bool { return s.Serial == 2 })
	if accepted.Load() != 1 || !refreshed.ConfirmedAt.After(initial.ConfirmedAt) {
		t.Fatal("authorized hint did not refresh and fan out")
	}
	for _, tc := range []struct {
		name, addr string
		proto      pipeline.Proto
		synthetic  bool
		msg        *dns.Msg
		want       int
	}{
		{"wrong source", "192.0.2.54:53", pipeline.ProtoUDP, false, notify(), dns.RcodeRefused},
		{"unverified transport", "192.0.2.53:53", pipeline.ProtoDoH, false, notify(), dns.RcodeRefused},
		{"synthetic API request", "192.0.2.53:53", pipeline.ProtoUDP, true, notify(), dns.RcodeRefused},
		{"unknown zone", "192.0.2.53:53", pipeline.ProtoUDP, false, new(dns.Msg).SetNotify("unknown."), dns.RcodeNotAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := send(tc.msg, tc.addr, "", tc.proto, tc.synthetic); got != tc.want {
				t.Fatalf("rcode %d want %d", got, tc.want)
			}
		})
	}
	bad := notify()
	bad.Question[0].Qtype = dns.TypeA
	if got := send(bad, "192.0.2.53:53", "", pipeline.ProtoTCP, false); got != dns.RcodeFormatError {
		t.Fatalf("malformed = %d", got)
	}
	if accepted.Load() != 1 {
		t.Fatal("rejected hint fanned out")
	}

	// Key authorization is a conjunction with source authorization. A verified
	// but unrelated key does not authorize the zone, nor does a matching key
	// authorize a primary address that is no longer configured.
	changed := dz.DeepCopy()
	changed.Spec.Secondary.TSIGKeyRef = "notify-key"
	obj, err := st.Apply(t.Context(), changed, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	dz = obj.(*v1alpha1.DNSZone)
	awaitManager(t, m, dz, func(Status) bool { return true })
	n.resolve(t.Context())
	for _, key := range []string{"", "other-key."} {
		if got := send(notify(), "192.0.2.53:53", key, pipeline.ProtoTCP, false); got != dns.RcodeRefused {
			t.Fatalf("key %q = %d", key, got)
		}
	}
	if got := send(notify(), "192.0.2.54:53", "notify-key.", pipeline.ProtoTCP, false); got != dns.RcodeRefused {
		t.Fatal("key bypassed source")
	}
	if got := send(notify(), "192.0.2.53:53", "notify-key.", pipeline.ProtoTCP, false); got != dns.RcodeSuccess {
		t.Fatalf("verified key = %d", got)
	}
}

func TestNotifyHostnameExpiryAndConfigurationIdentity(t *testing.T) {
	clock := newManagerClock()
	m, _, dz := managerFor(t, clock, &managerReceiver{serial: 1}, managerDB(t))
	runManager(t, m)
	awaitManager(t, m, dz, func(s Status) bool { return s.Available })
	n := NewNotifyReceiver(m, nil)
	address := netip.MustParseAddr("::ffff:192.0.2.53")
	n.Lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{address}, nil }
	n.resolve(t.Context())
	id := zone.SecondaryIdentity(dz)
	if !n.sourceAllowed(dz, id, address.Unmap()) {
		t.Fatal("resolved primary not authorized")
	}
	n.Lookup = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("resolver offline") }
	clock.advance(time.Minute)
	n.resolve(t.Context())
	if !n.sourceAllowed(dz, id, address.Unmap()) {
		t.Fatal("lost bounded last-good resolution")
	}
	changed := dz.DeepCopy()
	changed.Spec.Secondary.Primaries = []string{"new-primary.example"}
	if n.sourceAllowed(changed, zone.SecondaryIdentity(changed), address.Unmap()) {
		t.Fatal("old identity authorizes new config")
	}
	clock.advance(time.Minute)
	if n.sourceAllowed(dz, id, address.Unmap()) {
		t.Fatal("expired address remains authorized")
	}
	changed.Spec.Secondary.Primaries = []string{"192.0.2.53:5300"}
	if !n.sourceAllowed(changed, zone.SecondaryIdentity(changed), address.Unmap()) {
		t.Fatal("numeric primary needs no lookup")
	}
}

func TestNotifyDoesNotRenewExpiryOrTrustAdvertisedSerial(t *testing.T) {
	clock := newManagerClock()
	receiver := &managerReceiver{serial: 1}
	m, _, dz := managerFor(t, clock, receiver, managerDB(t))
	runManager(t, m)
	initial := awaitManager(t, m, dz, func(s Status) bool { return s.Available })
	receiver.set(1, errors.New("primary offline"))
	clock.advance(time.Second)
	for range 1000 {
		if !m.Trigger(zone.SecondaryIdentity(dz)) {
			t.Fatal("duplicate hints did not coalesce")
		}
	}
	failed := awaitManager(t, m, dz, func(s Status) bool { return s.Error != "" })
	if !failed.ExpiresAt.Equal(initial.ExpiresAt) || failed.Serial != 1 {
		t.Fatal("hint altered data or expiry")
	}
	if m.Trigger("not-current") {
		t.Fatal("unknown configuration queued")
	}
}

func TestNotifyWhileRefreshBusyIsNotLost(t *testing.T) {
	clock := newManagerClock()
	receiver := &managerReceiver{serial: 1}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	receiver.soaHook = func(ctx context.Context, _ string) error {
		if calls.Add(1) == 2 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	m, _, dz := managerFor(t, clock, receiver, managerDB(t))
	runManager(t, m)
	awaitManager(t, m, dz, func(s Status) bool { return s.Available })
	clock.advance(time.Second)
	m.Trigger(zone.SecondaryIdentity(dz))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	receiver.set(2, nil)
	clock.advance(time.Second)
	m.Trigger(zone.SecondaryIdentity(dz))
	close(release)
	awaitManager(t, m, dz, func(s Status) bool { return s.Serial == 2 })
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() < 3 {
		t.Fatal("hint received during transfer was lost")
	}
}

func TestUpdateOpcodeReachesDedicatedHandler(t *testing.T) {
	auth := authoritative.New(nil)
	called := false
	auth.Update = func(req *pipeline.Request) (*pipeline.Response, error) {
		called = true
		return pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL), nil
	}
	chain := pipeline.MustNew(&ingress.Stage{}, identify.New(nil, nil), control.New(control.Options{}), auth)
	msg := new(dns.Msg).SetUpdate("example.")
	resp, err := chain.Serve(&pipeline.Request{Msg: msg})
	if err != nil || !called || resp.Msg.Rcode != dns.RcodeRefused {
		t.Fatalf("UPDATE was not dispatched: %+v %v", resp, err)
	}
}
