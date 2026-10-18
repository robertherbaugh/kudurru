package dnsserver

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// TestMeasureAXFR100k transfers a 100,000-record zone over TCP loopback
// with dns.Transfer, unsigned and TSIG-signed, and logs the time, the
// message count and the bytes on the wire (dev-docs/milestones/m2/xfr.md). It runs with
// KUDURRU_MEASURE=1.
func TestMeasureAXFR100k(t *testing.T) {
	if os.Getenv("KUDURRU_MEASURE") == "" {
		t.Skip("set KUDURRU_MEASURE=1")
	}
	dz := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "bench"},
		Spec: v1alpha1.DNSZoneSpec{ZoneName: "bench.example.", Type: v1alpha1.ZoneTypePrimary, DefaultTTL: 300,
			Nameservers: []string{"ns1.bench.example."},
			Policy:      v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllow}},
		Status: v1alpha1.DNSZoneStatus{Serial: 1},
	}
	eps := make([]v1alpha1.Endpoint, 0, 100_000)
	for i := range 80_000 {
		eps = append(eps, v1alpha1.Endpoint{DNSName: fmt.Sprintf("h%d.r%d.bench.example", i, i%100), RecordType: "A",
			Targets: []string{fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)}})
	}
	for i := range 20_000 {
		eps = append(eps, v1alpha1.Endpoint{DNSName: fmt.Sprintf("t%d.bench.example", i), RecordType: "TXT",
			Targets: []string{fmt.Sprintf("v=spf1 ip4:10.0.%d.0/24 -all", i&255)}})
	}
	z, errs := zone.Build(dz, []*v1alpha1.DNSRecordSet{{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "b"}, Spec: v1alpha1.DNSRecordSetSpec{Endpoints: eps}}}, zone.BuildOptions{})
	if !errs.Empty() {
		t.Fatal(errs)
	}
	snap, _ := zone.NewSnapshot(1, true, z)
	h := new(zone.Holder)
	h.Store(snap)
	reg, err := NewRegistry(Options{Handler: &Handler{Chain: pipeline.MustNew(authoritative.New(h)), Logger: quietLogger}, Logger: quietLogger, TSIG: testKeyring(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(Spec{Protocol: ProtoTCP, Address: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = reg.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	addr := reg.Addr(ProtoTCP).String()
	for _, signed := range []bool{false, true} {
		for range 3 {
			m := new(dns.Msg)
			m.SetAxfr("bench.example.")
			tr := &dns.Transfer{}
			if signed {
				tr.TsigSecret = map[string]string{xfrKey: b64(xfrSecret)}
				m.SetTsig(xfrKey, dns.HmacSHA256, 300, time.Now().Unix())
			}
			start := time.Now()
			ch, err := tr.In(m, addr)
			if err != nil {
				t.Fatal(err)
			}
			msgs, rrs, octets := 0, 0, 0
			for env := range ch {
				if env.Error != nil {
					t.Fatal(env.Error)
				}
				msgs++
				rrs += len(env.RR)
				r := new(dns.Msg)
				r.Answer = env.RR
				r.Compress = true
				octets += r.Len()
			}
			took := time.Since(start)
			t.Logf("AXFR 100k signed=%v: %d RRs in %d messages, ~%.1f MB compressed, %s (%.0f RRs/s)",
				signed, rrs, msgs, float64(octets)/1e6, took.Round(time.Millisecond), float64(rrs)/took.Seconds())
		}
	}
}
