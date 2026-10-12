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
