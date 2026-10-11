package main

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/listfetcher"
)

// Serve wiring of the blocked-services and safe-search catalogue (D60):
// one Holder feeds the policy index (blockedServices IDs), the list
// resolver (the "service:<id>" lists), the blocking stage (safe search),
// the controller (Degraded{UnknownService}), the internal listener (GET
// /internal/catalogue for replicas) and the API (GET /catalogue...).

// catalogueRefresher returns the runtime refresh of blocking.catalogue.url,
// or nil when it is empty and the embedded catalogue serves. The leader
// downloads; replicas pull the leader's copy over the internal listener.
func catalogueRefresher(cfg *config.Config, h *catalogue.Holder, m cluster.Membership, client *internalapi.Client, logger *slog.Logger) (*catalogue.Refresher, error) {
	c := cfg.Blocking.Catalogue
	if c.URL == "" {
		return nil, nil
	}
	var pull catalogue.PullFunc
	if client != nil {
		pull = func(ctx context.Context, addr string) ([]byte, string, error) {
			b, err := client.Catalogue(ctx, addr)
			return b.Body, b.Source, err
		}
	}
	return catalogue.NewRefresher(catalogue.RefresherOptions{
		URL: c.URL, RefreshInterval: c.RefreshInterval.Std(), Holder: h,
		Dir:        filepath.Join(cfg.Node.DataDir, "catalogue"),
		Membership: m, Pull: pull,
		Client: listfetcher.HTTPClient(cfg.Blocking.AllowLocalListURLs),
		Logger: logger,
	})
}

// catalogueBody serves GET /internal/catalogue.
func catalogueBody(h *catalogue.Holder) func() internalapi.CatalogueBody {
	return func() internalapi.CatalogueBody {
		c := h.Current()
		return internalapi.CatalogueBody{Body: c.Raw(), Source: c.Origin()}
	}
}

// catalogueAPI backs the /catalogue routes.
func catalogueAPI(h *catalogue.Holder, r *catalogue.Refresher) api.CatalogueSource {
	src := api.CatalogueSource{Holder: h}
	if r != nil {
		src.Status = r.Status
	}
	return src
}
