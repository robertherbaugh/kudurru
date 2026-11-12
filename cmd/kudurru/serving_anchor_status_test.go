package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// anchorStatusGauge reads one kudurru_root_anchor_* sample from a registry.
func anchorStatusGauge(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	metrics:
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if labels[pair.GetName()] != pair.GetValue() {
					continue metrics
				}
			}
			return metric.GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s %v not exported", name, labels)
	return 0
}

func anchorStatusMetrics(t *testing.T, s *servingRuntime) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(newAnchorMetrics(s)); err != nil {
		t.Fatal(err)
	}
	var already prometheus.AlreadyRegisteredError
	if err := reg.Register(newAnchorMetrics(s)); !errors.As(err, &already) {
		t.Fatal("a second registration of the anchor metrics was not refused", err)
	}
	return reg
}

// F28-GATE-4 (D369): a complete but invalid root reply parks the elected
// updater for its writer term. The status projection and the metrics show
// it, with its fixed error category, while the owner stays dormant.
func TestAnchorStatusShowsTerminalUpdater(t *testing.T) {
	seed, _ := anchorWireSignedReply(t)
	endpoint, reads := anchorWireServer(t, "tcp", func([]byte) []byte { return []byte{1} })
	s, original, target, host := anchorOperationFixture(t, endpoint.String(), seed.DS)
	reg := anchorStatusMetrics(t, s)
	if got := anchorStatusGauge(t, reg, "kudurru_root_anchor_worker_state", map[string]string{"worker": "updater", "state": anchorWorkerNotRunning}); got != 1 {
		t.Fatal("updater is not reported as not running before it starts", got)
	}
	ctx, cancel := context.WithCancel(original)
	done := make(chan error, 1)
	go func() { done <- s.runAnchorWriter(ctx, target, host) }()
	waitAnchorWriter(t, s, true)
	if reads.Load() != 1 {
		t.Fatal("fixture did not make one acquisition", reads.Load())
	}
	status, err := s.rootAnchorStatus(original)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if status.Updater.State != anchorWorkerStopped || status.Updater.Error == "" || status.Updater.Since == nil || status.Owner.Phase != string(accepted.Dormant) || status.Pull != nil {
		cancel()
		t.Fatalf("terminal updater is not visible: %+v %+v", status.Updater, status.Owner)
	}
	if !slices.ContainsFunc(status.Warnings, func(w string) bool { return strings.HasPrefix(w, "the root-anchor updater stopped: ") }) {
		cancel()
		t.Fatal("terminal updater has no warning", status.Warnings)
	}
	if got := anchorStatusGauge(t, reg, "kudurru_root_anchor_worker_state", map[string]string{"worker": "updater", "state": anchorWorkerStopped}); got != 1 {
		cancel()
		t.Fatal("terminal updater is not exported", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal child did not join")
	}
	// The ended child keeps its last error category for the operator.
	status, err = s.rootAnchorStatus(original)
	if err != nil || status.Updater.State != anchorWorkerNotRunning || status.Updater.LastError == "" {
		t.Fatalf("ended updater lost its last error: %+v %v", status.Updater, err)
	}
}

// F28-GATE-4 (D369): a bootstrapped primary's projection is its committed
// owner: one Valid key with its DS tag, the next refresh about one minimum
// refresh interval ahead, no carrier denial, and a running updater that waits
// for that refresh.
func TestAnchorStatusShowsCommittedOwnerAndSchedule(t *testing.T) {
	endpoint, seeds, reads := anchorSupervisorServer(t, func(int32, *dns.Msg) {})
	s, original, target, host := anchorOperationFixtureTopology(t, endpoint, seeds, "direct")
	ctx, cancel := context.WithCancel(original)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.runAnchorWriter(ctx, target, host) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := s.rootAnchorStatus(original)
		if err != nil {
			t.Fatal(err)
		}
		if status.Owner.Phase == string(accepted.Managed) && status.Updater.State == anchorWorkerRunning && status.Updater.Next != nil && status.Updater.Next.After(time.Now().Add(30*time.Minute)) {
			owner := anchorOperationOwner(original, t, s).Version()
			if status.Owner.Digest != owner.Digest || status.Owner.Generation != owner.Generation || status.Owner.PureGeneration != owner.Pure.Generation ||
				len(status.Owner.Keys) != 1 || status.Owner.Keys[0].Status != "Valid" || status.Owner.Keys[0].KeyTag == 0 || status.Owner.Quarantined ||
				status.Owner.NextRefresh == nil || !status.Owner.NextRefresh.Equal(*status.Updater.Next) {
				t.Fatalf("projection is not the committed owner: %+v", status.Owner)
			}
			if status.Local == nil || status.Local.Retired || len(status.Local.Denied) != 0 || status.Local.OwnerGeneration != owner.Generation {
				t.Fatalf("projection carrier: %+v", status.Local)
			}
			if !status.AdmissionCurrent || status.AnchorMode != "managed" || len(status.Warnings) != 0 || reads.Load() != 1 {
				t.Fatalf("bootstrapped primary status: admission %v mode %q warnings %v reads %d", status.AdmissionCurrent, status.AnchorMode, status.Warnings, reads.Load())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("updater never reported the bootstrapped schedule: %+v %+v", status.Updater, status.Owner)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("updater did not join")
	}
}

// F28-GATE-4 (D369): a follower whose deny-only check verified a signed
// REVOKE shows a retired supervisor and a pending local denial: the key is
// denied in its carrier while its installed owner still trusts it.
func TestAnchorStatusShowsRetiredFollowerAndPendingDenial(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	endpoint, seeds, _ := anchorOperationServer(t, 2, func(n int) {
		if n == 2 {
			close(entered)
			<-release
		}
	})
	f, ctx, _ := anchorFollowerPollerFixture(t, endpoint, seeds)
	stop := startAnchorFollowerFixture(ctx, t, f)
	defer stop()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("follower never acquired")
	}
	running, err := f.s.rootAnchorStatus(ctx)
	if err != nil || running.Supervisor.State != anchorWorkerRunning || running.Authority.Mode != "follower" || running.Owner.Phase != string(accepted.Managed) {
		t.Fatalf("polling follower: %+v %+v %v", running.Supervisor, running.Authority, err)
	}
	unblock()
	awaitServingRuntime(t, f.s, func() bool { return !f.s.publisher.Status().ProtectedReady })
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := f.s.rootAnchorStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// The board and the carrier are separate reads; wait for both.
		if status.Supervisor.State == anchorWorkerRetired && status.Local != nil && status.Local.Retired {
			if status.Local == nil || !status.Local.Retired || len(status.Local.Denied) != 1 || status.Local.Denied[0].RevokedByOwner || status.Local.Denied[0].KeyTag != status.Owner.Keys[0].KeyTag {
				t.Fatalf("retired follower carrier: %+v", status.Local)
			}
			if len(status.Owner.Keys) != 1 || status.Owner.Keys[0].Status != "Valid" {
				t.Fatalf("the follower's canonical owner changed: %+v", status.Owner)
			}
			for _, want := range []string{"retired its output", "as the primary it would answer SERVFAIL"} {
				if !slices.ContainsFunc(status.Warnings, func(w string) bool { return strings.Contains(w, want) }) {
					t.Fatal("missing warning", want, status.Warnings)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor never reported its retirement: %+v", status.Supervisor)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The board keeps a refusal of the primary's owner, with when it began,
// across unreachable pulls, and ends it at the next install. Metrics follow.
func TestAnchorStatusBoardTracksAdoptionRefusal(t *testing.T) {
	var now time.Time
	s := &servingRuntime{}
	s.anchorStatus.now = func() time.Time { return now }
	reg := anchorStatusMetrics(t, s)
	refused := func(reason sqlite.AnchorRefusalReason) float64 {
		return anchorStatusGauge(t, reg, "kudurru_root_anchor_adoption_refused", map[string]string{"reason": string(reason)})
	}
	start := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	now = start
	s.anchorStatus.observePull(errors.Join(errors.New("context"), &sqlite.AnchorRefusal{Reason: sqlite.AnchorRefusedCarriedRevocation, Keys: []string{"k1"}}))
	now = start.Add(time.Minute)
	s.anchorStatus.observePull(clusterreplica.ErrUnavailable)
	now = start.Add(2 * time.Minute)
	s.anchorStatus.observePull(&sqlite.AnchorRefusal{Reason: sqlite.AnchorRefusedCarriedRevocation, Keys: []string{"k1"}})
	snap := s.anchorStatus.snapshot()
	if snap.pull.result != anchorPullRefused || snap.pull.refusal == nil || !snap.pull.refusedSince.Equal(start) || !snap.pull.at.Equal(start.Add(2*time.Minute)) ||
		!slices.Equal(snap.pull.refusal.Keys, []string{"k1"}) || !errors.Is(snap.pull.refusal, accepted.ErrCAS) {
		t.Fatalf("refusal run not tracked: %+v", snap.pull)
	}
	if refused(sqlite.AnchorRefusedCarriedRevocation) != 1 || refused(sqlite.AnchorRefusedPolicy) != 0 {
		t.Fatal("refusal not exported by reason")
	}
	now = start.Add(3 * time.Minute)
	s.anchorStatus.observePull(nil)
	snap = s.anchorStatus.snapshot()
	if snap.pull.result != anchorPullInstalled || snap.pull.refusal != nil || !snap.pull.refusedSince.IsZero() || refused(sqlite.AnchorRefusedCarriedRevocation) != 0 {
		t.Fatalf("install did not end the refusal: %+v", snap.pull)
	}
	s.anchorStatus.observePull(errors.New("other"))
	if s.anchorStatus.snapshot().pull.result != anchorPullRejected {
		t.Fatal("an unclassified rejection is not reported as rejected")
	}
	if anchorStatusGauge(t, reg, "kudurru_root_anchor_admission_current", nil) != 0 {
		t.Fatal("a runtime without anchors reports admission")
	}
}
