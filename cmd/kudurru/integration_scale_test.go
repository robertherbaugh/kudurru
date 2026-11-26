package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// integrationScaleSeedCutoff bounds fixture construction only. It is not
	// an integration host operation and must not be reported as the production
	// 30-second Apply bound that the measured operation uses below.
	integrationScaleSeedCutoff = 5 * time.Minute
	// integrationScaleCleanupCutoff bounds this test's observation of several
	// independently 30-second host operations; it does not change a host bound.
	integrationScaleCleanupCutoff = 2 * time.Minute
)

// TestIntegrationScale measures the mounted SQLite integration path at its
// production host bounds: capture is limited to 10 seconds and each host
// operation to 30 seconds. It is deliberately opt-in: a cell can allocate a
// large SQLite generation and may establish that the production bound is not
// met. It neither changes IntegrationBounds nor makes those bounds configurable.
//
// Run one, bounded cell at a time. The default cell is suitable as a local
// smoke test; the other supported values form the #151 matrix:
//
//	KUDURRU_INTEGRATION_SCALE=1 KUDURRU_INTEGRATION_SCALE_REVISION="$(git rev-parse HEAD)" go test -count=1 -run '^TestIntegrationScale$' -v ./cmd/kudurru
//
//	KUDURRU_INTEGRATION_SCALE=1 KUDURRU_INTEGRATION_SCALE_CELL=10000:4k:loaded go test -count=1 -run '^TestIntegrationScale$' -v ./cmd/kudurru
//
// Cells are <objects>:<small|4k|32k>:<idle|loaded>. Run the same command with
// -race separately when collecting the race-detector column. A cell reports a
// capture, Apply, or cleanup deadline as an outcome rather than extending the
// production bound, so the raw test log remains a useful matrix observation.
func TestIntegrationScale(t *testing.T) {
	if os.Getenv("KUDURRU_INTEGRATION_SCALE") != "1" {
		t.Skip("set KUDURRU_INTEGRATION_SCALE=1 to run one integration scale cell")
	}
	cell := integrationScaleCellFromEnvironment(t)
	if cell.objects > 1_000 && testing.Short() {
		t.Skip("-short permits only the 1,000-object integration scale smoke cell")
	}

	t.Logf("integration scale cell=%s host=%s go=%s revision=%s", cell, integrationScaleHost(), runtime.Version(), integrationScaleRevision())
	f := newIntegrationDeleteFixture(t, nil)
	if f.s.opts.IntegrationBounds != (integrationBounds{}) {
		t.Fatal("scale fixture must use the host production bounds")
	}

	seedStarted := time.Now()
	seeded, before, seedErr := integrationScaleSeed(t, f.s, cell)
	if seedErr != nil {
		t.Logf("integration scale result cell=%s seed=%s seed_cutoff=%s seed_duration=%s objects_before=%d objects_seeded=%d", cell, integrationScaleOutcome(seedErr), integrationScaleSeedCutoff, time.Since(seedStarted).Round(time.Millisecond), before, seeded)
		if !errors.Is(seedErr, replication.ErrTooLarge) && !errors.Is(seedErr, context.DeadlineExceeded) {
			t.Fatal(seedErr)
		}
		return
	}
	expectedProviderIDs := integrationScaleProviderIDs(t, f)
	foreignID := integrationForeignID(1)
	f.seed(foreignID, "foreign.example", "A", "198.51.100.1", "")

	stopLoad := integrationScaleLoad(t, f.s, cell.load)
	defer stopLoad()

	captureStarted := time.Now()
	captureContext, cancelCapture := context.WithTimeout(t.Context(), 10*time.Second)
	_, captureErr := f.s.integrations.capture(captureContext)
	if captureErr == nil {
		captureErr = captureContext.Err()
	}
	cancelCapture()
	captureDuration := time.Since(captureStarted)

	applyStarted := time.Now()
	applyErr := integrationScaleApply(t.Context(), f.s, f.record)
	applyDuration := time.Since(applyStarted)

	cleanupStarted := time.Now()
	requestsBefore, writesBefore, _ := f.traffic()
	deleteContext, cancelDelete := context.WithTimeout(t.Context(), 30*time.Second)
	deleteErr := f.s.opts.Store.Delete(deleteContext, store.KeyOf(f.d), store.DeleteOptions{Manager: store.ManagerAPI})
	cancelDelete()
	cleanupDone := false
	if deleteErr == nil {
		f.s.integrations.host.Wake()
		cleanupDone = integrationScaleAwaitGone(t, f, integrationScaleCleanupCutoff)
	}
	cleanupDuration := time.Since(cleanupStarted)
	requestsAfter, writesAfter, deleted := f.traffic()
	slices.Sort(deleted)

	if cleanupDone && (writesAfter-writesBefore != 1 || !slices.Equal(deleted, expectedProviderIDs)) {
		t.Fatalf("Cloudflare double cleanup writes=%d deleted=%v provider_ids=%v", writesAfter-writesBefore, deleted, expectedProviderIDs)
	}
	if cleanupDone {
		f.expectOwnedDeletedOnce(t, writesBefore, 1)
	}
	f.peer.mu.Lock()
	foreignIntact := f.peer.rows[foreignID]["content"] == "198.51.100.1"
	f.peer.mu.Unlock()
	if !foreignIntact {
		t.Fatal("cleanup touched the foreign provider row")
	}
	cleanupOutcome := "finished"
	if deleteErr != nil {
		cleanupOutcome = "delete-request-" + integrationScaleOutcome(deleteErr)
	} else if !cleanupDone {
		cleanupOutcome = "harness-cutoff"
		phase, reason := f.status()
		_, captureFailure := f.s.integrations.host.StatusSnapshot()
		ledger := integrationRows(t, f.s, kinds.KindIntegrationLedger)
		operations := integrationRows(t, f.s, kinds.KindIntegrationOperation)
		t.Logf("integration scale cleanup cutoff cell=%s status=%s/%s capture_failure=%q remaining_ledger=%d remaining_operations=%d", cell, phase, reason, captureFailure, len(ledger), len(operations))
	}
	t.Logf("integration scale result cell=%s objects_before=%d objects_seeded=%d capture=%s/%s apply=%s/%s cleanup=%s/%s cleanup_cutoff=%s cloudflare_requests=%d cloudflare_writes=%d cloudflare_provider_ids=%v", cell, before, seeded, captureDuration.Round(time.Millisecond), integrationScaleOutcome(captureErr), applyDuration.Round(time.Millisecond), integrationScaleOutcome(applyErr), cleanupDuration.Round(time.Millisecond), cleanupOutcome, integrationScaleCleanupCutoff, requestsAfter-requestsBefore, writesAfter-writesBefore, deleted)
}

type integrationScaleCell struct {
	objects int
	size    int
	label   string
	load    string
}

func (c integrationScaleCell) String() string {
	return fmt.Sprintf("%d:%s:%s", c.objects, c.label, c.load)
}

func integrationScaleCellFromEnvironment(t *testing.T) integrationScaleCell {
	t.Helper()
	value := os.Getenv("KUDURRU_INTEGRATION_SCALE_CELL")
	if value == "" {
		return integrationScaleCell{objects: 1_000, label: "small", load: "idle"}
	}
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		t.Fatalf("KUDURRU_INTEGRATION_SCALE_CELL must be <objects>:<small|4k|32k>:<idle|loaded>, got %q", value)
	}
	objects, err := strconv.Atoi(parts[0])
	if err != nil || (objects != 1_000 && objects != 10_000 && objects != 25_000 && objects != 50_000 && objects != 100_000) {
		t.Fatalf("integration scale object count %q is not one of 1000, 10000, 25000, 50000, 100000", parts[0])
	}
	cell := integrationScaleCell{objects: objects, label: parts[1], load: parts[2]}
	switch cell.label {
	case "small":
	case "4k":
		cell.size = 4 << 10
	case "32k":
		cell.size = 32 << 10
	default:
		t.Fatalf("integration scale size %q is not small, 4k, or 32k", cell.label)
	}
	if cell.load != "idle" && cell.load != "loaded" {
		t.Fatalf("integration scale load %q is not idle or loaded", cell.load)
	}
	return cell
}

// integrationScaleSeed writes each cell in one real SQLite command. That is
// intentional: sequential Store.Apply would itself re-read the growing image
// for every seed object and measure fixture construction, not the three paths
// under test. The measured Apply below commits one mutation with the command
// postimage check.
func integrationScaleSeed(t *testing.T, s *servingRuntime, cell integrationScaleCell) (seeded, before int, result error) {
	t.Helper()
	image, err := s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		return 0, 0, err
	}
	before = len(image.Payload.Objects)
	if before > cell.objects {
		return 0, before, fmt.Errorf("fixture already has %d objects, above requested %d", before, cell.objects)
	}
	want := cell.objects - before
	if want == 0 {
		return 0, before, nil
	}
	if int64(want)*int64(cell.size) > (replication.Limits{}).Defaults().MaxBytes {
		t.Logf("generation excludes this shape: the annotation padding alone exceeds the image byte limit")
		return 0, before, replication.ErrTooLarge
	}
	ctx, cancel := context.WithTimeout(t.Context(), integrationScaleSeedCutoff)
	defer cancel()
	err = s.opts.Store.Command(ctx, s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for i := 0; i < want; i++ {
			// These rows remain in ReadGeneration and are decoded by capture,
			// but must not create provider work that would change the fixture's
			// Cloudflare double or fence capture for an unrelated source.
			annotations := map[string]string{public.EnabledAnnotation: "false"}
			if cell.size != 0 {
				annotations["scale.kudurru.io/payload"] = strings.Repeat("x", cell.size)
			}
			row := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("scale-%06d", i), Namespace: "default", Annotations: annotations}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: fmt.Sprintf("scale-%06d.example.", i), RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.1"}}}}}
			if _, err := tx.Apply(ctx, row, store.ApplyOptions{Manager: store.ManagerSystem, CreateOnly: true}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, before, err
	}
	return want, before, nil
}

func integrationScaleApply(ctx context.Context, s *servingRuntime, record *public.DNSRecordSet) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var current public.DNSRecordSet
	if err := s.opts.Store.Get(ctx, store.KeyOf(record), &current); err != nil {
		return err
	}
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations["scale.kudurru.io/applied-at"] = time.Now().UTC().Format(time.RFC3339Nano)
	return s.opts.Store.Command(ctx, s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		changed, err := tx.Apply(ctx, &current, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: current.ResourceVersion})
		if err == nil && changed.GetResourceVersion() == current.ResourceVersion {
			return errors.New("scale apply did not change the resource version")
		}
		return err
	})
}

// integrationScaleLoad makes the loaded column reproducible without an
// unbounded stress process: two readers repeatedly take the same complete
// generation path as capture. The two goroutines are bounded and stopped at
// test teardown; they do not use an external or unbounded stress process.
func integrationScaleLoad(t *testing.T, s *servingRuntime, mode string) func() {
	t.Helper()
	if mode == "idle" {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			defer func() { done <- struct{}{} }()
			for {
				if _, err := s.opts.Store.ReadGeneration(ctx); err != nil && !errors.Is(err, context.Canceled) {
					t.Logf("integration scale synthetic load stopped: %v", err)
					return
				}
				select {
				case <-ctx.Done():
					return
				default:
				}
			}
		}()
	}
	return func() {
		cancel()
		<-done
		<-done
	}
}

func integrationScaleAwaitGone(t *testing.T, f *integrationDeleteFixture, limit time.Duration) bool {
	t.Helper()
	until := time.NewTimer(limit)
	defer until.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if integrationStored(t, f.s, f.d) == nil {
			return true
		}
		select {
		case <-until.C:
			return false
		case <-tick.C:
		}
	}
}

func integrationScaleOutcome(err error) string {
	if err == nil {
		return "finished"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, ih.ErrUnavailable) {
		return "unavailable"
	}
	if errors.Is(err, replication.ErrTooLarge) || errors.Is(err, store.ErrTooLarge) {
		return "cap"
	}
	return fmt.Sprintf("error:%T", err)
}

func integrationScaleProviderIDs(t *testing.T, f *integrationDeleteFixture) []string {
	t.Helper()
	rows := integrationRows(t, f.s, kinds.KindIntegrationLedger)
	if len(rows) != 1 {
		t.Fatalf("Cloudflare double has %d ledger rows, want 1", len(rows))
	}
	ids := slices.Clone(rows[0].(*kinds.IntegrationLedger).Spec.ProviderIDs)
	slices.Sort(ids)
	if !slices.Equal(ids, f.published) {
		t.Fatalf("Cloudflare double provider IDs %v, peer rows %v", ids, f.published)
	}
	return ids
}

func integrationScaleHost() string {
	host, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return host
}

func integrationScaleRevision() string {
	if revision := os.Getenv("KUDURRU_INTEGRATION_SCALE_REVISION"); revision != "" {
		return revision
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "unknown"
}
