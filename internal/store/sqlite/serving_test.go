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
	install(t, s, imageOf(t, p))
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
	install(t, s, imageOf(t, p))
	_, token, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, p, zone("replacement", "replacement.example."), store.ManagerAPI)
	install(t, s, imageOf(t, p))
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
