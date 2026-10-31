package servingbuild

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type notifyOffline struct{}

func (notifyOffline) SOA(context.Context, *public.DNSZone, string) (*dns.SOA, error) {
	return nil, errors.New("test primary offline")
}
func (notifyOffline) AXFR(context.Context, *public.DNSZone, string) ([]dns.RR, error) {
	return nil, errors.New("test primary offline")
}

type notifyFixture struct {
	*fixture
	keys      *dnsserver.StaticKeyring
	cache     *zonecache.Cache
	manager   *secondary.Manager
	receiver  *secondary.NotifyReceiver
	secondary *public.DNSZone
	accepted  atomic.Int32
}

func notifyTestRing(t *testing.T, value string) *dnsserver.StaticKeyring {
	t.Helper()
	keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "notify.", Algorithm: dnsserver.TSIGHMACSHA256, Secret: []byte(value)})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	keys := notifyTestRing(t, "test-captured-notify-secret-1")
	owner, err := NewKeyringOwner(keys)
	if err != nil {
		t.Fatal(err)
	}
	f := &notifyFixture{fixture: newFixtureWithKeyrings(t, owner), keys: keys}
	object, err := f.store.Apply(t.Context(), &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "secondary"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeSecondary, ZoneName: "example.", Secondary: &public.SecondarySpec{Primaries: []string{"127.0.0.1:5300"}, TSIGKeyRef: "notify."}}}, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	f.secondary = object.(*public.DNSZone)
	f.cache, err = zonecache.New(f.store, zonecache.Options{Debounce: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(t.TempDir(), "copies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	f.manager, err = secondary.New(secondary.Options{Cache: f.cache, Blobs: blobs, ClusterID: f.store.Info().ClusterID, Keyring: keys, Receiver: notifyOffline{}, PollInterval: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cacheDone, managerDone := make(chan error, 1), make(chan error, 1)
	go func() { cacheDone <- f.cache.Start(ctx) }()
	go func() { managerDone <- f.manager.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-managerDone; err != nil {
			t.Error(err)
		}
		if err := <-cacheDone; err != nil {
			t.Error(err)
		}
	})
	select {
	case <-f.cache.Initialized():
	case <-time.After(5 * time.Second):
		t.Fatal("secondary cache did not initialize")
	}
	f.receiver = secondary.NewNotifyReceiver(f.manager, nil)
	f.receiver.Accepted = func(string) { f.accepted.Add(1) }
	f.waitSource(t, f.secondary)
	f.waitCurrent(t)
	return f
}

func (f *notifyFixture) waitSource(t *testing.T, z *public.DNSZone) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status, ok := f.manager.StatusFor(z); ok && status.Identity == zone.SecondaryIdentity(z) && status.Error != "" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("manager did not observe exact unavailable source")
}

func (f *notifyFixture) request(t *testing.T, lease *serving.Lease, apex string) *pipeline.Request {
	t.Helper()
	if lease == nil {
		var err error
		lease, err = f.pub.CaptureHTTP(t.Context())
		if err != nil {
			t.Fatal(err)
		}
	} else {
		var err error
		lease, err = lease.Clone()
		if err != nil {
			t.Fatal(err)
		}
	}
	req := &pipeline.Request{Ctx: t.Context(), Serving: servingdns.Wrap(lease), Msg: new(dns.Msg).SetNotify(apex), Client: pipeline.Client{Addr: netip.MustParseAddrPort("127.0.0.1:5300"), Proto: pipeline.ProtoUDP}}
	t.Cleanup(req.Serving.Release)
	return req
}

func notifyCode(t *testing.T, callback pipeline.Next, req *pipeline.Request, want int) {
	t.Helper()
	response, err := callback(req)
	if err != nil || response == nil || response.Msg == nil || response.Msg.Rcode != want || !response.Msg.Authoritative {
		t.Fatal("captured hint", response, err, want)
	}
}

func notifyNative(t *testing.T, f *notifyFixture, before func(*pipeline.Request)) *dnsserver.Registry {
	t.Helper()
	callback := CapturedNotify(f.receiver, nil)
	a := authoritative.New(nil)
	a.Notify = func(req *pipeline.Request) (*pipeline.Response, error) {
		if before != nil {
			before(req)
		}
		return callback(req)
	}
	chain := pipeline.MustNew(&ingress.Stage{}, a)
	chain.SetCapture(servingdns.Capture(f.pub))
	logger := slog.New(slog.DiscardHandler)
	r, err := dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: chain, Logger: logger}, Logger: logger, TSIG: f.keys, Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add(dnsserver.Spec{Protocol: dnsserver.ProtoUDP, Address: "127.0.0.1:0"}, dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	for end := time.Now().Add(5 * time.Second); ; {
		if ready, _ := r.Ready(); ready {
			break
		}
		if time.Now().After(end) {
			t.Fatal("native NOTIFY listeners not ready")
		}
		time.Sleep(time.Millisecond)
	}
	return r
}

func nativeNotifyResult(f *notifyFixture, r *dnsserver.Registry, proto dnsserver.Protocol, apex string) (*dns.Msg, error) {
	msg := new(dns.Msg).SetNotify(apex)
	msg.SetTsig("notify.", dns.HmacSHA256, 300, time.Now().Unix())
	response, _, err := (&dns.Client{Net: string(proto), Timeout: 5 * time.Second, TsigProvider: dnsserver.TSIGProvider(f.keys)}).Exchange(msg, r.Addr(proto).String())
	return response, err
}

func TestCapturedNotifyNativeUnavailableSecondaryRefreshAndExactApex(t *testing.T) {
	f := newNotifyFixture(t)
	r := notifyNative(t, f, nil)
	lease, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	value, _ := lease.Bundle().Component("runtime")
	model := value.(*RuntimeOwner).Zones().Zones(time.Now()).Match("example.")
	lease.Release()
	if model == nil || model.Type() != public.ZoneTypeSecondary || model.RoutingAvailableAt(time.Now()) {
		t.Fatal("fixture has no unavailable secondary route")
	}
	before, _ := f.manager.StatusFor(f.secondary)
	for _, proto := range []dnsserver.Protocol{dnsserver.ProtoUDP, dnsserver.ProtoTCP} {
		response, err := nativeNotifyResult(f, r, proto, "EXAMPLE.")
		if err != nil || response == nil || response.Rcode != dns.RcodeSuccess || response.IsTsig() == nil {
			t.Fatal("signed captured secondary refresh", response, err)
		}
	}
	for _, apex := range []string{"child.example.", "uncommitted.example."} {
		response, err := nativeNotifyResult(f, r, dnsserver.ProtoTCP, apex)
		// miekg returns ErrAuth for NOTAUTH before inspecting the response MAC.
		if (err != nil && !errors.Is(err, dns.ErrAuth)) || response == nil || response.Rcode != dns.RcodeNotAuth {
			t.Fatal("ancestor/primary coverage authorized hint", response, err)
		}
	}
	after, _ := f.manager.StatusFor(f.secondary)
	if f.accepted.Load() != 2 || after.Available || !after.ExpiresAt.Equal(before.ExpiresAt) || f.manager.Snapshot().Lookup(zone.SecondaryIdentity(f.secondary)) != nil {
		t.Fatal("hint installed data/renewed expiry or bad route accepted")
	}
}

func TestCapturedNotifyLifecycleCoverageAndSourceReplacementDenyRefresh(t *testing.T) {
	f := newNotifyFixture(t)
	native := notifyNative(t, f, nil)
	send := func(want int) {
		t.Helper()
		response, err := nativeNotifyResult(f, native, dnsserver.ProtoTCP, "example.")
		if err != nil && (want != dns.RcodeNotAuth || !errors.Is(err, dns.ErrAuth)) {
			t.Fatal("native lifecycle hint", err)
		}
		if response == nil || response.Rcode != want || response.IsTsig() == nil {
			t.Fatal("native lifecycle hint", response, want)
		}
	}
	runtime, _, ctx := primaryFixture(t, f.fixture)
	l, err := pr.NewLifecycle(f.store.Info().ClusterID, f.secondary, nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err = runtime.Transition(ctx, nil, l)
	if err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	callback := CapturedNotify(f.receiver, time.Now)
	notifyCode(t, callback, f.request(t, nil, "example."), dns.RcodeNotAuth)
	send(dns.RcodeNotAuth)
	next := l.DeepCopy()
	next.Spec.Sequence++
	next.Spec.Phase = zk.PhaseFencing
	l, err = runtime.Transition(ctx, l, next)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Activate(ctx, l.Spec.Zone)
	if err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	send(dns.RcodeSuccess)
	if err := f.store.Delete(ctx, store.KeyOf(f.secondary), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	object, err := f.store.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: f.secondary.Name}, Spec: f.secondary.Spec}, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	newSource := object.(*public.DNSZone)
	if newSource.UID == f.secondary.UID {
		t.Fatal("source recreation retained UID")
	}
	f.waitSource(t, newSource)
	f.waitCurrent(t)
	notifyCode(t, callback, f.request(t, nil, "example."), dns.RcodeNotAuth)
	send(dns.RcodeNotAuth)
	if f.accepted.Load() != 1 {
		t.Fatal("incomplete/replaced lifecycle accepted refresh")
	}
}

func TestCapturedNotifyNativeHeldRotationAndCancellationDenyRefresh(t *testing.T) {
	for _, mode := range []string{"rotation", "dependency", "cancel", "source-recreation"} {
		t.Run(mode, func(t *testing.T) {
			f := newNotifyFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			r := notifyNative(t, f, func(req *pipeline.Request) {
				if mode == "cancel" {
					ctx, cancel := context.WithCancel(req.Ctx)
					req.Ctx = ctx
					cancel()
				}
				close(entered)
				<-release
			})
			type result struct {
				msg *dns.Msg
				err error
			}
			done := make(chan result, 1)
			go func() { msg, err := nativeNotifyResult(f, r, dnsserver.ProtoTCP, "example."); done <- result{msg, err} }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("native request not captured")
			}
			switch mode {
			case "rotation":
				if err := f.keyrings.Replace(t.Context(), notifyTestRing(t, "test-captured-notify-secret-2")); err != nil {
					close(release)
					t.Fatal(err)
				}
			case "dependency":
				f.epoch.Add(1)
				f.pub.Wake()
			case "source-recreation":
				if err := f.store.Delete(t.Context(), store.KeyOf(f.secondary), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
					close(release)
					t.Fatal(err)
				}
				object, err := f.store.Apply(t.Context(), &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: f.secondary.Name}, Spec: f.secondary.Spec}, store.ApplyOptions{Manager: store.ManagerSystem})
				if err != nil {
					close(release)
					t.Fatal(err)
				}
				f.waitSource(t, object.(*public.DNSZone))
				f.waitCurrent(t)
			}
			close(release)
			got := <-done
			if got.err != nil || got.msg == nil || got.msg.Rcode != dns.RcodeServerFailure || got.msg.IsTsig() == nil || f.accepted.Load() != 0 {
				t.Fatal("obsolete captured native hint accepted", got.msg, got.err)
			}
			if mode == "rotation" {
				f.waitCurrent(t)
				// A fresh complete lease contains the new keys. The old native
				// listener can verify an old signature, but cannot relabel it.
				fresh := notifyNative(t, f, nil)
				response, err := nativeNotifyResult(f, fresh, dnsserver.ProtoTCP, "example.")
				if err != nil || response == nil || response.Rcode != dns.RcodeRefused || response.IsTsig() == nil || f.accepted.Load() != 0 {
					t.Fatal("old native credential accepted by rotated lease", response, err)
				}
			}
		})
	}
}

func TestCapturedNotifyMissingLeaseMalformedTransportAndFabricatedProof(t *testing.T) {
	f := newNotifyFixture(t)
	callback := CapturedNotify(f.receiver, nil)
	for _, mode := range []string{"missing-lease", "nil-context", "canceled", "synthetic", "doh", "wrong-type", "bad-answer", "unexpected-extra", "fake-key", "nil-receiver", "nil-manager"} {
		t.Run(mode, func(t *testing.T) {
			req := f.request(t, nil, "example.")
			want := dns.RcodeServerFailure
			fn := callback
			switch mode {
			case "missing-lease":
				req.Serving = nil
			case "nil-context":
				req.Ctx = nil
			case "canceled":
				ctx, cancel := context.WithCancel(req.Ctx)
				cancel()
				req.Ctx = ctx
			case "synthetic":
				req.Client.Synthetic = true
				want = dns.RcodeRefused
			case "doh":
				req.Client.Proto = pipeline.ProtoDoH
				want = dns.RcodeRefused
			case "wrong-type":
				req.Msg.Question[0].Qtype = dns.TypeA
				want = dns.RcodeFormatError
			case "bad-answer":
				req.Msg.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeA, Class: dns.ClassINET}}}
				want = dns.RcodeFormatError
			case "unexpected-extra":
				req.Msg.Extra = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: "example.", Rrtype: dns.TypeTXT}}}
				want = dns.RcodeFormatError
			case "fake-key":
				req.Client.TSIGKey = "notify."
				want = dns.RcodeRefused
			case "nil-receiver":
				fn = CapturedNotify(nil, nil)
			case "nil-manager":
				fn = CapturedNotify(&secondary.NotifyReceiver{}, nil)
			}
			notifyCode(t, fn, req, want)
		})
	}
	for _, req := range []*pipeline.Request{nil, {}} {
		if response, err := callback(req); response != nil || !errors.Is(err, pipeline.ErrServingCapture) {
			t.Fatal("nil input did not fail closed", response, err)
		}
	}
	if f.accepted.Load() != 0 {
		t.Fatal("rejected request reached hint fanout")
	}
}
