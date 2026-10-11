package listfetcher

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Objects supplies the BlockList objects both halves work from and tells
// them when those objects change.
type Objects interface {
	// BlockLists returns the current BlockLists (shared, read-only).
	BlockLists() []*v1alpha1.BlockList
	// Subscribe returns a channel that receives a value (coalesced) after
	// every change to the BlockList set, including status changes.
	Subscribe() <-chan struct{}
	// Initialized is closed once BlockLists reflects the store or a
	// local.db snapshot.
	Initialized() <-chan struct{}
	// Live reports whether BlockLists reflects the live store (not a
	// local.db snapshot), which is when unreferenced cache entries may be
	// removed.
	Live() bool
}

// ZoneCacheObjects adapts a zonecache.Cache to Objects. Create one per
// Cache and share it: it registers a single OnPublish hook.
type ZoneCacheObjects struct {
	zc *zonecache.Cache

	mu   sync.Mutex
	subs []chan struct{}
}

// NewZoneCacheObjects registers the change hook on zc.
func NewZoneCacheObjects(zc *zonecache.Cache) *ZoneCacheObjects {
	o := &ZoneCacheObjects{zc: zc}
	zc.OnPublish(func(s *zonecache.Snapshot) {
		if !s.Objects.Changed(zonecache.KindBlockList) {
			return
		}
		o.notify()
	})
	return o
}

func (o *ZoneCacheObjects) notify() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, ch := range o.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// BlockLists implements Objects. BlockLists annotated kudurru.io/enabled:
// "false" are left out: the leader stops fetching them and every node
// unloads them as if they were deleted (their cached bodies are collected
// once the store is live; enabling the list fetches it again).
func (o *ZoneCacheObjects) BlockLists() []*v1alpha1.BlockList {
	return EnabledBlockLists(o.zc.Snapshot().Objects.BlockLists())
}

// EnabledBlockLists returns bls without the disabled ones
// (v1alpha1.IsDisabled); it returns bls itself when none is disabled.
func EnabledBlockLists(bls []*v1alpha1.BlockList) []*v1alpha1.BlockList {
	for i, bl := range bls {
		if !v1alpha1.IsDisabled(bl) {
			continue
		}
		out := append(make([]*v1alpha1.BlockList, 0, len(bls)-1), bls[:i]...)
		for _, rest := range bls[i+1:] {
			if !v1alpha1.IsDisabled(rest) {
				out = append(out, rest)
			}
		}
		return out
	}
	return bls
}

// Subscribe implements Objects.
func (o *ZoneCacheObjects) Subscribe() <-chan struct{} {
	ch := make(chan struct{}, 1)
	o.mu.Lock()
	o.subs = append(o.subs, ch)
	o.mu.Unlock()
	return ch
}

// Initialized implements Objects.
func (o *ZoneCacheObjects) Initialized() <-chan struct{} { return o.zc.Initialized() }

// Live implements Objects.
func (o *ZoneCacheObjects) Live() bool { return o.zc.Snapshot().State == zonecache.Live }

// ListName is the name a BlockList's compiled list carries in the blocking
// snapshot, the query log and metrics: "<namespace>/<name>". A Policy's
// blockListRefs/allowListRefs name lists in the Policy's own namespace, so
// the policy stage looks up ListName(policy.Namespace, ref).
func ListName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// plannedSource is one source of a BlockList with everything both halves
// need to know about it.
type plannedSource struct {
	index  int
	name   string // blocking.SourceName; keys status.sources
	key    string // cache key; "" for inline sources
	format parser.Format
	inline []string
	cm     *ConfigMapRef
	// status is the published status entry for this source, if any.
	status *v1alpha1.SourceStatus
}

func (p *plannedSource) download() Source {
	return Source{Key: p.key, Format: p.format, ConfigMap: p.cm}
}

// inlineDigest is the digest reported for inline sources: the sha256 of
// the lines joined by "\n".
func inlineDigest(lines []string) string {
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}

type plannedList struct {
	obj      *v1alpha1.BlockList
	name     string // ListName
	mode     blocking.Mode
	refresh  time.Duration
	sources  []plannedSource
	statusBy map[string]*v1alpha1.SourceStatus
}

// plan expands a BlockList into its sources. defaultRefresh applies when
// spec.refreshInterval is unset; minRefresh is a floor.
func plan(bl *v1alpha1.BlockList, defaultRefresh, minRefresh time.Duration) plannedList {
	pl := plannedList{
		obj:      bl,
		name:     ListName(bl.Namespace, bl.Name),
		mode:     blocking.Mode(bl.Spec.Mode),
		refresh:  bl.Spec.RefreshInterval.Duration,
		statusBy: map[string]*v1alpha1.SourceStatus{},
	}
	if pl.mode == "" {
		pl.mode = blocking.ModeBlock
	}
	if pl.refresh <= 0 {
		pl.refresh = defaultRefresh
	}
	pl.refresh = max(pl.refresh, minRefresh)
	for i := range bl.Status.Sources {
		s := &bl.Status.Sources[i]
		pl.statusBy[s.Name] = s
	}
	for i, src := range bl.Spec.Sources {
		ps := plannedSource{index: i, name: blocking.SourceName(i, src), format: parser.Format(src.Format)}
		switch {
		case len(src.Inline) > 0:
			ps.inline = src.Inline
		case src.URL != "":
			ps.key = src.URL
		case src.ConfigMapRef != nil:
			ns := src.ConfigMapRef.Namespace
			if ns == "" {
				ns = bl.Namespace
			}
			ps.cm = &ConfigMapRef{Namespace: ns, Name: src.ConfigMapRef.Name}
			ps.key = ConfigMapKey(ns, src.ConfigMapRef.Name)
		default:
			// An empty inline list: nothing to fetch, nothing to match.
			ps.inline = []string{}
		}
		ps.status = pl.statusBy[ps.name]
		pl.sources = append(pl.sources, ps)
	}
	return pl
}
