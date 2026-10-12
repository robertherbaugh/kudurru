package listfetcher

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
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

// SourceKey is the list cache key of a URL or ConfigMap source of a
// BlockList in namespace: the URL, or configmap://<namespace>/<name>; ""
// for sources kept in the object.
func SourceKey(namespace string, src v1alpha1.ListSource) string {
	switch {
	case src.URL != "":
		return src.URL
	case src.ConfigMapRef != nil:
		ns := src.ConfigMapRef.Namespace
		if ns == "" {
			ns = namespace
		}
		return ConfigMapKey(ns, src.ConfigMapRef.Name)
	}
	return ""
}

// plannedSource is one source of a BlockList with everything both halves
// need to know about it.
type plannedSource struct {
	index  int
	name   string // blocking.SourceName; keys status.sources
	key    string // cache key; "" for inline sources
	format parser.Format
	// subdomains is spec.sources[].subdomains.
	subdomains bool
	// disabled is spec.sources[].enabled: false. A disabled source is
	// neither downloaded nor compiled; its cache entry stays (it is still
	// referenced) so that switching it back on needs no download.
	disabled bool
	inline   []string
	// exclude is spec.sources[].exclude plus, for structured entries, the
	// rules of the entries switched off.
	exclude []string
	cm      *ConfigMapRef
	// status is the published status entry for this source, if any.
	status *v1alpha1.SourceStatus
}

// fp is the part of a list fingerprint this source contributes besides
// its digest.
func (p *plannedSource) fp(digest string) []string {
	return []string{p.name, string(p.format), strconv.FormatBool(p.subdomains), digest, blocking.ExcludeDigest(p.exclude)}
}

// compileSource is the blocking.Source of an inline or structured source.
func (p *plannedSource) compileSource() blocking.Source {
	src := blocking.LinesSource(p.name, p.format, p.inline)
	src.HostsSubdomains = p.subdomains
	src.Exclude = p.exclude
	return src
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
		ps := plannedSource{index: i, name: blocking.SourceName(i, src), format: parser.Format(src.Format), subdomains: src.Subdomains, disabled: !src.IsEnabled()}
		ps.exclude = src.Exclude
		switch {
		case len(src.Inline) > 0 || len(src.Entries) > 0:
			ps.inline, ps.format, ps.exclude, _ = blocking.InlineLines(src)
			if len(src.Entries) > 0 {
				ps.subdomains = false
			}
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

// allDisabled reports whether the list has sources and every one of them
// is switched off (spec.sources[].enabled: false). Such a list compiles
// empty: it is loaded with 0 entries instead of waiting for a body.
func (pl *plannedList) allDisabled() bool {
	if len(pl.sources) == 0 {
		return false
	}
	for i := range pl.sources {
		if !pl.sources[i].disabled {
			return false
		}
	}
	return true
}

// AllSourcesDisabled is the name of the empty source an all-disabled list
// compiles from.
const AllSourcesDisabled = "all sources disabled"

// emptySource is the source of a list whose sources are all disabled.
func emptySource() blocking.Source {
	return blocking.LinesSource(AllSourcesDisabled, parser.FormatDomains, nil)
}

// disabledStatus is the status entry of a disabled source: the digest,
// etag, fetch time and counts of its last fetch, enabled: false and no
// error.
func disabledStatus(ps *plannedSource) v1alpha1.SourceStatus {
	off := false
	ss := v1alpha1.SourceStatus{Name: ps.name, Enabled: &off}
	if ps.key == "" {
		ss.Digest = inlineDigest(ps.inline)
	}
	if p := ps.status; p != nil {
		if ps.key != "" {
			ss.Digest, ss.ETag = p.Digest, p.ETag
		}
		ss.Entries, ss.Invalid, ss.Excluded = p.Entries, p.Invalid, p.Excluded
		ss.StaleExcludes = slices.Clone(p.StaleExcludes)
		ss.LastFetched = p.LastFetched.DeepCopy()
	}
	return ss
}
