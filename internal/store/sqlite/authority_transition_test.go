package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestFailedDurableFenceStaysBlockedAndCanRetry(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "higher", true: "conflict"}[conflict], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), FileName)
			m := NewMetrics(nil)
			s := openAt(t, path, func(o *Options) { o.Metrics = m })
			assertFencedMetric(t, m, 0)
			mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
			term := s.Authority().Term
			term.PrimaryID, term.PromotionID = "other", "other-promotion"
			if !conflict {
				term.Epoch++
			}
			if _, err := s.w.Exec(`CREATE TRIGGER reject_fence BEFORE UPDATE ON meta WHEN NEW.key='replication_authority' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ObserveAuthority(ctx, term); err == nil {
				t.Fatal("disk failure was not returned")
			}
			assertFencedMetric(t, m, 1)
			if _, err := s.Apply(ctx, zone("blocked", "blocked.example."), store.ApplyOptions{Manager: store.ManagerSystem, Force: true}); !errors.Is(err, replication.ErrFenced) {
				t.Fatalf("failed fence allowed mutation: %v", err)
			}
			if _, err := s.w.Exec(`DROP TRIGGER reject_fence`); err != nil {
				t.Fatal(err)
			}
			_, err := s.ObserveAuthority(ctx, term)
			if conflict && !errors.Is(err, replication.ErrPromotionConflict) {
				t.Fatalf("retry: %v", err)
			}
			if !conflict && err != nil {
				t.Fatal(err)
			}
			if s.writeBlocked.Load() {
				t.Fatal("durable retry left transient block")
			}
			assertFencedMetric(t, m, 1)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			back := openAt(t, path, func(o *Options) { o.InitialMode = replication.Primary; o.Metrics = m })
			assertFencedMetric(t, m, 1)
			if back.Authority().Writable() || (conflict && back.Authority().Conflict == nil) || (!conflict && !back.Authority().Term.Same(term)) {
				t.Fatal("durable retry did not survive restart")
			}
		})
	}
}

func TestDivergencePersistsAndExplicitResolutionIsAtomic(t *testing.T) {
	ctx := context.Background()
	p := openTemp(t)
	mustApply(t, p, zone("home", "home.example."), store.ManagerAPI)
	path := filepath.Join(t.TempDir(), FileName)
	m := NewMetrics(nil)
	s := openAt(t, path, func(o *Options) { o.InitialMode = replication.Follower; o.Metrics = m })
	assertFencedMetric(t, m, 0)
	if err := s.Enroll(ctx, p.Authority().Term); err != nil {
		t.Fatal(err)
	}
	base := imageOf(t, p)
	install(t, s, base)
	assertFencedMetric(t, m, 0)
	bad := *base
	bad.Payload.Runtime = []replication.Entry{{Key: "blocking", Data: json.RawMessage(`{"mode":"disabled"}`)}}
	if err := replication.Seal(&bad); err != nil {
		t.Fatal(err)
	}
	v, err := s.ValidateImage(ctx, bad)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.InstallReplica(ctx, v, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()})
	if !errors.Is(err, replication.ErrDivergent) || s.Authority().Divergence == nil {
		t.Fatalf("divergence: %v", err)
	}
	assertFencedMetric(t, m, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path, func(o *Options) { o.InitialMode = replication.Primary; o.Metrics = m })
	assertFencedMetric(t, m, 1)
	if s.Authority().Mode != replication.Fenced || s.Authority().Divergence == nil || !imageOf(t, s).Generation().Same(base.Generation()) {
		t.Fatal("divergence diagnosis/last-good lost")
	}
	req := replication.ResolveRequest{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation(), ExpectedDivergence: s.Authority().Divergence, RequestID: "choose-divergent-branch"}
	if _, err := s.w.Exec(`CREATE TRIGGER reject_resolution BEFORE INSERT ON replication_sections BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	before := s.Authority()
	if _, err := s.ResolveConflict(ctx, v, req); err == nil {
		t.Fatal("failed resolution succeeded")
	}
	assertFencedMetric(t, m, 1)
	if !reflect.DeepEqual(before, s.Authority()) || !imageOf(t, s).Generation().Same(base.Generation()) {
		t.Fatal("partial resolution committed")
	}
	if _, err := s.ReadQuarantined(ctx, base.Generation()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rollback leaked quarantine: %v", err)
	}
	if _, err := s.w.Exec(`DROP TRIGGER reject_resolution`); err != nil {
		t.Fatal(err)
	}
	a, err := s.ResolveConflict(ctx, v, req)
	if err != nil {
		t.Fatal(err)
	}
	assertFencedMetric(t, m, 0)
	if !a.Writable() || a.Term.Epoch != base.Term.Epoch+1 || a.Parent == nil || !a.Parent.Same(bad.Generation()) {
		t.Fatal("resolution did not allocate new chosen branch")
	}
	again, err := s.ResolveConflict(ctx, v, req)
	if err != nil || !again.Term.Same(a.Term) {
		t.Fatalf("resolution retry: %v", err)
	}
	old, err := s.ReadQuarantined(ctx, base.Generation())
	if err != nil || !old.Generation().Same(base.Generation()) {
		t.Fatalf("losing branch not recoverable: %v", err)
	}
	if len(imageOf(t, s).Payload.Runtime) != 1 {
		t.Fatal("chosen extension rows lost")
	}
}

func TestHigherBranchWithLowerRVQuarantinesAndReplaces(t *testing.T) {
	ctx := context.Background()
	p := openTemp(t, internalOptions)
	mustApply(t, p, zone("home", "home.example."), store.ManagerAPI)
	s := mirror(t, p)
	first := imageOf(t, p)
	install(t, s, first)
	req := replication.PromoteRequest{ExpectedTerm: first.Term, ExpectedInstalled: first.Generation(), RequestID: "promote"}
	if _, err := s.Promote(ctx, req); err != nil {
		t.Fatal(err)
	}
	mustApply(t, s, zone("losing", "losing.example."), store.ManagerAPI)
	losing := imageOf(t, s)
	winner := *first
	winner.Term.Epoch = losing.Term.Epoch + 1
	winner.Term.PrimaryID, winner.Term.PromotionID = "winner", "winning-promotion"
	winner.Parent = nil
	if err := replication.Seal(&winner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveAuthority(ctx, winner.Term); err != nil {
		t.Fatal(err)
	}
	install(t, s, &winner)
	if s.Authority().Mode != replication.Follower || s.ResourceVersion() != "1" {
		t.Fatal("source RV compared across terms")
	}
	old, err := s.ReadQuarantined(ctx, losing.Generation())
	if err != nil || !old.Generation().Same(losing.Generation()) {
		t.Fatalf("quarantine: %v", err)
	}
}

func TestEnablePrimaryCASFencesOldTokenAndCloseStopsElection(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnablePrimary(ctx, replication.Generation{}); !errors.Is(err, replication.ErrCAS) {
		t.Fatal(err)
	}
	g := imageOf(t, s).Generation()
	if _, err := s.EnablePrimary(ctx, g); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(WithWriterToken(ctx, token), zone("old", "old.example."), store.ApplyOptions{Manager: store.ManagerSystem}); !errors.Is(err, replication.ErrFenced) {
		t.Fatalf("pre-enable token: %v", err)
	}
	started, stopped := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Elector("close").Run(ctx, func(term context.Context) { close(started); <-term.Done() }, func() { close(stopped) })
	}()
	<-started
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("close did not stop leader work")
	}
	if err := <-done; !errors.Is(err, store.ErrClosed) {
		t.Fatal(err)
	}
}

func TestCorruptAuthorityRefusesOpenAndPromotionOverflow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path)
	a := s.Authority()
	a.MemberID = ""
	raw, _ := json.Marshal(a)
	if _, err := s.w.Exec(`UPDATE meta SET value=? WHERE key=?`, string(raw), metaAuthority); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if back, err := Open(path, testOptions()); err == nil {
		_ = back.Close()
		t.Fatal("corrupt authority booted writable")
	}
	p := openTemp(t, internalOptions)
	m := mirror(t, p)
	install(t, m, imageOf(t, p))
	a = m.Authority()
	a.MaxObservedEpoch = math.MaxUint64
	tx, err := m.w.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAuthority(ctx, tx, a); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	m.authority.Store(&a)
	if _, err := m.Promote(ctx, replication.PromoteRequest{ExpectedTerm: a.Term, ExpectedInstalled: imageOf(t, m).Generation(), RequestID: "overflow"}); !errors.Is(err, replication.ErrInvalid) {
		t.Fatalf("wrapped epoch: %v", err)
	}
}

func TestApplyClusterRejectsAmbiguousJSONDuplicateOwnersAndBounds(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t, func(o *Options) { o.ReplicationLimits.MaxEntries = 1 })
	before := imageOf(t, s).Generation()
	for _, sections := range [][]SectionChange{
		{{Section: "security", Key: "dup", Data: json.RawMessage(`{"a":1,"a":2}`)}},
		{{Section: "security", Key: "deep", Data: json.RawMessage(`null`)}},
		{{Section: "runtime", Key: "one", Data: json.RawMessage(`{}`)}, {Section: "runtime", Key: "two", Data: json.RawMessage(`{}`)}},
	} {
		if _, err := s.ApplyCluster(ctx, nil, store.ApplyOptions{}, sections); err == nil {
			t.Fatal("ambiguous/bounded input accepted")
		}
		if !imageOf(t, s).Generation().Same(before) {
			t.Fatal("rejected transaction changed data")
		}
	}
	if _, err := s.ApplyCluster(ctx, []store.Object{zone("home", "home.example."), zone("home", "home.example.")}, store.ApplyOptions{Manager: store.ManagerAPI}, nil); !errors.Is(err, replication.ErrInvalid) {
		t.Fatalf("duplicate object: %v", err)
	}
	if _, err := s.ApplyCluster(ctx, nil, store.ApplyOptions{}, []SectionChange{{Section: "runtime", Key: "one", Data: json.RawMessage(`{"a": 1}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyCluster(ctx, nil, store.ApplyOptions{}, []SectionChange{{Section: "runtime", Key: "two", Data: json.RawMessage(`{}`)}}); !errors.Is(err, replication.ErrTooLarge) {
		t.Fatalf("total entry bounds: %v", err)
	}
}
