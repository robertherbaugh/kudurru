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
					IconRef: iconRef(svc.IconSlug()),
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
