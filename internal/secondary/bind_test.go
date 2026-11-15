package secondary

import (
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func TestSnapshotBindAcquiredNativeCredentialsAndDurableRestore(t *testing.T) {
	ring := transferRing(t, transferKey, dns.HmacSHA256, transferSecret)
	soa := transferSOA()
	records := []dns.RR{soa, transferRR(t, "example.org. 60 IN NS ns.example.org."), transferRR(t, "www.example.org. 60 IN A 192.0.2.8")}
	address := transferPrimary(t, ring, func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		if w.TsigStatus() != nil || q.IsTsig() == nil {
			return
		}
		answer := records[:1]
		if q.Question[0].Qtype == dns.TypeAXFR {
			answer = append(append([]dns.RR(nil), records...), dns.Copy(soa))
		}
		m := transferReply(q, answer...)
		m.SetTsig(q.IsTsig().Hdr.Name, q.IsTsig().Algorithm, 300, time.Now().Unix())
		if err := w.WriteMsg(m); err != nil {
			t.Error(err)
		}
	})
	dz := transferZone(true)
	dz.Name, dz.UID = "native", "native-uid"
	dz.Spec.Secondary.Primaries = []string{address}
	m := &Manager{opts: Options{ClusterID: "cluster", Keyring: ring, Receiver: &TransferClient{Keyring: ring}, Blobs: managerDB(t), Now: time.Now}}
	e := &entry{dz: dz, id: zone.SecondaryIdentity(dz)}
	r := m.refresh(t.Context(), e)
	if r.err != nil || r.data == nil {
		t.Fatal("native acquired copy", r.err)
	}
	if err := m.persist(t.Context(), e.id, r.data); err != nil {
		t.Fatal(err)
	}
	e.data, e.z, e.credential = r.data, r.z, r.data.Credential
	rotated := transferRing(t, transferKey, dns.HmacSHA256, []byte("rotated-secondary-test-secret"))
	expected, _ := boundCredential(dz.Spec.Secondary.TSIGKeyRef, rotated)
	e.credential = expected // A later job's key must never relabel acquired A.
	if err := m.publish(t.Context(), map[string]*entry{e.id: e}); err != nil {
		t.Fatal(err)
	}
	held := m.Snapshot()
	bound, err := held.Bind([]*public.DNSZone{dz}, ring)
	if err != nil || bound.Lookup(e.id) == nil {
		t.Fatal("acquired native credential rejected", err)
	}
	for _, current := range []*dnsserver.StaticKeyring{rotated, transferRing(t, transferKey, dns.HmacSHA512, transferSecret)} {
		changed, err := held.Bind([]*public.DNSZone{dz}, current)
		if err != nil || changed.Lookup(e.id) != nil {
			t.Fatal("old runtime relabeled as new key/algorithm", err)
		}
	}
	if _, err := held.Bind([]*public.DNSZone{dz}, nil); err == nil {
		t.Fatal("nil provider accepted")
	}
	missing := &Snapshot{zones: held.zones}
	if b, err := missing.Bind([]*public.DNSZone{dz}, ring); err != nil || b.Lookup(e.id) != nil {
		t.Fatal("missing acquired metadata accepted", err)
	}
	replaced := dz.DeepCopy()
	replaced.UID = "replacement"
	if b, err := held.Bind([]*public.DNSZone{replaced}, ring); err != nil || b.Lookup(zone.SecondaryIdentity(replaced)) != nil {
		t.Fatal("delete/recreate reused acquired copy", err)
	}
	restore := &entry{dz: dz, id: e.id, credential: r.data.Credential}
	restore.data, restore.z, err = m.restore(t.Context(), restore)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.publish(t.Context(), map[string]*entry{restore.id: restore}); err != nil {
		t.Fatal(err)
	}
	b, err := m.Snapshot().Bind([]*public.DNSZone{dz}, ring)
	if err != nil || b.Lookup(e.id) == nil || b.Lookup(e.id).AvailableAt(r.data.ExpiresAt) || !bound.Lookup(e.id).AvailableAt(r.data.ExpiresAt.Add(-time.Nanosecond)) {
		t.Fatal("restore/binding renewed absolute expiry", err)
	}
	// Published immutable metadata survives scheduler/data mutation after capture.
	e.data.Credential = "mutated"
	if b, err := held.Bind([]*public.DNSZone{dz}, ring); err != nil || b.Lookup(e.id) == nil {
		t.Fatal("held snapshot retained mutable acquired envelope", err)
	}
}

func TestSnapshotBindExplicitUnsignedAndEmptyProvider(t *testing.T) {
	keys, err := dnsserver.NewStaticKeyring()
	if err != nil {
		t.Fatal(err)
	}
	dz := managerZone()
	model, err := zone.BuildSecondary(dz, managerRecords(1), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	id := zone.SecondaryIdentity(dz)
	s := &Snapshot{zones: map[string]*zone.Zone{id: model}, credentials: map[string]string{id: "unsigned"}}
	bound, err := s.Bind([]*public.DNSZone{dz}, keys)
	if err != nil || bound.Lookup(id) == nil {
		t.Fatal("explicit unsigned copy with empty provider unavailable", err)
	}
	delete(s.credentials, id)
	if b, err := s.Bind([]*public.DNSZone{dz}, keys); err != nil || b.Lookup(id) != nil {
		t.Fatal("missing metadata inferred as unsigned", err)
	}
}
