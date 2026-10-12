package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
)

// CatalogueSource is the blocked-services and safe-search catalogue in
// service (catalogue.Holder); Status, when set, reports the runtime
// refresh of blocking.catalogue.url and ImportStatus the opt-in AdGuard
// import of blocking.catalogue.adguardServicesURL.
type CatalogueSource struct {
	Holder       *catalogue.Holder
	Status       func() catalogue.Status
	ImportStatus func() catalogue.Status
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
	// Services, Categories and SafeSearchEngines count the entries in
	// service (imported services included).
	Services          int `json:"services"`
	Categories        int `json:"categories"`
	SafeSearchEngines int `json:"safeSearchEngines"`
	// Refresh is the runtime refresh state; absent when
	// blocking.catalogue.url is empty.
	Refresh *CatalogueRefresh `json:"refresh,omitempty"`
	// Sources are the active sources of services and their licences: the
	// kudurru catalogue always, the AdGuard import when it is configured.
	Sources []CatalogueSourceInfo `json:"sources" nullable:"false"`
	Took
}

// CatalogueSourceInfo is one active source of services.
type CatalogueSourceInfo struct {
	ID      string `json:"id" enum:"kudurru,adguard" doc:"kudurru: the catalogue document (embedded or blocking.catalogue.url). adguard: the opt-in import of blocking.catalogue.adguardServicesURL."`
	Name    string `json:"name"`
	License string `json:"license" doc:"SPDX licence of the source's data (Apache-2.0 for kudurru, GPL-3.0 for AdGuard's)."`
	// Origin is "embedded" or the URL the data came from; for an import
	// that has not been downloaded yet, the configured URL.
	Origin   string `json:"origin"`
	Digest   string `json:"digest,omitempty" doc:"SHA-256 of the document in service; absent before the first successful download."`
	Loaded   bool   `json:"loaded" doc:"A document of this source is in service."`
	Services int    `json:"services" doc:"Services this source contributes to the catalogue in service."`
	// Shadowed, Skipped and DroppedRules describe an import.
	Shadowed     int               `json:"shadowed,omitempty" doc:"Imported services left out because the kudurru catalogue has the same ID (kudurru's entry wins)."`
	Skipped      int               `json:"skipped,omitempty" doc:"Imported entries left out: an ID kudurru cannot use, a duplicate, no name, or no usable rule."`
	DroppedRules int               `json:"droppedRules,omitempty" doc:"Imported rules left out because they are not plain block rules."`
	Refresh      *CatalogueRefresh `json:"refresh,omitempty"`
}

// CatalogueCategory is one item of GET /catalogue/categories.
type CatalogueCategory struct {
	ID          string `json:"id" doc:"The ID Policy.spec.blockedCategories names."`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty" doc:"A Lucide icon name (kebab-case)."`
	Order       int    `json:"order" doc:"Sort order (ascending)."`
	Services    int    `json:"services" doc:"Services in the category."`
}

type catalogueCategoriesOutput struct{ Body *List[CatalogueCategory] }

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
	Category string `json:"category" doc:"The category ID (GET /catalogue/categories): ai, social, video, ..."`
	// List is the list name the query log records for its blocks.
	List  string `json:"list" doc:"The list name the query log records for this service's blocks (service:<id>)."`
	Rules int    `json:"rules" doc:"Number of rules (||domain^)."`
	// Domains are the rules as list text, for the "what does this block" view.
	Domains []string `json:"domains" nullable:"false" doc:"The service's rules as list text (||domain^)."`
	Source  string   `json:"source" doc:"Where the entry comes from: a vendor documentation URL or \"first-party domains of the service\"; adguard-hostlists-registry for imported services."`
	// Origin and License say which source the entry is from.
	Origin  string `json:"origin" enum:"kudurru,adguard" doc:"kudurru, or adguard for a service of the opt-in AdGuard import."`
	License string `json:"license" doc:"SPDX licence of the entry (Apache-2.0, or GPL-3.0 for imported services)."`
	// Requires, Related and Note are hints for the picker.
	Requires []string `json:"requires,omitempty" doc:"Services this one also depends on: block them too to stop it (Fortnite requires epicgames)."`
	Related  []string `json:"related,omitempty" doc:"Services of the same vendor or closely tied to it."`
	Note     string   `json:"note,omitempty" doc:"A caveat about blocking the service."`
	IconRef
}

// IconRef locates an entry's logo; both fields are absent when the binary
// has none (the dashboard draws a monogram).
type IconRef struct {
	Icon string `json:"icon,omitempty" doc:"Logo path, /api/v1/catalogue/icons/{slug}.svg; absent when there is none."`
	// IconOnDark is the logo's variant for dark backgrounds, when one exists.
	IconOnDark string `json:"iconOnDark,omitempty" doc:"The logo drawn for dark backgrounds, when the default one is dark artwork."`
}

// iconRef builds the IconRef of an icon slug ("" for none).
func iconRef(slug string) IconRef {
	if slug == "" {
		return IconRef{}
	}
	r := IconRef{Icon: iconPath(slug)}
	if v, ok := catalogue.IconOnDark(slug); ok {
		r.IconOnDark = iconPath(v)
	}
	return r
}

func iconPath(slug string) string { return BasePath + "/catalogue/icons/" + slug + ".svg" }

// CatalogueIcon is one item of GET /catalogue/icons.
type CatalogueIcon struct {
	ID         string `json:"id" doc:"The icon slug (the homarr-labs/dashboard-icons file name)."`
	Path       string `json:"path" doc:"/api/v1/catalogue/icons/{id}.svg"`
	OnDarkPath string `json:"onDark,omitempty" doc:"Path of the variant for dark backgrounds, when one exists."`
}

type catalogueIconsOutput struct{ Body *List[CatalogueIcon] }

type iconInput struct {
	File        string `path:"file" doc:"{slug}.svg"`
	IfNoneMatch string `header:"If-None-Match"`
}

type iconOutput struct {
	Status        int
	ContentType   string `header:"Content-Type"`
	CacheControl  string `header:"Cache-Control"`
	ETag          string `header:"ETag"`
	CSP           string `header:"Content-Security-Policy"`
	ContentLength string `header:"Content-Length"`
	Body          []byte
}

// iconCSP applies when an icon is opened directly: no script, no external
// fetch, inline style sheets only.
const iconCSP = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

// iconCacheControl: the bytes of a slug change only with the binary, and
// the ETag revalidates after a day.
const iconCacheControl = "public, max-age=86400"

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
	IconRef
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
				Origin: c.Origin(), Digest: c.Digest(), Services: len(c.Services), Categories: len(c.Categories), SafeSearchEngines: len(c.SafeSearch),
			}
			if st := s.opts.Catalogue.Status; st != nil {
				info.Refresh = catalogueRefresh(st())
			}
			info.Sources = s.catalogueSources(c)
			return &catalogueInfoOutput{Body: info}, nil
		})
	register(s, operation(http.MethodGet, "/catalogue/services", "listCatalogueServices", "Services Policy.spec.blockedServices can block", "catalogue"), p,
		func(_ context.Context, in *catalogueServicesInput) (*catalogueServicesOutput, error) {
			var items []CatalogueService
			c := s.catalogue()
			for _, svc := range c.Services {
				if in.Category != "" && svc.Category != in.Category {
					continue
				}
				items = append(items, catalogueService(&svc, c.License))
			}
			return &catalogueServicesOutput{Body: newList(items, "")}, nil
		})
	register(s, operation(http.MethodGet, "/catalogue/categories", "listCatalogueCategories", "Categories Policy.spec.blockedCategories can block", "catalogue"), p,
		func(context.Context, *struct{}) (*catalogueCategoriesOutput, error) {
			c := s.catalogue()
			items := make([]CatalogueCategory, 0, len(c.Categories))
			for _, k := range c.Categories {
				ids, _ := c.CategoryServices(k.ID)
				items = append(items, CatalogueCategory{ID: k.ID, Name: k.Name, Description: k.Description, Icon: k.Icon, Order: k.Order, Services: len(ids)})
			}
			return &catalogueCategoriesOutput{Body: newList(items, "")}, nil
		})
	register(s, operation(http.MethodGet, "/catalogue/safesearch", "listCatalogueSafeSearch", "Search engines Policy.spec.safeSearch rewrites", "catalogue"), p,
		func(context.Context, *struct{}) (*catalogueSafeSearchOutput, error) {
			var items []SafeSearchEngine
			for _, e := range s.catalogue().SafeSearch {
				items = append(items, SafeSearchEngine{
					ID: e.ID, Name: e.Name, List: catalogue.SafeSearchPrefix + e.ID, Target: e.Target, ModerateTarget: e.ModerateTarget,
					Addresses: e.Addresses, Domains: len(e.Domains), Source: e.Source, IconRef: iconRef(e.IconSlug()),
				})
			}
			return &catalogueSafeSearchOutput{Body: newList(items, "")}, nil
		})

	// The logos are static assets of the binary, the same for everyone, so
	// they are public like the dashboard bundle (an <img> on the login page
	// or a cached copy must not need a session).
	register(s, operation(http.MethodGet, "/catalogue/icons", "listCatalogueIcons", "Logos embedded in this build", "catalogue"), perm{public: true},
		func(context.Context, *struct{}) (*catalogueIconsOutput, error) {
			slugs := catalogue.IconSlugs()
			items := make([]CatalogueIcon, 0, len(slugs))
			for _, slug := range slugs {
				it := CatalogueIcon{ID: slug, Path: iconPath(slug)}
				if v, ok := catalogue.IconOnDark(slug); ok {
					it.OnDarkPath = iconPath(v)
				}
				items = append(items, it)
			}
			return &catalogueIconsOutput{Body: newList(items, "")}, nil
		})
	iconOp := operation(http.MethodGet, "/catalogue/icons/{file}", "getCatalogueIcon", "One embedded logo (image/svg+xml)", "catalogue")
	iconOp.Description = "{file} is {slug}.svg. The SVGs come from homarr-labs/dashboard-icons (Apache-2.0) and carry no script, event handler or external reference. " +
		"Strong ETag; If-None-Match answers 304. Show them with <img>; the logos are trademarks of their owners and identify the service only."
	iconOp.Responses = map[string]*huma.Response{
		"200": {Description: "The SVG document", Content: map[string]*huma.MediaType{"image/svg+xml": {Schema: &huma.Schema{Type: "string"}}}},
		"304": {Description: "Not modified"},
	}
	register(s, iconOp, perm{public: true}, func(_ context.Context, in *iconInput) (*iconOutput, error) {
		slug, ok := strings.CutSuffix(in.File, ".svg")
		ic, found := catalogue.Icon(slug)
		if !ok || !found {
			return nil, errNotFound("icon " + in.File)
		}
		out := &iconOutput{Status: http.StatusOK, ContentType: "image/svg+xml", CacheControl: iconCacheControl, ETag: ic.ETag, CSP: iconCSP}
		if etagMatches(in.IfNoneMatch, ic.ETag) {
			out.Status = http.StatusNotModified
			return out, nil
		}
		out.ContentLength, out.Body = strconv.Itoa(len(ic.SVG)), ic.SVG
		return out, nil
	})
}

// catalogueService is the API form of svc; license is the catalogue
// document's licence.
func catalogueService(svc *catalogue.Service, license string) CatalogueService {
	out := CatalogueService{
		ID: svc.ID, Name: svc.Name, Category: svc.Category, List: catalogue.ListName(svc.ID),
		Rules: len(svc.Rules), Domains: append([]string{}, svc.Rules...), Source: svc.Source,
		Origin: svc.Origin(), License: license, Requires: svc.Requires, Related: svc.Related, Note: svc.Note,
		IconRef: iconRef(svc.IconSlug()),
	}
	if out.Origin == catalogue.OriginAdGuard {
		out.License = catalogue.AdGuardLicense
	}
	return out
}

func catalogueRefresh(rs catalogue.Status) *CatalogueRefresh {
	r := &CatalogueRefresh{URL: rs.URL, LastError: rs.LastError}
	if !rs.LastAttempt.IsZero() {
		r.LastAttempt = &rs.LastAttempt
	}
	if !rs.LastSuccess.IsZero() {
		r.LastSuccess = &rs.LastSuccess
	}
	return r
}

// catalogueSources describes the sources of c's services.
func (s *Server) catalogueSources(c *catalogue.Catalogue) []CatalogueSourceInfo {
	imp, shadowed := c.Imported()
	own := CatalogueSourceInfo{ID: catalogue.OriginKudurru, Name: "kudurru catalogue", License: c.License, Origin: c.Origin(), Digest: c.Digest(), Loaded: true}
	if st := s.opts.Catalogue.Status; st != nil {
		own.Refresh = catalogueRefresh(st())
	}
	out := []CatalogueSourceInfo{own}
	if imp == nil && s.opts.Catalogue.ImportStatus == nil {
		return countSources(c, out)
	}
	ag := CatalogueSourceInfo{ID: catalogue.OriginAdGuard, Name: "AdGuard HostlistsRegistry services (opt-in)", License: catalogue.AdGuardLicense, Shadowed: shadowed}
	if st := s.opts.Catalogue.ImportStatus; st != nil {
		rs := st()
		ag.Origin, ag.Refresh = rs.URL, catalogueRefresh(rs)
	}
	if imp != nil {
		ag.Origin, ag.Digest, ag.Loaded, ag.Skipped, ag.DroppedRules = imp.Origin(), imp.Digest(), true, imp.Skipped, imp.DroppedRules
	}
	return countSources(c, append(out, ag))
}

func countSources(c *catalogue.Catalogue, out []CatalogueSourceInfo) []CatalogueSourceInfo {
	n := map[string]int{}
	for i := range c.Services {
		n[c.Services[i].Origin()]++
	}
	for i := range out {
		out[i].Services = n[out[i].ID]
	}
	return out
}

// etagMatches implements the If-None-Match comparison (weak, RFC 9110
// §13.1.2): "*" or any listed tag equal to etag, W/ prefixes ignored.
func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}
