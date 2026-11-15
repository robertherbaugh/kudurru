package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// webhookRecordsFixtureFor is webhookRecordsFixture with the published
// endpoints and an edit of the Integration chosen by the test. It returns
// once every endpoint and its registry row are at the peer and no intent is
// pending.
func webhookRecordsFixtureFor(t *testing.T, edit func(*public.Integration), endpoints ...public.Endpoint) (*integrationOperationFixture, *webhookRecordsPeer) {
	t.Helper()
	f := &integrationOperationFixture{}
	var peer *webhookRecordsPeer
	f.s, f.path = integrationFixture(t, func(o *servingRuntimeOptions) {
		peer = newWebhookRecordsPeer(t, o.Config.Credentials.AllowedFiles[0])
		o.Config.Credentials.AllowedFiles = append(o.Config.Credentials.AllowedFiles, peer.ca)
	}, nil)
	t.Cleanup(func() { integrationTerminatingLog(t, f.s, nil) })
	integrationOperationCaller(t, f)
	record := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "published", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: endpoints}}
	if _, err := f.s.opts.Store.Apply(t.Context(), record, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	definition := webhookRecordsDefinition(f, peer)
	if edit != nil {
		edit(definition)
	}
	integrationOperationStart(t, f, definition)
	awaitServingRuntime(t, f.s, func() bool {
		rows := peer.rows(t)
		for _, e := range endpoints {
			name := strings.TrimSuffix(e.DNSName, ".")
			if !slices.Equal(rows[name+"/"+e.RecordType], e.Targets) || len(rows["_kudurru-"+strings.ToLower(e.RecordType)+"."+name+"/TXT"]) != 1 {
				return false
			}
		}
		return webhookRecordsPendingIn(t, f.s.opts.Store) == 0
	})
	return f, peer
}

func webhookRecordsStatus(f *integrationOperationFixture) (phase, reason string) {
	owner := integrationOwner(f.s, f.d)
	for _, status := range f.s.integrations.host.Statuses() {
		if status.Owner == owner {
			return status.Phase, status.Reason
		}
	}
	return "", ""
}

// webhookRecordsOperations counts the stored operations at name: all of
// them and the abandoned ones.
func webhookRecordsOperations(t *testing.T, s *servingRuntime, name string) (total, abandoned int) {
	t.Helper()
	for _, o := range integrationRows(t, s, kinds.KindIntegrationOperation) {
		op := o.(*kinds.IntegrationOperation)
		if op.Spec.Record.Key.Name != name {
			continue
		}
		total++
		if !op.Spec.AbandonedAt.IsZero() {
			abandoned++
		}
	}
	return total, abandoned
}

// A 2xx whose effect the verifying reread does not show is ambiguous, not
// untouched work (D298, D380). With a peer that took the body without
// effect, the intent stays pending and the next sync settles that same
// operation through recovery, with one more POST and nothing abandoned.
// With a peer that applied the data but dropped the registry row, the
// intent stays pending across syncs and nothing is sent again, because the
// data row is no longer provably this instance's.
func TestIntegrationWebhookMountedRecordUnconfirmedApplyStaysPending(t *testing.T) {
	for _, mode := range []string{"no-effect", "drop-registry"} {
		t.Run(mode, func(t *testing.T) {
			f, peer := webhookRecordsFixture(t)
			owner := integrationOwner(f.s, f.d)
			peer.wipe(t)
			peer.mu.Lock()
			peer.ignoreNext, peer.dropRegistry = mode == "no-effect", mode == "drop-registry"
			peer.mu.Unlock()
			integrationAdminSettled(t, f)
			before := peer.postCount()
			if _, err := f.s.integrations.host.Sync(t.Context(), owner, ""); err == nil {
				t.Fatal("an unconfirmed 2xx was confirmed")
			}
			if peer.postCount() != before+1 || webhookRecordsPendingIn(t, f.s.opts.Store) != 1 {
				t.Fatal("unconfirmed effect was replayed or not left pending", peer.postCount()-before, webhookRecordsPendingIn(t, f.s.opts.Store))
			}
			if mode == "no-effect" {
				// The next sync recovers the same operation: it rereads, sends
				// the change once more, and confirms it.
				awaitServingRuntime(t, f.s, func() bool {
					res, err := f.s.integrations.host.Sync(t.Context(), owner, "")
					return err == nil && res.Pending == 0
				})
				if !webhookRecordsPublished(t, peer) || webhookRecordsPendingIn(t, f.s.opts.Store) != 0 || peer.postCount() != before+2 {
					t.Fatal("recovery did not settle the ignored change with one more POST", peer.postCount()-before, peer.rows(t))
				}
			} else {
				for range 2 {
					if _, err := f.s.integrations.host.Sync(t.Context(), owner, ""); err == nil {
						t.Fatal("a data row without its registry row was confirmed")
					}
				}
				rows := peer.rows(t)
				if !slices.Equal(rows["web.example/A"], []string{"192.0.2.80"}) || rows["_kudurru-a.web.example/TXT"] != nil || webhookRecordsPendingIn(t, f.s.opts.Store) != 1 || peer.postCount() != before+1 {
					t.Fatal("a partial apply was replayed or settled", peer.postCount()-before, rows)
				}
			}
			if _, abandoned := webhookRecordsOperations(t, f.s, "web.example."); abandoned != 0 {
				t.Fatal("an operation that reached the peer was abandoned as untouched", abandoned)
			}
		})
	}
}

// A webhook publisher with deletionPolicy Delete is cleaned up like a
// Cloudflare one (D367, D380): while the peer is unreachable the object
// stays terminating with its finalizer and nothing is forgotten; once the
// peer answers, the cleanup deletes the data and registry rows its registry
// names, leaves unowned, other-kudurru and external-dns rows untouched,
// forgets its ledger and operations, and releases the finalizer.
func TestIntegrationWebhookMountedRecordDeleteCleanup(t *testing.T) {
	f, peer := webhookRecordsFixtureFor(t, func(d *public.Integration) {
		d.Spec.DeletionPolicy = public.IntegrationDeletionDelete
		d.Spec.Resync = metav1.Duration{Duration: time.Second}
	}, public.Endpoint{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}})
	if !store.HasFinalizer(f.d, store.IntegrationCleanupFinalizer) {
		t.Fatal("a Delete instance has no cleanup finalizer")
	}
	peer.put(t,
		&externaldns.Endpoint{DNSName: "manual.example", RecordType: "A", RecordTTL: 300, Targets: []string{"198.51.100.7"}},
		&externaldns.Endpoint{DNSName: "other.example", RecordType: "A", RecordTTL: 300, Targets: []string{"198.51.100.8"}},
		&externaldns.Endpoint{DNSName: "_kudurru-a.other.example", RecordType: "TXT", RecordTTL: 300, Targets: []string{`"heritage=kudurru,kudurru/owner=` + strings.Repeat("f", 64) + `"`}},
		&externaldns.Endpoint{DNSName: "legacy.example", RecordType: "A", RecordTTL: 300, Targets: []string{"198.51.100.9"}},
		&externaldns.Endpoint{DNSName: "a-legacy.example", RecordType: "TXT", RecordTTL: 300, Targets: []string{`"heritage=external-dns,external-dns/owner=legacy"`}},
	)
	foreign := peer.rows(t)
	delete(foreign, "web.example/A")
	delete(foreign, "_kudurru-a.web.example/TXT")
	if len(foreign) != 5 {
		t.Fatal("foreign rows", foreign)
	}

	peer.setDown(true)
	integrationRequestDelete(t, f.s, f.d)
	f.s.integrations.host.Wake()
	awaitServingRuntime(t, f.s, func() bool {
		phase, reason := webhookRecordsStatus(f)
		return phase == ih.PhaseTerminating && reason != ""
	})
	// Several resync intervals later the object is still held.
	time.Sleep(3 * time.Second)
	held := integrationStored(t, f.s, f.d)
	if held == nil || held.DeletionTimestamp == nil || !store.HasFinalizer(held, store.IntegrationCleanupFinalizer) {
		t.Fatal("cleanup finalized while the peer was unreachable")
	}
	if phase, reason := webhookRecordsStatus(f); phase != ih.PhaseTerminating {
		t.Fatal("unreachable peer status", phase, reason)
	} else {
		t.Log("unreachable peer reason:", reason)
	}
	if rows := integrationRows(t, f.s, kinds.KindIntegrationLedger); len(rows) != 1 {
		t.Fatal("a cleanup without its peer forgot the ledger", len(rows))
	}

	posts := peer.postCount()
	peer.setDown(false)
	f.s.integrations.host.Wake()
	awaitServingRuntime(t, f.s, func() bool { return integrationStored(t, f.s, f.d) == nil })
	rows := peer.rows(t)
	if rows["web.example/A"] != nil || rows["_kudurru-a.web.example/TXT"] != nil {
		t.Fatal("cleanup left the owned rows", rows)
	}
	if !mapsEqual(foreign, rows) {
		t.Fatal("cleanup changed rows the instance does not own", foreign, rows)
	}
	if peer.postCount() != posts+1 {
		t.Fatal("cleanup took other than one change", peer.postCount()-posts)
	}
	for _, kind := range []store.Kind{kinds.KindIntegrationLedger, kinds.KindIntegrationOperation} {
		if n := len(integrationRows(t, f.s, kind)); n != 0 {
			t.Fatal("rows remain for a finalized instance", kind, n)
		}
	}
	summary := ""
	for _, entry := range integrationAudit(t, f.s, f.d) {
		if entry.Action == "integration.cleanup" {
			summary = entry.Reason
		}
	}
	if !strings.Contains(summary, "deleted 1 provider record sets") || !strings.Contains(summary, "forgot 1 ledger rows") {
		t.Fatalf("cleanup summary: %q", summary)
	}
}

// Narrowing spec.webhook.domains after publication keeps the record sets
// this instance's registry rows claim outside the new domains (D380): the
// sync policy deletes them at once; upsert-only keeps them, and a Delete
// cleanup then removes them instead of finishing clean over rows it could
// not see; Retain leaves every row at the peer. Unowned rows under the
// removed domain are never touched.
func TestIntegrationWebhookMountedRecordNarrowedDomains(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		deletion     string
	}{
		{"sync", "", ""},
		{"upsert-only-delete", "upsert-only", public.IntegrationDeletionDelete},
		{"upsert-only-retain", "upsert-only", public.IntegrationDeletionRetain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, peer := webhookRecordsFixtureFor(t, func(d *public.Integration) {
				d.Spec.Webhook.Domains = []string{"a.example", "b.example"}
				d.Spec.Webhook.Policy = tc.policy
				d.Spec.DeletionPolicy = tc.deletion
				d.Spec.Resync = metav1.Duration{Duration: time.Second}
			},
				public.Endpoint{DNSName: "web.a.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.81"}},
				public.Endpoint{DNSName: "web.b.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.82"}},
			)
			peer.put(t, &externaldns.Endpoint{DNSName: "manual.b.example", RecordType: "A", RecordTTL: 300, Targets: []string{"198.51.100.7"}})
			owned := func(rows map[string][]string, name string) bool {
				return rows[name+"/A"] != nil && rows["_kudurru-a."+name+"/TXT"] != nil
			}
			gone := func(rows map[string][]string, name string) bool {
				return rows[name+"/A"] == nil && rows["_kudurru-a."+name+"/TXT"] == nil
			}
			stored := integrationStored(t, f.s, f.d)
			stored.Spec.Webhook.Domains = []string{"a.example"}
			f.d = integrationApply(t, f.s, stored)
			// The narrowed revision has been synced.
			awaitServingRuntime(t, f.s, func() bool {
				current := integrationStored(t, f.s, f.d)
				owner := integrationOwner(f.s, f.d)
				for _, status := range f.s.integrations.host.Statuses() {
					if status.Owner == owner && current != nil && status.Revision == current.ResourceVersion && status.Phase == "Ready" {
						return tc.policy != "" || gone(peer.rows(t), "web.b.example")
					}
				}
				return false
			})
			rows := peer.rows(t)
			if !owned(rows, "web.a.example") || rows["manual.b.example/A"] == nil {
				t.Fatal("narrowing changed a row it must keep", rows)
			}
			if tc.policy == "" {
				if !gone(rows, "web.b.example") {
					t.Fatal("sync kept an owned row outside the narrowed domains", rows)
				}
				return
			}
			if !owned(rows, "web.b.example") {
				t.Fatal("upsert-only deleted an owned row", rows)
			}
			integrationRequestDelete(t, f.s, f.d)
			f.s.integrations.host.Wake()
			awaitServingRuntime(t, f.s, func() bool { return integrationStored(t, f.s, f.d) == nil })
			rows = peer.rows(t)
			if rows["manual.b.example/A"] == nil {
				t.Fatal("cleanup removed an unowned row", rows)
			}
			if tc.deletion == public.IntegrationDeletionRetain {
				if !owned(rows, "web.a.example") || !owned(rows, "web.b.example") {
					t.Fatal("Retain removed rows at the peer", rows)
				}
				return
			}
			if !gone(rows, "web.a.example") || !gone(rows, "web.b.example") {
				t.Fatal("Delete cleanup finished with owned rows at the peer", rows)
			}
		})
	}
}

// Removing every domain ends publication (D380): against a records-only
// peer the instance has no role, so its lease ends as ProvisionFailed and
// no sync reads or writes the peer; the record sets its registry rows claim
// stay there, as under Retain. A Delete cleanup still removes them, because
// it provisions and checks the instance without asking for roles, and it
// leaves rows the instance does not own untouched.
func TestIntegrationWebhookMountedRecordDomainsRemovedCleanup(t *testing.T) {
	f, peer := webhookRecordsFixtureFor(t, func(d *public.Integration) {
		d.Spec.DeletionPolicy = public.IntegrationDeletionDelete
		d.Spec.Resync = metav1.Duration{Duration: time.Second}
	}, public.Endpoint{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}})
	peer.put(t, &externaldns.Endpoint{DNSName: "manual.example", RecordType: "A", RecordTTL: 300, Targets: []string{"198.51.100.7"}})
	published := peer.rows(t)

	stored := integrationStored(t, f.s, f.d)
	stored.Spec.Webhook.Domains = nil
	f.d = integrationApply(t, f.s, stored)
	owner := integrationOwner(f.s, f.d)
	awaitServingRuntime(t, f.s, func() bool {
		current := integrationStored(t, f.s, f.d)
		for _, status := range f.s.integrations.host.Statuses() {
			if status.Owner == owner && current != nil && status.Revision == current.ResourceVersion {
				return status.Reason == "ProvisionFailed"
			}
		}
		return false
	})
	posts := peer.postCount()
	// Several resync intervals pass without a change at the peer.
	time.Sleep(3 * time.Second)
	if peer.postCount() != posts || !mapsEqual(published, peer.rows(t)) {
		t.Fatal("an instance without domains changed the peer", peer.postCount()-posts, peer.rows(t))
	}
	if rows := integrationRows(t, f.s, kinds.KindIntegrationLedger); len(rows) != 1 {
		t.Fatal("removing the domains forgot the ledger", len(rows))
	}

	integrationRequestDelete(t, f.s, f.d)
	f.s.integrations.host.Wake()
	awaitServingRuntime(t, f.s, func() bool { return integrationStored(t, f.s, f.d) == nil })
	rows := peer.rows(t)
	if rows["web.example/A"] != nil || rows["_kudurru-a.web.example/TXT"] != nil {
		t.Fatal("cleanup left the owned rows", rows)
	}
	if rows["manual.example/A"] == nil || len(rows) != 1 {
		t.Fatal("cleanup changed rows the instance does not own", rows)
	}
	if peer.postCount() != posts+1 {
		t.Fatal("cleanup took other than one change", peer.postCount()-posts)
	}
	for _, kind := range []store.Kind{kinds.KindIntegrationLedger, kinds.KindIntegrationOperation} {
		if n := len(integrationRows(t, f.s, kind)); n != 0 {
			t.Fatal("rows remain for a finalized instance", kind, n)
		}
	}
}
