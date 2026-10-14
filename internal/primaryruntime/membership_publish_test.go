package primaryruntime

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
)

func TestExactPublicationSurvivesActualSameHeadReconcile(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := t.Context()
	l := h.begin(h.zone("primary", "example."))
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	before, err := h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Uint32
	h.r.o.Publish = func(ctx context.Context, _ Cut, m Member) error {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		v, err := h.repo.Read(ctx, l.Spec.HeadZone())
		if err != nil {
			return err
		}
		if m.Version != version(v) {
			return ErrConflict
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- h.r.Republish(ctx, l.Spec.Zone) }()
	<-entered
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal("same exact head invalidated completed publication", err)
	}
	after, err := h.repo.Read(ctx, l.Spec.HeadZone())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("no-op reconcile changed durable head", err)
	}
	m, ok := h.r.Capture(time.Now()).Member(l.Spec.Zone.Apex)
	if !ok || !m.Published || m.Version != version(before) {
		t.Fatal("exact live entry was not acknowledged", m)
	}
}

func TestHeldPublicationRejectsChangedCurrentIdentityAfterRootAck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*entry)
	}{
		{"version", func(e *entry) { e.member.Version.CommitSequence++ }},
		{"phase", func(e *entry) { e.member.Phase = zk.PhaseRetiring }},
		{"parents", func(e *entry) { e.member.Parents = []zk.ParentFence{{Incarnation: 2}} }},
		{"lifecycle-token", func(e *entry) { e.lifecycle.ResourceVersion = "different-opaque-rv" }},
		{"head-token", func(e *entry) { e.view.Head.ResourceVersion = "different-opaque-rv" }},
		{"head-spec", func(e *entry) { e.view.Head.Spec.NotifySequence++ }},
		{"frozen-config", func(e *entry) { e.view.Aggregate.Config = append(e.view.Aggregate.Config, ' ') }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "sqlite")
			ctx := t.Context()
			l := h.begin(h.zone("primary", "example."))
			if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			h.r.o.Publish = func(ctx context.Context, _ Cut, _ Member) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			done := make(chan error, 1)
			go func() { done <- h.r.Republish(ctx, l.Spec.Zone) }()
			<-entered
			h.r.mu.Lock()
			old := h.r.entries[l.Spec.Zone.Apex]
			cp := *old
			cp.member.Published = false
			cp.lifecycle = old.lifecycle.DeepCopy()
			view := *old.view
			view.Head = old.view.Head.DeepCopy()
			aggregate := *view.Aggregate
			aggregate.Config = append([]byte(nil), aggregate.Config...)
			view.Aggregate = &aggregate
			cp.view = &view
			tc.change(&cp)
			h.r.entries[l.Spec.Zone.Apex] = &cp
			h.r.rebuild()
			h.r.mu.Unlock()
			close(release)
			if err := <-done; !errors.Is(err, ErrConflict) {
				t.Fatal("changed current identity accepted", err)
			}
			m, _ := h.r.Capture(time.Now()).Member(l.Spec.Zone.Apex)
			if m.Published {
				t.Fatal("changed entry marked published")
			}
		})
	}
}
