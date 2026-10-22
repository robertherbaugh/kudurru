package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// newServingCacheCommand admits local invalidation on the primary. Forwarding
// would flush the wrong node. The cache must be the SAME instance used by DNS.
func newServingCacheCommand(runtime *servingRuntime, responses *cache.Cache) (api.CacheFlushCommand, error) {
	if runtime == nil || runtime.primary == nil || runtime.providers == nil || responses == nil {
		return nil, serving.ErrComponents
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return func(ctx context.Context, name string) (int, error) {
		if ctx == nil || len(name) > 1024 || strings.TrimSpace(name) != name || (name != "" && name != dns.CanonicalName(name)) {
			return 0, &api.Error{Status: http.StatusBadRequest, Body: api.ErrorBody{Key: api.KeyInvalid, Message: "cache command requires a canonical domain name"}}
		}
		if name != "" {
			if _, ok := dns.IsDomainName(name); !ok {
				return 0, &api.Error{Status: http.StatusBadRequest, Body: api.ErrorBody{Key: api.KeyInvalid, Message: "cache command requires a canonical domain name"}}
			}
		}
		if err := ctx.Err(); err != nil {
			return 0, currentCommandAPIError(err)
		}
		proof, err := servinghttp.Credential(ctx)
		if err != nil {
			return 0, currentCommandAPIError(err)
		}
		select {
		case <-ctx.Done():
			return 0, currentCommandAPIError(ctx.Err())
		case <-gate:
		}
		defer func() { gate <- struct{}{} }()
		removed := 0
		_, err = runtime.currentAuthCommandThen(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			_, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, rbac.Attributes{Resource: "cache", Verb: "flush", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}, runtime.opts.Now)
			return err
		}, func(ctx context.Context, _ serving.TermActivation) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Local memory changes occur outside transaction/dependency pins.
			// Cancellation after invalidation cannot undo the completed work.
			if name == "" {
				removed = responses.FlushCount()
			} else {
				removed = responses.FlushName(name)
			}
			return nil
		})
		return removed, currentCommandAPIError(err)
	}, nil
}
