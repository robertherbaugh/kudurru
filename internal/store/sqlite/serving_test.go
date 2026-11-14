package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestServingTokenCoherentPrimaryAndPublicationCAS(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	image, token, err := s.ReadServingCandidate(ctx)
	if err != nil || !image.Generation().Same(token.Generation()) || !token.Matches(s.Authority()) {
		t.Fatalf("candidate: %v", err)
	}
	if token.Authority().Installed.Digest != "" || token.Generation().Digest == "" {
		t.Fatal("primary cursor must stay distinct from full image identity")
	}
	called := false
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{}, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("current primary: %v", err)
	}
	want := errors.New("dependency changed")
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{}, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("callback error: %v", err)
	}
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	called = false
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{}, func() error { called = true; return nil }); !errors.Is(err, replication.ErrCAS) || called {
		t.Fatalf("stale token executed: %v", err)
	}
	other := openTemp(t)
	if err := other.PublishIfCurrent(ctx, token, PublicationOptions{}, func() error { return nil }); !errors.Is(err, replication.ErrInvalid) {
		t.Fatalf("foreign token: %v", err)
	}
	if err := s.PublishIfCurrent(ctx, GenerationToken{}, PublicationOptions{}, func() error { return nil }); !errors.Is(err, replication.ErrInvalid) {
		t.Fatalf("zero token: %v", err)
	}
}

func TestServingPublicationHoldsActualWriterBarrier(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	_, token, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inside, release := make(chan struct{}), make(chan struct{})
	published := make(chan error, 1)
	go func() {
		published <- s.PublishIfCurrent(ctx, token, PublicationOptions{}, func() error {
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	written := make(chan error, 1)
	go func() {
		_, e := s.Apply(ctx, zone("new", "new.example."), store.ApplyOptions{Manager: store.ManagerAPI})
		written <- e
	}()
	select {
	case e := <-written:
		t.Fatalf("writer crossed publication barrier: %v", e)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if e := <-published; e != nil {
		t.Fatal(e)
	}
	if e := <-written; e != nil {
		t.Fatal(e)
	}
	if token.Matches(s.Authority()) {
		t.Fatal("completed mutation did not invalidate token")
	}
}

func TestServingFollowerAuthorityOnlyFenceAndExplicitDNSOnly(t *testing.T) {
	ctx := context.Background()
	p := openTemp(t, internalOptions)
	mustApply(t, p, zone("home", "home.example."), store.ManagerAPI)
	s := mirror(t, p)
	copyReplica(t, s, p)
	_, old, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	authority, generation := s.AuthorityChanges(), s.GenerationChanges()
	higher := p.Authority().Term
	higher.Epoch++
	higher.PrimaryID, higher.PromotionID = "promoted-member", "promotion"
	if _, err := s.ObserveAuthority(ctx, higher); err != nil {
		t.Fatal(err)
	}
	select {
	case <-authority:
	default:
		t.Fatal("authority-only fence did not signal")
	}
	select {
	case <-generation:
		t.Fatal("fence must not pretend a new image was installed")
	default:
	}
	if err := s.PublishIfCurrent(ctx, old, PublicationOptions{}, func() error { return nil }); !errors.Is(err, replication.ErrCAS) {
		t.Fatalf("old normal token after fence: %v", err)
	}
	image, fenced, err := s.ReadServingCandidate(ctx)
	if err != nil || !image.Generation().Same(old.Generation()) || !fenced.DNSOnly() {
		t.Fatalf("last-good candidate lost: %v", err)
	}
	if err := s.PublishIfCurrent(ctx, fenced, PublicationOptions{}, func() error { return nil }); !errors.Is(err, replication.ErrFenced) {
		t.Fatalf("implicit DNS-only mode: %v", err)
	}
	if err := s.PublishIfCurrent(ctx, fenced, PublicationOptions{DNSOnly: true}, func() error { return nil }); err != nil {
		t.Fatalf("explicit last-good DNS: %v", err)
	}
	s.writeBlocked.Store(true)
	if _, blocked := s.ServingAuthority(); !blocked {
		t.Fatal("undurable fence not visible to protected-read gate")
	}
	if err := s.PublishIfCurrent(ctx, fenced, PublicationOptions{DNSOnly: true}, func() error { return nil }); !errors.Is(err, replication.ErrFenced) {
		t.Fatalf("undurable fence publication: %v", err)
	}
	s.writeBlocked.Store(false)
}

func TestServingCandidateCutDuringWritesAndInstallInvalidation(t *testing.T) {
	ctx := context.Background()
	p := openTemp(t, internalOptions)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 20 {
			if _, err := p.Apply(ctx, zone("home", "home.example."), store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
				t.Error(err)
				return
			}
			if err := p.Delete(ctx, store.Key{Kind: "DNSZone", Name: "home"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 30 {
		image, token, err := p.ReadServingCandidate(ctx)
		if err != nil || token.Authority().Installed.Revision != image.Revision || !token.Generation().Same(image.Generation()) {
			t.Fatalf("torn read transaction: %v", err)
		}
	}
	wg.Wait()
	s := mirror(t, p)
	copyReplica(t, s, p)
	_, token, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, p, zone("replacement", "replacement.example."), store.ManagerAPI)
	copyReplica(t, s, p)
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{}, func() error { return nil }); !errors.Is(err, replication.ErrCAS) {
		t.Fatalf("install did not invalidate token: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.AuthorityChanges():
	default:
		t.Fatal("close signal missing")
	}
	if _, _, err := s.ReadServingCandidate(ctx); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("read after close: %v", err)
	}
}

// AdvancedTo accepts the token's own writable authority at the same or a later
// installed revision and nothing else: every other field is compared exactly.
func TestServingTokenAdvancedToComparesEveryOtherFieldExactly(t *testing.T) {
	s := openTemp(t)
	_, token, err := s.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	original := token.Authority()
	if !token.AdvancedTo(original) || !token.Matches(original) {
		t.Fatal("the token's own authority was refused")
	}
	mustApply(t, s, zone("home", "home.example."), store.ManagerAPI)
	mustApply(t, s, zone("lab", "lab.example."), store.ManagerAPI)
	later := s.Authority()
	if later.Installed.Revision != original.Installed.Revision+2 || token.Matches(later) || !token.AdvancedTo(later) {
		t.Fatal("an authority advanced only in its installed revision was refused")
	}
	_, newer, err := s.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if newer.AdvancedTo(original) {
		t.Fatal("an earlier revision was accepted as an advance")
	}
	conflict := later.Term
	conflict.PromotionID = "another-promotion"
	for name, change := range map[string]func(*replication.Authority){
		"incarnation":    func(a *replication.Authority) { a.Incarnation++ },
		"mode":           func(a *replication.Authority) { a.Mode = replication.Fenced },
		"member":         func(a *replication.Authority) { a.MemberID = "another-member" },
		"term":           func(a *replication.Authority) { a.Term.Epoch++ },
		"installed term": func(a *replication.Authority) { a.Installed.Term.Epoch++ },
		"digest":         func(a *replication.Authority) { a.Installed.Digest = "sha256:other" },
		"observed epoch": func(a *replication.Authority) { a.MaxObservedEpoch++ },
		"lineage":        func(a *replication.Authority) { a.WatchLineage = "another-lineage" },
		"install":        func(a *replication.Authority) { a.InstallSequence++ },
		"parent":         func(a *replication.Authority) { a.Parent = &replication.Generation{Term: a.Term, Revision: 1} },
		"conflict":       func(a *replication.Authority) { a.Conflict = &conflict },
		"divergence":     func(a *replication.Authority) { a.Divergence = &replication.Generation{Term: a.Term, Revision: 1} },
		"promotion":      func(a *replication.Authority) { a.LastPromotionRequest = "another-request" },
		"resolution":     func(a *replication.Authority) { a.LastResolutionRequest = "another-request" },
	} {
		changed := s.Authority()
		change(&changed)
		if token.AdvancedTo(changed) {
			t.Fatalf("a changed %s was accepted as an advance of the same authority", name)
		}
	}
	if (GenerationToken{}).AdvancedTo(replication.Authority{}) {
		t.Fatal("a zero token was accepted")
	}
	// Only a writable source can advance by its own commits. A follower's
	// installed source and a fenced last-good cut never qualify.
	primary := openTemp(t, internalOptions)
	mustApply(t, primary, zone("home", "home.example."), store.ManagerAPI)
	follower := mirror(t, primary)
	copyReplica(t, follower, primary)
	_, installed, err := follower.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if installed.AdvancedTo(installed.Authority()) || !installed.Matches(follower.Authority()) {
		t.Fatal("a follower's installed source was accepted as a writable advance")
	}
	higher := primary.Authority().Term
	higher.Epoch++
	higher.PrimaryID, higher.PromotionID = "promoted-member", "promotion"
	if _, err := follower.ObserveAuthority(t.Context(), higher); err != nil {
		t.Fatal(err)
	}
	_, fenced, err := follower.ReadServingCandidate(t.Context())
	if err != nil || !fenced.DNSOnly() || fenced.AdvancedTo(fenced.Authority()) {
		t.Fatal("a fenced last-good cut was accepted as a writable advance", err)
	}
}
