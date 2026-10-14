package rpz

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/secondary"
)

func snapshotRing(t *testing.T, algorithm string, secret []byte) *dnsserver.StaticKeyring {
	t.Helper()
	k, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "rpz-key.", Algorithm: algorithm, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSnapshotBindNativeAcquisitionRotationAndRestore(t *testing.T) {
	ring := snapshotRing(t, dns.HmacSHA256, []byte("native-rpz-test-secret"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan error, 1)
	server := &dns.Server{Listener: listener, TsigProvider: dnsserver.TSIGProvider(ring), NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		if q.IsTsig() == nil || w.TsigStatus() != nil {
			return
		}
		records := runtimeRecords(1)
		if q.Question[0].Qtype == dns.TypeSOA {
			records = records[:1]
		} else {
			records = append(records, dns.Copy(records[0]))
		}
		msg := new(dns.Msg)
		msg.SetReply(q)
		msg.Authoritative, msg.Answer = true, records
		msg.SetTsig(q.IsTsig().Hdr.Name, q.IsTsig().Algorithm, 300, time.Now().Unix())
		if err := w.WriteMsg(msg); err != nil {
			t.Error(err)
		}
	})}
	go func() { done <- server.ActivateAndServe() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("native RPZ fixture did not start")
	}
	t.Cleanup(func() {
		_ = server.Shutdown()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	list := runtimeList()
	list.Spec.Sources[0].RPZ.TSIGKeyRef = "rpz-key."
	list.Spec.Sources[0].RPZ.Primaries = []string{listener.Addr().String()}
	spec, config, err := sourceConfig(list, 0)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, _ := boundCredential("rpz-key.", ring)
	m := &Manager{opts: Options{ClusterID: "cluster", Blobs: runtimeDB(t), Receiver: &secondary.TransferClient{Keyring: ring}, Keyring: ring, Now: time.Now, Timeout: time.Second}}
	r := m.refresh(t.Context(), transferJob{spec: spec, dz: config, credential: fingerprint})
	if r.err != nil || r.data == nil {
		t.Fatal("native acquired feed", r.err)
	}
	if err := m.persist(t.Context(), spec.Identity, r.data); err != nil {
		t.Fatal(err)
	}
	if err := m.accept(t.Context(), spec.Identity, r.data); err != nil {
		t.Fatal(err)
	}
	rotated := snapshotRing(t, dns.HmacSHA256, []byte("rotated-rpz-test-secret"))
	newFingerprint, _ := boundCredential("rpz-key.", rotated)
	e := &entry{spec: spec, dz: config, data: r.data, z: r.z, credential: newFingerprint}
	m.publish(map[string]*entry{spec.Identity: e})
	s := m.Snapshot()
	bound, err := s.Bind([]*public.BlockList{list}, ring)
	if err != nil || bound.Select([]*public.BlockList{list}, true).Evaluate(Context{Name: "bad.test."}, time.Now()).Action != ActionNXDOMAIN {
		t.Fatal("acquired native credential rejected", err)
	}
	for _, keys := range []*dnsserver.StaticKeyring{rotated, snapshotRing(t, dns.HmacSHA512, []byte("native-rpz-test-secret"))} {
		b, err := s.Bind([]*public.BlockList{list}, keys)
		if err != nil || !b.Select([]*public.BlockList{list}, true).Evaluate(Context{Name: "bad.test."}, time.Now()).Unavailable {
			t.Fatal("rotated feed omitted or relabeled instead of required failure", err)
		}
	}
	if _, err := s.Bind([]*public.BlockList{list}, nil); err == nil {
		t.Fatal("nil provider accepted")
	}
	missing := &Snapshot{zones: s.zones}
	if b, err := missing.Bind([]*public.BlockList{list}, ring); err != nil || !b.Select([]*public.BlockList{list}, true).Evaluate(Context{Name: "bad.test."}, time.Now()).Unavailable {
		t.Fatal("missing metadata accepted", err)
	}
	if !bound.Select([]*public.BlockList{list}, true).Evaluate(Context{Name: "bad.test."}, r.data.ExpiresAt).Unavailable {
		t.Fatal("binding renewed exact expiry")
	}
	restore := &entry{spec: spec, dz: config, credential: fingerprint}
	restore.data, restore.z, err = m.restore(t.Context(), restore)
	if err != nil {
		t.Fatal(err)
	}
	m.publish(map[string]*entry{spec.Identity: restore})
	if b, err := m.Snapshot().Bind([]*public.BlockList{list}, rotated); err != nil || !b.Select([]*public.BlockList{list}, true).Evaluate(Context{Name: "bad.test."}, time.Now()).Unavailable {
		t.Fatal("restored A accepted by current B", err)
	}
	e.data.Credential = "mutated"
	if b, err := s.Bind([]*public.BlockList{list}, ring); err != nil || b.Select([]*public.BlockList{list}, true).Evaluate(Context{Name: "bad.test."}, time.Now()).Unavailable {
		t.Fatal("held published metadata changed", err)
	}
}
