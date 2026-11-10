package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func integrationStored(t *testing.T, s *servingRuntime, d *public.Integration) *public.Integration {
	t.Helper()
	var current public.Integration
	err := s.opts.Store.Get(t.Context(), store.KeyOf(d), &current)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &current
}

func integrationRows(t *testing.T, s *servingRuntime, kind store.Kind) []store.Object {
	t.Helper()
	rows, err := s.opts.Store.List(t.Context(), kind, store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return rows.Items
}

// integrationRequestDelete is the delete request of every SQLite path: the
// store keeps an object that has finalizers and stamps its deletion time.
func integrationRequestDelete(t *testing.T, s *servingRuntime, d *public.Integration) {
	t.Helper()
	if err := s.opts.Store.Delete(t.Context(), store.KeyOf(d), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
}

func integrationAudit(t *testing.T, s *servingRuntime, d *public.Integration) []sqlite.AuditEntry {
	t.Helper()
	entries, err := s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Integration", Namespace: d.Namespace, Name: d.Name, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func integrationTerminatingLog(t *testing.T, s *servingRuntime, peer *integrationCloudflarePeer) {
	t.Helper()
	if !t.Failed() {
		return
	}
	for _, status := range s.integrations.host.Statuses() {
		t.Log("integration", status.Phase, status.Reason, fmt.Sprintf("%+v", status.Result))
	}
	for _, kind := range []store.Kind{kinds.KindIntegrationBudget, kinds.KindIntegrationLedger, kinds.KindIntegrationOperation, kinds.KindIntegrationSourceHead, kinds.KindIntegrationNameAllocation} {
		rows, err := s.opts.Store.List(context.Background(), kind, store.ListOptions{})
		t.Log("private rows", kind, len(rows.Items), err != nil)
	}
	if peer != nil {
		peer.mu.Lock()
		t.Log("peer requests", peer.requests, "writes", peer.writes, "rows", len(peer.rows), "deleted", peer.deleted)
		peer.mu.Unlock()
	}
}

// integrationDeleteFixture is a published Cloudflare instance with
// deletionPolicy Delete on the mounted SQLite runtime: the real writer,
// publisher, host, ledger and budget, and the registered Cloudflare adapter
// and SDK against the verified-TLS peer of its canonical origin.
type integrationDeleteFixture struct {
	peer      *integrationCloudflarePeer
	s         *servingRuntime
	path      string
	template  *public.Integration
	d         *public.Integration
	owner     kinds.Owner
	record    *public.DNSRecordSet
	published []string
}

func newIntegrationDeleteFixture(t *testing.T, edit func(*public.Integration)) *integrationDeleteFixture {
	t.Helper()
	f := &integrationDeleteFixture{peer: integrationCloudflareTLS(t)}
	f.s, f.path = integrationFixture(t, nil, nil)
	t.Cleanup(func() { integrationTerminatingLog(t, f.s, f.peer) })
	f.record = &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "published", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80", "192.0.2.81"}}}}}
	if _, err := f.s.opts.Store.Apply(t.Context(), f.record, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	// The zone is upsert-only: an ordinary sync never deletes there. Delete
	// overrides that for what the instance provably owns.
	definition := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "publisher", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "cloudflare", DeletionPolicy: public.IntegrationDeletionDelete, Resync: metav1.Duration{Duration: time.Hour}, Credentials: map[string]public.CredentialReference{"apiToken": {File: f.path}}, Cloudflare: &public.CloudflareIntegration{Zones: []public.CloudflareZone{{Zone: "example", ZoneID: integrationCloudflareZone, Policy: "upsert-only"}}}}}
	if edit != nil {
		edit(definition)
	}
	f.template = definition.DeepCopy()
	f.d = integrationApply(t, f.s, definition)
	if !store.HasFinalizer(f.d, store.IntegrationCleanupFinalizer) {
		t.Fatal("the Delete instance was stored without its cleanup finalizer")
	}
	f.owner = integrationOwner(f.s, f.d)
	integrationRun(t, f.s)
	awaitServingRuntime(t, f.s, func() bool {
		rows := integrationRows(t, f.s, kinds.KindIntegrationLedger)
		if len(rows) != 1 || rows[0].(*kinds.IntegrationLedger).Spec.Pending != nil || len(rows[0].(*kinds.IntegrationLedger).Spec.ProviderIDs) != 2 {
			return false
		}
		return f.phase() == "Ready"
	})
	f.peer.mu.Lock()
	for id := range f.peer.rows {
		f.published = append(f.published, id)
	}
	f.peer.mu.Unlock()
	slices.Sort(f.published)
	if len(f.published) != 2 {
		t.Fatal("the instance did not publish its record set", f.published)
	}
	return f
}

func (f *integrationDeleteFixture) status() (string, string) {
	for _, status := range f.s.integrations.host.Statuses() {
		if status.Owner == f.owner {
			return status.Phase, status.Reason
		}
	}
	return "", ""
}

func (f *integrationDeleteFixture) phase() string { phase, _ := f.status(); return phase }

func (f *integrationDeleteFixture) awaitStatus(t *testing.T, phase, reason string) {
	t.Helper()
	awaitServingRuntime(t, f.s, func() bool { p, r := f.status(); return p == phase && r == reason })
}

// traffic is what reached the peer: every request, every batch, and the IDs
// that delete effects named.
func (f *integrationDeleteFixture) traffic() (requests, writes int, deleted []string) {
	f.peer.mu.Lock()
	defer f.peer.mu.Unlock()
	return f.peer.requests, f.peer.writes, slices.Clone(f.peer.deleted)
}

// seed adds a row of another owner to the peer's zone.
func (f *integrationDeleteFixture) seed(id, name, typ, content, comment string) {
	row := map[string]any{"id": id, "name": name, "type": typ, "content": content, "ttl": 300, "comment": comment}
	if typ != "TXT" {
		row["proxied"] = false
	}
	f.peer.mu.Lock()
	f.peer.rows[id] = row
	f.peer.mu.Unlock()
}

func (f *integrationDeleteFixture) requestDelete(t *testing.T) {
	t.Helper()
	integrationRequestDelete(t, f.s, f.d)
	f.s.integrations.host.Wake()
}

func (f *integrationDeleteFixture) awaitGone(t *testing.T) {
	t.Helper()
	awaitServingRuntime(t, f.s, func() bool { return integrationStored(t, f.s, f.d) == nil })
}

func integrationForeignID(n int) string { return strings.Repeat("a", 31) + fmt.Sprint(n) }

// TestIntegrationDeleteCloudflareCleanupRemovesOnlyOwnedRecordsAndFinalizes is
// the mounted Delete lifecycle (D367): a delete request leaves a terminating
// object, the elected cleanup deletes exactly the record set the instance
// provably owns, forgets its ledger, and finalizes with an audit summary.
func TestIntegrationDeleteCloudflareCleanupRemovesOnlyOwnedRecordsAndFinalizes(t *testing.T) {
	f := newIntegrationDeleteFixture(t, nil)
	peer, s, d, owner, path, record, published := f.peer, f.s, f.d, f.owner, f.path, f.record, f.published
	// Rows of other owners in the same zone: a previous incarnation's marker,
	// a hand-made record, and an external-dns record with its heritage TXT.
	foreignID := integrationForeignID
	seeded := map[string]map[string]any{
		foreignID(1): {"name": "previous.example", "type": "A", "content": "198.51.100.1", "ttl": 300, "proxied": false, "comment": "kudurru:" + kinds.Digest([]byte("another incarnation"))},
		foreignID(2): {"name": "manual.example", "type": "A", "content": "198.51.100.2", "ttl": 300, "proxied": false, "comment": ""},
		foreignID(3): {"name": "legacy.example", "type": "A", "content": "198.51.100.3", "ttl": 300, "proxied": false, "comment": ""},
		foreignID(4): {"name": "legacy.example", "type": "TXT", "content": `"heritage=external-dns,external-dns/owner=legacy"`, "ttl": 300, "comment": ""},
	}
	peer.mu.Lock()
	for id, row := range seeded {
		row["id"] = id
		peer.rows[id] = row
	}
	before, _ := json.Marshal(seeded)
	writes := peer.writes
	// Hold the peer: the delete request must leave a visible terminating
	// object, with everything cleanup needs, before any provider answer.
	integrationRequestDelete(t, s, d)
	s.integrations.host.Wake()
	held := integrationStored(t, s, d)
	if held == nil || held.DeletionTimestamp == nil || !store.HasFinalizer(held, store.IntegrationCleanupFinalizer) || held.UID != d.UID || held.Spec.Credentials["apiToken"].File != path {
		peer.mu.Unlock()
		t.Fatal("the delete request did not leave a terminating object with its spec and credential reference")
	}
	awaitTerminating := func() bool {
		for _, status := range s.integrations.host.Statuses() {
			if status.Owner == owner && status.Phase == ih.PhaseTerminating {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(15 * time.Second); !awaitTerminating(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			peer.mu.Unlock()
			t.Fatal("the host did not report the instance as terminating")
		}
	}
	// Operator commands are refused for a terminating instance.
	if _, err := s.integrations.host.Sync(t.Context(), owner, ""); err == nil {
		peer.mu.Unlock()
		t.Fatal("a terminating instance accepted a sync")
	}
	peer.mu.Unlock()

	awaitServingRuntime(t, s, func() bool { return integrationStored(t, s, d) == nil })
	peer.mu.Lock()
	after, _ := json.Marshal(func() map[string]map[string]any {
		out := map[string]map[string]any{}
		for id, row := range peer.rows {
			if _, ok := seeded[id]; ok {
				out[id] = row
			}
		}
		return out
	}())
	remaining, deleted, batches := len(peer.rows), slices.Clone(peer.deleted), peer.writes-writes
	peer.mu.Unlock()
	slices.Sort(deleted)
	if !slices.Equal(published, deleted) || batches != 1 {
		t.Fatalf("delete effects named %v in %d batches, published were %v", deleted, batches, published)
	}
	if remaining != len(seeded) || string(before) != string(after) {
		t.Fatal("cleanup changed rows the instance does not own")
	}
	for _, kind := range []store.Kind{kinds.KindIntegrationLedger, kinds.KindIntegrationOperation} {
		for _, row := range integrationRows(t, s, kind) {
			t.Errorf("%s row %s remains for a finalized instance", kind, row.GetName())
		}
	}
	// The shared request budget outlives every instance.
	if rows := integrationRows(t, s, kinds.KindIntegrationBudget); len(rows) != 1 {
		t.Fatal("the shared request budget was removed or never charged", len(rows))
	}
	// The desired record the instance published from is the operator's.
	var source public.DNSRecordSet
	if err := s.opts.Store.Get(t.Context(), store.KeyOf(record), &source); err != nil || source.DeletionTimestamp != nil {
		t.Fatal("cleanup touched the operator's record set", err != nil)
	}
	entries := integrationAudit(t, s, d)
	var actions []string
	summary := ""
	for _, entry := range entries {
		actions = append(actions, entry.Action)
		if entry.Action == "integration.cleanup" {
			summary = entry.Reason
		}
	}
	if n := len(actions); n < 2 || !slices.Equal(actions[n-2:], []string{"integration.finalize", "integration.cleanup"}) && !slices.Equal(actions[n-2:], []string{"integration.cleanup", "integration.finalize"}) {
		t.Fatalf("audit trail does not end with the finalization and its summary: %v", actions)
	}
	if !strings.Contains(summary, "deleted 1 provider record sets") || !strings.Contains(summary, "forgot 1 ledger rows and 2 operations") || !strings.Contains(summary, string(d.UID)) {
		t.Fatalf("cleanup summary: %q", summary)
	}
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range image.Payload.Objects {
		if strings.Contains(string(raw), "test-only-private-credential") {
			t.Fatal("credential persisted in object image")
		}
	}
	// A recreated Integration of the same name is a new incarnation.
	next := integrationApply(t, s, f.template)
	if next.UID == d.UID || next.DeletionTimestamp != nil || !store.HasFinalizer(next, store.IntegrationCleanupFinalizer) {
		t.Fatal("the name was not free for a new armed incarnation")
	}
}

// TestIntegrationDeleteUniFiPurgesLocalStateAndFinalizes: an inbound instance
// owns nothing at its provider, so its cleanup makes no provider call. One
// command removes its source-owned record sets, source head and chunks and
// name allocations and withdraws its inventory clients; the operator's own
// group members and records stay.
func TestIntegrationDeleteUniFiPurgesLocalStateAndFinalizes(t *testing.T) {
	server, calls, _ := integrationController(t)
	s, path := integrationFixture(t, nil, nil)
	defer integrationTerminatingLog(t, s, nil)
	group := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "devices", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{MAC: "00:00:00:00:00:09"}}, Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: "inventory"}}}}
	manual := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "printer.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.50"}}}}}
	for _, obj := range []store.Object{group, manual} {
		if _, err := s.opts.Store.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	definition := integrationPinnedDefinition(t, server, path)
	definition.Spec.DeletionPolicy = public.IntegrationDeletionDelete
	d := integrationApply(t, s, definition)
	owner := integrationOwner(s, d)
	integrationRun(t, s)
	address := netip.MustParseAddr("192.0.2.19")
	awaitServingRuntime(t, s, func() bool {
		snapshot := integrationInventory(t, s)
		return snapshot != nil && snapshot.Lookup(address, time.Now()) != nil
	})
	answers := func(name string) bool {
		lease, err := s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("zones")
		model := value.(*servingzones.Prepared).Zones(time.Now()).Match(name)
		return model != nil && model.Lookup(name, dns.TypeA).Kind == zone.Success
	}
	awaitServingRuntime(t, s, func() bool { return answers("laptop.example.") && answers("printer.example.") })
	owned := func() []store.Object {
		var out []store.Object
		for _, kind := range []store.Kind{"DNSRecordSet", "DNSZone"} {
			for _, obj := range integrationRows(t, s, kind) {
				if sourceOwned(obj, owner) {
					out = append(out, obj)
				}
			}
		}
		return out
	}
	if len(owned()) != 1 || len(integrationRows(t, s, kinds.KindIntegrationSourceHead)) != 1 || len(integrationRows(t, s, kinds.KindIntegrationSourceChunk)) == 0 || len(integrationRows(t, s, kinds.KindIntegrationNameAllocation)) != 1 {
		t.Fatal("the instance did not commit the local state this test purges")
	}
	integrationReadyCommand(t, s, owner)
	polled := calls.Load()

	integrationRequestDelete(t, s, d)
	s.integrations.host.Wake()
	awaitServingRuntime(t, s, func() bool { return integrationStored(t, s, d) == nil })

	if calls.Load() != polled {
		t.Fatal("the cleanup of an inbound instance called its provider", calls.Load()-polled)
	}
	if rows := owned(); len(rows) != 0 {
		t.Fatal("source-owned objects remain", len(rows))
	}
	for _, kind := range []store.Kind{kinds.KindIntegrationSourceHead, kinds.KindIntegrationSourceChunk, kinds.KindIntegrationNameAllocation, kinds.KindIntegrationDelivery, kinds.KindIntegrationLedger, kinds.KindIntegrationOperation} {
		if rows := integrationRows(t, s, kind); len(rows) != 0 {
			t.Errorf("%d %s rows remain for a finalized instance", len(rows), kind)
		}
	}
	// The published inventory has no client of the purged source. (A capture
	// can be refused while the publisher catches up with the final commit.)
	awaitServingRuntime(t, s, func() bool {
		snapshot := integrationInventory(t, s)
		if snapshot == nil || snapshot.Lookup(address, time.Now()) != nil {
			return false
		}
		for _, client := range snapshot.Clients() {
			if client.Source == ih.SourceID(owner) {
				return false
			}
		}
		return true
	})
	awaitServingRuntime(t, s, func() bool { return !answers("laptop.example.") })
	if !answers("printer.example.") {
		t.Fatal("the purge removed an operator's record")
	}
	var kept public.ClientGroup
	if err := s.opts.Store.Get(t.Context(), store.KeyOf(group), &kept); err != nil || !reflect.DeepEqual(kept.Spec.Clients, []public.ClientMatcher{{MAC: "00:00:00:00:00:09"}}) {
		t.Fatal("the purge changed the operator's group members", err != nil)
	}
	var operator public.DNSRecordSet
	if err := s.opts.Store.Get(t.Context(), store.KeyOf(manual), &operator); err != nil || store.ManagedBy(&operator) != store.ManagerAPI {
		t.Fatal("the purge touched an operator's record set", err != nil)
	}
	// One command: the purge's deletes and its summary row are one unbroken
	// run of the audit log, which the single writer appends in commit order.
	all, err := s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var block []int64
	purged := ""
	for _, entry := range all {
		private := slices.Contains([]store.Kind{kinds.KindIntegrationSourceHead, kinds.KindIntegrationSourceChunk, kinds.KindIntegrationNameAllocation}, entry.Key.Kind)
		record := entry.Key.Kind == "DNSRecordSet" && entry.Manager == integrationManager(owner)
		switch {
		case (private || record) && strings.HasSuffix(entry.Action, ".delete") && entry.Manager == integrationManager(owner):
			block = append(block, entry.ID)
		case entry.Action == "integration.purge":
			block = append(block, entry.ID)
			purged = entry.Reason
		}
	}
	slices.Sort(block)
	if len(block) < 5 || block[len(block)-1]-block[0] != int64(len(block)-1) {
		t.Fatalf("the purge is not one unbroken commit in the audit log: %v", block)
	}
	if !strings.Contains(purged, "removed 1 source-owned objects") || !strings.Contains(purged, "1 name allocations") || !strings.Contains(purged, "withdrew 1 inventory clients") {
		t.Fatalf("purge summary: %q", purged)
	}
	entries := integrationAudit(t, s, d)
	summary := ""
	for _, entry := range entries {
		if entry.Action == "integration.cleanup" {
			summary = entry.Reason
		}
	}
	if !strings.Contains(summary, "nothing of uid "+string(d.UID)+" remains") || !strings.Contains(summary, "purged 1 source-owned objects") {
		t.Fatalf("cleanup summary: %q", summary)
	}
}

// TestIntegrationDeleteHeldSourceObjectKeepsTheInstanceTerminating: a record
// set of the instance's source that carries someone else's finalizer cannot
// be removed by the purge. The Integration stays terminating with a specific
// reason, and finishes after that finalizer is gone and the object is
// declared again.
func TestIntegrationDeleteHeldSourceObjectKeepsTheInstanceTerminating(t *testing.T) {
	server, _, _ := integrationController(t)
	s, path := integrationFixture(t, nil, nil)
	defer integrationTerminatingLog(t, s, nil)
	group := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "devices", Namespace: "default"}, Spec: public.ClientGroupSpec{Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: "inventory"}}}}
	if _, err := s.opts.Store.Apply(t.Context(), group, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	definition := integrationPinnedDefinition(t, server, path)
	definition.Spec.DeletionPolicy = public.IntegrationDeletionDelete
	d := integrationApply(t, s, definition)
	owner := integrationOwner(s, d)
	integrationRun(t, s)
	generated := func() *public.DNSRecordSet {
		for _, obj := range integrationRows(t, s, "DNSRecordSet") {
			if sourceOwned(obj, owner) {
				return obj.(*public.DNSRecordSet)
			}
		}
		return nil
	}
	awaitServingRuntime(t, s, func() bool { return generated() != nil })
	integrationReadyCommand(t, s, owner)
	record := generated()
	const hold = "example.com/hold"
	if _, err := s.opts.Store.UpdateFinalizer(t.Context(), store.KeyOf(record), store.FinalizerOptions{Finalizer: hold, ExpectedUID: string(record.UID), ExpectedResourceVersion: record.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	integrationRequestDelete(t, s, d)
	s.integrations.host.Wake()
	awaitServingRuntime(t, s, func() bool {
		for _, status := range s.integrations.host.Statuses() {
			if status.Owner == owner && status.Phase == ih.PhaseTerminating && status.Reason == ih.ReasonLocalStateRemains {
				return true
			}
		}
		return false
	})
	// The durable status says the same; after it no write of the job is due.
	var held *public.Integration
	awaitServingRuntime(t, s, func() bool {
		held = integrationStored(t, s, d)
		if held == nil || !store.HasFinalizer(held, store.IntegrationCleanupFinalizer) {
			t.Fatal("the instance was finalized over a source object that still exists")
		}
		for _, condition := range held.Status.Conditions {
			if condition.Type == "Ready" && condition.Reason == ih.ReasonLocalStateRemains {
				return true
			}
		}
		return false
	})
	record = generated()
	if record == nil || record.DeletionTimestamp == nil || !store.HasFinalizer(record, hold) {
		t.Fatal("the purge did not request the deletion of the held record set, or removed it despite its finalizer")
	}
	// The rest of the source went in the same commit; the image stays valid.
	for _, kind := range []store.Kind{kinds.KindIntegrationSourceHead, kinds.KindIntegrationSourceChunk, kinds.KindIntegrationNameAllocation} {
		if rows := integrationRows(t, s, kind); len(rows) != 0 {
			t.Errorf("%d %s rows remain after the purge", len(rows), kind)
		}
	}
	if _, err := s.opts.Store.UpdateFinalizer(t.Context(), store.KeyOf(record), store.FinalizerOptions{Finalizer: hold, Remove: true, ExpectedUID: string(record.UID), ExpectedResourceVersion: record.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	if generated() != nil {
		t.Fatal("removing the last finalizer did not remove the terminating record set")
	}
	// Declaring the terminating object again, still with Delete, re-runs its
	// cleanup at once.
	held = integrationStored(t, s, d)
	held.Spec.Resync = metav1.Duration{Duration: 30 * time.Minute}
	if again := integrationApply(t, s, held); again.DeletionTimestamp == nil || !store.HasFinalizer(again, store.IntegrationCleanupFinalizer) {
		t.Fatal("a re-declared Delete object left the terminating state")
	}
	awaitServingRuntime(t, s, func() bool { return integrationStored(t, s, d) == nil })
}

// TestIntegrationDeleteReleaseNeedsNoLicenseCredentialOrProvider: a
// terminating object that still carries the cleanup finalizer although its
// policy is not Delete is released by the elected host in one command. No
// provider is built, nothing is cleaned, and neither a license nor the
// credential is needed, so an expired license or a lost credential cannot
// hold such an object.
func TestIntegrationDeleteReleaseNeedsNoLicenseCredentialOrProvider(t *testing.T) {
	var factories atomic.Int32
	s, path := integrationFixture(t, func(o *servingRuntimeOptions) {
		var err error
		if o.License, err = license.New(license.Options{}); err != nil {
			t.Fatal(err)
		}
		// The credential reference no longer resolves.
		o.Config.Credentials.AllowedFiles = nil
	}, func(string) (integration.Integration, error) {
		factories.Add(1)
		return &integrationProbe{observe: func(context.Context, ih.OwnerHost) (ih.SourceSnapshot, error) {
			return ih.SourceSnapshot{Complete: true, Clients: []inventory.Client{}}, nil
		}}, nil
	})
	defer integrationTerminatingLog(t, s, nil)
	d := integrationApply(t, s, integrationDefinition("released", path))
	if len(d.Finalizers) != 0 {
		t.Fatal("a Retain instance was armed")
	}
	// SQLite admission never stores this state through an apply; a metadata
	// write does, as a host on a store without admission would leave it.
	out, err := s.opts.Store.UpdateFinalizer(t.Context(), store.KeyOf(d), store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, ExpectedUID: string(d.UID), ExpectedResourceVersion: d.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	d = out.(*public.Integration)
	owner := integrationOwner(s, d)
	// What a retained instance leaves behind stays behind.
	identity := kinds.RecordIdentity{Owner: owner, Destination: kinds.Destination{Provider: "cloudflare", Account: "account", Zone: "zone"}, Key: kinds.RecordKey{Name: "kept.example.", Type: "A"}}
	ledger := &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(identity)}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: kinds.FormatVersion, Record: identity, Sequence: 1, ContentHash: kinds.Digest([]byte("content")), ProviderIDs: []string{"provider-id"}, LastOperationID: kinds.Digest([]byte("operation")), LastApplied: time.Now().UTC().Truncate(time.Second)}}
	if _, err := s.opts.Store.Apply(t.Context(), ledger, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	integrationRequestDelete(t, s, d)
	if held := integrationStored(t, s, d); held == nil || held.DeletionTimestamp == nil || held.Spec.DeletionPolicy == public.IntegrationDeletionDelete {
		t.Fatal("fixture is not a terminating Retain object with the cleanup finalizer")
	}
	integrationRun(t, s)
	awaitServingRuntime(t, s, func() bool { return integrationStored(t, s, d) == nil })
	if factories.Load() != 0 {
		t.Fatal("the release built a provider")
	}
	if rows := integrationRows(t, s, kinds.KindIntegrationLedger); len(rows) != 1 || rows[0].GetName() != ledger.Name {
		t.Fatal("the release removed publication state")
	}
	if rows := integrationRows(t, s, kinds.KindIntegrationBudget); len(rows) != 0 {
		t.Fatal("the release charged a request budget")
	}
	released := ""
	for _, entry := range integrationAudit(t, s, d) {
		if entry.Action == "integration.cleanup" {
			t.Fatal("a release was recorded as a finished cleanup")
		}
		if entry.Action == "integration.release" {
			released = entry.Reason
		}
	}
	if !strings.HasPrefix(released, "released without cleanup: deletionPolicy is Retain; removed "+store.IntegrationCleanupFinalizer) || !strings.Contains(released, "1 ledger rows") {
		t.Fatalf("release audit reason: %q", released)
	}
}

// TestIntegrationDeleteBlockedCleanupReportsAndNeverFinalizes: a record of
// another owner at the name of an owned record set blocks finalization. The
// object stays, its durable status says why with the remaining counts, and
// nothing at that name is deleted. The cleanup then finishes when the
// conflict is gone, or the operator releases the object by declaring Retain,
// which contacts no provider and keeps the ledger.
func TestIntegrationDeleteBlockedCleanupReportsAndNeverFinalizes(t *testing.T) {
	for _, outcome := range []string{"resolved", "released"} {
		t.Run(outcome, func(t *testing.T) {
			// A blocked cleanup is re-evaluated at the resync interval.
			f := newIntegrationDeleteFixture(t, func(d *public.Integration) { d.Spec.Resync = metav1.Duration{Duration: time.Second} })
			f.seed(integrationForeignID(5), "web.example", "TXT", `"operator note"`, "")
			f.requestDelete(t)
			f.awaitStatus(t, ih.PhaseTerminating, ih.ReasonRecordsConflict)
			var held *public.Integration
			awaitServingRuntime(t, f.s, func() bool {
				held = integrationStored(t, f.s, f.d)
				if held == nil {
					return false
				}
				for _, condition := range held.Status.Conditions {
					if condition.Type == "Ready" && condition.Status == metav1.ConditionFalse && condition.Reason == ih.ReasonRecordsConflict {
						return true
					}
				}
				return false
			})
			if held.DeletionTimestamp == nil || !store.HasFinalizer(held, store.IntegrationCleanupFinalizer) || held.Status.Records.Managed != 1 || held.Status.Records.Conflicts != 1 || held.Status.Records.Drift != 0 {
				t.Fatalf("blocked object: finalizers=%v records=%+v", held.Finalizers, held.Status.Records)
			}
			// The status write was a new resource version; the host's report
			// follows it, so a reader that matches by revision sees the reason.
			awaitServingRuntime(t, f.s, func() bool {
				current := integrationStored(t, f.s, f.d)
				for _, status := range f.s.integrations.host.Statuses() {
					if status.Owner == f.owner {
						return current != nil && status.Revision == current.ResourceVersion && status.Reason == ih.ReasonRecordsConflict && status.Result.Remaining == 1 && status.Result.Conflicts == 1
					}
				}
				return false
			})
			held = integrationStored(t, f.s, f.d)
			_, writes, deleted := f.traffic()
			if len(deleted) != 0 || len(integrationRows(t, f.s, kinds.KindIntegrationLedger)) != 1 {
				t.Fatal("a blocked cleanup deleted or forgot a record at the conflicted name", deleted)
			}
			if outcome == "resolved" {
				f.peer.mu.Lock()
				delete(f.peer.rows, integrationForeignID(5))
				f.peer.mu.Unlock()
				f.awaitGone(t)
				_, _, deleted = f.traffic()
				slices.Sort(deleted)
				if !slices.Equal(deleted, f.published) || len(integrationRows(t, f.s, kinds.KindIntegrationLedger)) != 0 {
					t.Fatal("the unblocked cleanup did not delete exactly the owned record set", deleted)
				}
				return
			}
			held.Spec.DeletionPolicy = public.IntegrationDeletionRetain
			released := integrationApply(t, f.s, held)
			if !store.Terminated(released) || integrationStored(t, f.s, f.d) != nil {
				t.Fatal("declaring Retain did not release the terminating object")
			}
			awaitServingRuntime(t, f.s, func() bool { return f.phase() == "" })
			time.Sleep(200 * time.Millisecond)
			_, after, deleted := f.traffic()
			if after != writes || len(deleted) != 0 {
				t.Fatal("the release wrote to the provider")
			}
			if rows := integrationRows(t, f.s, kinds.KindIntegrationLedger); len(rows) != 1 {
				t.Fatal("the release did not retain the ledger", len(rows))
			}
			entries := integrationAudit(t, f.s, f.d)
			if last := entries[len(entries)-1]; last.Action != "integration.finalize" || !strings.HasPrefix(last.Reason, "released without cleanup: deletionPolicy is Retain") {
				t.Fatalf("release audit row: %s %q", last.Action, last.Reason)
			}
		})
	}
}

// TestIntegrationDeleteWithoutCredentialMakesNoProviderCallAndResumes: a
// publisher's cleanup needs its credential. While the reference does not
// resolve the object stays terminating with a credentials reason and the
// provider sees no request; restoring the credential finishes the cleanup.
func TestIntegrationDeleteWithoutCredentialMakesNoProviderCallAndResumes(t *testing.T) {
	f := newIntegrationDeleteFixture(t, nil)
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	f.s.integrations.host.Wake()
	f.awaitStatus(t, "Paused", "CredentialsUnavailable")
	requests, _, _ := f.traffic()
	f.requestDelete(t)
	f.awaitStatus(t, ih.PhaseTerminating, ih.ReasonCredentials)
	awaitServingRuntime(t, f.s, func() bool {
		held := integrationStored(t, f.s, f.d)
		if held == nil {
			t.Fatal("cleanup finalized without its credential")
		}
		for _, condition := range held.Status.Conditions {
			if condition.Type == "Ready" && condition.Reason == ih.ReasonCredentials {
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	if after, _, deleted := f.traffic(); after != requests || len(deleted) != 0 {
		t.Fatal("cleanup called the provider without its credential", after-requests)
	}
	// The same value the fixture granted: the keyed fingerprint returns too.
	if err := os.WriteFile(f.path, []byte("test-only-private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	f.s.integrations.host.Wake()
	f.awaitGone(t)
	_, _, deleted := f.traffic()
	slices.Sort(deleted)
	if !slices.Equal(deleted, f.published) {
		t.Fatal("the resumed cleanup did not delete exactly the owned record set", deleted)
	}
}
