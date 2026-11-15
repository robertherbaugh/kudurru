package listfetcher

import (
	"errors"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
)

func TestPrepareCompleteAggregateProgramRefusal(t *testing.T) {
	_, first, opts := completeFixture(t)
	first.Spec.Sources = []public.ListSource{{Name: "original", Format: "regex", Inline: []string{"a.*z"}}}
	first.Status.Sources = nil
	one, err := PrepareComplete(t.Context(), []*public.BlockList{first}, opts)
	if err != nil {
		t.Fatal(err)
	}
	bytes := one.Snapshot.List(ListName(first.Namespace, first.Name)).ProgramBytes()
	if bytes < 1 {
		t.Fatal("missing reserve")
	}
	second := first.DeepCopy()
	second.Name = "second"
	// Each list fits independently. Together they exceed the one candidate
	// ceiling; neither list nor an embedded catalog subset may be returned.
	opts.MaxProgramBytes = bytes + bytes/2
	parent := matcher.NewProgramBudget(128 << 20)
	opts.Compile.ProgramBudget = parent
	if partial, err := PrepareComplete(t.Context(), []*public.BlockList{first, second}, opts); partial != nil || !errors.Is(err, matcher.ErrProgramBudget) || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("partial=%v error=%v", partial != nil, err)
	}
	if parent.Used() != 0 {
		t.Fatal("refused candidate leaked shared reservation")
	}
	if one.Snapshot.Contains("abz", ListName(first.Namespace, first.Name)) == nil {
		t.Fatal("held original candidate changed")
	}
	opts.MaxProgramBytes = 128 << 20
	two, err := PrepareComplete(t.Context(), []*public.BlockList{first, second}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(two.Snapshot.Contains("abz", ListName(first.Namespace, first.Name), ListName(second.Namespace, second.Name))) != 2 {
		t.Fatal("complete admitted candidate lost provenance")
	}
	opts.MaxProgramBytes--
	other, err := PrepareComplete(t.Context(), []*public.BlockList{first, second}, opts)
	if err != nil || other.Identity == two.Identity {
		t.Fatal("compile resource policy absent from candidate identity", err)
	}
}

func TestPrepareCompleteProgramDefaultAndHardCap(t *testing.T) {
	_, list, opts := completeFixture(t)
	list.Spec.Sources = []public.ListSource{{Name: "original", Format: "regex", Inline: []string{"a.*z"}}}
	list.Status.Sources = nil
	objects := []*public.BlockList{list}
	defaulted, err := PrepareComplete(t.Context(), objects, opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.MaxProgramBytes = 128 << 20
	explicit, err := PrepareComplete(t.Context(), objects, opts)
	if err != nil || explicit.Identity != defaulted.Identity {
		t.Fatal("default candidate budget differs from explicit 128 MiB", err)
	}
	for _, limit := range []int64{-1, 128<<20 + 1, 512 << 20} {
		opts.MaxProgramBytes = limit
		parent := matcher.NewProgramBudget(128 << 20)
		opts.Compile.ProgramBudget = parent
		if partial, err := PrepareComplete(t.Context(), objects, opts); partial != nil || !errors.Is(err, ErrTooLarge) {
			t.Fatalf("candidate limit=%d partial=%v error=%v", limit, partial != nil, err)
		}
		if parent.Used() != 0 {
			t.Fatal("invalid candidate ceiling leaked a reservation")
		}
	}
}
