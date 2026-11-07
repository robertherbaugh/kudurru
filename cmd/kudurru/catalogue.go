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
// one Holder feeds the policy index (blockedServices and
// blockedCategories), the list resolver (the "service:<id>" lists), the
// blocking stage (safe search), the controller (Degraded{UnknownService},
// {UnknownCategory}), the internal listener (GET /internal/catalogue and
// /internal/catalogue/adguard for replicas) and the API (GET /catalogue...).

// catalogueRefreshers are the runtime refreshes of the catalogue.
type catalogueRefreshers struct {
	// document refreshes blocking.catalogue.url; nil when it is empty and
	// the embedded catalogue serves.
	document *catalogue.Refresher
	// adguard imports blocking.catalogue.adguardServicesURL; nil when the
	// opt-in is off.
	adguard *catalogue.Refresher
}

// newCatalogueRefreshers returns the refreshers the configuration asks
// for. The leader downloads; replicas pull the leader's copy over the
// internal listener.
func newCatalogueRefreshers(cfg *config.Config, h *catalogue.Holder, m cluster.Membership, client *internalapi.Client, logger *slog.Logger) (catalogueRefreshers, error) {
	var out catalogueRefreshers
	c := cfg.Blocking.Catalogue
	opts := func(kind, url string, pull catalogue.PullFunc) catalogue.RefresherOptions {
		return catalogue.RefresherOptions{
			Kind: kind, URL: url, RefreshInterval: c.RefreshInterval.Std(), Holder: h,
			Dir:        filepath.Join(cfg.Node.DataDir, "catalogue"),
			Membership: m, Pull: pull,
			Client:    listfetcher.HTTPClient(cfg.Blocking.AllowLocalListURLs),
			HTTPSOnly: cfg.Blocking.HTTPSOnly,
			Logger:    logger,
		}
	}
	pull := func(get func(context.Context, string) (internalapi.CatalogueBody, error)) catalogue.PullFunc {
		if client == nil {
			return nil
		}
		return func(ctx context.Context, addr string) ([]byte, string, error) {
			b, err := get(ctx, addr)
			return b.Body, b.Source, err
		}
	}
	var err error
	if c.URL != "" {
		var get func(context.Context, string) (internalapi.CatalogueBody, error)
		if client != nil {
			get = client.Catalogue
		}
		if out.document, err = catalogue.NewRefresher(opts(catalogue.KindCatalogue, c.URL, pull(get))); err != nil {
			return out, err
		}
	}
	if c.AdGuardServicesURL != "" {
		var get func(context.Context, string) (internalapi.CatalogueBody, error)
		if client != nil {
			get = client.CatalogueImport
		}
		if out.adguard, err = catalogue.NewRefresher(opts(catalogue.KindAdGuard, c.AdGuardServicesURL, pull(get))); err != nil {
			return out, err
		}
	}
	return out, nil
}

// catalogueBody serves GET /internal/catalogue: the catalogue document
// alone, never the import.
func catalogueBody(h *catalogue.Holder) func() internalapi.CatalogueBody {
	return func() internalapi.CatalogueBody {
		c := h.Base()
		return internalapi.CatalogueBody{Body: c.Raw(), Source: c.Origin()}
	}
}

// catalogueImportBody serves GET /internal/catalogue/adguard.
func catalogueImportBody(h *catalogue.Holder) func() (internalapi.CatalogueBody, bool) {
	return func() (internalapi.CatalogueBody, bool) {
		imp := h.Import()
		if imp == nil {
			return internalapi.CatalogueBody{}, false
		}
		return internalapi.CatalogueBody{Body: imp.Raw(), Source: imp.Origin()}, true
	}
}

// catalogueAPI backs the /catalogue routes.
func catalogueAPI(h *catalogue.Holder, r catalogueRefreshers) api.CatalogueSource {
	src := api.CatalogueSource{Holder: h}
	if r.document != nil {
		src.Status = r.document.Status
	}
	if r.adguard != nil {
		src.ImportStatus = r.adguard.Status
	}
	return src
}
