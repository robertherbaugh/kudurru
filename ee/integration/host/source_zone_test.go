package host

import (
	"context"
	"errors"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sourceZoneObjects(t *testing.T, z *public.DNSZone) zonecache.ObjectSet {
	t.Helper()
	st := memory.New()
	if _, err := st.Apply(context.Background(), z, store.ApplyOptions{Manager: "fixture"}); err != nil {
		t.Fatal(err)
	}
	c, err := zonecache.New(st, zonecache.Options{Kinds: []store.Kind{zonecache.KindDNSZone}, Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan zonecache.ObjectSet, 1)
	c.OnPublish(func(s *zonecache.Snapshot) {
		if s.Objects.Count(zonecache.KindDNSZone) == 1 {
			select {
			case ready <- s.Objects:
			default:
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	select {
	case objects := <-ready:
		return objects
	case <-time.After(5 * time.Second):
		t.Fatal("zone capture timed out")
		return zonecache.ObjectSet{}
	}
}

func TestSourceZonePinsCopyAndRejectsInvalidTarget(t *testing.T) {
	for _, mode := range []string{"valid", "forward", "secondary", "stub", "disabled", "deleting", "root", "bad_apex", "missing"} {
		t.Run(mode, func(t *testing.T) {
			h, j, _, _, _ := engineFixture(t)
			z := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "lan"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "lan.example."}}
			switch mode {
			case "forward":
				z.Spec.Type = public.ZoneTypeForward
			case "secondary":
				z.Spec.Type = public.ZoneTypeSecondary
			case "stub":
				z.Spec.Type = public.ZoneTypeStub
			case "deleting":
				at := metav1.NewTime(time.Now())
				z.DeletionTimestamp = &at
			case "disabled":
				z.Annotations = map[string]string{public.EnabledAnnotation: "false"}
			case "root":
				z.Spec.ZoneName = "."
			case "bad_apex":
				z.Spec.ZoneName = "bad name."
			}
			j.frame.input.Objects = sourceZoneObjects(t, z)
			_, g, _ := h.captured(j)
			p := providerHost{h, j}
			ref := public.ObjectRef{Name: "lan"}
			if mode == "missing" {
				ref.Name = "missing"
			}
			got, err := p.SourceZone(scoped(context.Background(), j, g), ref)
			if mode != "valid" {
				if err == nil || got != nil {
					t.Fatal("unavailable target admitted", got, err)
				}
				return
			}
			if err != nil || got.UID == "" || got.ResourceVersion == "" {
				t.Fatal(got, err)
			}
			uid := got.UID
			got.Spec.ZoneName = "mutated."
			got.UID = "mutated"
			again, err := p.SourceZone(scoped(context.Background(), j, g), ref)
			if err != nil || again.Spec.ZoneName != "lan.example." || again.UID != uid {
				t.Fatal("captured object escaped through copy", again, err)
			}
			for _, invalid := range []public.ObjectRef{{Name: "lan", Namespace: "dns"}, {Name: ""}, {Name: "../lan"}} {
				if _, err := p.SourceZone(scoped(context.Background(), j, g), invalid); !errors.Is(err, ErrInvalid) {
					t.Fatal("invalid reference accepted", invalid, err)
				}
			}
		})
	}
}

func TestSourceZoneRejectsStaleScopeAndReplacement(t *testing.T) {
	for _, mode := range []string{"unscoped", "bundle", "config", "credential", "uid", "term", "lease", "license", "replacement", "incomplete", "during_fence", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			h, j, _, _, _ := engineFixture(t)
			j.frame.input.Objects = sourceZoneObjects(t, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "lan"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "lan.example."}})
			_, guard, _ := h.captured(j)
			ctx := scoped(context.Background(), j, guard)
			h.mu.Lock()
			switch mode {
			case "unscoped":
				ctx = context.Background()
			case "bundle":
				j.frame.input.BundleID = "new-cut"
			case "config":
				j.guard.ConfigDigest = "different"
			case "credential":
				j.guard.CredentialDigest = "different"
			case "uid":
				j.guard.Owner.UID = "recreated"
			case "term":
				j.guard.Term.Epoch++
			case "lease":
				j.cancel()
			case "license":
				h.opts.Allowed = func() bool { return false }
			case "replacement":
				replacement := *j
				h.jobs[j.key] = &replacement
			case "incomplete":
				j.frame.input.Complete = false
			case "during_fence":
				h.opts.Fence = func(context.Context, ExecutionGuard) error {
					h.mu.Lock()
					j.frame.input.BundleID = "changed-during-fence"
					h.mu.Unlock()
					return nil
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			h.mu.Unlock()
			if z, err := (providerHost{h, j}).SourceZone(ctx, public.ObjectRef{Name: "lan"}); err == nil || z != nil {
				t.Fatal("stale request inherited a target", z, err)
			}
		})
	}
}

func TestSourceZoneRetainsOriginalInputAcrossEquivalentRefresh(t *testing.T) {
	h, j, _, _, _ := engineFixture(t)
	original := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "lan"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "original.example."}}
	j.frame.input.Objects = sourceZoneObjects(t, original)
	f, g, err := h.captured(j)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := h.scopedOriginal(t.Context(), j, f, g)
	if err != nil {
		t.Fatal(err)
	}
	replacement := original.DeepCopy()
	replacement.Spec.ZoneName = "successor.example."
	j.frame.input.Objects = sourceZoneObjects(t, replacement)
	got, err := (providerHost{h, j}).SourceZone(ctx, public.ObjectRef{Name: "lan"})
	if err != nil || got.Spec.ZoneName != "original.example." {
		t.Fatal("captured source read adopted future inputs", err)
	}
}
