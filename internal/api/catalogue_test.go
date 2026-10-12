package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
)

func TestCatalogueRoutes(t *testing.T) {
	when := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, func(o *Options) {
		o.Catalogue = CatalogueSource{Holder: catalogue.NewHolder(nil), Status: func() catalogue.Status {
			return catalogue.Status{URL: "https://data.example/catalogue.json", LastAttempt: when, LastError: "GET: 503"}
		}}
	})
	var info CatalogueInfo
	h.do(http.MethodGet, "/api/v1/catalogue", "viewer", nil).want(http.StatusOK).json(&info)
	emb := catalogue.Embedded()
	if info.SchemaVersion != catalogue.SchemaVersion || info.Version != emb.Version || info.Origin != "embedded" || info.Digest != emb.Digest() ||
		info.Services != len(emb.Services) || info.SafeSearchEngines != len(emb.SafeSearch) {
		t.Errorf("info = %+v", info)
	}
	if r := info.Refresh; r == nil || r.URL != "https://data.example/catalogue.json" || r.LastAttempt == nil || r.LastSuccess != nil || r.LastError == "" {
		t.Errorf("refresh = %+v", r)
	}

	var svcs List[CatalogueService]
	h.do(http.MethodGet, "/api/v1/catalogue/services", "viewer", nil).want(http.StatusOK).json(&svcs)
	if len(svcs.Items) != len(emb.Services) {
		t.Fatalf("%d services", len(svcs.Items))
	}
	var yt *CatalogueService
	for i := range svcs.Items {
		if svcs.Items[i].ID == "youtube" {
			yt = &svcs.Items[i]
		}
	}
	if yt == nil || yt.Name != "YouTube" || yt.Category != "video" || yt.List != "service:youtube" || yt.Rules != len(yt.Domains) || yt.Domains[0] != "||youtube.com^" || yt.Source == "" {
		t.Errorf("youtube = %+v", yt)
	}
	h.do(http.MethodGet, "/api/v1/catalogue/services?category=gaming", "viewer", nil).want(http.StatusOK).json(&svcs)
	for _, s := range svcs.Items {
		if s.Category != "gaming" {
			t.Errorf("category filter returned %+v", s)
		}
	}
	if len(svcs.Items) == 0 {
		t.Error("no gaming services")
	}

	var engines List[SafeSearchEngine]
	h.do(http.MethodGet, "/api/v1/catalogue/safesearch", "viewer", nil).want(http.StatusOK).json(&engines)
	found := map[string]SafeSearchEngine{}
	for _, e := range engines.Items {
		found[e.ID] = e
	}
	if g := found["google"]; g.Target != "forcesafesearch.google.com" || g.Domains < 100 || g.List != "safesearch:google" {
		t.Errorf("google = %+v", g)
	}
	if y := found["youtube"]; y.ModerateTarget != "restrictmoderate.youtube.com" {
		t.Errorf("youtube = %+v", y)
	}
	if y := found["yandex"]; len(y.Addresses) != 1 || y.Target != "" {
		t.Errorf("yandex = %+v", y)
	}

	if len(info.Sources) != 1 || info.Sources[0].ID != "kudurru" || info.Sources[0].License != "Apache-2.0" || info.Sources[0].Services != len(emb.Services) || info.Categories != len(emb.Categories) {
		t.Errorf("sources = %+v", info.Sources)
	}
	var cats List[CatalogueCategory]
	h.do(http.MethodGet, "/api/v1/catalogue/categories", "viewer", nil).want(http.StatusOK).json(&cats)
	if len(cats.Items) != len(emb.Categories) || cats.Items[0].ID != emb.Categories[0].ID || cats.Items[0].Icon == "" {
		t.Fatalf("categories = %+v", cats.Items)
	}
	for _, k := range cats.Items {
		if ids, _ := emb.CategoryServices(k.ID); k.Services != len(ids) {
			t.Errorf("category %s: %d services, want %d", k.ID, k.Services, len(ids))
		}
	}
	if yt.Origin != "kudurru" || yt.License != "Apache-2.0" {
		t.Errorf("youtube origin %q licence %q", yt.Origin, yt.License)
	}

	// policies.read: anonymous callers are refused.
	h.do(http.MethodGet, "/api/v1/catalogue/services", "", nil).want(http.StatusUnauthorized)

	// Without a holder the embedded catalogue serves and there is no
	// refresh state.
	h = newHarness(t)
	info = CatalogueInfo{}
	h.do(http.MethodGet, "/api/v1/catalogue", "viewer", nil).want(http.StatusOK).json(&info)
	if info.Origin != "embedded" || info.Refresh != nil {
		t.Errorf("default info = %+v", info)
	}
}

// TestCatalogueImportSources: with the AdGuard import the info reports
// both sources and their licences, and imported services are marked.
func TestCatalogueImportSources(t *testing.T) {
	imp, err := catalogue.ParseAdGuard([]byte(`{"blocked_services":[
		{"id":"example_stream","name":"Example Stream","group":"streaming","rules":["||stream.example^","@@||x.example^"]},
		{"id":"youtube","name":"Shadowed","group":"streaming","rules":["||yt.example^"]}]}`), "https://mirror.example/services.json")
	if err != nil {
		t.Fatal(err)
	}
	hold := catalogue.NewHolder(nil)
	hold.SetImport(imp)
	h := newHarness(t, func(o *Options) {
		o.Catalogue = CatalogueSource{Holder: hold, ImportStatus: func() catalogue.Status {
			return catalogue.Status{Kind: catalogue.KindAdGuard, URL: "https://mirror.example/services.json"}
		}}
	})
	var info CatalogueInfo
	h.do(http.MethodGet, "/api/v1/catalogue", "viewer", nil).want(http.StatusOK).json(&info)
	if len(info.Sources) != 2 {
		t.Fatalf("sources = %+v", info.Sources)
	}
	ag := info.Sources[1]
	if ag.ID != "adguard" || ag.License != "GPL-3.0" || !ag.Loaded || ag.Services != 1 || ag.Shadowed != 1 || ag.DroppedRules != 1 ||
		ag.Origin != "https://mirror.example/services.json" || ag.Refresh == nil {
		t.Errorf("adguard source = %+v", ag)
	}
	var svcs List[CatalogueService]
	h.do(http.MethodGet, "/api/v1/catalogue/services?category=video", "viewer", nil).want(http.StatusOK).json(&svcs)
	var found bool
	for _, s := range svcs.Items {
		if s.ID == "example-stream" {
			found = s.Origin == "adguard" && s.License == "GPL-3.0" && s.Source == "adguard-hostlists-registry" && s.Icon == ""
		}
		if s.ID == "youtube" && s.Origin != "kudurru" {
			t.Errorf("youtube = %+v", s)
		}
	}
	if !found {
		t.Errorf("imported service missing or unmarked: %+v", svcs.Items)
	}
	// Configured but not downloaded yet: the source is listed, not loaded.
	h = newHarness(t, func(o *Options) {
		o.Catalogue = CatalogueSource{Holder: catalogue.NewHolder(nil), ImportStatus: func() catalogue.Status {
			return catalogue.Status{Kind: catalogue.KindAdGuard, URL: "https://mirror.example/services.json", LastError: "GET: 503"}
		}}
	})
	info = CatalogueInfo{}
	h.do(http.MethodGet, "/api/v1/catalogue", "viewer", nil).want(http.StatusOK).json(&info)
	if len(info.Sources) != 2 || info.Sources[1].Loaded || info.Sources[1].Refresh.LastError == "" {
		t.Errorf("pending import = %+v", info.Sources)
	}
}

func TestCatalogueIcons(t *testing.T) {
	h := newHarness(t)
	var svcs List[CatalogueService]
	h.do(http.MethodGet, "/api/v1/catalogue/services", "viewer", nil).want(http.StatusOK).json(&svcs)
	icons := map[string]IconRef{}
	for _, s := range svcs.Items {
		icons[s.ID] = s.IconRef
	}
	if icons["youtube"].Icon != "/api/v1/catalogue/icons/youtube.svg" || icons["youtube"].IconOnDark != "" {
		t.Errorf("youtube icon = %+v", icons["youtube"])
	}
	if icons["twitter"] != (IconRef{Icon: "/api/v1/catalogue/icons/x.svg", IconOnDark: "/api/v1/catalogue/icons/x-light.svg"}) {
		t.Errorf("twitter icon = %+v", icons["twitter"])
	}
	if icons["roblox"] != (IconRef{}) {
		t.Errorf("roblox has no logo, got %+v", icons["roblox"])
	}
	var engines List[SafeSearchEngine]
	h.do(http.MethodGet, "/api/v1/catalogue/safesearch", "viewer", nil).want(http.StatusOK).json(&engines)
	for _, e := range engines.Items {
		if (e.ID == "pixabay") != (e.Icon == "") {
			t.Errorf("engine %s icon %q", e.ID, e.Icon)
		}
	}

	// The index and the files are public.
	var idx List[CatalogueIcon]
	h.do(http.MethodGet, "/api/v1/catalogue/icons", "", nil).want(http.StatusOK).json(&idx)
	byID := map[string]CatalogueIcon{}
	for _, it := range idx.Items {
		byID[it.ID] = it
	}
	if len(idx.Items) != len(catalogue.IconSlugs()) || byID["cloudflare"].Path != "/api/v1/catalogue/icons/cloudflare.svg" ||
		byID["tiktok"].OnDarkPath != "/api/v1/catalogue/icons/tiktok-light.svg" {
		t.Errorf("index = %+v", idx.Items)
	}
	r := h.do(http.MethodGet, "/api/v1/catalogue/icons/cloudflare.svg", "", nil).want(http.StatusOK)
	ic, _ := catalogue.Icon("cloudflare")
	hdr := r.Header()
	if hdr.Get("Content-Type") != "image/svg+xml" || hdr.Get("ETag") != ic.ETag || hdr.Get("Cache-Control") != iconCacheControl ||
		hdr.Get("Content-Security-Policy") != iconCSP || hdr.Get("X-Content-Type-Options") != "nosniff" || r.Body.String() != string(ic.SVG) {
		t.Errorf("icon response headers %v, %d bytes", hdr, r.Body.Len())
	}
	r = h.do(http.MethodGet, "/api/v1/catalogue/icons/cloudflare.svg", "", nil, "If-None-Match", `W/"x", `+ic.ETag).want(http.StatusNotModified)
	if r.Body.Len() != 0 || r.Header().Get("ETag") != ic.ETag {
		t.Errorf("304 with %d bytes, ETag %q", r.Body.Len(), r.Header().Get("ETag"))
	}
	h.do(http.MethodGet, "/api/v1/catalogue/icons/roblox.svg", "", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/catalogue/icons/cloudflare", "", nil).want(http.StatusNotFound)
	h.do(http.MethodGet, "/api/v1/catalogue/icons/..%2fcatalogue.json", "", nil).want(http.StatusNotFound)
}
