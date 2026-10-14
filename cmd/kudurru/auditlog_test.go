package main

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/audit/auditlog"
	"github.com/robertherbaugh/kudurru/internal/audit/spool"
)

func TestAuditSealKeysRotation(t *testing.T) {
	current, previous := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	keys, err := auditSealKeys(current, previous)
	if err != nil {
		t.Fatal(err)
	}
	wantCurrent, _ := auditlog.SealKey(current)
	wantPrevious, _ := auditlog.SealKey(previous)
	if len(keys) != 2 || !bytes.Equal(keys[0], wantCurrent) || !bytes.Equal(keys[1], wantPrevious) {
		t.Fatal("writers must use the current seal key and readers must accept the previous one")
	}
	keys, err = auditSealKeys(current, nil)
	if err != nil || len(keys) != 1 || !bytes.Equal(keys[0], wantCurrent) {
		t.Fatalf("without previous key: %v", err)
	}
}

func TestOpenAuditLogStartupDeadline(t *testing.T) {
	// Discovery does not use the List context. A caller deadline would mask
	// losing the separate HTTP timeout on the constructor's REST mapper.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	release := make(chan struct{})
	cancelled := make(chan string, 10)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			select {
			case cancelled <- r.URL.Path:
			default:
			}
		case <-release:
			// Unblock even an unbounded discovery client on test failure.
			http.Error(w, "test shutdown", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(func() {
		cancel()
		close(release)
		api.Close()
	})
	sp, err := spool.Open(t.Context(), spool.Options{Path: filepath.Join(t.TempDir(), spool.FileName), Node: "timeout-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	cfg := &rest.Config{Host: api.URL} // No transport timeout from the caller.
	type result struct {
		store *auditLogStore
		err   error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		store, err := openAuditLog(ctx, cfg, sp, bytes.Repeat([]byte{1}, 32), nil, slog.New(slog.DiscardHandler), 100*time.Millisecond)
		done <- result{store: store, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.store == nil {
			t.Fatalf("API outage must retain a retryable audit store: store=%v, err=%v", got.store, got.err)
		}
		if got.store.shipper == nil || got.store.shipper.Source != sp || got.store.shipper.Target == nil || got.store.reader == nil {
			t.Fatal("outage store lost its shipper, source, target or reader")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("openAuditLog did not bound black-holed discovery without a caller deadline")
	}
	if cfg.Timeout != 0 {
		t.Fatal("constructor changed the caller's ordinary audit transport timeout")
	}
	select {
	case path := <-cancelled:
		if path != "/api" && path != "/apis" {
			t.Fatalf("expected discovery request cancellation, got %s", path)
		}
		t.Logf("black-holed discovery cancelled at %s; retryable constructor returned in %s", path, time.Since(start))
	case <-time.After(time.Second):
		t.Fatal("no black-holed discovery request was cancelled")
	}
}

func TestOpenAuditLogPreviousKeyRotation(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		dirs, err := filepath.Glob(filepath.Join("..", "..", "bin", "k8s", "*-"+runtime.GOOS+"-"+runtime.GOARCH))
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(dirs)
		if len(dirs) == 0 {
			t.Skip("envtest binaries unavailable; run make envtest or set KUBEBUILDER_ASSETS")
		}
		assets = dirs[len(dirs)-1]
	}
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary)) //nolint:gosec // G703: trusted test binary directory from KUBEBUILDER_ASSETS or the repository
		if os.IsNotExist(err) {
			t.Skip("envtest binaries unavailable; run make envtest or set KUBEBUILDER_ASSETS")
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() {
			t.Fatalf("envtest %s is a directory", binary)
		}
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "audit")},
		ErrorIfCRDPathMissing: true,
	}
	args := env.ControlPlane.GetAPIServer().Configure()
	args.Set("advertise-address", "127.0.0.1")
	args.Set("endpoint-reconciler-type", "none")
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("envtest stop: %v", err)
		}
	})
	path := filepath.Join(t.TempDir(), spool.FileName)
	sp, err := spool.Open(t.Context(), spool.Options{Path: path, Node: "rotation-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	open := func(current, previous []byte) *auditLogStore {
		t.Helper()
		store, err := openAuditLog(t.Context(), cfg, sp, current, previous, slog.New(slog.DiscardHandler), 2*time.Second)
		if err != nil || store == nil {
			t.Fatalf("openAuditLog: store=%v, err=%v", store, err)
		}
		return store
	}
	ship := func(store *auditLogStore, reason string) {
		t.Helper()
		if err := sp.Record(t.Context(), audit.Event{Action: "dnszone.update", Result: audit.ResultSuccess, Reason: reason}); err != nil {
			t.Fatal(err)
		}
		after, err := store.shipper.Target.Cursor(t.Context(), sp)
		if err != nil {
			t.Fatal(err)
		}
		events, err := sp.ReadAfter(t.Context(), after, 10)
		if err != nil || len(events) != 1 {
			t.Fatalf("new batch events: %d, %v", len(events), err)
		}
		if err := store.shipper.Target.Append(t.Context(), sp, events); err != nil {
			t.Fatal(err)
		}
	}
	oldKey, newKey := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	old := open(oldKey, nil)
	ship(old, "before rotation")
	if err := old.reader.Verify(t.Context()); err != nil {
		t.Fatalf("original sealed batch: %v", err)
	}
	rotated := open(newKey, oldKey)
	ship(rotated, "after rotation")
	var batches auditv1.AuditLogList
	if err := rotated.client.List(t.Context(), &batches, client.InNamespace(rotated.reader.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(batches.Items) != 2 {
		t.Fatalf("expected two retained sealed batches, got %d", len(batches.Items))
	}
	slices.SortFunc(batches.Items, func(a, b auditv1.AuditLog) int {
		return cmp.Compare(a.Spec.FirstID, b.Spec.FirstID)
	})
	first, second := batches.Items[0].Spec, batches.Items[1].Spec
	if first.FirstID != 1 || second.FirstID != 2 || first.Seal == "" || second.Seal == "" || second.PrevHash != first.Hash {
		t.Fatal("old/new batches must be sealed and linked across rotation")
	}
	if err := rotated.reader.Verify(t.Context()); err != nil {
		t.Fatalf("constructor must verify retained old/new sealed batches: %v", err)
	}
	if err := open(newKey, nil).reader.Verify(t.Context()); !errors.Is(err, audit.ErrTampered) {
		t.Fatalf("old batch must fail without the previous key: %v", err)
	}
	if err := old.reader.Verify(t.Context()); !errors.Is(err, audit.ErrTampered) {
		t.Fatalf("new batch must fail with only the old key: %v", err)
	}
	// Leave the third event unshipped: the constructor's verifier must
	// cover the real local spool as well as the two valid remote batches.
	if err := sp.Record(t.Context(), audit.Event{Action: "dnszone.update", Result: audit.ResultSuccess, Reason: "pending original"}); err != nil {
		t.Fatal(err)
	}
	after, err := rotated.shipper.Target.Cursor(t.Context(), sp)
	if err != nil || after != 2 {
		t.Fatalf("remote cursor before pending corruption: %d, %v", after, err)
	}
	pending, err := sp.ReadAfter(t.Context(), after, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("unshipped events: %d, %v", len(pending), err)
	}
	if err := rotated.reader.Verify(t.Context()); err != nil {
		t.Fatalf("valid sealed batches and pending spool: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	res, err := db.ExecContext(t.Context(), `UPDATE event SET doc = json_set(doc, '$.reason', 'tampered') WHERE id = ?`, pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("corrupt exactly one pending event: %d, %v", n, err)
	}
	if err := rotated.reader.Verify(t.Context()); !errors.Is(err, audit.ErrTampered) {
		t.Fatalf("constructor reader missed corrupted pending spool: %v", err)
	}
	t.Log("constructor verifies old/new sealed batches, rejects missing rotation keys, and detects unshipped spool corruption")
}
