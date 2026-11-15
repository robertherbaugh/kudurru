package primaryruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestDurableMemberSurvivesSameApexDesiredUIDReplacement(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	z := h.zone("primary", "example.")
	l := h.begin(z)
	if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	if err := h.st.Delete(ctx, store.Key{Kind: "DNSZone", Name: z.Name}, store.DeleteOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}

	removed := l.Spec.Source
	removed.Removed = true
	retired, err := h.r.RetireSource(ctx, l.Spec.Zone, removed)
	if err != nil {
		t.Fatal(err)
	}
	replacement := h.zone("primary", "example.")
	p := prepareTestImage(t, readTestImage(t, h), imageTestOptions(t, h))
	placeholder, _ := p.Cut(time.Now()).Member("example.")
	if placeholder.Incarnation != 0 || placeholder.Zone.UID != string(replacement.UID) {
		t.Fatal(placeholder)
	}
	m, ok := p.DurableMember(l.Spec.Zone)
	if !ok || m.Incarnation != retired.Spec.Incarnation || m.Sequence != retired.Spec.Sequence || m.Phase != zk.PhaseRetired || m.Version.CommitSequence == 0 || m.Published {
		t.Fatal(m, ok)
	}
	if _, ok := p.DurableMember(placeholder.Zone); ok {
		t.Fatal("placeholder used as durable acknowledgment")
	}
	assertImageCoverage(t, p, "example.", time.Now())
}

func TestRestorePrebuildThenAtomicCompleteMemoryInstall(t *testing.T) {
	h := newHarness(t, "sqlite")
	ctx := context.Background()
	h.r.o.Proofs = testIssuer{}
	for _, name := range []string{"one", "two"} {
		l := h.begin(h.zone(name, name+".example."))
		if _, err := h.r.Reconcile(ctx, l.Spec.Zone); err != nil {
			t.Fatal(err)
		}

		if out := h.r.Apply(ctx, h.request(l, "intent", "x."+name+".example. 60 IN A 192.0.2.1", false)); out.Err != nil {
			t.Fatal(out)
		}
	}
	st := h.st.(*sqlite.Store)
	image, token, err := st.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := prepareTestImage(t, *image, imageTestOptions(t, h))
	restarted, err := New(h.r.o)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := restarted.PrepareRestore(p)
	if err != nil {
		t.Fatal(err)
	}
	// Install memory only under the real writer barrier, which disallows reentry.
	done := make(chan error, 1)
	go func() {
		done <- st.PublishIfCurrent(ctx, token, sqlite.PublicationOptions{}, func() error { return restarted.InstallRestore(restore) })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("restore reentered store or compiled under barrier")
	}
	cut := restarted.Capture(time.Now())
	for _, name := range []string{"one", "two"} {
		m, ok := cut.Member(name + ".example.")
		if !ok || m.Published || m.Version.CommitSequence == 0 {
			t.Fatal(m)
		}
	}
	if err := restarted.InstallRestore(restore); !errors.Is(err, ErrConflict) {
		t.Fatal("restore reused", err)
	}
	for _, e := range restarted.entries {
		if e.view.Head.Spec.NotifySequence == 0 {
			t.Fatal("restore lost pending NOTIFY")
		}
		if err := restarted.Republish(ctx, e.member.Zone); err != nil {
			t.Fatal(err)
		}
	}
	if len(cut.entries) != 2 {
		t.Fatal("held cut mutated")
	}
	for _, name := range []string{"one", "two"} {
		m, _ := cut.Member(name + ".example.")
		if m.Published {
			t.Fatal("restored mutable entries leaked into held cut")
		}
	}
	// Source invalidation between prebuild and barrier cannot install stale data.
	_, oldToken, err := st.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := restarted.PrepareRestore(p)
	if err != nil {
		t.Fatal(err)
	}
	h.zone("third", "third.example.")
	if err := st.PublishIfCurrent(ctx, oldToken, sqlite.PublicationOptions{}, func() error { return restarted.InstallRestore(rejected) }); err == nil {
		t.Fatal("stale token admitted restore")
	}
	if rejected.installed {
		t.Fatal("failed barrier changed memory")
	}
}
