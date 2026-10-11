package policy

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// ViewName is the zonecache view the builder publishes.
const ViewName = "policy"

// Builder returns the zonecache.Builder that rebuilds the Index from every
// snapshot: ObjectSet.ClientGroups, Policies, BlockLists and the compiled
// Lists (zonecache.Cache.SetLists triggers a rebuild, so selections follow
// list swaps). The previous Index is reused when only zones or record sets
// changed. New diagnoses are logged once (Warn; Info for NotLoaded, which
// is normal while lists load after start). logger may be nil.
//
// Register it next to the zone view:
//
//	zc.Register(policy.Builder(policy.Options{DefaultNamespace: ns}, logger))
func Builder(opts Options, logger *slog.Logger) zonecache.Builder {
	if logger == nil {
		logger = slog.Default()
	}
	var (
		mu         sync.Mutex
		generation uint64
		last       []Diagnosis
		prev       *Index
	)
	return zonecache.NewBuilder(ViewName, func(objects zonecache.ObjectSet) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		if prev != nil && !objects.ListsChanged() && !objects.Changed(zonecache.KindClientGroup) &&
			!objects.Changed(zonecache.KindPolicy) && !objects.Changed(zonecache.KindBlockList) {
			return prev, nil // only zones or record sets changed
		}
		generation++
		ix := Build(Input{
			ClientGroups: objects.ClientGroups(),
			Policies:     objects.Policies(),
			BlockLists:   objects.BlockLists(),
			Lists:        objects.Lists(),
			Generation:   generation,
		}, opts)
		if diags := ix.Diagnoses(); !slices.Equal(diags, last) {
			for _, d := range diags {
				if slices.Contains(last, d) {
					continue
				}
				level := slog.LevelWarn
				if d.Reason == ReasonNotLoaded {
					level = slog.LevelInfo // normal while the lists load after start
				}
				logger.Log(context.Background(), level, "policy configuration", "kind", d.Kind, "object", d.Object, "reason", d.Reason, "message", d.Message)
			}
			last = diags
		}
		prev = ix
		return ix, nil
	})
}

// Source reads the current Index from a zonecache.Cache, once per query.
type Source struct {
	Cache *zonecache.Cache
}

// Index returns the Index of the current snapshot; nil before the first
// build (the identify stage then attaches no policy).
func (s Source) Index() *Index {
	if s.Cache == nil {
		return nil
	}
	snap := s.Cache.Snapshot()
	if snap == nil {
		return nil
	}
	ix, _ := zonecache.View[*Index](snap, ViewName)
	return ix
}

// Static is a Source that always returns the same Index (tests, tools).
type Static struct{ Idx *Index }

// Index returns the Index.
func (s Static) Index() *Index { return s.Idx }
