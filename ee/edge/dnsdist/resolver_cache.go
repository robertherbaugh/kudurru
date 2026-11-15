package dnsdist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

const maxResolverSources = 128

type resolverFence struct{ source, child uint64 }
type zoneObservation struct {
	fence resolverFence
	names []string
}
type backendZones struct {
	endpoint cluster.Endpoint
	zones    internalapi.ZoneList
	err      error
}
type resolverObservation struct {
	fence     resolverFence
	endpoints []cluster.Endpoint
	eligible  bool
	results   []backendZones
	sent      time.Time
}

func (s *Supervisor) wakeResolverCache() {
	select {
	case s.cacheWake <- struct{}{}:
	default:
	}
}
func (s *Supervisor) noteResolverSource(eps []cluster.Endpoint, failed bool) {
	s.mu.Lock()
	s.cacheSourceEpoch++
	s.cacheDesired, s.cacheWatchFailed = slices.Clone(eps), failed
	s.mu.Unlock()
	s.wakeResolverCache()
}
func (s *Supervisor) resolverFence() (resolverFence, []cluster.Endpoint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return resolverFence{s.cacheSourceEpoch, s.cacheChildEpoch}, slices.Clone(s.endpoints), !s.restarting && !s.cacheWatchFailed && s.status != nil && s.statusErr == nil && slices.Equal(s.cacheDesired, s.endpoints)
}
func (s *Supervisor) resolverObservationCurrent(fence resolverFence) bool {
	current, _, eligible := s.resolverFence()
	return eligible && current == fence
}
func (s *Supervisor) offerResolverObservation(o resolverObservation) {
	select {
	case <-s.cacheObservations:
	default:
	}
	select {
	case s.cacheObservations <- o:
	default:
	}
}

// readBackendZones has one total deadline, two workers and a finite source
// budget. An absent/unpolled source is a failed proof, never inferred current.
func (s *Supervisor) readBackendZones(ctx context.Context, eps []cluster.Endpoint) ([]backendZones, time.Time) {
	sent := time.Now()
	if len(eps) > maxResolverSources {
		return nil, sent
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	results := make([]backendZones, len(eps))
	jobs := make(chan int, len(eps))
	for i, e := range eps {
		results[i] = backendZones{endpoint: e, err: errors.New("source unavailable")}
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for i := range jobs {
				if ctx.Err() != nil || eps[i].InternalAddress == "" {
					continue
				}
				results[i].zones, results[i].err = s.cfg.Zones.Zones(ctx, eps[i].InternalAddress)
			}
		})
	}
	wg.Wait()
	return results, sent
}

type resolverHistory struct {
	instance, identity string
	publication        uint64
	observed           time.Time
	retired            map[string]bool
}

// accept cannot turn an A->B->A publication or retired source instance back
// into a cache hit. Overflow keeps caching disabled instead of forgetting proof.
func (h *resolverHistory) accept(st *internalapi.ResolverState) bool {
	if h.instance == "" {
		h.instance = st.Instance
		h.retired = map[string]bool{}
	}
	if h.instance != st.Instance {
		if h.retired[st.Instance] || len(h.retired) >= 32 {
			return false
		}
		h.retired[h.instance] = true
		h.instance, h.identity, h.publication, h.observed = st.Instance, "", 0, time.Time{}
	}
	if st.Publication < h.publication || st.Publication == h.publication && st.Identity != h.identity || st.ObservedAt.Before(h.observed) {
		return false
	}
	h.publication, h.identity, h.observed = st.Publication, st.Identity, st.ObservedAt
	return true
}

func resolverSourceKey(e cluster.Endpoint) string {
	data, _ := json.Marshal([]string{e.Name, e.Address, e.InternalAddress})
	return string(data)
}

// resolverTarget returns only a local eligibility key and expiry; no remote
// field can select a pool, name, cache size or console payload.
func resolverTarget(o resolverObservation, now time.Time, histories map[string]*resolverHistory) (string, time.Time, string) {
	if !o.eligible || len(o.endpoints) == 0 || len(o.endpoints) > maxResolverSources || len(o.results) != len(o.endpoints) {
		return "", time.Time{}, "bypass-source"
	}
	live := map[string]bool{}
	for _, e := range o.endpoints {
		live[resolverSourceKey(e)] = true
	}
	for key := range histories {
		if !live[key] {
			delete(histories, key)
		}
	}
	keys := make([]string, 0, len(o.results))
	var expires time.Time
	mode := "qualified"
	for _, result := range o.results {
		st, e := result.zones.Resolver, result.endpoint
		if result.err != nil {
			mode = "bypass-unavailable"
			continue
		}
		if st == nil {
			mode = "bypass-legacy"
			continue
		}
		lifetime := st.ValidUntil.Sub(st.ObservedAt)
		deadline := o.sent.Add(lifetime) // request latency can only shorten proof
		if st.Version != 1 || st.Node == "" || len(st.Node) > 256 || st.Node != result.zones.Node || e.Name != "" && e.Name != st.Node || !internalapi.ValidDigest(st.Instance) || !internalapi.ValidDigest(st.Identity) || st.Publication == 0 || len(st.Phase) > 64 || st.Phase == "" || st.ObservedAt.IsZero() || lifetime <= 0 || lifetime > internalapi.ResolverLease || !now.Before(deadline) {
			mode = "bypass-invalid"
			continue
		}
		key := resolverSourceKey(e)
		h := histories[key]
		if h == nil {
			h = &resolverHistory{}
			histories[key] = h
		}
		if !h.accept(st) {
			mode = "bypass-stale"
			continue
		}
		if !st.Available {
			mode = "bypass-input-unavailable"
			switch st.UnavailableReason {
			case "async-ptr", "dhcp-runtime", "client-scope", "rule-client-scope", "zone-client-acl", "zone-client-view", "client-acl", "ecs", "rebinding-client-scope", "rate-limit", "managed-anchors":
				mode = "bypass-" + st.UnavailableReason
			}
			continue
		}
		// A captured future blocking boundary cannot outlive its lease.
		if st.DisabledUntil.After(st.ObservedAt) && st.DisabledUntil.Sub(st.ObservedAt) < lifetime {
			mode = "bypass-invalid"
			continue
		}
		data, _ := json.Marshal(struct {
			Source, Instance, Identity, Phase string
			Publication                       uint64
			Enabled                           bool
			DisabledUntil                     time.Time
		}{key, st.Instance, st.Identity, st.Phase, st.Publication, st.BlockingEnabled, st.DisabledUntil})
		keys = append(keys, string(data))
		if expires.IsZero() || deadline.Before(expires) {
			expires = deadline
		}
	}
	if mode != "qualified" {
		return "", time.Time{}, mode
	}
	slices.Sort(keys)
	data, _ := json.Marshal(keys)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), expires, mode
}

func (s *Supervisor) resolverCacheCommand(enabled bool) string {
	if !enabled || s.cfg.Options.ManagedCacheBypass {
		return `getPool("resolver"):unsetCache()`
	}
	c := edge.DefaultResolverCache()
	if s.cfg.Options.ResolverCacheSize > 0 {
		c.Size = s.cfg.Options.ResolverCacheSize
	}
	return fmt.Sprintf(`getPool("resolver"):setCache(newPacketCache(%d,{maxTTL=%d,minTTL=%d,staleTTL=%d,temporaryFailureTTL=%d,maxNegativeTTL=%d}))`, c.Size, c.MaxTTL, c.MinTTL, c.StaleTTL, c.TemporaryFailureTTL, c.MaxNegativeTTL)
}

// authoritativeTarget adds an explicit original zone proof to the SAME
// validated source/child/time observation. No remote field selects a pool.
func authoritativeTarget(o resolverObservation, key, mode string, enabled bool) (string, string) {
	if !enabled {
		return "", "disabled"
	}
	if key == "" {
		return "", mode
	}
	if len(o.results) == 0 || len(o.results) != len(o.endpoints) {
		return "", "bypass-source"
	}
	for _, result := range o.results {
		st := result.zones.Resolver
		if st == nil || !st.AuthoritativeAvailable {
			mode = "bypass-authoritative-proof"
			if st != nil {
				switch st.AuthoritativeReason {
				case "zone-runtime", "zone-unavailable", "zone-expiry", "signed-zone", "zone-client-view", "zone-client-acl":
					mode = "bypass-" + st.AuthoritativeReason
				}
			}
			return "", mode
		}
	}
	return key, "qualified"
}

func (s *Supervisor) authoritativeCacheCommand(enabled bool) string {
	if !enabled || s.cfg.Options.ManagedCacheBypass {
		return `getPool("authoritative"):unsetCache()`
	}
	c := edge.AuthoritativeCache(s.cfg.Options.AuthoritativeCacheTTL)
	return fmt.Sprintf(`getPool("authoritative"):setCache(newPacketCache(%d,{maxTTL=%d,minTTL=%d,staleTTL=%d,temporaryFailureTTL=%d,maxNegativeTTL=%d}))`, c.Size, c.MaxTTL, c.MinTTL, c.StaleTTL, c.TemporaryFailureTTL, c.MaxNegativeTTL)
}

type packetCacheKeys struct{ resolver, authoritative string }

// runResolverCache is the sole remote packet-cache mutator for both fixed
// pools. One bounded console acknowledgment covers every changed target.
func (s *Supervisor) runResolverCache(ctx context.Context) {
	if s.cfg.Options.ManagedCacheBypass {
		s.runManagedCacheBypass(ctx)
		return
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	histories := map[string]*resolverHistory{}
	var latest *resolverObservation
	var appliedFence resolverFence
	applied := packetCacheKeys{}
	var retry time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-s.cacheObservations:
			latest = &o
		case <-s.cacheWake:
		case <-tick.C:
		}
		now := time.Now()
		fence, _, eligible := s.resolverFence()
		if appliedFence.child != fence.child {
			// Every new child loads the validated restart target with BOTH pools
			// uncached. Never restore former authorization from persisted YAML.
			applied, appliedFence = packetCacheKeys{}, fence
		}
		key, mode := "", "bypass-source"
		var observation resolverObservation
		if latest != nil && eligible && latest.fence == fence {
			observation = *latest
			key, _, mode = resolverTarget(observation, now, histories)
		}
		authKey, authMode := authoritativeTarget(observation, key, mode, s.cfg.Options.AuthoritativeCache)
		target := packetCacheKeys{key, authKey}
		if !retry.IsZero() && now.Before(retry) {
			continue
		}
		if applied == target && (appliedFence == fence || target == (packetCacheKeys{})) {
			appliedFence = fence
			s.mu.Lock()
			s.cacheMode, s.cacheIdentity, s.cacheError = mode, key, ""
			s.authoritativeCacheMode, s.authoritativeCacheIdentity = authMode, authKey
			s.mu.Unlock()
			continue
		}
		var commands []string
		if applied.resolver != key || appliedFence != fence && key != "" {
			commands = append(commands, s.resolverCacheCommand(key != ""))
		}
		if applied.authoritative != authKey || appliedFence != fence && authKey != "" {
			commands = append(commands, s.authoritativeCacheCommand(authKey != ""))
		}
		err := s.console(ctx, strings.Join(commands, ";"))
		after, _, current := s.resolverFence()
		if err != nil || after != fence || (key != "" || authKey != "") && !current {
			applied = packetCacheKeys{"uncertain", "uncertain"}
			s.mu.Lock()
			s.cacheMode, s.cacheIdentity = "uncertain", ""
			s.authoritativeCacheMode, s.authoritativeCacheIdentity = "uncertain", ""
			s.cacheError = "packet cache convergence failed"
			child := s.child
			if s.cacheChildEpoch != fence.child {
				child = nil
			}
			s.mu.Unlock()
			// Unknown acknowledgment can leave either target attached. Retire only
			// this captured child; its successor starts uncached. Never replay DNS.
			if child != nil && ctx.Err() == nil {
				child.Stop()
			}
			retry = time.Now().Add(time.Second)
			continue
		}
		applied, appliedFence, retry = target, fence, time.Time{}
		s.mu.Lock()
		s.cacheMode, s.cacheIdentity, s.cacheError = mode, key, ""
		s.authoritativeCacheMode, s.authoritativeCacheIdentity = authMode, authKey
		s.mu.Unlock()
	}
}
