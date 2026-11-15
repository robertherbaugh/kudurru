package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The mounted acceptance of the SQLite Delete lifecycle (D367) that needs a
// whole runtime to stop, move or lose its license in the middle of a cleanup.
// Each test uses the registered Cloudflare adapter and SDK against the
// verified-TLS peer of its canonical origin, the real SQLite writer,
// publisher, host, ledger and budget.

// ledgerPending reports whether the instance's single ledger row holds an
// intent whose outcome no receipt has settled.
func (f *integrationDeleteFixture) ledgerPending(t *testing.T) bool {
	t.Helper()
	rows := integrationRows(t, f.s, kinds.KindIntegrationLedger)
	return len(rows) == 1 && rows[0].(*kinds.IntegrationLedger).Spec.Pending != nil
}

// expectOwnedDeletedOnce checks that delete effects named exactly the
// published provider IDs, each once, in batches POSTs since writes.
func (f *integrationDeleteFixture) expectOwnedDeletedOnce(t *testing.T, writes, batches int) {
	t.Helper()
	_, after, deleted := f.traffic()
	slices.Sort(deleted)
	if !slices.Equal(deleted, f.published) || after-writes != batches {
		t.Fatalf("delete effects named %v in %d batches; published were %v, want %d batches", deleted, after-writes, f.published, batches)
	}
	for _, kind := range []store.Kind{kinds.KindIntegrationLedger, kinds.KindIntegrationOperation} {
		if rows := integrationRows(t, f.s, kind); len(rows) != 0 {
			t.Errorf("%d %s rows remain for a finalized instance", len(rows), kind)
		}
	}
	if rows := integrationRows(t, f.s, kinds.KindIntegrationBudget); len(rows) != 1 {
		t.Fatal("the shared request budget was removed", len(rows))
	}
}

// restart stops the whole runtime of f the way a process stops, closes the
// database, and starts a new runtime on the reopened file. Nothing of the old
// runtime's memory survives: the new one has only what SQLite stored.
func (f *integrationDeleteFixture) restart(t *testing.T, between func()) {
	t.Helper()
	old := f.s
	f.stopWriter()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := old.Close(ctx); err != nil {
		t.Fatal(err)
	}
	path := old.opts.Store.Path()
	if err := old.opts.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if between != nil {
		between()
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
	next.integrations.factory = old.integrations.factory
	f.s = next
	f.stopWriter = integrationRun(t, next)
}

// TestIntegrationDeleteRetainLeavesProviderRecordsAndLedger is the control:
// under Retain the object carries no finalizer, a delete removes it at once,
// and nothing reaches the provider; the published records and the ledger
// stay.
func TestIntegrationDeleteRetainLeavesProviderRecordsAndLedger(t *testing.T) {
	f := newIntegrationDeleteFixture(t, func(d *public.Integration) { d.Spec.DeletionPolicy = public.IntegrationDeletionRetain })
	requests, writes, _ := f.traffic()
	integrationRequestDelete(t, f.s, f.d)
	if integrationStored(t, f.s, f.d) != nil {
		t.Fatal("a Retain object outlived its delete request")
	}
	f.s.integrations.host.Wake()
	awaitServingRuntime(t, f.s, func() bool { return f.phase() == "" })
	time.Sleep(300 * time.Millisecond)
	if after, afterWrites, deleted := f.traffic(); after != requests || afterWrites != writes || len(deleted) != 0 {
		t.Fatal("deleting a Retain object reached the provider", after-requests, afterWrites-writes)
	}
	f.peer.mu.Lock()
	for _, id := range f.published {
		if _, ok := f.peer.rows[id]; !ok {
			t.Error("a Retain delete removed a published record", id)
		}
	}
	f.peer.mu.Unlock()
	if rows := integrationRows(t, f.s, kinds.KindIntegrationLedger); len(rows) != 1 {
		t.Fatal("Retain did not keep the ledger", len(rows))
	}
	for _, entry := range integrationAudit(t, f.s, f.d) {
		if entry.Action == "integration.cleanup" || entry.Action == "integration.release" {
			t.Fatal("a Retain delete ran the cleanup lifecycle", entry.Action)
		}
	}
}

// TestIntegrationDeleteResumesAfterRestart stops and restarts the whole
// runtime in the middle of a cleanup:
//
//   - unreadable: the provider's record list cannot be read when the delete
//     is requested, so the first runtime deletes nothing. After the restart
//     the new runtime finishes with one batch.
//   - ambiguous: the provider applies the delete batch but answers 503 and
//     then cannot list. The intent stays pending; nothing is sent again. After
//     the restart the new runtime confirms the absence by a reread, without a
//     second batch, and finishes.
func TestIntegrationDeleteResumesAfterRestart(t *testing.T) {
	for _, mode := range []string{"unreadable", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			f := newIntegrationDeleteFixture(t, func(d *public.Integration) { d.Spec.Resync.Duration = time.Second })
			_, writes, _ := f.traffic()
			f.peer.mu.Lock()
			if mode == "unreadable" {
				f.peer.unreadable = true
			} else {
				f.peer.ambiguous = true
			}
			f.peer.mu.Unlock()
			f.requestDelete(t)
			if mode == "unreadable" {
				awaitServingRuntime(t, f.s, func() bool {
					phase, reason := f.status()
					return phase == ih.PhaseTerminating && reason != "" && reason != "LicenseUnavailable"
				})
				// The failed run is retried with backoff; none of its tries
				// may delete anything without a complete read.
				time.Sleep(time.Second)
				if _, after, deleted := f.traffic(); after != writes || len(deleted) != 0 {
					t.Fatal("cleanup deleted without a complete read", deleted)
				}
			} else {
				awaitServingRuntime(t, f.s, func() bool {
					_, after, _ := f.traffic()
					return after == writes+1 && f.ledgerPending(t)
				})
				time.Sleep(time.Second)
				if _, after, _ := f.traffic(); after != writes+1 || !f.ledgerPending(t) {
					t.Fatal("an unacknowledged batch was sent again or settled without a reread", after-writes)
				}
			}
			if held := integrationStored(t, f.s, f.d); held == nil || !store.HasFinalizer(held, store.IntegrationCleanupFinalizer) {
				t.Fatal("the instance was finalized before its cleanup finished")
			}
			f.restart(t, func() {
				f.peer.mu.Lock()
				f.peer.unreadable, f.peer.ambiguous = false, false
				f.peer.mu.Unlock()
			})
			f.awaitGone(t)
			f.expectOwnedDeletedOnce(t, writes, 1)
		})
	}
}

// TestIntegrationDeleteFailoverFencesHeldBatchAndSuccessorFinishes holds the
// cleanup's delete batch at the provider while a replica is promoted. The
// old writer's batch then completes at the provider, but it can record no
// receipt and cannot finalize; the promoted writer resolves the intent by a
// reread, sends nothing again, and finishes.
func TestIntegrationDeleteFailoverFencesHeldBatchAndSuccessorFinishes(t *testing.T) {
	f := newIntegrationDeleteFixture(t, nil)
	primary := f.s
	hold, entered := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	releaseHold := func() {
		if released.CompareAndSwap(false, true) {
			close(hold)
		}
	}
	defer releaseHold()
	_, writes, _ := f.traffic()
	f.peer.mu.Lock()
	f.peer.hold, f.peer.entered = hold, entered
	f.peer.mu.Unlock()
	f.requestDelete(t)
	integrationAwaitSignal(t, entered)
	if !f.ledgerPending(t) {
		t.Fatal("the batch reached the provider before its intent was durable")
	}

	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "follower.db"), sqlite.Options{Scheme: scheme, Validator: primary.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "integration-successor", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
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
	var replicated public.Integration
	if err := st.Get(t.Context(), store.KeyOf(f.d), &replicated); err != nil || replicated.DeletionTimestamp == nil || !store.HasFinalizer(&replicated, store.IntegrationCleanupFinalizer) {
		t.Fatal("the replica did not receive the terminating object", err)
	}
	promoted, err := st.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "integration-delete-failover"})
	if err != nil {
		t.Fatal(err)
	}
	f.stopWriter()
	if _, err = primary.opts.Store.ObserveAuthority(t.Context(), promoted.Term); err != nil {
		t.Fatal(err)
	}
	// The held batch now completes at the provider.
	releaseHold()
	awaitServingRuntime(t, primary, func() bool {
		f.peer.mu.Lock()
		defer f.peer.mu.Unlock()
		return len(f.peer.deleted) == len(f.published)
	})
	time.Sleep(500 * time.Millisecond)
	// The fenced writer recorded no receipt and did not finalize.
	if !f.ledgerPending(t) {
		t.Fatal("the fenced writer settled the intent")
	}
	if held := integrationStored(t, primary, f.d); held == nil || !store.HasFinalizer(held, store.IntegrationCleanupFinalizer) {
		t.Fatal("the fenced writer finalized")
	}
	for _, entry := range integrationAudit(t, primary, f.d) {
		if entry.Action == "integration.cleanup" {
			t.Fatal("the fenced writer recorded a cleanup")
		}
	}

	f.s = follower
	f.stopWriter = integrationWriter(t, follower)
	follower.integrations.host.Wake()
	f.awaitGone(t)
	f.expectOwnedDeletedOnce(t, writes, 1)
	summary := ""
	for _, entry := range integrationAudit(t, follower, f.d) {
		if entry.Action == "integration.cleanup" {
			summary = entry.Reason
		}
	}
	if summary == "" {
		t.Fatal("the promoted writer finalized without its audit summary")
	}
}

// TestIntegrationDeleteLicenseExpiryPausesCleanup lets the license expire
// while the cleanup's delete batch is held at the provider. The cleanup
// stops with LicenseUnavailable and makes no further provider request; the
// object stays terminating. Then either a renewed license lets it confirm
// the batch by a reread and finish without sending it again, or the reviewed
// release command removes the object without any license, keeping the
// ledger.
func TestIntegrationDeleteLicenseExpiryPausesCleanup(t *testing.T) {
	for _, outcome := range []string{"renewed", "released"} {
		t.Run(outcome, func(t *testing.T) {
			pub, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().Truncate(time.Second)
			var clock atomic.Int64
			clock.Store(at.UnixNano())
			lic, err := license.New(license.Options{Keyring: license.Keyring{"fixture": pub}, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
			if err != nil {
				t.Fatal(err)
			}
			issue := func(expires time.Time) {
				t.Helper()
				key, err := license.Sign(private, "fixture", license.Claims{ID: "lic_fixture", Licensee: "Fixture", Plan: license.PlanPersonal, Nodes: 1, Features: []license.Feature{license.FeatureIntegrations}, IssuedAt: at.Add(-time.Hour).Unix(), ExpiresAt: expires.Unix()})
				if err != nil {
					t.Fatal(err)
				}
				if err = lic.Set(key, "fixture"); err != nil {
					t.Fatal(err)
				}
			}
			expires := at.Add(time.Hour)
			issue(expires)
			f := newIntegrationDeleteFixtureWith(t, func(o *servingRuntimeOptions) { o.License = lic }, func(d *public.Integration) { d.Spec.Resync.Duration = time.Second })
			admin, router, _ := integrationDeleteAdmin(t, f)
			_, writes, _ := f.traffic()
			hold, entered := make(chan struct{}), make(chan struct{})
			var released atomic.Bool
			releaseHold := func() {
				if released.CompareAndSwap(false, true) {
					close(hold)
				}
			}
			defer releaseHold()
			f.peer.mu.Lock()
			f.peer.hold, f.peer.entered = hold, entered
			f.peer.mu.Unlock()
			f.requestDelete(t)
			integrationAwaitSignal(t, entered)
			clock.Store(expires.Add(license.Grace + time.Minute).UnixNano())
			releaseHold()
			f.s.integrations.host.Wake()
			f.awaitStatus(t, ih.PhaseTerminating, "LicenseUnavailable")
			requests, paused, _ := f.traffic()
			if paused != writes+1 {
				t.Fatal("the held batch was not the only one", paused-writes)
			}
			time.Sleep(time.Second)
			f.s.integrations.host.Wake()
			time.Sleep(300 * time.Millisecond)
			if after, _, _ := f.traffic(); after != requests {
				t.Fatal("a cleanup without a license reached the provider", after-requests)
			}
			held := integrationStored(t, f.s, f.d)
			if held == nil || held.DeletionTimestamp == nil || !store.HasFinalizer(held, store.IntegrationCleanupFinalizer) {
				t.Fatal("the cleanup finalized without a license")
			}
			if outcome == "renewed" {
				issue(at.Add(24 * time.Hour))
				f.s.integrations.host.Wake()
				f.awaitGone(t)
				f.expectOwnedDeletedOnce(t, writes, 1)
				return
			}
			summary := integrationSummaryOf(t, router, admin, "/integrations/default/publisher")
			if !summary.Terminating || summary.Revision != held.ResourceVersion {
				t.Fatalf("summary of the paused cleanup: %+v", summary)
			}
			w := integrationAdminCall(t, router, admin, http.MethodPost, "/integrations/default/publisher/release", api.IntegrationReview{UID: summary.UID, Revision: summary.Revision}, admin.token)
			var result api.IntegrationReleaseResult
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Released || result.LedgerRows != 1 {
				t.Fatalf("release without a license: %d %s", w.Code, w.Body.String())
			}
			if integrationStored(t, f.s, f.d) != nil {
				t.Fatal("the released object remains")
			}
			time.Sleep(300 * time.Millisecond)
			if after, afterWrites, _ := f.traffic(); after != requests || afterWrites != paused {
				t.Fatal("the release reached the provider", after-requests)
			}
			if rows := integrationRows(t, f.s, kinds.KindIntegrationLedger); len(rows) != 1 {
				t.Fatal("the release did not retain the ledger", len(rows))
			}
		})
	}
}

// integrationSeedOperations stores n copies of template under new operation
// IDs in one command each of at most 200 rows, edited by edit.
func integrationSeedOperations(t *testing.T, s *servingRuntime, template *kinds.IntegrationOperation, label string, n int, edit func(*kinds.IntegrationOperation)) {
	t.Helper()
	for start := 0; start < n; start += 200 {
		err := s.opts.Store.Command(t.Context(), s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			for i := start; i < min(start+200, n); i++ {
				op := template.DeepCopy()
				op.Spec.OperationID = kinds.Digest([]byte(label + " " + strconv.Itoa(i)))
				op.ObjectMeta = metav1.ObjectMeta{Name: kinds.OperationName(op.Spec.OperationID)}
				edit(op)
				if err := op.Validate(); err != nil {
					return err
				}
				if _, err := tx.Apply(ctx, op, store.ApplyOptions{Manager: "integration-ledger", CreateOnly: true}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages runs the cleanup
// of an instance whose operation history spans several stored pages of the
// real SQLite store, whose continuation is a keyset, not an offset: the
// forgetting deletes rows behind the read and must still see every one. The
// kind holds, interleaved in store order, settled operations of the published
// record (which leave with its ledger row), intents of a record that never
// got a ledger row (forgotten as leftovers, page by page), and the history
// of a previous incarnation of the same name, which stays byte for byte.
//
// Every changing SQLite command reads the whole stored image back before it
// commits, and so does every capture of the host. With 1,000 operation rows
// under the race detector on a loaded machine a command took 2 to 2.8
// seconds and a capture 6 to 10 seconds; a cleanup run makes about a dozen
// commands (a budget charge per provider request, the intent, the receipt,
// the confirmation), so it reached the host's 30-second bound after its
// delete intent and before the delete request was confirmed, and a capture
// past its 10-second bound dropped the job.
// That is a property of the store's size under the race detector, not of
// the paging this test is about, so the host gets bounds that a run of this
// history fits in, and the summary is exact again: one run reaches the peer
// with one delete intent.
func TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages(t *testing.T) {
	f := newIntegrationDeleteFixtureWith(t, func(o *servingRuntimeOptions) {
		o.IntegrationBounds = integrationBounds{Capture: time.Minute, Operation: 2 * time.Minute}
	}, nil)
	rows := integrationRows(t, f.s, kinds.KindIntegrationOperation)
	if len(rows) != 1 {
		t.Fatalf("operations of the published record: %d", len(rows))
	}
	template := rows[0].(*kinds.IntegrationOperation)
	const settled, orphans, foreign = 300, 400, 300
	integrationSeedOperations(t, f.s, template, "settled", settled, func(*kinds.IntegrationOperation) {})
	orphan := template.Spec.Record
	orphan.Key.Name = "never.example."
	integrationSeedOperations(t, f.s, template, "orphan", orphans, func(op *kinds.IntegrationOperation) {
		op.Spec.Record, op.Spec.Receipt = orphan, nil
		op.Spec.After.DNSName = orphan.Key.Name
		op.Spec.AfterHash = kinds.EndpointDigest(op.Spec.After)
	})
	integrationSeedOperations(t, f.s, template, "previous", foreign, func(op *kinds.IntegrationOperation) { op.Spec.Record.Owner.UID = "uid-previous" })
	kept := map[string]string{}
	pages := 0
	for continuation := ""; ; {
		page, err := f.s.opts.Store.List(t.Context(), kinds.KindIntegrationOperation, store.ListOptions{Limit: 256, Continue: continuation})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, obj := range page.Items {
			if op := obj.(*kinds.IntegrationOperation); op.Spec.Record.Owner != f.owner {
				encoded, _ := json.Marshal(op)
				kept[op.Name] = string(encoded)
			}
		}
		if continuation = page.Continue; continuation == "" {
			break
		}
	}
	if pages < 4 || len(kept) != foreign {
		t.Fatalf("the history spans %d stored pages and %d foreign rows", pages, len(kept))
	}
	_, writes, _ := f.traffic()
	f.requestDelete(t)
	// A store of this size makes every root capture and command slower than
	// the fixture's usual patience allows under the race detector.
	last := ""
	for until := time.Now().Add(5 * time.Minute); integrationStored(t, f.s, f.d) != nil; time.Sleep(20 * time.Millisecond) {
		if phase, reason := f.status(); phase+"/"+reason != last {
			last = phase + "/" + reason
			t.Logf("status %s", last)
		}
		if time.Now().After(until) {
			t.Fatalf("the cleanup did not finalize; status %s", last)
		}
	}
	_, after, deleted := f.traffic()
	slices.Sort(deleted)
	if !slices.Equal(deleted, f.published) || after-writes != 1 {
		t.Fatalf("delete effects named %v in %d batches; published were %v", deleted, after-writes, f.published)
	}
	if rows := integrationRows(t, f.s, kinds.KindIntegrationLedger); len(rows) != 0 {
		t.Fatalf("%d ledger rows remain", len(rows))
	}
	// Only the previous incarnation's operations are left, unchanged.
	left := integrationRows(t, f.s, kinds.KindIntegrationOperation)
	if len(left) != foreign {
		t.Fatalf("%d operation rows remain, %d belong to the previous incarnation", len(left), foreign)
	}
	for _, obj := range left {
		encoded, _ := json.Marshal(obj)
		if kept[obj.GetName()] != string(encoded) {
			t.Fatal("the cleanup changed or removed an operation of another incarnation", obj.GetName())
		}
	}
	summary := ""
	for _, entry := range integrationAudit(t, f.s, f.d) {
		if entry.Action == "integration.cleanup" {
			summary = entry.Reason
		}
	}
	// Every operation of this incarnation is forgotten: the seeded history,
	// the published record's operation and the one delete intent.
	if !strings.Contains(summary, "forgot 1 ledger rows and "+strconv.Itoa(settled+orphans+2)+" operations") {
		t.Fatalf("cleanup summary: %q", summary)
	}
}
