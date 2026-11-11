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
	for _, digest := range []string{strings.Repeat("a", 64), strings.ToUpper(entry.Digest)} {
		list.Status.Sources[0].Digest = digest
		if candidate, err := PrepareComplete(t.Context(), []*public.BlockList{list}, opts); err == nil || candidate != nil {
			t.Fatal("unproven digest accepted")
		}
	}
	// Without a published digest no body was ever published for the
	// source: it compiles empty and never from the cache (D373).
	list.Status.Sources[0].Digest = ""
	candidate, err := PrepareComplete(t.Context(), []*public.BlockList{list}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Snapshot.Contains("new.example.test", name)[name] != "" || candidate.Snapshot.Contains("old.example.test", name)[name] != "" {
		t.Fatal("a cached body without a published digest was served")
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

// A URL source for which no body was ever published compiles empty, as
// 0.1.0 served it, instead of holding back every candidate (D373): a source
// whose first download has not finished (no status entry, or an entry with
// neither digest nor error, as while the URL accepts the connection and
// never answers), and one whose fetch failed before any body arrived. A
// cached body without a published digest is never used, the identity tells
// the states apart, and a published digest is again served from the cache.
func TestPrepareCompleteServesAFailedFirstFetchEmpty(t *testing.T) {
	_, list, opts := completeFixture(t)
	name := ListName(list.Namespace, list.Name)
	inline := public.ListSource{Name: "inline", Format: "domains", Inline: []string{"inline.example.test"}}
	list.Spec.Sources = append(list.Spec.Sources, inline)
	published := list.Status.Sources[0]

	identities := map[string]string{}
	for state, status := range map[string][]public.SourceStatus{
		"never attempted":          nil,
		"first download running":   {{Name: "source"}},
		"first download failed":    {{Name: "source", Error: "listfetcher: GET https://lists.example.test/domains: 404 Not Found"}},
		"first download timed out": {{Name: "source", Error: "listfetcher: GET https://lists.example.test/domains: context deadline exceeded"}},
	} {
		list.Status.Sources = status
		got, err := PrepareComplete(t.Context(), []*public.BlockList{list}, opts)
		if err != nil {
			t.Fatalf("%s: the source held back the candidate: %v", state, err)
		}
		if got.Snapshot.Contains("old.example.test", name)[name] != "" {
			t.Fatalf("%s: a cached body without a published digest was served", state)
		}
		if got.Snapshot.Contains("inline.example.test", name)[name] == "" {
			t.Fatalf("%s: the other sources of the list were not served", state)
		}
		identities[state] = got.Identity
	}
	if identities["never attempted"] != identities["first download running"] || identities["first download failed"] != identities["first download timed out"] ||
		identities["never attempted"] == identities["first download failed"] {
		t.Fatalf("identities %v", identities)
	}
	list.Status.Sources = []public.SourceStatus{published}
	got, err := PrepareComplete(t.Context(), []*public.BlockList{list}, opts)
	if err != nil || got.Snapshot.Contains("old.example.test", name)[name] == "" || got.Identity == identities["never attempted"] {
		t.Fatal("the published body was not served", err)
	}
}
