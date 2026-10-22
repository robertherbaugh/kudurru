package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
)

var capturedCatalogPaths = []string{"catalogue", "catalogue/services", "catalogue/categories", "catalogue/safesearch"}

func TestCapturedCatalogReadsNoLiveFallbackOrRefresh(t *testing.T) {
	for _, imported := range []bool{false, true} {
		t.Run(map[bool]string{false: "embedded", true: "imported"}[imported], func(t *testing.T) {
			holder := catalogue.NewHolder(nil)
			if imported {
				imp, err := catalogue.ParseAdGuard([]byte(`{"blocked_services":[
					{"id":"extra_stream","name":"Extra Stream","group":"streaming","rules":["||extra.example^","@@||allowed.example^"]},
					{"id":"youtube","name":"Shadowed","group":"streaming","rules":["||shadowed.example^"]}]}`), "https://actual.example/import")
				if err != nil {
					t.Fatal(err)
				}
				holder.SetImport(imp)
			}
			c := holder.Current()
			calls := 0
			h := newHarness(t, func(o *Options) {
				o.Catalogue = CatalogueSource{Holder: catalogue.NewHolder(nil), Read: func(context.Context) (*catalogue.Catalogue, error) {
					calls++
					return c, nil
				}, Status: func() catalogue.Status { panic("live catalog status consulted") }, ImportStatus: func() catalogue.Status { panic("live import status consulted") }}
			})
			var info CatalogueInfo
			h.do(http.MethodGet, "/api/v1/catalogue", "viewer", nil).want(http.StatusOK).json(&info)
			wantSources := 1
			if imported {
				wantSources++
			}
			if info.Origin != catalogue.OriginEmbedded || info.Digest != c.Digest() || info.Refresh != nil || len(info.Sources) != wantSources || info.Services != len(c.Services) {
				t.Fatalf("captured provenance mixed live source: %+v", info)
			}
			for _, source := range info.Sources {
				if !source.Loaded || source.Refresh != nil {
					t.Fatalf("captured source mixed live attempts: %+v", source)
				}
			}
			if imported {
				imp, shadowed := c.Imported()
				source := info.Sources[1]
				if source.Origin != imp.Origin() || source.Digest != imp.Digest() || source.Shadowed != shadowed || shadowed != 1 || source.Services != 1 || source.DroppedRules != imp.DroppedRules || source.DroppedRules == 0 {
					t.Fatalf("actual import receipt lost: %+v", source)
				}
			}
			for _, path := range capturedCatalogPaths[1:] {
				h.do(http.MethodGet, "/api/v1/"+path, "viewer", nil).want(http.StatusOK)
			}
			if calls != len(capturedCatalogPaths) {
				t.Fatalf("catalog GET skipped original read: %d calls", calls)
			}
			// Static icons require neither a catalog read nor authentication.
			h.do(http.MethodGet, "/api/v1/catalogue/icons", "", nil).want(http.StatusOK)
			if calls != len(capturedCatalogPaths) {
				t.Fatal("static icons captured a catalog")
			}
		})
	}
}

func TestCapturedCatalogReadFailureIsUnavailable(t *testing.T) {
	for _, fail := range []string{"nil", "error"} {
		t.Run(fail, func(t *testing.T) {
			h := newHarness(t, func(o *Options) {
				o.Catalogue = CatalogueSource{Holder: catalogue.NewHolder(nil), Read: func(context.Context) (*catalogue.Catalogue, error) {
					if fail == "error" {
						return catalogue.Embedded(), errors.New("original lease unavailable")
					}
					return nil, nil
				}, Status: func() catalogue.Status { panic("fallback to live status") }}
			})
			for _, path := range capturedCatalogPaths {
				h.do(http.MethodGet, "/api/v1/"+path, "viewer", nil).want(http.StatusServiceUnavailable)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := &Server{opts: Options{Catalogue: CatalogueSource{Read: func(context.Context) (*catalogue.Catalogue, error) {
		cancel()
		return catalogue.Embedded(), nil
	}}}}
	if _, err := s.catalogueFor(ctx); err == nil {
		t.Fatal("read ignored cancellation during callback")
	}
	s.opts.Catalogue.Read = func(context.Context) (*catalogue.Catalogue, error) {
		t.Fatal("canceled read called callback")
		return nil, nil
	}
	if _, err := s.catalogueFor(ctx); err == nil {
		t.Fatal("read accepted canceled request")
	}
	if _, err := s.catalogueFor(nil); err == nil { //nolint:staticcheck // Deliberately exercise a missing request context.
		t.Fatal("read accepted missing context")
	}
}

func TestCapturedCatalogProjectionDetachesNestedArrays(t *testing.T) {
	svc := catalogue.Service{ID: "test", Rules: []string{"||test.example^"}, Requires: []string{"dependency"}, Related: []string{"related"}}
	out := catalogueService(&svc, "Apache-2.0")
	out.Domains[0], out.Requires[0], out.Related[0] = "changed", "changed", "changed"
	if !slices.Equal(svc.Rules, []string{"||test.example^"}) || !slices.Equal(svc.Requires, []string{"dependency"}) || !slices.Equal(svc.Related, []string{"related"}) {
		t.Fatal("service response mutates captured arrays", svc)
	}
	e := catalogue.Engine{ID: "engine", Addresses: []string{"192.0.2.1"}, Domains: []string{"search.example"}}
	engine := catalogueSafeSearch(e)
	engine.Addresses[0] = "changed"
	if e.Addresses[0] != "192.0.2.1" || engine.Domains != 1 {
		t.Fatal("engine response mutates captured addresses", e)
	}
}
