package servingbuild

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// A committed UPDATE is acknowledged by the complete publisher through a
// later published head of the same zone incarnation only when that head's
// ledger carries the UPDATE's own receipt with its own commit sequence
// (D409); the exact wait still refuses the superseded member, and a head of
// another incarnation or head identity, a receipt the head does not carry
// or one from another commit never acknowledges.
func TestPrimaryPublicationIncludingAcceptsOnlyADescendantHead(t *testing.T) {
	ring, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "key.", Algorithm: dns.HmacSHA256, Secret: []byte("including-test-secret")})
	if err != nil {
		t.Fatal(err)
	}
	keyrings, err := NewKeyringOwner(ring)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, keyrings)
	guard, err := currentguard.New(currentguard.Options{Pin: keyrings.PrimaryPin(f.pub)})
	if err != nil {
		t.Fatal(err)
	}
	a, err := sqliteadapter.New(sqliteadapter.Options{Store: f.store, Background: guard.Background(), Credentials: guard.Credentials()})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := f.store.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	repo := zonestate.NewRepository(backend, zonestate.Limits{})
	publication, err := NewPrimaryPublication(f.pub, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	authority := f.store.Authority()
	issuer := &diagnosticIssuer{f: f, fence: pr.PublicationFence{Term: authority.Term, Incarnation: authority.Incarnation}}
	r, err := pr.New(pr.Options{Store: f.store, Repository: repo, Reader: primaryzone.NewReader(f.store, primaryzone.ReaderOptions{Policy: guard.CapturePolicy}), LifecycleCAS: a.LifecycleCAS, Admit: a.Admission, Publish: publication.Publish, PublishIncluding: publication.PublishIncluding, Proofs: issuer})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(pr.WithPublicationFence(t.Context(), issuer.fence), 30*time.Second)
	defer cancel()
	desired := &public.DNSZone{}
	if err := f.store.Get(ctx, store.Key{Kind: "DNSZone", Name: "uncommitted"}, desired); err != nil {
		t.Fatal(err)
	}
	desired.Spec.Policy.QueryAccess, desired.Spec.Policy.Update = public.AccessAllow, public.AccessAllow
	desired.Spec.Policy.UpdatePolicies = []public.UpdatePolicy{{TSIGKeyRef: "key.", Name: "*.uncommitted.example."}}
	if _, err := f.store.Apply(ctx, desired, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	desired, lifecycle := beginPrimary(ctx, t, f, r)
	issuer.id = lifecycle.Spec.Zone
	if _, err := r.Reconcile(ctx, issuer.id); err != nil {
		t.Fatal(err)
	}
	registry := diagnosticNative(t, f, issuer, r)
	update := func(i int) {
		t.Helper()
		msg := new(dns.Msg)
		msg.SetUpdate(desired.Spec.ZoneName)
		rr, err := dns.NewRR(fmt.Sprintf("dynamic%d.%s 60 IN A 192.0.2.%d", i, desired.Spec.ZoneName, i+1))
		if err != nil {
			t.Fatal(err)
		}
		msg.Insert([]dns.RR{rr})
		msg.SetTsig("key.", dns.HmacSHA256, 300, time.Now().Unix())
		client := &dns.Client{Net: "tcp", Timeout: 5 * time.Second, TsigProvider: dnsserver.TSIGProvider(ring)}
		response, _, err := client.ExchangeContext(ctx, msg, registry.Addr(dnsserver.ProtoTCP).String())
		if err != nil || response == nil || response.Rcode != dns.RcodeSuccess {
			t.Fatalf("UPDATE %d: %v %v", i, response, err)
		}
	}
	update(0)
	first, ok := r.Capture(f.now).Member(desired.Spec.ZoneName)
	if !ok || !first.Published {
		t.Fatal("first UPDATE not published", first)
	}
	v, err := repo.Read(ctx, lifecycle.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Aggregate.Receipts) != 1 || v.Aggregate.Receipts[0].CommitSequence != first.Version.CommitSequence {
		t.Fatal("first UPDATE's receipt", v.Aggregate.Receipts)
	}
	receipt := v.Aggregate.Receipts[0]
	update(1)
	writer, _ := pr.PublicationFenceFrom(ctx)
	if err := publication.Wait(ctx, writer, first); !errors.Is(err, serving.ErrSuperseded) {
		t.Fatal("exact wait acknowledged a superseded member", err)
	}
	if err := publication.WaitIncluding(ctx, writer, first, receipt.ID, receipt.CommitSequence); err != nil {
		t.Fatal("a later head that carries the receipt did not acknowledge the UPDATE", err)
	}
	if err := publication.PublishIncluding(ctx, first, receipt.ID, receipt.CommitSequence); err != nil {
		t.Fatal("publication hook", err)
	}
	if err := publication.PublishIncluding(t.Context(), first, receipt.ID, receipt.CommitSequence); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("hook without a writer fence", err)
	}
	for name, alter := range map[string]func(*pr.Member, *string, *uint64){
		"absent receipt":      func(_ *pr.Member, id *string, _ *uint64) { *id = strings.Repeat("0", 64) },
		"other commit":        func(_ *pr.Member, _ *string, c *uint64) { *c-- },
		"other incarnation":   func(m *pr.Member, _ *string, _ *uint64) { m.Incarnation++ },
		"other head identity": func(m *pr.Member, _ *string, _ *uint64) { m.Version.Zone.UID = "other" },
		"newer than served":   func(m *pr.Member, _ *string, _ *uint64) { m.Version.CommitSequence += 10 },
	} {
		m, id, committed := first, receipt.ID, receipt.CommitSequence
		alter(&m, &id, &committed)
		if err := publication.WaitIncluding(ctx, writer, m, id, committed); !errors.Is(err, serving.ErrSuperseded) {
			t.Fatalf("%s acknowledged: %v", name, err)
		}
	}
	writer.Incarnation++
	if err := publication.WaitIncluding(ctx, writer, first, receipt.ID, receipt.CommitSequence); !errors.Is(err, serving.ErrSuperseded) {
		t.Fatal("other writer incarnation acknowledged", err)
	}
}
