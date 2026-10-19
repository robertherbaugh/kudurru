package network_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	network "github.com/robertherbaugh/kudurru/internal/primaryruntime/network"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

type notifyPeer struct {
	target netip.AddrPort
	calls  atomic.Int64
	bad    atomic.Bool
	hook   atomic.Pointer[func()]
}

func (p *notifyPeer) setHook(fn func()) { p.hook.Store(&fn) }
func peer(t *testing.T, signed bool) *notifyPeer {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	check(t, err)
	p := &notifyPeer{target: netip.MustParseAddrPort(packet.LocalAddr().String())}
	started := make(chan struct{})
	server := &dns.Server{PacketConn: packet, TsigProvider: dnsserver.TSIGProvider(ring(t, "test-original-secret")), NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		if m.Opcode != dns.OpcodeNotify || len(m.Answer) != 1 || (signed && (m.IsTsig() == nil || w.TsigStatus() != nil)) {
			p.bad.Store(true)
		}
		p.calls.Add(1)
		if hook := p.hook.Load(); hook != nil {
			(*hook)()
		}
		r := new(dns.Msg)
		r.SetReply(m)
		if signed {
			r.SetTsig("key.", dns.HmacSHA256, 300, time.Now().Unix())
		}
		if err := w.WriteMsg(r); err != nil {
			p.bad.Store(true)
		}
	})}
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { check(t, server.Shutdown()); check(t, <-done) })
	return p
}
func notifyPolicy(target netip.AddrPort, signed bool) public.ZonePolicy {
	p := public.ZonePolicy{Notify: public.NotifySpecified, NotifyTargets: []string{target.String()}}
	if signed {
		p.TransferTSIGKeyRefs = []string{"key."}
	}
	return p
}

func TestActualSignedNotifyAckFaultCompleteRestartDuplicatesSafely(t *testing.T) {
	peer := peer(t, true)
	f := newFixture(t, notifyPolicy(peer.target, true))
	f.change()
	pending := f.view()
	db, err := sql.Open("sqlite", f.path)
	check(t, err)
	defer func() { check(t, db.Close()) }()
	_, err = db.Exec(`CREATE TRIGGER fail_notify_ack BEFORE UPDATE ON objects WHEN NEW.kind='ZoneState' BEGIN SELECT RAISE(ABORT,'injected-notify-ack'); END`)
	check(t, err)
	if err := f.r.NotifyOnce(f.ctx()); err == nil {
		t.Fatal("actual ack fault admitted success")
	}
	if peer.bad.Load() || peer.calls.Load() != 1 || !reflect.DeepEqual(pending, f.view()) {
		t.Fatal("signed send/failed ack mutated head or did not send")
	}
	_, err = db.Exec(`DROP TRIGGER fail_notify_ack`)
	check(t, err)
	// Cold runtime: restore only ONE complete real durable candidate, never a
	// desired RR build or independently sampled head/roster.
	f.r, err = pr.New(f.options)
	check(t, err)
	scheme, err := runtimekinds.NewScheme()
	check(t, err)
	var kinds []replication.Kind
	known := scheme.KnownTypes(public.GroupVersion)
	for name, typ := range known {
		if _, ok := known[name+"List"]; !ok {
			continue
		}
		if _, ok := reflect.New(typ).Interface().(store.Object); ok {
			kinds = append(kinds, replication.Kind{APIVersion: public.GroupVersion.String(), Kind: name})
		}
	}
	slices.SortFunc(kinds, func(a, b replication.Kind) int {
		if a.Kind < b.Kind {
			return -1
		}
		if a.Kind > b.Kind {
			return 1
		}
		return 0
	})
	image, token, err := f.s.ReadServingCandidate(f.ctx())
	check(t, err)
	prepared, err := pr.PrepareImage(f.ctx(), *image, pr.ImageOptions{ClusterID: f.fence.Term.ClusterID, Scheme: scheme, Validation: replication.Validation{SchemaVersion: store.SchemaVersion, Kinds: kinds}, Now: time.Now()})
	check(t, err)
	restore, err := f.r.PrepareRestore(prepared)
	check(t, err)
	check(t, f.s.PublishIfCurrent(f.ctx(), token, sqlite.PublicationOptions{}, func() error { return f.r.InstallRestore(restore) }))
	check(t, f.r.Republish(f.ctx(), f.l.Spec.Zone))
	check(t, f.r.NotifyOnce(f.ctx()))
	acked := f.view()
	if peer.calls.Load() != 2 || peer.bad.Load() || acked.Head.Spec.NotifySequence != 0 || acked.Head.Spec.Serial != pending.Head.Spec.Serial || acked.Head.Spec.ContentHash != pending.Head.Spec.ContentHash {
		t.Fatal("restart lost intent or repeated RR semantics")
	}
}

func TestActualNotifyRotationPolicyChangeAndFinalCheckNeverAckObsoleteAttempt(t *testing.T) {
	for _, mode := range []string{"rotation", "desired-policy", "final-hook"} {
		t.Run(mode, func(t *testing.T) {
			p := peer(t, true)
			f := newFixture(t, notifyPolicy(p.target, true))
			f.change()
			pending := f.view()
			switch mode {
			case "rotation":
				p.setHook(func() {
					ctx, cancel := context.WithTimeout(f.ctx(), time.Second)
					defer cancel()
					check(t, f.keys.Replace(ctx, ring(t, "rotated-secret")))
				})
			case "desired-policy":
				p.setHook(func() {
					z := f.zone.DeepCopy()
					z.Spec.Policy.Notify = public.NotifyNone
					_, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: store.ManagerSystem})
					check(t, err)
				})
			case "final-hook":
				f.invalidateFinal.Store(true)
			}
			if err := f.r.NotifyOnce(f.ctx()); err == nil {
				t.Fatal("obsolete attempt acknowledged")
			}
			if p.calls.Load() != 1 || p.bad.Load() || !reflect.DeepEqual(pending, f.view()) {
				t.Fatal("failed attempt cleared intent or changed head")
			}
			if mode == "final-hook" {
				f.blocked.Store(false)
				f.invalidateFinal.Store(false)
				f.pub.Wake()
				check(t, f.r.NotifyOnce(f.ctx()))
				if p.calls.Load() != 2 || f.view().Head.Spec.NotifySequence != 0 {
					t.Fatal("final check recovery did not duplicate and ack")
				}
			}
		})
	}
}

func TestNotifyRequiresPrivateCapabilityAndActualSuccessfulAttempt(t *testing.T) {
	p := peer(t, true)
	f := newFixture(t, notifyPolicy(p.target, true))
	f.change()
	pending := f.view()
	if _, err := f.a.Admission(f.ctx(), pr.Command{Lifecycle: f.l, Expected: pending.Expected, Reason: "notify-delivery"}); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("public context admitted delivery", err)
	}
	ctx, release, m, z := f.bound()
	defer release()
	if err := f.sender.Send(ctx, m, z); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("unadmitted attempt sent", err)
	}
	check(t, func() error {
		_, err := f.a.Admission(ctx, pr.Command{Lifecycle: f.l, Expected: pending.Expected, Reason: "notify-delivery"})
		return err
	}())
	prepared, err := zonestate.Prepare(pending.Aggregate, pending.Head, zonestate.Limits{})
	check(t, err)
	prepared.Head.Spec.NotifySequence, prepared.Head.Spec.NotifySerial = 0, 0
	if _, err := f.a.Admission(ctx, pr.Command{Lifecycle: f.l, Expected: pending.Expected, Prepared: prepared, Reason: "notify-ack"}); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("unsent attempt admitted ack", err)
	}
	other, err := network.NewNotify(network.NotifyOptions{Capture: f.captureNotify})
	check(t, err)
	if err := other.Send(ctx, m, z); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("attempt transplanted to another issuer", err)
	}
	release()
	if err := f.sender.Send(ctx, m, z); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("released attempt sent", err)
	}
	if p.calls.Load() != 0 || !reflect.DeepEqual(pending, f.view()) {
		t.Fatal("rejected capability mutated or sent")
	}
}

func TestNotifyDisabledAndUnsignedDeliveryStillUseDurableAck(t *testing.T) {
	for _, none := range []bool{true, false} {
		t.Run(fmt.Sprint(none), func(t *testing.T) {
			p := peer(t, false)
			policy := notifyPolicy(p.target, false)
			if none {
				policy.Notify = public.NotifyNone
			}
			f := newFixture(t, policy)
			f.change()
			check(t, f.r.NotifyOnce(f.ctx()))
			want := int64(1)
			if none {
				want = 0
			}
			if p.calls.Load() != want || p.bad.Load() || f.view().Head.Spec.NotifySequence != 0 {
				t.Fatal("explicit no-network/unsigned acknowledgment", p.calls.Load())
			}
		})
	}
}

type resolveFunc func(context.Context, string, string) ([]netip.Addr, error)

func (r resolveFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return r(ctx, network, host)
}

func TestNotifyBoundsResolverFailureAndPartialDeliveryRetainIntent(t *testing.T) {
	for _, mode := range []string{"targets", "names", "addresses", "resolver", "partial"} {
		t.Run(mode, func(t *testing.T) {
			p := peer(t, true)
			policy := notifyPolicy(p.target, true)
			if mode == "names" || mode == "addresses" || mode == "resolver" {
				policy.Notify = public.NotifyZoneNameServers
			}
			f := newFixture(t, policy)
			if mode == "names" || mode == "addresses" || mode == "resolver" {
				z := f.zone.DeepCopy()
				z.Spec.Nameservers = []string{"ns.example.", "other.example."}
				saved, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: store.ManagerSystem})
				check(t, err)
				f.zone = saved.(*public.DNSZone)
			}
			if mode == "targets" || mode == "partial" {
				z := f.zone.DeepCopy()
				z.Spec.Policy.NotifyTargets = append(z.Spec.Policy.NotifyTargets, "127.0.0.1:1")
				saved, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: store.ManagerSystem})
				check(t, err)
				f.zone = saved.(*public.DNSZone)
			}
			f.change()
			pending := f.view()
			o := network.NotifyOptions{Capture: f.captureNotify}
			switch mode {
			case "targets":
				o.MaxTargets = 1
			case "names":
				o.MaxNames = 1
			case "addresses":
				o.MaxAddresses = 1
				o.Resolver = resolveFunc(func(context.Context, string, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, nil
				})
			case "resolver":
				o.Resolver = resolveFunc(func(context.Context, string, string) ([]netip.Addr, error) { return nil, nil })
			}
			sender, err := network.NewNotify(o)
			check(t, err)
			_, release, m, z := f.bound()
			release()
			ctx, release, err := sender.Bind(f.ctx(), m, z)
			if err == nil {
				defer release()
				// This explicitly bound validator uses the real production pin and tx.
				pin := f.keys.PrimaryPin(f.pub)
				err = pin(ctx, f.fence, func(pinned currentguard.Pinned) error {
					return f.s.Command(ctx, f.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						return sender.Validator(pinned.Identity, pinned.Keys)(ctx, tx, sqliteadapter.BackgroundCheck{Lifecycle: f.l, Source: f.zone, Policy: &pending.Aggregate.Policy, Reason: "notify-delivery"})
					})
				})
				if err == nil {
					err = sender.Send(ctx, m, z)
				}
			}
			if err == nil {
				t.Fatal("bounded/partial round succeeded")
			}
			if !reflect.DeepEqual(pending, f.view()) {
				t.Fatal("unsuccessful round lost intent")
			}
			if mode == "partial" && p.calls.Load() != 1 {
				t.Fatal("partial round did not deliver reachable target")
			}
		})
	}
}

func TestNotifyFallbackAcknowledgesWithoutNetwork(t *testing.T) {
	p := peer(t, false)
	f := newFixture(t, notifyPolicy(p.target, false))
	z := f.zone.DeepCopy()
	z.Spec.Fallback = &public.ZoneFallbackSpec{}
	_, err := f.s.Apply(f.ctx(), z, store.ApplyOptions{Manager: store.ManagerSystem})
	check(t, err)
	f.change()
	check(t, f.r.NotifyOnce(f.ctx()))
	if p.calls.Load() != 0 || f.view().Head.Spec.NotifySequence != 0 {
		t.Fatal("fallback sent a NOTIFY or failed to acknowledge disabled intent")
	}
}
