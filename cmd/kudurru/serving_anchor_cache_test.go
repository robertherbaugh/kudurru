package main

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
)

func TestRootAnchorInsertionActualBootstrapDrainsMutation(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	req := &pipeline.Request{Ctx: ctx, Serving: servingdns.Wrap(lease)}
	insertion, err := captureRootAnchorInsertion(req)
	if err != nil {
		t.Fatal(err)
	}
	late, err := captureRootAnchorInsertion(req)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := captureOutputBootstrap(ctx, t, f)
	c := cache.New(cache.Options{MaxEntries: 100})
	key := cache.MakeKey("original.example.", 1, 1, false, false, netip.Prefix{})
	entered, release := make(chan struct{}), make(chan struct{})
	inserted := make(chan error, 1)
	go func() {
		inserted <- insertion.Apply(func() error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			c.Set(key, []byte("original"), cache.Positive, time.Minute)
			return nil
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	started, committed := make(chan struct{}), make(chan error, 1)
	go func() { close(started); committed <- bootstrap() }()
	<-started
	select {
	case err := <-committed:
		t.Fatal("first managed commit crossed active cache insertion", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-inserted; err != nil {
		t.Fatal(err)
	}
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	entry, ok := c.Peek(key)
	if !ok || string(entry.Wire) != "original" {
		t.Fatal("actual admitted cache write missing")
	}
	if err := late.Apply(func() error { c.Set(key, []byte("retired"), cache.Positive, time.Minute); return nil }); err == nil {
		t.Fatal("held old insertion adopted managed successor")
	}
	entry, _ = c.Peek(key)
	if string(entry.Wire) != "original" {
		t.Fatal("retired mutation entered")
	}
	if err := insertion.Apply(func() error { t.Fatal("insertion reused"); return nil }); err == nil {
		t.Fatal("consumed insertion accepted")
	}
	if _, err := captureRootAnchorInsertion(req); err == nil {
		t.Fatal("old complete lease recaptured a successor insertion")
	}
}

func TestRootAnchorInsertionOriginalCancellationAndMissingComponent(t *testing.T) {
	f := newServingCommandFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	lease, err := f.s.publisher.CaptureHTTP(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	req := &pipeline.Request{Ctx: ctx, Serving: servingdns.Wrap(lease)}
	insertion, err := captureRootAnchorInsertion(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := insertion.Apply(func() error { t.Fatal("canceled mutation entered"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("original cancellation not retained", err)
	}
	if err := insertion.Apply(func() error { t.Fatal("canceled mutation renewed"); return nil }); err == nil {
		t.Fatal("canceled attempt reused")
	}
	if _, err := captureRootAnchorInsertion(&pipeline.Request{Ctx: context.Background()}); err == nil {
		t.Fatal("missing complete component accepted")
	}
}
