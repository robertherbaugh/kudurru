package servingbuild

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// PolicyListReads reports actual text/RPZ counts from original request owners.
// It has no live source, manager, store, or publisher to recapture.
type PolicyListReads struct{}

// NewPolicyListReads constructs the stateless original-request list reader.
func NewPolicyListReads() *PolicyListReads { return &PolicyListReads{} }

// PolicyListState preserves raw acquired counts for unavailable RPZ data.
// Desired source/status metadata is projected separately by the API. Callers
// supply the single observation time shared with schedule evaluation.
func (r *PolicyListReads) PolicyListState(ctx context.Context, namespace, name string, now time.Time) (int, bool, error) {
	if r == nil || ctx == nil || now.IsZero() || name == "" || len(name) > 253 || len(namespace) > 253 ||
		!utf8.ValidString(name) || !utf8.ValidString(namespace) || strings.ContainsAny(name+namespace, "/\x00") {
		return 0, false, serving.ErrComponents
	}
	identity, err := servinghttp.Identity(ctx)
	if err != nil {
		return 0, false, err
	}
	objects, err := servinghttp.Component[*zonecache.Snapshot](ctx, "objects")
	if err != nil {
		return 0, false, err
	}
	runtime, err := servinghttp.Component[*RuntimeOwner](ctx, "runtime")
	if err != nil {
		return 0, false, err
	}
	zones, err := servinghttp.Component[*servingzones.Prepared](ctx, "zones")
	if err != nil {
		return 0, false, err
	}
	if identity.Base.Validate() != nil || !slices.Equal(objects.Objects.Kinds(), zonecache.DefaultKinds()) || diagnosticOwners(objects, runtime, zones, identity) != nil || objects.Objects.Lists() == nil {
		return 0, false, serving.ErrComponents
	}
	obj, found := objects.Objects.Get(store.Key{Kind: "BlockList", Namespace: namespace, Name: name})
	if !found {
		return 0, false, store.ErrNotFound
	}
	list, ok := obj.(*public.BlockList)
	canonical := name
	if namespace != "" {
		canonical = namespace + "/" + name
	}
	if !ok || list.UID == "" || list.Namespace != namespace || list.Name != name || !zones.HasList(canonical) {
		return 0, false, serving.ErrComponents
	}
	if public.IsDisabled(list) {
		return 0, false, nil
	}
	entries, ready := 0, false
	mode := blocking.Mode(list.Spec.Mode)
	if mode == "" {
		mode = blocking.ModeBlock
	}
	if text := objects.Objects.Lists().List(canonical); text != nil {
		if text.Name() != canonical || text.Mode() != mode || text.Stats().Entries < 0 {
			return 0, false, serving.ErrComponents
		}
		entries, ready = text.Stats().Entries, true
	}
	for _, source := range list.Spec.Sources {
		if source.IsEnabled() && (source.RPZ != nil || source.Format == "rpz") {
			selection, err := zones.SelectRPZ([]string{canonical})
			if err != nil {
				return 0, false, serving.ErrComponents
			}
			rules, available := selection.Summary(now)
			if entries > math.MaxInt-rules {
				return 0, false, serving.ErrComponents
			}
			entries, ready = entries+rules, ready && available
			break
		}
	}
	if ctx.Err() != nil {
		return 0, false, ctx.Err()
	}
	return entries, ready, nil
}
