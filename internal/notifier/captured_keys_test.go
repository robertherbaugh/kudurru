package notifier

import (
	"bytes"
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestCapturedNotifyKeysRefuseAndRetainOriginalRound(t *testing.T) {
	old := keyring(t)
	fresh, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: keyName, Algorithm: dns.HmacSHA256, Secret: bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[dnsserver.StaticKeyring]
	var captures, sends atomic.Int32
	z := testZone(t, 1, specified(netip.MustParseAddrPort("127.0.0.1:5353"), keyName))
	n, err := New(Options{Source: holder(t, zone.GateOpen, z), CaptureKeys: func() *dnsserver.StaticKeyring { captures.Add(1); return current.Load() }, send: func(_ context.Context, msg zone.Notify) error {
		sends.Add(1)
		current.Store(fresh)
		q := new(dns.Msg).SetQuestion(z.Name(), dns.TypeSOA)
		q.SetTsig(msg.KeyName, msg.Algorithm, 30, time.Now().Unix())
		wire, _, e := dns.TsigGenerateWithProvider(q, msg.TSIG, "", false)
		if e != nil || dns.TsigVerifyWithProvider(bytes.Clone(wire), dnsserver.TSIGProvider(old), "", false) != nil {
			t.Error("round replaced its original signing key")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	n.notifyZone(t.Context(), z)
	if sends.Load() != 0 || captures.Load() != 1 {
		t.Fatal("unavailable source dispatched signed NOTIFY")
	}
	current.Store(old)
	n.notifyZone(t.Context(), z)
	if sends.Load() != 1 || captures.Load() != 2 {
		t.Fatal("round recaptured credentials")
	}
}
