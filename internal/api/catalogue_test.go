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
	if info.SchemaVersion != 1 || info.Version != emb.Version || info.Origin != "embedded" || info.Digest != emb.Digest() ||
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
