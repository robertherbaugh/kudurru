package listfetcher

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func completeFixture(t *testing.T) (*DiskCache, *public.BlockList, CompleteOptions) {
	t.Helper()
	cache, err := OpenDiskCache(t.TempDir(), quiet())
	if err != nil {
		t.Fatal(err)
	}
	const url = "https://lists.example.test/domains"
	e, _, err := cache.Put(url, strings.NewReader("old.example.test\n"), PutOptions{MaxSize: 1024, Fetched: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "source", URL: url, Format: "domains"}}}, Status: public.BlockListStatus{Sources: []public.SourceStatus{{Name: "source", Digest: e.Digest}}}}
	return cache, list, CompleteOptions{Cache: cache, Catalogue: catalogue.Embedded()}
}

func TestPrepareCompletePinsPublishedDigestsWithoutPartialFallback(t *testing.T) {
	cache, list, opts := completeFixture(t)
	first, err := PrepareComplete(t.Context(), []*public.BlockList{list}, opts)
	if err != nil {
		t.Fatal(err)
	}
	name := ListName(list.Namespace, list.Name)
	if first.Snapshot.Contains("old.example.test", name)[name] == "" {
		t.Fatal("published body absent")
	}
	entry, _, err := cache.Put(list.Spec.Sources[0].URL, strings.NewReader("new.example.test\n"), PutOptions{MaxSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if failed, err := PrepareComplete(t.Context(), []*public.BlockList{list}, opts); !errors.Is(err, ErrIncomplete) || failed != nil {
		t.Fatal("unpublished replacement served", err)
	}
	list.Status.Sources[0].Digest = entry.Digest
	second, err := PrepareComplete(t.Context(), []*public.BlockList{list}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity == second.Identity || second.Snapshot.Contains("new.example.test", name)[name] == "" || first.Snapshot.Contains("old.example.test", name)[name] == "" || first.Snapshot.Contains("new.example.test", name)[name] != "" {
		t.Fatal("compiled generations were mixed")
	}
	missing := list.DeepCopy()
	missing.Name = "missing"
	missing.Spec.Sources[0].URL = "https://lists.example.test/not-cached"
	if candidate, err := PrepareComplete(t.Context(), []*public.BlockList{list, missing}, opts); err == nil || candidate != nil {
		t.Fatal("partial list generation accepted")
	}
	for _, digest := range []string{"", strings.Repeat("a", 64), strings.ToUpper(entry.Digest)} {
		list.Status.Sources[0].Digest = digest
		if candidate, err := PrepareComplete(t.Context(), []*public.BlockList{list}, opts); err == nil || candidate != nil {
			t.Fatal("unproven digest accepted")
		}
	}
}

func TestPrepareCompleteRejectsCorruptionAndAmbiguousInputs(t *testing.T) {
	cache, list, opts := completeFixture(t)
	for _, edit := range []func(*public.BlockList){
		func(b *public.BlockList) { b.Status.Sources = append(b.Status.Sources, b.Status.Sources[0]) },
		func(b *public.BlockList) { b.Spec.Sources = append(b.Spec.Sources, b.Spec.Sources[0]) },
		func(b *public.BlockList) { b.Spec.Sources[0].Format = "unsupported" },
	} {
		bad := list.DeepCopy()
		edit(bad)
		if got, err := PrepareComplete(t.Context(), []*public.BlockList{bad}, opts); err == nil || got != nil {
			t.Fatal("ambiguous or invalid candidate compiled")
		}
	}
	if got, err := PrepareComplete(t.Context(), []*public.BlockList{list, list}, opts); err == nil || got != nil {
		t.Fatal("duplicate list accepted")
	}
	if err := os.WriteFile(cache.Path(list.Spec.Sources[0].URL), []byte("bad.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := PrepareComplete(t.Context(), []*public.BlockList{list}, opts); !errors.Is(err, ErrDigestMismatch) || got != nil {
		t.Fatal("corrupt body accepted", err)
	}
}

func TestPrepareCompleteDisabledAndRPZAreExplicit(t *testing.T) {
	_, list, opts := completeFixture(t)
	inline := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "inline"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Inline: []string{"||inline.example.test^"}}}}}
	empty := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "empty"}}
	disabled := list.DeepCopy()
	disabled.Name = "disabled"
	disabled.Annotations = map[string]string{public.EnabledAnnotation: "false"}
	disabled.Status.Sources = nil
	disabledSource := list.DeepCopy()
	disabledSource.Name = "source-disabled"
	off := false
	disabledSource.Spec.Sources[0].Enabled = &off
	disabledSource.Status.Sources = nil
	rpz := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "rpz"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Format: "rpz", RPZ: &public.RPZSource{ZoneName: "rpz.example.test", Primaries: []string{"192.0.2.1:53"}}}}}}
	got, err := PrepareComplete(t.Context(), []*public.BlockList{inline, disabled, disabledSource, empty, rpz}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got.Snapshot.List("default/disabled") != nil || got.Snapshot.List("default/source-disabled") == nil || got.Snapshot.List("empty") == nil || got.Snapshot.List("rpz") == nil || got.Snapshot.Contains("inline.example.test", "inline")["inline"] == "" {
		t.Fatal("explicit empty/disabled/inline projection lost")
	}
	selection, err := got.Snapshot.Select([]string{"rpz"}, blocking.SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if names := selection.BlockLists(); len(names) != 1 || names[0] != "rpz" {
		t.Fatal("RPZ policy identity disappeared from selection")
	}
	oldID := got.Identity
	inline.Spec.Sources[0].Inline[0] = "||replacement.example.test^"
	if got.Snapshot.Contains("inline.example.test", "inline")["inline"] == "" {
		t.Fatal("caller mutation changed prepared rules")
	}
	fresh, err := PrepareComplete(t.Context(), []*public.BlockList{inline}, opts)
	if err != nil || fresh.Identity == oldID {
		t.Fatal("source identity was reused", err)
	}
}

func TestPrepareCompleteBoundsAndCancellation(t *testing.T) {
	_, list, opts := completeFixture(t)
	for _, edit := range []func(*CompleteOptions){
		func(o *CompleteOptions) { o.MaxBodyBytes = 1 }, func(o *CompleteOptions) { o.MaxBytes = 1 }, func(o *CompleteOptions) { o.MaxLists = -1 }, func(o *CompleteOptions) { o.Compile.RegexHardCap = -1 },
	} {
		o := opts
		edit(&o)
		if got, err := PrepareComplete(t.Context(), []*public.BlockList{list}, o); err == nil || got != nil {
			t.Fatal("unbounded or oversized candidate accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := PrepareComplete(ctx, []*public.BlockList{list}, opts); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatal("canceled preparation continued", err)
	}
}
