package objects

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// cleanupHarness is the file reconciler over a real SQLite store with one
// Integration document, which is how an Integration exists on SQLite. The
// memory driver has neither admission nor deletion timestamps, so the
// reconciler's part of the cleanup finalizer contract (D367) is only visible
// on the real driver.
type cleanupHarness struct {
	st   *sqlite.Store
	dir  string
	file string
	logs *bytes.Buffer
	key  store.Key
}

func newCleanupHarness(t *testing.T) *cleanupHarness {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), sqlite.FileName), sqlite.Options{Validator: testValidator(t), Identity: "test-node"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dir := filepath.Join(t.TempDir(), "objects.d")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	return &cleanupHarness{st: st, dir: dir, file: filepath.Join(dir, "integration.yaml"), logs: &bytes.Buffer{}, key: nsKey("Integration", "apps", "cleanup")}
}

// loader returns a new Loader on the harness's store and directory: a process
// start, with no memory of earlier passes. Load is synchronous here (no
// watcher, no poll), so the log buffer needs no lock.
func (h *cleanupHarness) loader(t *testing.T) *Loader {
	t.Helper()
	l, err := New(Options{Store: h.st, Validator: testValidator(t), Dir: h.dir, PollInterval: -1, Logger: slog.New(slog.NewTextHandler(h.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// declare writes the Integration document with the given deletionPolicy line
// ("" omits the line); remove deletes the document.
func (h *cleanupHarness) declare(t *testing.T, policy string) {
	t.Helper()
	doc := integrationDocument(policy)
	if err := os.WriteFile(h.file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *cleanupHarness) remove(t *testing.T) {
	t.Helper()
	if err := os.Remove(h.file); err != nil {
		t.Fatal(err)
	}
}

func integrationDocument(policy string) string {
	doc := "apiVersion: kudurru.io/v1alpha1\nkind: Integration\nmetadata:\n  name: cleanup\n  namespace: apps\nspec:\n  type: unifi\n" +
		"  credentials:\n    apiKey:\n      file: /run/secrets/unifi-api-key\n  unifi:\n    endpoint: https://controller.invalid\n"
	if policy != "" {
		doc += "  deletionPolicy: " + policy + "\n"
	}
	return doc
}

// stored returns the Integration, or nil when it does not exist.
func (h *cleanupHarness) stored(t *testing.T) *v1alpha1.Integration {
	t.Helper()
	return get(t, h.st, h.key, &v1alpha1.Integration{})
}

// load runs one pass and fails on any error diagnosis.
func (h *cleanupHarness) load(t *testing.T, l *Loader, what string) Report {
	t.Helper()
	rep := l.Load(context.Background())
	if rep.Count(SeverityError) != 0 || rep.Count(SeverityWarning) != 0 {
		t.Fatalf("%s: %v", what, rep.Diagnoses)
	}
	return rep
}

// logged counts the log records whose message is msg.
func (h *cleanupHarness) logged(msg string) int {
	return strings.Count(h.logs.String(), `msg="`+msg+`"`)
}

// Removing the document of an armed Integration is one delete request. The
// object then waits for cleanup with its deletion timestamp, possibly for a
// long time, and every later pass (each poll and file event) must leave it
// alone: no second request, no "deleted" count, metric or log line.
func TestReconcileRequestsDeletionOfATerminatingObjectOnce(t *testing.T) {
	h := newCleanupHarness(t)
	l := h.loader(t)
	h.declare(t, "Delete")
	if rep := h.load(t, l, "armed declaration"); rep.Created != 1 {
		t.Fatalf("created=%d", rep.Created)
	}
	armed := h.stored(t)
	if armed == nil || !store.HasFinalizer(armed, store.IntegrationCleanupFinalizer) {
		t.Fatal("a declared Delete policy was stored without its cleanup finalizer")
	}
	h.remove(t)
	if rep := h.load(t, l, "document removal"); rep.Deleted != 1 {
		t.Fatalf("document removal: deleted=%d, want the one delete request", rep.Deleted)
	}
	deleting := h.stored(t)
	if deleting == nil || deleting.DeletionTimestamp == nil || !store.HasFinalizer(deleting, store.IntegrationCleanupFinalizer) || deleting.UID != armed.UID {
		t.Fatalf("the armed object must stay, terminating: %+v", deleting)
	}
	audited := func() int {
		t.Helper()
		entries, err := h.st.ReadAudit(context.Background(), sqlite.AuditQuery{Kind: "Integration", Name: "cleanup"})
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	rows := audited()
	for pass := 1; pass <= 3; pass++ {
		rep := h.load(t, l, "pass over the terminating object")
		if rep.Deleted != 0 || rep.Created+rep.Updated+rep.Unchanged+rep.Skipped != 0 {
			t.Fatalf("pass %d over the terminating object reported deleted=%d created=%d updated=%d unchanged=%d skipped=%d, want nothing",
				pass, rep.Deleted, rep.Created, rep.Updated, rep.Unchanged, rep.Skipped)
		}
	}
	// A new process finds the same terminating object and does the same.
	if rep := h.load(t, h.loader(t), "pass after a restart"); rep.Deleted != 0 {
		t.Fatalf("pass after a restart reported deleted=%d", rep.Deleted)
	}
	if got := h.stored(t); got == nil || got.ResourceVersion != deleting.ResourceVersion {
		t.Fatal("a later pass changed the terminating object")
	}
	if got := testutil.ToFloat64(l.m.applied.WithLabelValues(ResultDeleted)); got != 1 {
		t.Fatalf("kudurru_objects_applied_total{result=deleted} = %v, want 1", got)
	}
	if got := h.logged("objects: deleted"); got != 1 {
		t.Fatalf("%d \"objects: deleted\" log lines for one delete request:\n%s", got, h.logs)
	}
	if got := strings.Count(h.logs.String(), "deleted=1"); got != 1 {
		t.Fatalf("%d load summaries report a deletion:\n%s", got, h.logs)
	}
	if got := audited(); got != rows {
		t.Fatalf("later passes wrote %d audit rows", got-rows)
	}
	// The cleanup owner's release then removes it, and nothing is left to
	// request.
	if _, err := h.st.UpdateFinalizer(context.Background(), h.key, store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, Remove: true,
		ExpectedUID: string(deleting.UID), ExpectedResourceVersion: deleting.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	if rep := h.load(t, l, "pass after finalization"); rep.Deleted != 0 || h.stored(t) != nil {
		t.Fatalf("after finalization: deleted=%d", rep.Deleted)
	}
}

// Admission arms the cleanup finalizer on writes. A live Integration that is
// stored with Delete but without the finalizer (written before admission
// existed, or after a live UpdateFinalizer removal) must not stay unarmed
// behind an unchanged document: the next pass applies the document, which
// arms the object and changes nothing else.
func TestReconcileArmsAnUnarmedDeleteIntegrationFromItsUnchangedDocument(t *testing.T) {
	h := newCleanupHarness(t)
	l := h.loader(t)
	h.declare(t, "Delete")
	h.load(t, l, "armed declaration")
	disarm := func() *v1alpha1.Integration {
		t.Helper()
		cur := h.stored(t)
		out, err := h.st.UpdateFinalizer(context.Background(), h.key, store.FinalizerOptions{Finalizer: store.IntegrationCleanupFinalizer, Remove: true,
			ExpectedUID: string(cur.UID), ExpectedResourceVersion: cur.ResourceVersion})
		if err != nil || store.HasFinalizer(out, store.IntegrationCleanupFinalizer) || out.GetDeletionTimestamp() != nil {
			t.Fatalf("fixture: live removal: %v", err)
		}
		return h.stored(t)
	}
	for _, tc := range []struct {
		name   string
		loader func() *Loader
	}{
		// The loader that already agreed with the armed object.
		{"same process", func() *Loader { return l }},
		// A process start: no remembered agreement.
		{"after a restart", func() *Loader { return h.loader(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unarmed := disarm()
			cur := tc.loader()
			rep := h.load(t, cur, "unchanged document over an unarmed object")
			got := h.stored(t)
			if got == nil || !store.HasFinalizer(got, store.IntegrationCleanupFinalizer) {
				t.Fatalf("a live Delete Integration stayed unarmed behind its unchanged document: updated=%d unchanged=%d finalizers=%v", rep.Updated, rep.Unchanged, got.Finalizers)
			}
			if rep.Updated != 1 || rep.Unchanged != 0 {
				t.Fatalf("arming pass reported updated=%d unchanged=%d", rep.Updated, rep.Unchanged)
			}
			// The write is the finalizer and nothing else.
			want := unarmed.DeepCopy()
			want.Finalizers = []string{store.IntegrationCleanupFinalizer}
			want.ResourceVersion = got.ResourceVersion
			if !reflect.DeepEqual(got, want) || got.ResourceVersion == unarmed.ResourceVersion {
				t.Fatalf("arming changed more than metadata.finalizers:\n got %+v\nwant %+v", got, want)
			}
			// Armed, the unchanged document is unchanged again.
			if rep := h.load(t, cur, "pass over the armed object"); rep.Unchanged != 1 || rep.Updated != 0 || h.stored(t).ResourceVersion != got.ResourceVersion {
				t.Fatalf("pass over the armed object: updated=%d unchanged=%d", rep.Updated, rep.Unchanged)
			}
		})
	}
	// A terminating object is never armed again: once the cleanup owner has
	// cleared the finalizer the object is gone, and before that it has it.
	// Retain is never armed.
	h.declare(t, "Retain")
	h.load(t, l, "declared Retain")
	if rep := h.load(t, h.loader(t), "unchanged Retain document"); rep.Unchanged != 1 || rep.Updated != 0 || len(h.stored(t).Finalizers) != 0 {
		t.Fatalf("unchanged Retain document: updated=%d unchanged=%d", rep.Updated, rep.Unchanged)
	}
}

// A driver without admission never arms the finalizer itself, so there the
// extra apply of an unchanged Delete document cannot settle the object. It
// must then not repeat on every pass: the agreement the apply returns is
// remembered like any other. (The memory driver rewrites the object on every
// Apply, so the first pass counts as an update here; on Kubernetes the same
// apply is a no-op and counts as unchanged, which
// TestFileDeclaredDeleteIntegrationIsNotRewrittenWithoutAdmission in
// internal/store/kube proves on the real driver.)
func TestReconcileUnarmedDeleteIntegrationWithoutAdmissionIsAppliedOnce(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, "integration.yaml", integrationDocument("Delete"))
	if rep := f.l.Load(ctx(t)); rep.Created != 1 || rep.Count(SeverityError) != 0 {
		t.Fatalf("create: %+v", rep)
	}
	key := nsKey("Integration", "apps", "cleanup")
	created := get(t, f.st, key, &v1alpha1.Integration{})
	if created == nil || !store.ReservedFinalizerUnarmed(created) {
		t.Fatalf("fixture: the memory driver must store the Delete policy unarmed: %+v", created)
	}
	applies := &countingStore{Store: f.st}
	restarted, err := New(Options{Store: applies, Validator: testValidator(t), Dir: f.dir, PollInterval: -1, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 3; pass++ {
		rep := restarted.Load(ctx(t))
		if rep.Created != 0 || rep.Updated+rep.Unchanged != 1 || rep.Count(SeverityError) != 0 || (pass > 1 && rep.Unchanged != 1) {
			t.Fatalf("pass %d: created=%d updated=%d unchanged=%d %v", pass, rep.Created, rep.Updated, rep.Unchanged, rep.Diagnoses)
		}
	}
	if got := get(t, f.st, key, &v1alpha1.Integration{}); got.UID != created.UID || got.Generation != created.Generation || len(got.Finalizers) != 0 {
		t.Fatalf("the apply changed the object: %+v", got.ObjectMeta)
	}
	// One apply, on the first pass; afterwards the remembered agreement holds.
	if applies.n != 1 {
		t.Fatalf("%d applies of the unchanged document over three passes, want 1", applies.n)
	}
}

type countingStore struct {
	store.Store
	n int
}

func (c *countingStore) Apply(ctx context.Context, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
	c.n++
	return c.Store.Apply(ctx, obj, opts)
}

// Declaring a terminating Integration again with the default policy (the
// deletionPolicy line omitted) or with Retain releases it: the store removes
// the object and nothing is cleaned at the provider. That must be visible:
// the log names the finalization instead of an ordinary update, and the
// audit row says which policy ended the obligation.
func TestReconcileReleaseByDeclarationIsVisible(t *testing.T) {
	for name, policy := range map[string]string{"omitted": "", "explicit": "Retain"} {
		t.Run(name, func(t *testing.T) {
			h := newCleanupHarness(t)
			l := h.loader(t)
			h.declare(t, "Delete")
			h.load(t, l, "armed declaration")
			armed := h.stored(t)
			h.remove(t)
			h.load(t, l, "document removal")
			if got := h.stored(t); got == nil || got.DeletionTimestamp == nil {
				t.Fatal("fixture: the object is not terminating")
			}
			h.logs.Reset()
			h.declare(t, policy)
			rep := h.load(t, l, "release by declaration")
			if h.stored(t) != nil {
				t.Fatal("the declaration did not release the terminating object")
			}
			if rep.Updated != 1 || rep.Deleted != 0 || rep.Created != 0 {
				t.Fatalf("release pass: created=%d updated=%d deleted=%d", rep.Created, rep.Updated, rep.Deleted)
			}
			if h.logged("objects: finalized") != 1 || h.logged("objects: updated") != 0 {
				t.Fatalf("the release must be logged as a finalization, not as an update:\n%s", h.logs)
			}
			entries, err := h.st.ReadAudit(context.Background(), sqlite.AuditQuery{Kind: "Integration", Name: "cleanup"})
			if err != nil || len(entries) == 0 {
				t.Fatalf("audit: %v", err)
			}
			// The reconciler defaults a document before it applies it, so
			// the omitted line reaches the store as Retain.
			want := "released without cleanup: deletionPolicy is Retain; dropped " + store.IntegrationCleanupFinalizer
			if last := entries[len(entries)-1]; last.Action != "integration.finalize" || last.Reason != want || last.Manager != store.ManagerFile("objects.d/integration.yaml") {
				t.Fatalf("release audit row: %s by %s, reason %q, want %q", last.Action, last.Manager, last.Reason, want)
			}
			// The still-declared document then creates a new Retain incarnation.
			if rep := h.load(t, l, "declaration after release"); rep.Created != 1 {
				t.Fatalf("declaration after release: created=%d", rep.Created)
			}
			if next := h.stored(t); next == nil || next.UID == armed.UID || len(next.Finalizers) != 0 || next.DeletionTimestamp != nil {
				t.Fatalf("the new incarnation inherited cleanup state: %+v", next)
			}
		})
	}
}
