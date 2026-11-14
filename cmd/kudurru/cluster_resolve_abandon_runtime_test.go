package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// F28-HISTORY: after `cluster resolve --offline --abandon-term` the member is
// the primary of a new term over its own branch. This starts an actual
// serving runtime on that database with the store's actual elector (the
// writer episode, the primary driver and publication), and requires it to
// run as that new term's writer: node readiness, protected publication, a
// running primary driver, a writer token for exactly the committed term,
// and a write that commits under it. The durable
// authority is unchanged by the start.
func TestClusterResolveAbandonedMemberRunsAsTheNewPrimary(t *testing.T) {
	r := joinResolutionReceiver(t)
	s, node, err := openOfflineMember(resolveAbandonOffline(t), r.dir)
	if err != nil {
		t.Fatal(err)
	}
	installed := s.Authority().Installed
	lost := installed.Term
	lost.Epoch++
	lost.PromotionID, lost.PrimaryID, lost.PrimaryName = uuid.NewString(), r.primary.opts.Store.Authority().MemberID, "lost-primary"
	if _, err := s.ObserveAuthority(resolveAbandonOffline(t), lost); err != nil || s.Authority().Mode != replication.Fenced {
		t.Fatal("fixture member was not fenced by the newer term", err)
	}
	if err := s.Close(); err != nil || node != "receiver" {
		t.Fatal(err, node)
	}
	_, path := r.review()
	raw, err := r.run("resolve", "--offline", "--data-dir", r.dir, "--review", path, "--confirm-node", "receiver", "--request-id", "abandon-run",
		"--reason", "primary and followers lost", "--abandon-term", strconv.FormatUint(lost.Epoch, 10))
	if err != nil {
		t.Fatal("abandonment of the lost term failed", err)
	}
	var receipt clusterResolutionReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil || !receipt.Committed || receipt.Term.Epoch != lost.Epoch+1 {
		t.Fatal("abandonment did not commit a new term", err, raw)
	}
	before := r.durable()

	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	opening, opened := context.WithTimeout(t.Context(), 10*time.Second)
	st, err := sqlite.OpenExisting(opening, filepath.Join(r.dir, sqlite.FileName), sqlite.Options{Scheme: scheme, Validator: r.primary.opts.Validator,
		InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	opened()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if a := st.Authority(); a.Mode != replication.Primary || !a.Term.Same(receipt.Term) {
		t.Fatal("abandoned member did not reopen as the new term's primary", a.Mode)
	}
	cfg, err := config.FromFlat(config.Flatten(r.primary.opts.Config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.IdentityProviders = nil
	cfg.Node.Name, cfg.Node.DataDir = "receiver", r.dir
	o := r.primary.opts
	o.Config, o.Store = cfg, st
	o.Transferred = servingbuild.NewTransferredRuntime(nil, nil, nil)
	startup, finish := context.WithTimeout(t.Context(), 10*time.Second)
	runtime, err := newServingRuntime(startup, o)
	finish()
	if err != nil {
		t.Fatal("abandoned member's runtime did not start", err)
	}
	run, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(run) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("runtime completion", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("runtime did not join")
		}
		closing, closed := context.WithTimeout(context.Background(), 10*time.Second)
		defer closed()
		if err := runtime.Close(closing); err != nil {
			t.Error(err)
		}
	}()
	// The store's actual elector carries the writer token into RunWriter, as
	// runServe mounts it.
	writerCtx, cancelWriter := context.WithCancel(t.Context())
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- st.Elector("receiver").Run(writerCtx, func(ctx context.Context) {
			if err := runtime.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error("writer ended early", err)
			}
		}, nil)
	}()
	defer func() {
		cancelWriter()
		if err := <-writerDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("writer shutdown: %v", err)
		}
	}()
	awaitServingRuntime(t, runtime, func() bool {
		ready, _ := runtime.ready()
		return ready && runtime.publisher.Status().ProtectedReady && runtime.primary.driver.Status().Running
	})
	token, err := st.WriterToken()
	if err != nil || !token.Term().Same(receipt.Term) {
		t.Fatal("the running writer does not hold the new term", err)
	}
	if status := runtime.publisher.Status(); !status.Authority.Term.Same(receipt.Term) || status.DNSOnly {
		t.Fatalf("publication is not the new term's complete bundle: %+v", status)
	}
	anchorOrdinaryWrite(sqlite.WithWriterToken(t.Context(), token), t, st, receipt.Term, "after-abandonment")
	if after := st.Authority(); after.Mode != replication.Primary || !after.Term.Same(before.Term) || after.Conflict != nil || after.Divergence != nil {
		t.Fatal("the runtime changed the abandoned member's durable authority", after.Mode)
	}
}
