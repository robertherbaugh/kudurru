package main

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// webhookRecordsRestart closes the runtime and starts a new one over the
// same database, as a process restart does.
func webhookRecordsRestart(t *testing.T, f *integrationOperationFixture) {
	t.Helper()
	old := f.s
	f.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	path := old.opts.Store.Path()
	if err := old.opts.Store.Close(); err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: old.opts.Validator, InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	opts := old.opts
	opts.Store = st
	next, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := next.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	f.s = next
	f.stop = integrationRun(t, next)
}

func webhookRecordsPendingIn(t *testing.T, st store.Store) int {
	t.Helper()
	list, err := st.List(t.Context(), kinds.KindIntegrationLedger, store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	for _, o := range list.Items {
		if l := o.(*kinds.IntegrationLedger); l.Spec.Pending != nil {
			pending++
		}
	}
	return pending
}

func webhookRecordsPublished(t *testing.T, p *webhookRecordsPeer) bool {
	t.Helper()
	rows := p.rows(t)
	return slices.Equal(rows["web.example/A"], []string{"192.0.2.80"}) && len(rows["_kudurru-a.web.example/TXT"]) == 1
}

// A pending operation survives a process restart, and the restarted
// runtime settles it from a reread without sending the change again
// (D380).
func TestIntegrationWebhookMountedRecordRestartRecoversByReread(t *testing.T) {
	f, peer := webhookRecordsFixture(t)
	owner := integrationOwner(f.s, f.d)
	peer.wipe(t)
	peer.setUnreadable(true)
	integrationAdminSettled(t, f)
	before := peer.postCount()
	if _, err := f.s.integrations.host.Sync(t.Context(), owner, ""); err == nil {
		t.Fatal("an unreadable result was confirmed")
	}
	if peer.postCount() != before+1 || webhookRecordsPendingIn(t, f.s.opts.Store) != 1 {
		t.Fatal("uncertain effect was replayed or not left pending", peer.postCount()-before)
	}
	peer.setUnreadable(false)
	webhookRecordsRestart(t, f)
	awaitServingRuntime(t, f.s, func() bool { return webhookRecordsPendingIn(t, f.s.opts.Store) == 0 })
	if peer.postCount() != before+1 || !webhookRecordsPublished(t, peer) {
		t.Fatal("restart replayed the change or lost it", peer.postCount()-before, peer.rows(t))
	}
}

// Replica promotion during an apply: the old writer's held change reaches
// the peer after its lease ended, the old writer records nothing, and the
// promoted successor settles the intent from a reread with no replay.
func TestIntegrationWebhookMountedRecordPromotionDuringApply(t *testing.T) {
	f, peer := webhookRecordsFixture(t)
	primary := f.s
	owner := integrationOwner(primary, f.d)
	peer.wipe(t)
	hold, entered := make(chan struct{}), make(chan struct{})
	released := false
	release := func() {
		if !released {
			released = true
			close(hold)
		}
	}
	defer release()
	peer.mu.Lock()
	peer.hold, peer.entered = hold, entered
	peer.mu.Unlock()
	integrationAdminSettled(t, f)
	before := peer.postCount()
	result := make(chan error, 1)
	go func() { _, err := primary.integrations.host.Sync(t.Context(), owner, ""); result <- err }()
	integrationAwaitSignal(t, entered)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "follower.db"), sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "records-successor", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Enroll(t.Context(), primary.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	installCtx := captureServingReplica(t, st)
	image, err := primary.opts.Store.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := st.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := st.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal(err)
	}
	if webhookRecordsPendingIn(t, st) != 1 {
		t.Fatal("the replicated image does not carry the reserved intent")
	}
	opts := primary.opts
	opts.Store = st
	follower, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := follower.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	runServingFixture(t, follower)
	awaitServingRuntime(t, follower, func() bool { return follower.publisher.Status().ProtectedReady })
	if peer.postCount() != before+1 {
		t.Fatal("a follower reached the peer", peer.postCount()-before)
	}
	promoted, err := st.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "records-failover"})
	if err != nil {
		t.Fatal(err)
	}
	f.stop()
	if _, err = primary.opts.Store.ObserveAuthority(t.Context(), promoted.Term); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("the old writer completed a sync after its lease ended")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the old writer's sync did not end")
	}
	// The held body is applied by the peer although its client is gone.
	awaitServingRuntime(t, follower, func() bool { return webhookRecordsPublished(t, peer) })
	integrationWriter(t, follower)
	awaitServingRuntime(t, follower, func() bool { return webhookRecordsPendingIn(t, st) == 0 })
	if peer.postCount() != before+1 {
		t.Fatal("the successor replayed the change", peer.postCount()-before)
	}
	list, err := st.List(t.Context(), kinds.KindIntegrationOperation, store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	confirmed := 0
	for _, o := range list.Items {
		op := o.(*kinds.IntegrationOperation)
		if op.Spec.Receipt != nil && op.Spec.Receipt.Complete && op.Spec.AbandonedAt.IsZero() && op.Spec.After != nil && op.Spec.After.DNSName == "web.example." {
			confirmed++
		}
	}
	if confirmed != 2 {
		t.Fatal("the held intent was not confirmed by the successor", confirmed)
	}
}

// Under sync, unowned, other-kudurru and external-dns rows stay untouched,
// a desired name held by an unowned row is a conflict, so is a desired
// name whose only row is another kudurru owner's registry row (D380: no
// create is planned over the claim, so no operation row is written and
// nothing is posted on any sync), and removing a
// source record set deletes the data and registry rows only where the
// registry names this instance.
func TestIntegrationWebhookMountedRecordForeignProtectionAndRemoval(t *testing.T) {
	f, peer := webhookRecordsFixture(t)
	foreign := []*externaldns.Endpoint{
		{DNSName: "manual.example", RecordType: "A", RecordTTL: 300, Targets: []string{"198.51.100.7"}},
		{DNSName: "other.example", RecordType: "A", RecordTTL: 300, Targets: []string{"198.51.100.8"}},
		{DNSName: "_kudurru-a.other.example", RecordType: "TXT", RecordTTL: 300, Targets: []string{`"heritage=kudurru,kudurru/owner=` + strings.Repeat("f", 64) + `"`}},
		{DNSName: "legacy.example", RecordType: "A", RecordTTL: 300, Targets: []string{"198.51.100.9"}},
		{DNSName: "a-legacy.example", RecordType: "TXT", RecordTTL: 300, Targets: []string{`"heritage=external-dns,external-dns/owner=legacy"`}},
		{DNSName: "_kudurru-a.claimed.example", RecordType: "TXT", RecordTTL: 300, Targets: []string{`"heritage=kudurru,kudurru/owner=` + strings.Repeat("e", 64) + `"`}},
	}
	peer.put(t, foreign...)
	untouched := func() map[string][]string {
		rows := peer.rows(t)
		delete(rows, "web.example/A")
		delete(rows, "_kudurru-a.web.example/TXT")
		return rows
	}
	baseline := untouched()
	if len(baseline) != len(foreign) {
		t.Fatal("foreign rows", baseline)
	}
	// Desire every foreign name: each is a conflict, none is written.
	desired := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "contested", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{
		{DNSName: "manual.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.7"}},
		{DNSName: "other.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.8"}},
		{DNSName: "legacy.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.9"}},
		{DNSName: "claimed.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.10"}},
	}}}
	if _, err := f.s.opts.Store.Apply(t.Context(), desired, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	f.s.integrations.host.Wake()
	awaitServingRuntime(t, f.s, func() bool {
		for _, s := range f.s.integrations.host.Statuses() {
			if s.Owner == integrationOwner(f.s, f.d) && s.Reason == "ConflictsOrPending" && s.Result.Conflicts >= 4 {
				return true
			}
		}
		return false
	})
	posts := peer.postCount()
	// Two more completed syncs; a status write may fence one, which is
	// asked again.
	for range 2 {
		awaitServingRuntime(t, f.s, func() bool {
			res, err := f.s.integrations.host.Sync(t.Context(), integrationOwner(f.s, f.d), "")
			if err == nil && (res.Conflicts != 4 || res.Pending != 0) {
				t.Fatal("contested sync", res)
			}
			return err == nil
		})
	}
	if total, _ := webhookRecordsOperations(t, f.s, "claimed.example."); total != 0 || peer.postCount() != posts {
		t.Fatal("a create was planned over another owner's claim", total, peer.postCount()-posts)
	}
	// Removing the published source record set removes only owned rows.
	var published public.DNSRecordSet
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "published"}, &published); err != nil {
		t.Fatal(err)
	}
	if err := f.s.opts.Store.Delete(t.Context(), store.KeyOf(&published), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	f.s.integrations.host.Wake()
	awaitServingRuntime(t, f.s, func() bool {
		rows := peer.rows(t)
		return rows["web.example/A"] == nil && rows["_kudurru-a.web.example/TXT"] == nil
	})
	if after := untouched(); !mapsEqual(baseline, after) {
		t.Fatal("foreign rows changed", baseline, after)
	}
	if peer.postCount() != posts+1 {
		t.Fatal("removal took more than one change", peer.postCount()-posts)
	}
}

func mapsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// Kudurru to kudurru (design record line 599): a webhook Integration on one
// runtime publishes into the external-dns listener of another runtime, and
// the record sets land in that runtime's SQLite store under the
// external-dns manager, registry row included.
func TestIntegrationWebhookMountedRecordKudurruToKudurru(t *testing.T) {
	sink, _ := integrationFixture(t, nil, nil)
	f, peer := webhookRecordsFixtureWith(t, func(token string) *webhookRecordsPeer {
		return newWebhookRecordsPeerOver(t, token, sink.opts.Store, sink.opts.Validator)
	})
	if !webhookRecordsPublished(t, peer) {
		t.Fatal("publication did not reach the other runtime", peer.rows(t))
	}
	list, err := sink.opts.Store.List(t.Context(), "DNSRecordSet", store.ListOptions{Namespace: "external-dns"})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, o := range list.Items {
		rs := o.(*public.DNSRecordSet)
		if store.ManagedBy(rs) != externaldns.Manager {
			t.Fatal("record set without the external-dns manager", rs.Name)
		}
		for _, e := range rs.Spec.Endpoints {
			names[e.DNSName+"/"+e.RecordType] = true
		}
	}
	if !names["web.example/A"] || !names["_kudurru-a.web.example/TXT"] {
		t.Fatal("the other runtime's store lacks the published rows", names)
	}
	if webhookRecordsPendingIn(t, f.s.opts.Store) != 0 {
		t.Fatal("the publishing runtime left an intent pending")
	}
}
