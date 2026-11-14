// Package zonestatetest exercises the same committed-state concurrency and
// fault contract against real backend implementations, without mock storage.
package zonestatetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Now is the fixed test planning clock.
var Now = time.Unix(1_800_000_000, 0)

// Principal is the transport-verified identity used by the suite.
var Principal = dynamicupdate.Principal{Key: "key.", Version: "v1"}

// Fixture creates a complete primary aggregate.
func Fixture(t testing.TB, id zonekinds.ZoneID) *zonestate.Aggregate {
	t.Helper()
	a := &zonestate.Aggregate{FormatVersion: zonekinds.FormatVersion, Zone: id, Config: json.RawMessage(`{"zoneName":"example.","type":"primary"}`), Policy: zonestate.Policy{Enabled: true, AuthEpoch: 1, Credentials: []dynamicupdate.Credential{{Key: "key.", Version: "v1"}}, Grants: []dynamicupdate.Grant{{Key: "key.", Owner: "*.example."}}}}
	for _, text := range []string{"example. 300 IN SOA ns.example. hostmaster.example. 10 60 30 600 30", "example. 300 IN NS ns.example.", "ns.example. 300 IN A 192.0.2.1", "marker.example. 300 IN TXT \"old\""} {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		p := zonestate.Provenance{SourceID: "system"}
		if rr.Header().Name == "marker.example." {
			p = zonestate.Provenance{Key: Principal.Key, CredentialVersion: Principal.Version}
		}
		r, err := zonestate.FromRR(rr, p)
		if err != nil {
			t.Fatal(err)
		}
		a.Records = append(a.Records, r)
	}
	return a
}

// Initialize durably creates a verified initial generation.
func Initialize(t testing.TB, r *zonestate.Repository, id zonekinds.ZoneID) *zonestate.View {
	t.Helper()
	ctx := context.Background()
	v, err := r.Read(ctx, id)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("initial read: %v", err)
	}
	p, err := zonestate.Prepare(Fixture(t, id), nil, zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	v, err = r.Commit(ctx, v.Expected, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Message parses an RFC 2136 test request.
func Message(t testing.TB, prereq, updates []string) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetUpdate("example.")
	for _, text := range prereq {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		m.Answer = append(m.Answer, rr)
	}
	for _, text := range updates {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		m.Ns = append(m.Ns, rr)
	}
	return m
}

// Candidate plans and prepares against an exact committed head.
func Candidate(v *zonestate.View, m *dns.Msg) (*zonestate.Prepared, error) {
	state, err := v.UpdateState()
	if err != nil {
		return nil, err
	}
	tx, e := dynamicupdate.Parse(m, dynamicupdate.Limits{})
	if e != nil {
		return nil, e
	}
	result, e := dynamicupdate.Plan(state, tx, Principal, Now, dynamicupdate.Limits{})
	if e != nil {
		return nil, e
	}
	a, err := zonestate.ApplyResult(v.Aggregate, result, Principal)
	if err != nil {
		return nil, err
	}
	return zonestate.Prepare(a, v.Head, zonestate.Limits{})
}
func raceCommit(t testing.TB, r *zonestate.Repository, e zonestate.Expected, a, b *zonestate.Prepared) [2]error {
	t.Helper()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var results [2]error
	for i, p := range []*zonestate.Prepared{a, b} {
		wg.Go(func() { <-start; _, results[i] = r.Commit(context.Background(), e, p, nil) })
	}
	close(start)
	wg.Wait()
	return results
}
func assertOneWinner(t testing.TB, results [2]error) int {
	t.Helper()
	winner := -1
	for i, err := range results {
		if err == nil {
			if winner != -1 {
				t.Fatal("two stale candidates committed")
			}
			winner = i
		} else if !errors.Is(err, zonestate.ErrConflict) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if winner == -1 {
		t.Fatal("no winning commit")
	}
	return winner
}
func has(rrs []dns.RR, name string) bool {
	for _, r := range rrs {
		if r.Header().Name == name {
			return true
		}
	}
	return false
}

// Run exercises both dynamic-only and genuine desired API/file Store writers.
// The caller supplies the driver; Kubernetes callers use real envtest etcd.
func Run(t *testing.T, r *zonestate.Repository, b zonestate.Backend, s store.Store, id zonekinds.ZoneID) {
	zoneFor := func(name string) zonekinds.ZoneID { z := id; z.ObjectName += "-" + name; return z }
	t.Run("concurrent initialization create-only", func(t *testing.T) {
		id := zoneFor("init")
		v, err := r.Read(context.Background(), id)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		p, err := zonestate.Prepare(Fixture(t, id), nil, zonestate.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		assertOneWinner(t, raceCommit(t, r, v.Expected, p, p))
	})
	t.Run("prerequisite winner and fresh loser", func(t *testing.T) {
		id := zoneFor("prereq")
		v := Initialize(t, r, id)
		var plans [2]*zonestate.Prepared
		for i := range 2 {
			m := Message(t, []string{"marker.example. 0 IN TXT \"old\""}, []string{fmt.Sprintf("winner%d.example. 60 IN A 192.0.2.8", i)})
			m.Ns = append(m.Ns, &dns.RR_Header{Name: "marker.example.", Rrtype: dns.TypeTXT, Class: dns.ClassANY})
			rr, _ := dns.NewRR(fmt.Sprintf("marker.example. 60 IN TXT \"winner%d\"", i))
			m.Ns = append(m.Ns, rr)
			var err error
			plans[i], err = Candidate(v, m)
			if err != nil {
				t.Fatal(err)
			}
		}
		winner := assertOneWinner(t, raceCommit(t, r, v.Expected, plans[0], plans[1]))
		fresh, err := r.Read(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Candidate(fresh, Message(t, []string{"marker.example. 0 IN TXT \"old\""}, []string{"loser.example. 60 IN A 192.0.2.8"}))
		var pe *dynamicupdate.ProtocolError
		if !errors.As(err, &pe) || pe.Rcode != dns.RcodeNXRrset {
			t.Fatalf("stale prerequisite accepted: %v", err)
		}
		rrs, err := fresh.Aggregate.RRs()
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Head.Spec.CommitSequence != 2 || !has(rrs, fmt.Sprintf("winner%d.example.", winner)) || has(rrs, "loser.example.") {
			t.Fatal("partial/stale generation")
		}
	})
	t.Run("API file and UPDATE retain unrelated records", func(t *testing.T) {
		id := zoneFor("sources")
		v := Initialize(t, r, id)
		// Real desired-object writes precede projection, matching the approved
		// desired/committed boundary; their RVs remain pending until head CAS.
		projections := make([]zonestate.Projection, 2)
		for i, manager := range []string{store.ManagerAPI, "file:test-zone"} {
			name := fmt.Sprintf("source-%d-%s", i, id.ObjectName)
			owner := fmt.Sprintf("source%d.example.", i)
			desired := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "dns"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: id.ObjectName}, Endpoints: []public.Endpoint{{DNSName: owner, RecordType: "A", RecordTTL: 60, Targets: []string{"192.0.2.9"}}}}}
			saved, err := s.Apply(context.Background(), desired, store.ApplyOptions{Manager: manager})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			rr, _ := dns.NewRR(owner + " 60 IN A 192.0.2.9")
			projections[i] = zonestate.Projection{Source: zonestate.Source{ID: name, UID: string(saved.GetUID()), ResourceVersion: saved.GetResourceVersion(), Manager: manager, Desired: raw}, Records: []dns.RR{rr}}
		}
		dynamic := Message(t, nil, []string{"dynamic.example. 60 IN A 192.0.2.7"})
		p, err := Candidate(v, dynamic)
		if err != nil {
			t.Fatal(err)
		}
		a, err := zonestate.ReplaceSource(v.Aggregate, projections[0], Now, zonestate.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		sourcePlan, err := zonestate.Prepare(a, v.Head, zonestate.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		assertOneWinner(t, raceCommit(t, r, v.Expected, p, sourcePlan))
		// Retry each complete intent from a fresh root, never change an RV on
		// an already computed candidate. This retains whichever writer won.
		for _, projection := range projections {
			fresh, err := r.Read(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			a, err := zonestate.ReplaceSource(fresh.Aggregate, projection, Now, zonestate.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			p, err := zonestate.Prepare(a, fresh.Head, zonestate.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Commit(context.Background(), fresh.Expected, p, nil); err != nil {
				t.Fatal(err)
			}
		}
		fresh, err := r.Read(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		p, err = Candidate(fresh, dynamic)
		if err != nil {
			t.Fatal(err)
		}
		final, err := r.Commit(context.Background(), fresh.Expected, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		rrs, err := final.Aggregate.RRs()
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"source0.example.", "source1.example.", "dynamic.example."} {
			if !has(rrs, name) {
				t.Fatalf("lost %s", name)
			}
		}
		// A dynamic delete cannot hide a managed source RRset.
		deleteMsg := Message(t, nil, nil)
		deleteMsg.Ns = []dns.RR{&dns.RR_Header{Name: "source0.example.", Class: dns.ClassANY, Rrtype: dns.TypeANY}}
		if _, err := Candidate(final, deleteMsg); err == nil {
			t.Fatal("managed deletion accepted")
		}
		// Real file prune removes only its fragment, preserving API/dynamic.
		key := store.Key{Kind: "DNSRecordSet", Namespace: "dns", Name: projections[1].Source.ID}
		if err := s.Delete(context.Background(), key, store.DeleteOptions{Manager: "file:test-zone"}); err != nil {
			t.Fatal(err)
		}
		projections[1].Removed = true
		a, err = zonestate.ReplaceSource(final.Aggregate, projections[1], Now, zonestate.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		p, err = zonestate.Prepare(a, final.Head, zonestate.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		pruned, err := r.Commit(context.Background(), final.Expected, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		rrs, err = pruned.Aggregate.RRs()
		if err != nil {
			t.Fatal(err)
		}
		if has(rrs, "source1.example.") || !has(rrs, "source0.example.") || !has(rrs, "dynamic.example.") {
			t.Fatal("file prune lost unrelated ownership")
		}
	})
	t.Run("revocation conflicts and reauthorizes", func(t *testing.T) {
		id := zoneFor("revoke")
		v := Initialize(t, r, id)
		stale, err := Candidate(v, Message(t, nil, []string{"rejected.example. 60 IN A 192.0.2.8"}))
		if err != nil {
			t.Fatal(err)
		}
		a := *v.Aggregate
		a.Policy = v.Aggregate.Policy
		a.Policy.Enabled = false
		a.Policy.AuthEpoch++
		p, err := zonestate.Prepare(&a, v.Head, zonestate.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		revoked, err := r.Commit(context.Background(), v.Expected, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Commit(context.Background(), v.Expected, stale, nil); !errors.Is(err, zonestate.ErrConflict) {
			t.Fatal("stale authorized update committed")
		}
		_, err = Candidate(revoked, Message(t, nil, []string{"rejected.example. 60 IN A 192.0.2.8"}))
		var pe *dynamicupdate.ProtocolError
		if !errors.As(err, &pe) || pe.Rcode != dns.RcodeRefused {
			t.Fatal("current revoked policy ignored")
		}
		if revoked.Head.Spec.Serial != v.Head.Spec.Serial || revoked.Head.Spec.AuthEpoch != 2 {
			t.Fatal("policy serial/epoch wrong")
		}
	})
	t.Run("preparation and guard failures leave prior head", func(t *testing.T) {
		id := zoneFor("faults")
		v := Initialize(t, r, id)
		p, err := Candidate(v, Message(t, nil, []string{"uncommitted.example. 60 IN A 192.0.2.8"}))
		if err != nil {
			t.Fatal(err)
		}
		fault := errors.New("fault before CAS")
		wrapped := &failStage{Backend: b, remaining: 1, fault: fault}
		if _, err := zonestate.NewRepository(wrapped, zonestate.Limits{}).Commit(context.Background(), v.Expected, p, nil); !errors.Is(err, fault) {
			t.Fatal(err)
		}
		fresh, err := r.Read(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Head.Spec.RootDigest != v.Head.Spec.RootDigest {
			t.Fatal("staged partial root visible")
		}
		if _, err := r.Commit(context.Background(), v.Expected, p, func(context.Context, *zonekinds.ZoneState) error { return fault }); !errors.Is(err, fault) {
			t.Fatal(err)
		}
		fresh, err = r.Read(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Head.Spec.RootDigest != v.Head.Spec.RootDigest {
			t.Fatal("guard failure published")
		}
		committed, err := r.Commit(context.Background(), v.Expected, p, nil)
		if err != nil {
			t.Fatal(err)
		}
		cold, err := zonestate.NewRepository(b, zonestate.Limits{}).Read(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if cold.Head.Spec.RootDigest != committed.Head.Spec.RootDigest || cold.Head.Spec.Serial != committed.Head.Spec.Serial {
			t.Fatal("fresh reader lost durable commit")
		}
	})
}

type failStage struct {
	zonestate.Backend
	remaining int
	fault     error
}

func (b *failStage) Stage(ctx context.Context, c *zonekinds.ZoneChunk) error {
	if b.remaining == 0 {
		return b.fault
	}
	b.remaining--
	return b.Backend.Stage(ctx, c)
}
