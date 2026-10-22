package secondary

import (
	"context"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

type capturedNotifyStage struct {
	receiver *NotifyReceiver
	objects  atomic.Pointer[zonecache.Snapshot]
	keys     atomic.Pointer[dnsserver.StaticKeyring]
}

func (*capturedNotifyStage) Name() string { return "authoritative" }
func (s *capturedNotifyStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	return s.receiver.HandleCaptured(req, s.objects.Load(), s.keys.Load())
}

func TestNotifyCapturedNativeCredentialsAndMissingView(t *testing.T) {
	keys := transferRing(t, "notify.", dns.HmacSHA256, []byte("captured-notify-test-secret-0001"))
	m, st, dz := managerFor(t, newManagerClock(), &managerReceiver{serial: 1}, managerDB(t))
	m.opts.Keyring = keys // Before starting the manager; no independent rotation.
	dz = dz.DeepCopy()
	dz.Spec.Secondary.Primaries = []string{"127.0.0.1:5300"}
	dz.Spec.Secondary.TSIGKeyRef = "notify"
	object, err := st.Apply(t.Context(), dz, store.ApplyOptions{Manager: store.ManagerAPI})
	if err != nil {
		t.Fatal(err)
	}
	dz = object.(*public.DNSZone)
	runManager(t, m)
	awaitManager(t, m, dz, func(s Status) bool { return s.Available })
	n := NewNotifyReceiver(m, nil)
	stage := &capturedNotifyStage{receiver: n}
	stage.objects.Store(m.opts.Cache.Snapshot())
	stage.keys.Store(keys)
	var accepted atomic.Int32
	n.Accepted = func(string) { accepted.Add(1) }
	logger := slog.New(slog.DiscardHandler)
	r, err := dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: pipeline.MustNew(stage), Logger: logger}, Logger: logger, TSIG: keys, Sockets: 1})
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
			t.Fatal("listener did not start")
		}
		time.Sleep(time.Millisecond)
	}
	send := func(proto dnsserver.Protocol, want int) {
		t.Helper()
		msg := new(dns.Msg).SetNotify("example.")
		msg.SetTsig("notify.", dns.HmacSHA256, 300, time.Now().Unix())
		response, _, err := (&dns.Client{Net: string(proto), Timeout: time.Second, TsigProvider: dnsserver.TSIGProvider(keys)}).Exchange(msg, r.Addr(proto).String())
		if err != nil || response == nil || response.Rcode != want || response.IsTsig() == nil {
			t.Fatal("captured NOTIFY response", response, err, want)
		}
	}
	send(dnsserver.ProtoUDP, dns.RcodeSuccess)
	send(dnsserver.ProtoTCP, dns.RcodeSuccess)
	rotated := transferRing(t, "notify.", dns.HmacSHA256, []byte("captured-notify-test-secret-0002"))
	stage.keys.Store(rotated)
	send(dnsserver.ProtoTCP, dns.RcodeRefused)
	stage.keys.Store(keys)
	stage.objects.Store(nil)
	send(dnsserver.ProtoTCP, dns.RcodeServerFailure)
	stage.objects.Store(m.opts.Cache.Snapshot())
	// Caller-supplied TSIGKey metadata cannot stand in for native proof.
	fake := &pipeline.Request{Ctx: t.Context(), Msg: new(dns.Msg).SetNotify("example."), Client: pipeline.Client{Addr: netip.MustParseAddrPort("127.0.0.1:5300"), Proto: pipeline.ProtoTCP, TSIGKey: "notify."}}
	response, err := stage.Serve(fake, nil)
	if err != nil || response.Msg.Rcode != dns.RcodeRefused {
		t.Fatal("fabricated key metadata authorized refresh", err)
	}
	if accepted.Load() != 2 {
		t.Fatal("rejected captured hint reached fanout", accepted.Load())
	}
}
