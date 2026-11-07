package servingbuild

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store"
	"k8s.io/apimachinery/pkg/api/meta"
)

// A successful startup SOA can precede the controller's first Ready projection.
// Drive that actual status write and source alignment separately: the signed
// request in the gap must fail, even though the durable head never changes.
func TestPrimaryStatusSourceRVNativeSignedTransferBarrier(t *testing.T) {
	h := newPrimaryFeedbackFixture(t)
	secret := bytes.Repeat([]byte{19}, 32)
	keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "transfer-test", Algorithm: dnsserver.TSIGHMACSHA256, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.f.keyrings.Replace(h.ctx, keys); err != nil {
		t.Fatal(err)
	}
	z := h.read("uncommitted")
	z.Spec.Policy.ZoneTransfer = public.AccessUseNetworkACL
	z.Spec.Policy.TransferACL = []string{"127.0.0.0/8"}
	z.Spec.Policy.TransferTSIGKeyRefs = []string{"transfer-test"}
	if _, err := h.f.store.Apply(h.ctx, z, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	h.align(z.Name)
	before, desired := h.observe(z.Name, true)
	if meta.IsStatusConditionTrue(desired.Status.Conditions, public.ConditionReady) {
		t.Fatal("fixture projected Ready before the controlled callback")
	}

	logger := slog.New(slog.DiscardHandler)
	listeners, err := dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: h.f.chain, Logger: logger}, Logger: logger, TSIG: keys, Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := listeners.Add(dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := listeners.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan error, 1)
	go func() { done <- listeners.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	addr := listeners.Addr(dnsserver.ProtoTCP).String()
	secret64 := base64.StdEncoding.EncodeToString(secret)
	client := &dns.Client{Net: "tcp", Timeout: 5 * time.Second, TsigSecret: map[string]string{"transfer-test.": secret64}}
	soa := new(dns.Msg)
	soa.SetQuestion(z.Spec.ZoneName, dns.TypeSOA)
	answer, _, err := client.ExchangeContext(ctx, soa, addr)
	if err != nil || answer == nil || answer.Rcode != dns.RcodeSuccess || !answer.Authoritative || len(answer.Answer) != 1 {
		t.Fatalf("initial native SOA failed: error=%v", err)
	}
	if rr, ok := answer.Answer[0].(*dns.SOA); !ok || rr.Serial != before.Serial {
		t.Fatal("initial native SOA did not identify the committed head")
	}

	written := h.writeCondition(z.Name, "True", public.ReasonServing)
	gap, pending := h.observe(z.Name, false)
	if !gap.ConfigMatchesDesired || gap.Member.Version != before.Member.Version || gap.Source.ResourceVersion == written.ResourceVersion || !meta.IsStatusConditionTrue(pending.Status.Conditions, public.ConditionReady) {
		t.Fatal("missing Ready=true status-only source gap")
	}
	challenged := new(dns.Msg)
	challenged.SetAxfr(z.Spec.ZoneName)
	challenged.SetTsig("transfer-test.", dns.HmacSHA256, 300, time.Now().Unix())
	answer, _, err = client.ExchangeContext(ctx, challenged, addr)
	if err != nil || answer == nil || answer.Rcode != dns.RcodeServerFailure || answer.IsTsig() == nil || len(answer.Answer) != 0 {
		t.Fatalf("native signed AXFR must refuse the source gap without data: error=%v", err)
	}

	// Only the real driver's source reconciliation can open the route. This
	// independent post-alignment request must serve the original head, while
	// the challenged request above remains a failed operation.
	h.align(z.Name)
	after, settled := h.observe(z.Name, true)
	if after.Source.ResourceVersion != written.ResourceVersion || after.Member.Version != before.Member.Version || !reflect.DeepEqual(written, settled) {
		t.Fatal("source alignment changed status or the committed head")
	}
	if result := h.project(z.Name); result.wrote || result.retry != 0 {
		t.Fatal("settled status still has controller work", result)
	}
	transfer := &dns.Transfer{DialTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, TsigSecret: client.TsigSecret}
	admitted := new(dns.Msg)
	admitted.SetAxfr(z.Spec.ZoneName)
	admitted.SetTsig("transfer-test.", dns.HmacSHA256, 300, time.Now().Unix())
	stream, err := transfer.In(admitted, addr)
	if err != nil {
		t.Fatal(err)
	}
	var records []dns.RR
	for envelope := range stream {
		if envelope.Error != nil {
			t.Fatal(envelope.Error)
		}
		records = append(records, envelope.RR...)
	}
	if len(records) < 3 {
		t.Fatal("aligned signed transfer is incomplete")
	}
	first, firstOK := records[0].(*dns.SOA)
	last, lastOK := records[len(records)-1].(*dns.SOA)
	if !firstOK || !lastOK || first.Serial != before.Serial || last.Serial != before.Serial {
		t.Fatal("aligned signed transfer did not retain both original SOAs")
	}
}
