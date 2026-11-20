package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store/follower"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
)

func newClusterResolveAnchorRuntime(t *testing.T, r resolutionReceiver, st *sqlite.Store, root *anchorHistoryAuthority) *servingRuntime {
	t.Helper()
	cfg, err := config.FromFlat(config.Flatten(r.primary.opts.Config))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.IdentityProviders = nil
	cfg.Node.Name, cfg.Node.DataDir = "receiver", r.dir
	cfg.Upstreams.Recursion.ManagedTopology = "direct"
	cfg.Upstreams.Recursion.AnchorMode = config.RootAnchorsManaged
	cfg.Upstreams.Recursion.TrustAnchors = root.seeds
	cfg.Upstreams.Recursion.RootHints = []string{root.endpoint}
	cfg.Upstreams.Recursion.IPv6 = "off"
	cfg.EnvLocked = append(cfg.EnvLocked, "upstreams.recursion.anchorMode", "upstreams.recursion.trustAnchors", "upstreams.recursion.rootHints", "upstreams.recursion.ipv6")
	opts := r.primary.opts
	opts.Config, opts.Store = cfg, st
	opts.Transferred = servingbuild.NewTransferredRuntime(nil, nil, nil)
	s, err := newServingRuntime(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// recordedAnchorHistorySource retains the exact immutable response image that
// the production puller validated and installed. Reading the live primary a
// second time would compare against a later writer cut instead of this pull.
type recordedAnchorHistorySource struct {
	source follower.Source
	image  *replication.Image
}

func (s *recordedAnchorHistorySource) Fetch(ctx context.Context, etag string) (follower.Response, error) {
	response, err := s.source.Fetch(ctx, etag)
	if err == nil {
		s.image = response.Image
	}
	return response, err
}

func runClusterResolveAnchorRuntime(t *testing.T, s *servingRuntime) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error("runtime completion", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("runtime did not join")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func electClusterResolveAnchorWriter(t *testing.T, s *servingRuntime) (*anchorHistoryWriter, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	ready := make(chan *anchorHistoryWriter, 1)
	done := make(chan error, 1)
	go func() {
		done <- s.opts.Store.Elector("cluster-resolve-anchor-test").Run(ctx, func(writer context.Context) {
			carried, ok := sqlite.WriterTokenFromContext(writer)
			if !ok {
				t.Error("elector omitted writer token")
				return
			}
			target := serving.TermActivation{Term: carried.Term(), Incarnation: carried.Incarnation()}
			if _, err := s.opts.Store.BindAnchorLifecycle(writer); err != nil {
				t.Error(err)
				return
			}
			host, err := s.resetWriter(writer, target)
			if err != nil {
				t.Error(err)
				return
			}
			if host.Release != nil {
				defer host.Release()
			}
			for {
				err = s.anchorPin(writer, primaryruntime.PublicationFence{Term: target.Term, Incarnation: target.Incarnation}, func(p currentguard.Pinned) error {
					if p.Current == nil || !p.Current() {
						return serving.ErrDependency
					}
					return nil
				})
				if err == nil {
					break
				}
				select {
				case <-writer.Done():
					t.Error("writer dependency pin unavailable", err)
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			select {
			case ready <- newAnchorHistoryWriter(writer, t, s, target, host):
			case <-writer.Done():
				return
			}
			<-writer.Done()
		}, nil)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error("writer completion", err)
			}
		})
	}
	t.Cleanup(stop)
	select {
	case writer := <-ready:
		return writer, stop
	case <-ctx.Done():
		t.Fatal("anchor writer did not start", ctx.Err())
		return nil, stop
	}
}

// A member whose branch retained a signed REVOKE abandons the newer term of a
// promoted member that missed it. The actual offline CLI starts a new term on
// the kept branch. When the lost primary returns through the production
// puller, its trust-restoring branch is quarantined and it follows the kept
// revocation before it can publish again.
func TestClusterResolveAbandonedTermMemberReturnsThroughAnchorPuller(t *testing.T) {
	root := newAnchorHistoryAuthority(t)
	r := joinResolutionReceiver(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)

	st, node, err := openOfflineMember(resolveAbandonOffline(t), r.dir)
	if err != nil || node != "receiver" {
		t.Fatal("open joined receiver", err, node)
	}
	firstStore := st
	var closeFirstStoreOnce sync.Once
	closeFirstStore := func() {
		closeFirstStoreOnce.Do(func() {
			if err := firstStore.Close(); err != nil {
				t.Error("close fixture store", err)
			}
		})
	}
	t.Cleanup(closeFirstStore)
	a := st.Authority()
	if _, err := st.Promote(ctx, replication.PromoteRequest{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed, RequestID: "abandon-anchor-fixture"}); err != nil {
		t.Fatal("promote kept member for fixture setup", err)
	}
	s := newClusterResolveAnchorRuntime(t, r, st, root)
	firstRuntime := s
	var closeFirstRuntimeOnce sync.Once
	closeFirstRuntime := func() {
		closeFirstRuntimeOnce.Do(func() {
			closing, closed := context.WithTimeout(context.Background(), 10*time.Second)
			defer closed()
			if err := firstRuntime.Close(closing); err != nil {
				t.Error("close fixture runtime", err)
			}
		})
	}
	t.Cleanup(closeFirstRuntime)
	stopRuntime := runClusterResolveAnchorRuntime(t, s)
	writer, stopWriter := electClusterResolveAnchorWriter(t, s)
	shared := writer.operate(t, root, false)
	lost := newAnchorFollowerFixture(ctx, t, s)
	root.revoke.Store(true)
	revoked := writer.operate(t, root, true)
	kept := anchorHistoryProjection(ctx, t, s)
	if shared.Generation+1 != revoked.Generation || kept.Disposition() != resolverstate.Usable || len(kept.RevokedIDs()) != 1 || !reflect.DeepEqual(kept.DS(), []string{root.kept}) {
		t.Fatal("kept branch did not revoke exactly the abandoned trust")
	}
	abandoned := anchorHistoryPromote(ctx, t, lost)
	lostBranch := anchorFollowerImage(ctx, t, lost.store)
	if !lostBranch.Term.Same(abandoned.Term) {
		t.Fatal("promoted branch is not the abandoned term", lostBranch.Term.Epoch, abandoned.Term.Epoch)
	}
	// End the writer and runtime before fencing the member. A running fenced
	// runtime deliberately starts its deny-only supervisor; that is a separate
	// acquisition from the abandoned-term return this test attributes exactly.
	stopWriter()
	stopRuntime()
	if _, err := st.ObserveAuthority(ctx, abandoned.Term); err != nil {
		t.Fatal("kept member did not observe the lost term", err)
	}
	if authority := st.Authority(); authority.Mode != replication.Fenced || !authority.Term.Same(abandoned.Term) {
		t.Fatal("kept member was not fenced by the lost term", authority.Mode)
	}
	closeFirstRuntime()
	closeFirstStore()

	review, reviewPath := r.review()
	if !review.Resolvable || review.Reason != resolutionFencedBehindTerm || !review.Term.Same(abandoned.Term) {
		t.Fatal("offline review did not identify the abandoned term", review.Mode, review.Reason)
	}
	raw, err := r.run("resolve", "--offline", "--data-dir", r.dir, "--review", reviewPath, "--confirm-node", "receiver", "--request-id", "abandon-anchor-return",
		"--reason", "primary and followers lost", "--abandon-term", strconv.FormatUint(abandoned.Term.Epoch, 10))
	if err != nil {
		t.Fatal("abandonment of the lost term failed", err)
	}
	var receipt clusterResolutionReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil || !receipt.Committed || receipt.Term.Epoch != abandoned.Term.Epoch+1 {
		t.Fatal("abandonment did not commit a new term", err, raw)
	}

	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	opening, opened := context.WithTimeout(ctx, 10*time.Second)
	st, err = sqlite.OpenExisting(opening, filepath.Join(r.dir, sqlite.FileName), sqlite.Options{Scheme: scheme, Validator: r.primary.opts.Validator,
		InternalKinds: privateStoreKinds(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	opened()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s = newClusterResolveAnchorRuntime(t, r, st, root)
	run := runClusterResolveAnchorRuntime(t, s)
	writerCtx, cancelWriter := context.WithCancel(ctx)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- st.Elector("receiver").Run(writerCtx, func(term context.Context) {
			if err := s.RunWriter(term); err != nil && term.Err() == nil {
				t.Error("writer ended early", err)
			}
		}, nil)
	}()
	var stopPrimaryWriterOnce sync.Once
	stopPrimaryWriter := func() {
		stopPrimaryWriterOnce.Do(func() {
			cancelWriter()
			if err := <-writerDone; err != nil && !errors.Is(err, context.Canceled) {
				t.Error("writer completion", err)
			}
		})
	}
	t.Cleanup(func() {
		stopPrimaryWriter()
		run()
		closing, closed := context.WithTimeout(context.Background(), 10*time.Second)
		defer closed()
		if err := s.Close(closing); err != nil {
			t.Error(err)
		}
	})
	awaitServingRuntime(t, s, func() bool {
		ready, _ := s.ready()
		return ready && s.publisher.Status().ProtectedReady && s.primary.driver.Status().Running
	})
	primaryMetrics := anchorStatusMetrics(t, s)
	primaryStatus, err := s.rootAnchorStatus(ctx)
	statusDeadline := time.Now().Add(10 * time.Second)
	for err == nil && (primaryStatus.Updater.State != anchorWorkerRunning || primaryStatus.Updater.Next == nil || primaryStatus.Owner.NextRefresh == nil ||
		!primaryStatus.Updater.Next.Equal(*primaryStatus.Owner.NextRefresh) || !primaryStatus.Updater.Next.After(time.Now().Add(30*time.Minute))) {
		if root.reads.Load() != 2 {
			t.Fatal("restarted primary acquired while settling its retained schedule", root.reads.Load())
		}
		if time.Now().After(statusDeadline) {
			t.Fatalf("restarted primary updater did not settle on the retained schedule: updater=%+v owner=%+v", primaryStatus.Updater, primaryStatus.Owner)
		}
		time.Sleep(20 * time.Millisecond)
		primaryStatus, err = s.rootAnchorStatus(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	if anchorStatusGauge(t, primaryMetrics, "kudurru_root_anchor_worker_state", map[string]string{"worker": "updater", "state": anchorWorkerRunning}) != 1 ||
		anchorStatusGauge(t, primaryMetrics, "kudurru_root_anchor_worker_state", map[string]string{"worker": "supervisor", "state": anchorWorkerNotRunning}) != 1 {
		t.Fatal("restarted primary exported the wrong anchor workers")
	}
	for outcome, count := range anchorAcquisitionCounts(t, primaryMetrics) {
		if count != 0 {
			t.Fatal("restarted primary recorded an unexpected updater acquisition", outcome, count)
		}
	}
	if root.reads.Load() != 2 {
		t.Fatal("restarted primary made an unexpected root acquisition", root.reads.Load())
	}
	if projection := anchorHistoryProjection(ctx, t, s); !reflect.DeepEqual(projection.RevokedIDs(), kept.RevokedIDs()) || !reflect.DeepEqual(projection.DS(), []string{root.kept}) {
		t.Fatal("abandoned primary did not keep the revocation")
	}
	if _, err := lost.store.ObserveAuthority(ctx, receipt.Term); err != nil {
		t.Fatal("lost primary did not observe the replacement term", err)
	}
	source := &recordedAnchorHistorySource{source: anchorHistorySource{primary: func() *sqlite.Store { return st }}}
	puller := &follower.Puller{Mirror: lost.store, Source: source}
	if err := puller.Pull(ctx); err != nil {
		t.Fatal("lost primary did not return through the production puller", err)
	}
	adopted := anchorFollowerImage(ctx, t, lost.store)
	authority := lost.store.Authority()
	if source.image == nil {
		t.Fatal("production source returned no image")
	}
	validatedResponse, err := lost.store.ValidateImage(ctx, *source.image)
	if err != nil {
		t.Fatal("recorded production response is not valid", err)
	}
	expected := validatedResponse.Image()
	if authority.Mode != replication.Follower || !authority.Term.Same(receipt.Term) || !anchorFollowerSameImage(adopted, &expected) {
		t.Fatalf("returning member did not follow the validated kept response image: mode=%s termSame=%v generationSame=%v objectResourceVersionSame=%v kindManifestSame=%v payloadSame=%v adopted=%+v response=%+v",
			authority.Mode, authority.Term.Same(receipt.Term), adopted.Generation().Same(expected.Generation()), adopted.ObjectResourceVersion == expected.ObjectResourceVersion,
			reflect.DeepEqual(adopted.KindManifest, expected.KindManifest), reflect.DeepEqual(adopted.Payload, expected.Payload), adopted.Generation(), expected.Generation())
	}
	if projection := anchorHistoryProjection(ctx, t, lost.s); !reflect.DeepEqual(projection.RevokedIDs(), kept.RevokedIDs()) || !reflect.DeepEqual(projection.DS(), []string{root.kept}) {
		t.Fatal("returning member adopted abandoned trust")
	}
	quarantined, err := lost.store.ReadQuarantined(ctx, lostBranch.Generation())
	if err != nil {
		t.Fatal("lost term branch was not quarantined", err)
	}
	if !quarantined.Term.Same(abandoned.Term) || !anchorFollowerSameImage(quarantined, lostBranch) || !reflect.DeepEqual(quarantined.Payload, lostBranch.Payload) {
		t.Fatal("quarantine does not exactly retain the abandoned term branch")
	}
	abandonedOwner, err := accepted.DecodeRows(quarantined.Payload.Runtime, quarantined.Term.ClusterID)
	if err != nil || abandonedOwner.Version() != shared {
		t.Fatal("quarantine does not hold the abandoned trust branch", err)
	}
	abandonedTrust, err := resolverstate.Project(abandonedOwner)
	if err != nil || len(abandonedTrust.RevokedIDs()) != 0 || len(abandonedTrust.DS()) != 2 {
		t.Fatal("quarantined branch is not the trust-restoring abandoned branch", err)
	}
	if root.reads.Load() != 2 {
		t.Fatal("production pull caused a root acquisition", root.reads.Load())
	}
	for outcome, count := range anchorAcquisitionCounts(t, primaryMetrics) {
		if count != 0 {
			t.Fatal("active primary acquired while serving the pull", outcome, count)
		}
	}

	returningMetrics := anchorStatusMetrics(t, lost.s)
	if anchorStatusGauge(t, returningMetrics, "kudurru_root_anchor_worker_state", map[string]string{"worker": "updater", "state": anchorWorkerNotRunning}) != 1 ||
		anchorStatusGauge(t, returningMetrics, "kudurru_root_anchor_worker_state", map[string]string{"worker": "supervisor", "state": anchorWorkerNotRunning}) != 1 {
		t.Fatal("unmounted returning member exported a running anchor worker")
	}
	runServingFixture(t, lost.s)
	anchorHistoryAwait(t, "returning member did not publish the adopted revocation", lost.s, root, func() bool {
		status := lost.s.publisher.Status()
		return status.ProtectedReady && !status.DNSOnly && root.reads.Load() == 3
	})
	returningStatus, err := lost.s.rootAnchorStatus(ctx)
	if err != nil || returningStatus.Updater.State != anchorWorkerNotRunning || returningStatus.Supervisor.State != anchorWorkerRunning {
		t.Fatalf("returned follower mounted the wrong anchor worker: err=%v updater=%+v supervisor=%+v", err, returningStatus.Updater, returningStatus.Supervisor)
	}
	if anchorStatusGauge(t, returningMetrics, "kudurru_root_anchor_worker_state", map[string]string{"worker": "updater", "state": anchorWorkerNotRunning}) != 1 ||
		anchorStatusGauge(t, returningMetrics, "kudurru_root_anchor_worker_state", map[string]string{"worker": "supervisor", "state": anchorWorkerRunning}) != 1 {
		t.Fatal("returned follower did not export its supervisor")
	}
	seal, dnsOnly, err := anchorHistoryPublished(ctx, t, lost.s)
	if err != nil || seal == nil || seal.managed == nil || seal.static != nil || !seal.current() || dnsOnly || !seal.base.Same(adopted.Generation()) {
		t.Fatal("returning member did not serve the adopted kept branch", err)
	}
	if root.reads.Load() != 3 {
		t.Fatal("abandon and return used an unexpected number of root acquisitions", root.reads.Load())
	}
}
