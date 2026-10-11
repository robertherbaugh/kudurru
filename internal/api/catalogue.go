package api

import (
	"context"
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
)

// CatalogueSource is the blocked-services and safe-search catalogue in
// service (catalogue.Holder); Status, when set, reports the runtime
// refresh of blocking.catalogue.url.
type CatalogueSource struct {
	Holder *catalogue.Holder
	Status func() catalogue.Status
}

// CatalogueInfo is GET /catalogue.
type CatalogueInfo struct {
	SchemaVersion int    `json:"schemaVersion" doc:"Catalogue document schema version."`
	Version       string `json:"version" doc:"Catalogue version (YYYY.MM.DD)."`
	License       string `json:"license" doc:"SPDX licence of the catalogue data."`
	Homepage      string `json:"homepage,omitempty"`
	// Origin is "embedded" or the URL the document was downloaded from.
	Origin string `json:"origin" doc:"\"embedded\" (built into the binary) or the blocking.catalogue.url it was downloaded from."`
	Digest string `json:"digest" doc:"SHA-256 of the catalogue document."`
	// Services and SafeSearchEngines count the entries.
	Services          int `json:"services"`
	SafeSearchEngines int `json:"safeSearchEngines"`
	// Refresh is the runtime refresh state; absent when
	// blocking.catalogue.url is empty.
	Refresh *CatalogueRefresh `json:"refresh,omitempty"`
	Took
}

// CatalogueRefresh is the state of the blocking.catalogue.url refresh.
type CatalogueRefresh struct {
	URL         string     `json:"url"`
	LastAttempt *time.Time `json:"lastAttempt,omitempty"`
	LastSuccess *time.Time `json:"lastSuccess,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
}

// CatalogueService is one blockable service (Policy.spec.blockedServices).
type CatalogueService struct {
	ID       string `json:"id" doc:"The ID Policy.spec.blockedServices names."`
	Name     string `json:"name"`
	Category string `json:"category" doc:"video, social, messaging, gaming, music, ai, dating, ..."`
	// List is the list name the query log records for its blocks.
	List  string `json:"list" doc:"The list name the query log records for this service's blocks (service:<id>)."`
	Rules int    `json:"rules" doc:"Number of rules (||domain^)."`
	// Domains are the rules as list text, for the "what does this block" view.
	Domains []string `json:"domains" nullable:"false" doc:"The service's rules as list text (||domain^)."`
	Source  string   `json:"source" doc:"Where the entry comes from: a vendor documentation URL or \"first-party domains of the service\"."`
}

// SafeSearchEngine is one safe-search rewrite (Policy.spec.safeSearch).
type SafeSearchEngine struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	List           string   `json:"list" doc:"The list name the query log records for its rewrites (safesearch:<id>)."`
	Target         string   `json:"target,omitempty" doc:"CNAME target in strict mode."`
	ModerateTarget string   `json:"moderateTarget,omitempty" doc:"CNAME target in moderate mode (safeSearchMode: moderate); empty when the engine has one level."`
	Addresses      []string `json:"addresses,omitempty" doc:"Addresses answered directly when the vendor documents an address instead of a host name."`
	Domains        int      `json:"domains" doc:"Number of host names rewritten."`
	Source         string   `json:"source" doc:"The vendor documentation the rewrite follows."`
}

type catalogueInfoOutput struct{ Body *CatalogueInfo }

type catalogueServicesInput struct {
	Category string `query:"category" doc:"Only services of this category."`
}

type catalogueServicesOutput struct{ Body *List[CatalogueService] }

type catalogueSafeSearchOutput struct{ Body *List[SafeSearchEngine] }

func (s *Server) catalogue() *catalogue.Catalogue { return s.opts.Catalogue.Holder.Current() }

// registerCatalogue declares the read-only catalogue routes (D60). They
// need policies.read: the catalogue is what a Policy's blockedServices and
// safeSearch refer to.
func (s *Server) registerCatalogue() {
	p := perm{resource: "policies", verb: "read"}
	register(s, operation(http.MethodGet, "/catalogue", "getCatalogue", "The blocked-services and safe-search catalogue in service", "catalogue"), p,
		func(context.Context, *struct{}) (*catalogueInfoOutput, error) {
			c := s.catalogue()
			info := &CatalogueInfo{
				SchemaVersion: c.SchemaVersion, Version: c.Version, License: c.License, Homepage: c.Homepage,
				Origin: c.Origin(), Digest: c.Digest(), Services: len(c.Services), SafeSearchEngines: len(c.SafeSearch),
			}
			if st := s.opts.Catalogue.Status; st != nil {
				rs := st()
				r := &CatalogueRefresh{URL: rs.URL, LastError: rs.LastError}
				if !rs.LastAttempt.IsZero() {
					r.LastAttempt = &rs.LastAttempt
				}
				if !rs.LastSuccess.IsZero() {
					r.LastSuccess = &rs.LastSuccess
				}
				info.Refresh = r
			}
			return &catalogueInfoOutput{Body: info}, nil
		})
	register(s, operation(http.MethodGet, "/catalogue/services", "listCatalogueServices", "Services Policy.spec.blockedServices can block", "catalogue"), p,
		func(_ context.Context, in *catalogueServicesInput) (*catalogueServicesOutput, error) {
			var items []CatalogueService
			for _, svc := range s.catalogue().Services {
				if in.Category != "" && svc.Category != in.Category {
					continue
				}
				items = append(items, CatalogueService{
					ID: svc.ID, Name: svc.Name, Category: svc.Category, List: catalogue.ListName(svc.ID),
					Rules: len(svc.Rules), Domains: append([]string{}, svc.Rules...), Source: svc.Source,
				})
			}
			return &catalogueServicesOutput{Body: newList(items, "")}, nil
		})
	register(s, operation(http.MethodGet, "/catalogue/safesearch", "listCatalogueSafeSearch", "Search engines Policy.spec.safeSearch rewrites", "catalogue"), p,
		func(context.Context, *struct{}) (*catalogueSafeSearchOutput, error) {
			var items []SafeSearchEngine
			for _, e := range s.catalogue().SafeSearch {
				items = append(items, SafeSearchEngine{
					ID: e.ID, Name: e.Name, List: catalogue.SafeSearchPrefix + e.ID, Target: e.Target, ModerateTarget: e.ModerateTarget,
					Addresses: e.Addresses, Domains: len(e.Domains), Source: e.Source,
				})
			}
			return &catalogueSafeSearchOutput{Body: newList(items, "")}, nil
		})
}
