package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
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
	cfg := resolvedMemberConfig(t, r)
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
	failed := make(chan error, 1)
	done := make(chan error, 1)
	reportSetupFailure := func(err error) {
		select {
		case failed <- err:
		default:
		}
	}
	go func() {
		done <- s.opts.Store.Elector("cluster-resolve-anchor-test").Run(ctx, func(writer context.Context) {
			carried, ok := sqlite.WriterTokenFromContext(writer)
			if !ok {
				reportSetupFailure(errors.New("elector omitted writer token"))
				return
			}
			target := serving.TermActivation{Term: carried.Term(), Incarnation: carried.Incarnation()}
			if _, err := s.opts.Store.BindAnchorLifecycle(writer); err != nil {
				reportSetupFailure(fmt.Errorf("bind anchor lifecycle: %w", err))
				return
			}
			host, err := s.resetWriter(writer, target)
			if err != nil {
				reportSetupFailure(fmt.Errorf("reset writer: %w", err))
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
					reportSetupFailure(fmt.Errorf("writer dependency pin: %w", err))
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
	case err := <-failed:
		t.Fatal("anchor writer setup failed", err)
		return nil, stop
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
	lost.reopen(ctx, t)
	source := &recordedAnchorHistorySource{source: anchorHistorySource{primary: func() *sqlite.Store { return st }}}
	lost.s.member, err = clusterreplica.NewCoordinator(clusterreplica.CoordinatorOptions{
		Store: lost.store, Source: source, NodeName: lost.store.Authority().MemberID, Served: lost.s.publisher.Pin,
		WithOperator: lost.s.withPromotionAuthorization, ObservePull: lost.s.anchorStatus.observePull,
	})
	if err != nil {
		t.Fatal(err)
	}
	lost.s.memberPullStart = make(chan struct{})
	runClusterResolveAnchorRuntime(t, lost.s)
	_, stopLostWriter := electClusterResolveAnchorWriter(t, lost.s)
	lostBranch := anchorFollowerImage(ctx, t, lost.store)
	if !lostBranch.Term.Same(abandoned.Term) {
		t.Fatal("promoted branch is not the abandoned term", lostBranch.Term.Epoch, abandoned.Term.Epoch)
	}
	anchorHistoryAwait(t, "lost term did not publish its branch before returning", lost.s, root, func() bool {
		status := lost.s.publisher.Status()
		return status.ProtectedReady && status.Served.Base.Same(lostBranch.Generation())
	})
	if seal, dnsOnly, err := anchorHistoryPublished(ctx, t, lost.s); err != nil || dnsOnly || seal == nil || !seal.base.Same(lostBranch.Generation()) {
		t.Fatal("lost primary did not admit its own published branch before fencing", err)
	}
	if projection := anchorHistoryProjection(ctx, t, lost.s); len(projection.RevokedIDs()) != 0 || len(projection.DS()) != 2 {
		t.Fatal("lost primary did not start with both anchors")
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

	st = openResolvedMember(ctx, t, r)
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
	stopLostWriter()
	anchorHistoryAwait(t, "fenced lost primary did not retire on signed REVOKE before pulling", lost.s, root, func() bool {
		status := lost.s.publisher.Status()
		return status.Authority.Mode == replication.Fenced && !status.ProtectedReady && lost.s.anchorStatus.snapshot().supervisor.state == anchorWorkerRetired && root.reads.Load() == 3
	})
	assertRetiredAnchorHistoryDNS(ctx, t, lost.s)
	if source.image != nil || !anchorFollowerSameImage(anchorFollowerImage(ctx, t, lost.store), lostBranch) {
		t.Fatal("fenced member did not retire its abandoned trust before pulling the replacement")
	}
	if err := lost.s.member.Pull(ctx); err != nil {
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
	for outcome, count := range anchorAcquisitionCounts(t, primaryMetrics) {
		if count != 0 {
			t.Fatal("active primary acquired while serving the pull", outcome, count)
		}
	}

	returningMetrics := anchorStatusMetrics(t, lost.s)
	anchorHistoryAwait(t, "returning member did not publish the adopted revocation", lost.s, root, func() bool {
		status := lost.s.publisher.Status()
		return status.ProtectedReady && !status.DNSOnly && status.Served.Base.Same(adopted.Generation()) && root.reads.Load() == 4
	})
	returningStatus, err := lost.s.rootAnchorStatus(ctx)
	if err != nil || returningStatus.Updater.State != anchorWorkerNotRunning || returningStatus.Supervisor.State != anchorWorkerRunning || returningStatus.Pull == nil || returningStatus.Pull.Result != "installed" {
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
	if root.reads.Load() != 4 {
		t.Fatal("abandon and return used an unexpected number of root acquisitions", root.reads.Load())
	}
}
