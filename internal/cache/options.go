package cache

import "github.com/robertherbaugh/kudurru/internal/config"

// OptionsFrom maps the cache settings onto Options (the stage-level TTL
// clamps are read by the cache stage). maxEntries 0 means the cache is
// disabled; callers should then leave the cache stage out of the chain.
func OptionsFrom(c config.Cache, m *Metrics) Options {
	return Options{
		MaxEntries:        c.MaxEntries,
		StaleTTL:          c.ServeStale.TTL.Std(),
		PrefetchThreshold: c.Prefetch.Threshold,
		PrefetchWindow:    c.Prefetch.Window.Std(),
		Metrics:           m,
	}
}
