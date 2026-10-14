package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge/dnsdist"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/blockpage"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	cachestage "github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// upstreamRetireGrace is how long a replaced upstream set stays open so
// exchanges in flight on it finish (longer than any group timeout).
const upstreamRetireGrace = 30 * time.Second

// reloadTargets are the components that apply cluster-scope settings
// without a restart (D25). Every key whose schema entry is not
// restartRequired must be handled here; config.TestRestartRequired pins
// the list.
type reloadTargets struct {
	logger    *slog.Logger
	level     *slog.LevelVar
	upstreams *upstreamstage.Stage
	upOpts    upstream.Options
	cache     *cache.Cache
	cacheStg  *cachestage.Stage
	blocking  *blockingstage.Stage
	blockPage *blockpage.Server
	// edgeFlush empties the local edge's resolver cache when the block
	// answers change; nil skips it.
	edgeFlush *dnsdist.CacheFlusher
	limiter   *ingress.RateLimiter
	qlAsync   *querylog.Async
	qlWriter  *sqlitelog.Writer
}

// subscribeReload applies settings changes from r (the settings file
// watcher and PATCH /api/v1/settings) to the running components.
func subscribeReload(ctx context.Context, r *config.Reloadable, t reloadTargets) {
	log := t.logger.With("component", "settings")
	r.Subscribe(func(_, next *config.Config, changed []string) {
		var applied, restart []string
		for _, p := range changed {
			if k, ok := config.Lookup(p); ok && k.RestartRequired {
				restart = append(restart, p)
			} else {
				applied = append(applied, p)
			}
		}
		if config.ChangedUnder(changed, "log.level") && t.level != nil {
			t.level.Set(parseLevel(next.Log.Level))
		}
		if config.ChangedUnder(changed, "upstreams", "dnssec") && t.upstreams != nil {
			validation, err := next.DNSSEC.Validator(nil)
			var set *upstream.Set
			if err == nil {
				set, err = buildUpstreams(next, changed, t.upOpts)
			}
			var prev *upstream.Set
			if err == nil {
				prev, err = t.upstreams.Replace(set, validation, next.DNSSEC.Enabled)
			}
			if err != nil {
				if set != nil {
					_ = set.Close()
				}
				log.Error("resolution settings not applied", "err", err)
			} else {
				go func() {
					select {
					case <-time.After(upstreamRetireGrace):
					case <-ctx.Done():
					}
					_ = prev.CloseReplacedBy(set)
				}()
				// Answers cached from the old upstreams (and conditional
				// routes) must not outlive them.
				if t.cache != nil {
					t.cache.Flush()
				}
				log.Info("resolution settings reloaded", "groups", set.Groups(), "dnssec", next.DNSSEC.Enabled)
			}
		}
		if config.ChangedUnder(changed, "cache") && t.cache != nil && t.cacheStg != nil {
			t.cache.Reconfigure(cache.OptionsFrom(next.Cache, nil))
			t.cacheStg.SetSettings(cachestage.SettingsFrom(next.Cache))
			off := next.Cache.MaxEntries == 0
			if off && !t.cacheStg.Disabled() {
				t.cache.Flush()
			}
			t.cacheStg.SetDisabled(off)
		}
		if config.ChangedUnder(changed, "blocking", "blockPage") && t.blocking != nil {
			t.blocking.Reconfigure(blockingstage.OptionsFromConfig(next))
			if t.edgeFlush != nil && config.ChangedUnder(changed, "blocking.defaultResponse", "blocking.blockTTL", "blocking.customIP", "blockPage.ipv4", "blockPage.ipv6") {
				t.edgeFlush.BlockResponsesChanged()
			}
		}
		if config.ChangedUnder(changed, "blockPage", "listeners.api", "unblockRequests", "blocking.blockTTL") && t.blockPage != nil {
			t.blockPage.Reconfigure(blockpage.SettingsFrom(next))
		}
		if config.ChangedUnder(changed, "ratelimit") && t.limiter != nil {
			rlc, err := ingress.RateLimitFromConfig(next.RateLimit)
			if err == nil {
				err = t.limiter.Reconfigure(rlc)
			}
			if err != nil {
				log.Error("ratelimit settings not applied", "err", err)
			}
		}
		if config.ChangedUnder(changed, "querylog.privacy", "querylog.anonymize", "querylog.exclude") && t.qlAsync != nil {
			p, err := querylog.PrivacyFrom(next.Querylog)
			if err != nil {
				log.Error("querylog privacy not applied", "err", err)
			} else {
				t.qlAsync.SetPrivacy(p)
				if t.qlWriter != nil {
					t.qlWriter.SetPrivacy(p.Mode())
				}
			}
		}
		if config.ChangedUnder(changed, "querylog.retention") && t.qlWriter != nil {
			t.qlWriter.SetRetention(next.Querylog.Retention.Raw.Std(), int64(next.Querylog.Retention.MaxDBBytes))
		}
		if len(applied) > 0 {
			log.Info("settings applied", "keys", applied)
		}
		if len(restart) > 0 {
			log.Warn("settings changed that take effect after a restart", "keys", restart)
		}
	})
}

// parseLevel maps log.level (its first element: "info,dns=debug") onto a
// slog level.
func parseLevel(s string) slog.Level {
	switch strings.SplitN(s, ",", 2)[0] {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// settingsWriter persists PATCH /api/v1/settings: into the settings file
// (Docker, systemd) or nowhere in Kubernetes, where the kudurru-settings
// ConfigMap owns the cluster settings. Without a settings file the change
// stays in memory (persisted: false).
func settingsWriter(cfg *config.Config) api.SettingsWriter {
	switch {
	case cfg.Store.Driver == "kube":
		return api.SettingsFunc(func(context.Context, *config.Config, []string) error {
			return api.ErrSettingsManaged("Kubernetes (the kudurru-settings ConfigMap)",
				"set config.settings in the Helm values (or edit the ConfigMap); every pod reloads it without a restart")
		})
	case cfg.SettingsFile == "":
		return nil
	}
	path := cfg.SettingsFile
	return api.SettingsFunc(func(_ context.Context, next *config.Config, changed []string) error {
		flat := config.Flatten(next)
		values := make(map[string]any, len(changed))
		for _, p := range changed {
			values[p] = flat[p]
		}
		return config.WriteSettingsFile(path, values)
	})
}

// buildUpstreams builds the upstream set for next and, when the recursion
// settings changed, applies them to the shared resolver, in that order:
// a reload that fails leaves the running set and the resolver as they
// were, so the two never disagree.
func buildUpstreams(next *config.Config, changed []string, opts upstream.Options) (*upstream.Set, error) {
	rc := upstream.RecursionFrom(next.Upstreams.Recursion)
	reconf := opts.Recursor != nil && config.ChangedUnder(changed, "upstreams.recursion")
	if reconf {
		if err := rc.Validate(); err != nil {
			return nil, err
		}
	}
	set, err := upstream.New(upstream.ConfigFrom(next.Upstreams), opts)
	if err != nil {
		return nil, err
	}
	if reconf {
		if err := opts.Recursor.Reconfigure(rc); err != nil {
			_ = set.Close()
			return nil, err
		}
	}
	return set, nil
}
