package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var testValidator = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

func openStore(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(path, sqlite.Options{Validator: testValidator(), Identity: "test-member", InitialMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func primary(t *testing.T) *sqlite.Store {
	t.Helper()
	return openStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Primary)
}

func applyPair(t *testing.T, s *sqlite.Store, label string) {
	t.Helper()
	var objects []store.Object
	for _, name := range []string{"one", "two"} {
		objects = append(objects, &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"cut": label}}, Spec: v1alpha1.DNSZoneSpec{Type: v1alpha1.ZoneTypePrimary, ZoneName: name + ".example."}})
	}
	if _, err := s.ApplyCluster(context.Background(), objects, store.ApplyOptions{Manager: store.ManagerAPI}, nil); err != nil {
		t.Fatal(err)
	}
}

func installImage(t *testing.T, s, p *sqlite.Store) replication.Generation {
	t.Helper()
	ctx := context.Background()
	i, err := p.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.ValidateImage(ctx, *i)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Authority()
	if !a.Term.Same(i.Term) {
		if a.Mode == replication.Unenrolled {
			if err := s.Enroll(ctx, i.Term); err != nil {
				t.Fatal(err)
			}
		} else if _, err := s.ObserveAuthority(ctx, i.Term); err != nil {
			t.Fatal(err)
		}
		a = s.Authority()
	}
	expected := a.Installed
	if expected.Term.ClusterID != "" {
		local, err := s.ReadGeneration(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expected = local.Generation()
	}
	g, err := s.InstallReplica(ctx, v, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: expected})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// fixtureView is a test owner projection of ACTUAL complete SQLite documents.
// No production owner functionality is supplied by this package/test fixture.
type fixtureView struct {
	Base   replication.Generation
	Names  []string
	Labels []string
}

func allComponents(candidate Candidate) (Prepared, error) {
	view := fixtureView{Base: candidate.Token.Generation()}
	for _, raw := range candidate.Image.Payload.Objects {
		var header struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			return Prepared{}, err
		}
		view.Names = append(view.Names, header.Metadata.Name)
		view.Labels = append(view.Labels, header.Metadata.Labels["cut"])
	}
	prepared := Prepared{Current: func() bool { return true }}
	for _, name := range CompleteManifest() {
		prepared.Components = append(prepared.Components, Component{Name: name, Base: view.Base, Identity: view.Base.Digest, Value: view})
	}
	return prepared, nil
}

func plainPrepare(_ context.Context, candidate Candidate) (Prepared, error) {
	return allComponents(candidate)
}

func runPublisher(t *testing.T, opts Options) *Publisher {
	t.Helper()
	if opts.Required == nil {
		opts.Required = CompleteManifest()
	}
	if opts.RetryInterval == 0 {
		opts.RetryInterval = time.Hour // explicit Wake/events make tests deterministic
	}
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, store.ErrClosed) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("publisher did not stop/join preparation")
		}
	})
	return p
}

func waitStatus(t *testing.T, p *Publisher, check func(Status) bool) Status {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		changed := p.Subscribe()
		status := p.Status()
		if check(status) {
			return status
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("status did not settle: %+v", p.Status())
		}
	}
}

func checkCut(t *testing.T, lease *Lease, labels []string) {
	t.Helper()
	b := lease.Bundle()
	for _, name := range CompleteManifest() {
		value, ok := b.Component(name)
		if !ok {
			t.Fatalf("missing %s", name)
		}
		view := value.(fixtureView)
		if !view.Base.Same(b.Identity().Base) || !slices.Equal(view.Labels, labels) {
			t.Fatalf("mixed owner cut %s: %+v vs %+v", name, view, b.Identity())
		}
	}
}

type countedSource struct {
	*sqlite.Store
	reads atomic.Int64
}

func (s *countedSource) ReadServingCandidate(ctx context.Context) (*replication.Image, sqlite.GenerationToken, error) {
	s.reads.Add(1)
	return s.Store.ReadServingCandidate(ctx)
}

func TestPublisherWholeCutFailurePendingAndNoQueryReads(t *testing.T) {
	s := &countedSource{Store: primary(t)}
	applyPair(t, s.Store, "old")
	var allow atomic.Bool
	allow.Store(true)
	p := runPublisher(t, Options{Source: s, Prepare: func(_ context.Context, c Candidate) (Prepared, error) {
		parts, err := allComponents(c)
		if !allow.Load() {
			return parts, errors.New("private owner download failed")
		}
		return parts, err
	}})
	first := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	old, err := p.CaptureHTTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	allow.Store(false)
	applyPair(t, s.Store, "new")
	pending := waitStatus(t, p, func(s Status) bool {
		return s.Reason == "prepare_failed" && s.Installed.Revision > first.Served.Base.Revision
	})
	if !pending.InstalledComplete || !pending.Served.Base.Same(first.Served.Base) || pending.ProtectedReady {
		t.Fatalf("installed falsely served: %+v", pending)
	}
	reads := s.reads.Load()
	for range 50 {
		if l, err := p.CaptureHTTP(context.Background()); !errors.Is(err, ErrPending) || l != nil {
			t.Fatalf("protected pending cut accepted: %v", err)
		}
		l, err := p.Capture()
		if err != nil {
			t.Fatal(err)
		}
		checkCut(t, l, []string{"old", "old"})
		l.Release()
		_ = p.Status()
	}
	if s.reads.Load() != reads {
		t.Fatal("query capture/status read database")
	}
	allow.Store(true)
	p.Wake()
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && s.Served.Base.Revision == pending.Installed.Revision })
	newLease, err := p.CaptureHTTP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer newLease.Release()
	checkCut(t, newLease, []string{"new", "new"})
	checkCut(t, old, []string{"old", "old"})
}

func TestPublisherCancelsObsoletePreparationAndNeverServesSkippedCut(t *testing.T) {
	s := primary(t)
	entered, canceled := make(chan struct{}), make(chan struct{})
	var discarded atomic.Int64
	p := runPublisher(t, Options{Source: s, Prepare: func(ctx context.Context, c Candidate) (Prepared, error) {
		parts, err := allComponents(c)
		if c.Image.Revision == 1 {
			parts.Components[0].Release = func() { discarded.Add(1) }
			close(entered)
			<-ctx.Done()
			close(canceled)
			return parts, ctx.Err()
		}
		return parts, err
	}})
	first := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	applyPair(t, s, "obsolete")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("preparer did not enter")
	}
	applyPair(t, s, "current")
	settled := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && s.Served.Base.Revision == 2 })
	if settled.Served.Publication != first.Served.Publication+1 || discarded.Load() != 1 {
		t.Fatal("obsolete candidate published or leaked")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("obsolete preparer not joined")
	}
	l, err := p.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	checkCut(t, l, []string{"current", "current"})
}

func TestPublisherActualPinSpansPromotionCASAndActivationWait(t *testing.T) {
	primaryStore := primary(t)
	applyPair(t, primaryStore, "accepted")
	s := openStore(t, filepath.Join(t.TempDir(), sqlite.FileName), replication.Follower)
	installed := installImage(t, s, primaryStore)
	preparedNewTerm := make(chan struct{}, 1)
	p := runPublisher(t, Options{Source: s, Prepare: func(_ context.Context, c Candidate) (Prepared, error) {
		if !c.Image.Term.Same(installed.Term) {
			preparedNewTerm <- struct{}{}
		}
		return allComponents(c)
	}})
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	var pin replica.ServingPin = p.Pin
	g, release, err := pin(context.Background())
	if err != nil || release == nil || !g.Same(installed) {
		t.Fatalf("pin: %v", err)
	}
	defer release()
	old, err := p.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	promoted, err := s.Promote(context.Background(), replication.PromoteRequest{ExpectedTerm: installed.Term, ExpectedInstalled: installed, RequestID: "operator-promotion"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-preparedNewTerm:
	case <-time.After(5 * time.Second):
		t.Fatal("new-term candidate not prepared")
	}
	if !p.Status().Served.Base.Same(installed) {
		t.Fatal("publication crossed held promotion pin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	target := TermActivation{Term: promoted.Term, Incarnation: promoted.Incarnation}
	if err := p.WaitTerm(ctx, target); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("activation did not wait for serving: %v", err)
	}
	if _, _, err := pin(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended Pin cancellation: %v", err)
	}
	if l, err := p.CaptureHTTP(context.Background()); !errors.Is(err, ErrPending) || l != nil {
		t.Fatal("old-term auth accepted after promotion")
	}
	// DNS capture remains available even with the publication barrier held.
	l, err := p.Capture()
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	release()
	release() // release is idempotent
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && s.Served.Base.Term.Same(promoted.Term) })
	if err := p.WaitTerm(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if err := p.WaitTerm(context.Background(), TermActivation{Term: installed.Term, Incarnation: promoted.Incarnation}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("old term activation: %v", err)
	}
}

func TestPublisherFencedRestartExplicitDNSOnlyLastGood(t *testing.T) {
	ctx := context.Background()
	primaryStore := primary(t)
	applyPair(t, primaryStore, "last-good")
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	s := openStore(t, path, replication.Follower)
	installed := installImage(t, s, primaryStore)
	higher := installed.Term
	higher.Epoch++
	higher.PrimaryID, higher.PromotionID = "new-primary", "new-promotion"
	if _, err := s.ObserveAuthority(ctx, higher); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, path, replication.Follower)
	p := runPublisher(t, Options{Source: reopened, Prepare: plainPrepare})
	status := waitStatus(t, p, func(s Status) bool { return s.Served.Publication > 0 })
	if !status.DNSOnly || status.ProtectedReady || !status.Served.Base.Same(installed) || !status.Authority.Term.Same(higher) {
		t.Fatalf("fenced restart status: %+v", status)
	}
	select {
	case <-p.Initialized():
		t.Fatal("DNS-only publication opened startup gate")
	default:
	}
	if l, err := p.CaptureHTTP(ctx); !errors.Is(err, ErrPending) || l != nil {
		t.Fatal("DNS-only auth accepted")
	}
	if _, release, err := p.Pin(ctx); !errors.Is(err, ErrPending) || release != nil {
		t.Fatal("DNS-only promotion pin accepted")
	}
	l, err := p.Capture()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if !l.Bundle().DNSOnly() {
		t.Fatal("last-good bundle not explicitly marked")
	}
	checkCut(t, l, []string{"last-good", "last-good"})
}

func TestPublisherLeaseRetirementSharedResourcesAndShutdown(t *testing.T) {
	s := primary(t)
	var retired atomic.Int64
	var sharedRefs, sharedClosed atomic.Int64
	sharedPool := &struct{ Address string }{Address: "fixed-owner-route"}
	var p *Publisher
	p = runPublisher(t, Options{Source: s, Prepare: func(_ context.Context, c Candidate) (Prepared, error) {
		parts, err := allComponents(c)
		// The same immutable route pool is reused across canonical cuts. Each
		// returned bundle receives its OWN owner reference and release hook.
		sharedRefs.Add(1)
		parts.Components[0].Value = sharedPool
		parts.Components[0].Release = func() {
			// These acquire store/publisher bookkeeping locks. A hook under
			// either publication/writer lock would deadlock this test.
			_ = s.AuthorityChanges()
			_ = p.Status()
			if l, e := p.Capture(); e == nil {
				l.Release()
			}
			retired.Add(1)
			if sharedRefs.Add(-1) == 0 {
				sharedClosed.Add(1)
			}
		}
		return parts, err
	}})
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	old, err := p.Capture()
	if err != nil {
		t.Fatal(err)
	}
	clone, err := old.Clone()
	if err != nil {
		t.Fatal(err)
	}
	applyPair(t, s, "replacement")
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && s.Served.Base.Revision == 1 })
	if retired.Load() != 0 {
		t.Fatal("old resource retired with live leases")
	}
	old.Release()
	old.Release()
	if retired.Load() != 0 {
		t.Fatal("clone did not retain owner resource")
	}
	clone.Release()
	if retired.Load() != 1 || sharedClosed.Load() != 0 || sharedRefs.Load() != 1 || old.Bundle() != nil {
		t.Fatal("retirement or released lease state incorrect")
	}
	if _, err := old.Clone(); !errors.Is(err, ErrReleased) {
		t.Fatal("released lease cloned")
	}
	current, err := p.Capture()
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	if retired.Load() != 1 || current.Bundle() == nil {
		t.Fatal("shutdown retired outstanding lease")
	}
	if _, err := p.Capture(); !errors.Is(err, ErrClosed) {
		t.Fatal("capture after shutdown accepted")
	}
	current.Release()
	if retired.Load() != 2 || sharedClosed.Load() != 1 || sharedRefs.Load() != 0 {
		t.Fatal("last shutdown lease did not retire resources")
	}
}

type interceptSource struct {
	*sqlite.Store
	before func(sqlite.GenerationToken)
}

func (s *interceptSource) PublishIfCurrent(ctx context.Context, token sqlite.GenerationToken, opts sqlite.PublicationOptions, publish func() error) error {
	s.before(token)
	return s.Store.PublishIfCurrent(ctx, token, opts, publish)
}

func TestPublisherRejectsDependencyRaceAtActualWriterBarrier(t *testing.T) {
	s := &interceptSource{Store: primary(t)}
	var epoch atomic.Int64
	epoch.Store(1)
	var intercept atomic.Bool
	var discarded atomic.Int64
	s.before = func(sqlite.GenerationToken) {
		if intercept.CompareAndSwap(true, false) {
			epoch.Add(1) // preparation was complete; final dependency receipt is stale
		}
	}
	p := runPublisher(t, Options{Source: s, Prepare: func(_ context.Context, c Candidate) (Prepared, error) {
		parts, err := allComponents(c)
		captured := epoch.Load()
		parts.Current = func() bool { return epoch.Load() == captured }
		parts.Components[0].Identity = fmt.Sprintf("runtime-%d", captured)
		parts.Components[0].Release = func() { discarded.Add(1) }
		return parts, err
	}})
	first := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	epoch.Add(1)
	intercept.Store(true)
	p.Wake()
	failed := waitStatus(t, p, func(s Status) bool { return s.Reason == "candidate_changed" && !s.Preparing })
	if failed.Served != first.Served || discarded.Load() != 1 || failed.ProtectedReady {
		t.Fatal("stale exact dependency was published or retained")
	}
	p.Wake()
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && s.Served.Publication == first.Served.Publication+1 })
}

func TestPublisherConcurrentCapturesRemainWholeCuts(t *testing.T) {
	s := primary(t)
	applyPair(t, s, "initial")
	p := runPublisher(t, Options{Source: s, Prepare: plainPrepare})
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	ctx, stop := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for ctx.Err() == nil {
				lease, err := p.Capture()
				if err != nil {
					t.Error(err)
					return
				}
				b := lease.Bundle()
				for _, name := range CompleteManifest() {
					value, ok := b.Component(name)
					if !ok {
						t.Error("missing captured owner")
						continue
					}
					view := value.(fixtureView)
					if len(view.Labels) != 2 || view.Labels[0] != view.Labels[1] || !view.Base.Same(b.Identity().Base) {
						t.Error("concurrent capture mixed canonical cuts")
					}
				}
				lease.Release()
			}
		})
	}
	for i := range 12 {
		applyPair(t, s, fmt.Sprintf("cut-%d", i))
	}
	waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && s.Served.Base.Revision == 13 })
	stop()
	readers.Wait()
}

func TestPublisherPreparationPanicAndStoreCloseFailClosed(t *testing.T) {
	s := primary(t)
	var panicOwner atomic.Bool
	var failOwner atomic.Bool
	var failedRelease atomic.Int64
	p := runPublisher(t, Options{Source: s, Prepare: func(_ context.Context, c Candidate) (Prepared, error) {
		if panicOwner.Load() {
			panic("owner private panic text")
		}
		parts, err := allComponents(c)
		if failOwner.Load() {
			parts.Components[0].Release = func() { panic("private release panic") }
			parts.Components[1].Release = func() { failedRelease.Add(1) }
			return parts, errors.New("required exact list body missing")
		}
		return parts, err
	}})
	first := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	panicOwner.Store(true)
	applyPair(t, s, "blocked")
	failed := waitStatus(t, p, func(s Status) bool { return s.Reason == "prepare_failed" && !s.Preparing })
	if failed.Served != first.Served || failed.ProtectedReady {
		t.Fatal("owner panic published partial state")
	}
	panicOwner.Store(false)
	failOwner.Store(true)
	p.Wake()
	waitStatus(t, p, func(s Status) bool { return s.Reason == "prepare_failed" && !s.Preparing && failedRelease.Load() == 1 })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, p, func(s Status) bool { return s.Reason == "closed" })
	if _, err := p.Capture(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed source serving accepted: %v", err)
	}
}

func TestPublisherDependencyCursorAndRuntimeOnlyPublication(t *testing.T) {
	s := primary(t)
	var epoch atomic.Int64
	epoch.Store(1)
	p := runPublisher(t, Options{Source: s, Prepare: func(_ context.Context, c Candidate) (Prepared, error) {
		parts, err := allComponents(c)
		captured := epoch.Load()
		parts.Current = func() bool { return epoch.Load() == captured }
		for i := range parts.Components {
			parts.Components[i].Identity = fmt.Sprintf("%s/local/%d", c.Token.Generation().Digest, captured)
		}
		return parts, err
	}})
	first := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady })
	epoch.Store(2)
	if l, err := p.CaptureHTTP(context.Background()); !errors.Is(err, ErrPending) || l != nil {
		t.Fatal("unpublished local dependency accepted")
	}
	p.Wake()
	second := waitStatus(t, p, func(s Status) bool { return s.ProtectedReady && s.Served.Publication > first.Served.Publication })
	if !second.Served.Base.Same(first.Served.Base) || second.Served.Dependencies == first.Served.Dependencies {
		t.Fatal("runtime epoch confused with canonical generation")
	}
}

func TestPublisherManifestAndWholePreparationValidation(t *testing.T) {
	s := primary(t)
	if _, err := New(Options{Source: s, Prepare: plainPrepare, Required: []string{"zones"}}); !errors.Is(err, ErrComponents) {
		t.Fatal("zone-only production manifest accepted")
	}
	if _, err := New(Options{Source: s, Required: CompleteManifest()}); err == nil {
		t.Fatal("missing all-owner preparer accepted")
	}
	image, token, err := s.ReadServingCandidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, alter := range []func(*Prepared){
		func(p *Prepared) { p.Components = p.Components[:len(p.Components)-1] },
		func(p *Prepared) { p.Components[1].Name = p.Components[0].Name },
		func(p *Prepared) { p.Components[0].Base.Revision++ },
		func(p *Prepared) { var v *fixtureView; p.Components[0].Value = v },
		func(p *Prepared) { p.Components[0].Identity = "" },
		func(p *Prepared) { p.Current = nil },
	} {
		parts, e := allComponents(Candidate{Image: image, Token: token})
		if e != nil {
			t.Fatal(e)
		}
		alter(&parts)
		if b, e := newBundle(token, parts, CompleteManifest()); !errors.Is(e, ErrComponents) || b != nil {
			t.Fatalf("invalid cut accepted: %v", e)
		}
	}
	if dependenciesCurrent(func() bool { panic("private error") }) {
		t.Fatal("panicked dependency check granted eligibility")
	}
}
