package dnsdist

import (
	"context"
	"slices"
	"time"
)

// This marker is emitted only beside the unconditional managed skip-cache
// rule. It identifies the running configuration, never a prepared restart file.
const luaManagedCacheBypass = `function kudurru_managed_cache_bypass() return true end`

// dnsdist exposes an absent cache as a userdata wrapper, not Lua nil. Apply
// both fixed unsets synchronously before acknowledgment; never infer absence
// from getCache equality or from a desired/rendered file.
const managedCacheCheck = `if type(kudurru_managed_cache_bypass) ~= "function" or not kudurru_managed_cache_bypass() then error("managed cache bypass unavailable") end; getPool("resolver"):unsetCache(); getPool("authoritative"):unsetCache()`

// Caller holds mu. This is local installed-state evidence for readiness, not
// a remote backend activation grant or a renewable cache eligibility lease.
func (s *Supervisor) managedBypassInstalledLocked() bool {
	return s.cfg.Options.ManagedCacheBypass && s.configState != nil && s.configState.bypassID != "" && s.cacheBypassChild != nil &&
		s.cacheBypassChild == s.child && s.child.Running() && !s.restarting &&
		!s.cacheWatchFailed && slices.Equal(s.cacheDesired, s.endpoints) &&
		s.cacheBypassFence == (resolverFence{s.cacheSourceEpoch, s.cacheChildEpoch})
}

// The fixed child-lifetime policy dominates every remote observation. No path
// here can attach either cache, including after source/child ABA or restart.
// Console work is outside supervisor locks; only the captured child and source
// can receive the final acknowledgment. Unknown completion retires that child.
func (s *Supervisor) runManagedCacheBypass(ctx context.Context) {
	defer func() { s.mu.Lock(); s.cacheBypassChild = nil; s.mu.Unlock() }()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.cacheWake:
		case <-s.cacheObservations:
		case <-tick.C:
		}
		fence, _, eligible := s.resolverFence()
		s.mu.Lock()
		s.cacheMode, s.authoritativeCacheMode = "bypass-managed-anchors", "bypass-managed-anchors"
		s.cacheIdentity, s.authoritativeCacheIdentity = "", ""
		child := s.child
		installed := s.managedBypassInstalledLocked()
		s.mu.Unlock()
		if installed || !eligible || child == nil || !child.Running() {
			continue
		}
		err := s.console(ctx, managedCacheCheck)
		s.mu.Lock()
		current := ctx.Err() == nil && child == s.child && child.Running() &&
			fence == (resolverFence{s.cacheSourceEpoch, s.cacheChildEpoch}) &&
			!s.restarting && !s.cacheWatchFailed && slices.Equal(s.cacheDesired, s.endpoints)
		if current && err == nil {
			s.cacheBypassChild, s.cacheBypassFence = child, fence
			s.cacheError = ""
		} else if current {
			s.cacheBypassChild = nil
			s.cacheError = "managed cache bypass was not acknowledged"
		}
		s.mu.Unlock()
		if current && err != nil {
			child.Stop()
		}
	}
}
