package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func viewEP(name, typ, view string, targets ...string) v1alpha1.Endpoint {
	e := ep(name, typ, targets...)
	if view != "" {
		e.Kudurru = &v1alpha1.EndpointExt{View: view}
	}
	return e
}

// TestZoneRecordWritesKeepViewsApart: the record API treats a client-group
// view (and public) of a name and type as its own endpoint (D403), on the
// direct store path and on the SQLite command path. Writing a view never
// replaces the default data, and DELETE ?view= removes one view only ("-":
// the default data); without view it still removes every view.
func TestZoneRecordWritesKeepViewsApart(t *testing.T) {
	for _, mode := range []string{"store", "command"} {
		t.Run(mode, func(t *testing.T) {
			var h *harness
			var objects store.Store
			if mode == "store" {
				h = newHarness(t)
				h.seed(store.ManagerAPI, zoneObj("example", "example.com."))
				objects = h.srv.opts.Store
			} else {
				var st *sqlite.Store
				h, st = zoneCommandHarness(t, nil)
				zoneSeed(t, st, store.ManagerAPI, zoneObj("example", "example.com."))
				objects = st
			}
			read := func() []v1alpha1.Endpoint {
				var rs v1alpha1.DNSRecordSet
				if err := objects.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "api-example"}, &rs); err != nil {
					return nil
				}
				return rs.Spec.Endpoints
			}
			views := func() []string {
				var out []string
				for _, e := range read() {
					out = append(out, endpointView(e)+"="+strings.Join(e.Targets, ","))
				}
				slices.Sort(out)
				return out
			}
			post := func(e v1alpha1.Endpoint, status int) {
				t.Helper()
				h.do(http.MethodPost, "/api/v1/zones/example/records", "operator", e).want(status)
			}
			post(viewEP("www", "A", "", "203.0.113.10"), http.StatusCreated)
			post(viewEP("www", "A", "lan", "10.0.0.10"), http.StatusCreated)
			post(viewEP("www", "A", "public", "198.51.100.10"), http.StatusCreated)
			if got := views(); !slices.Equal(got, []string{"=203.0.113.10", "lan=10.0.0.10", "public=198.51.100.10"}) {
				t.Fatalf("a view replaced another view's endpoint: %v", got)
			}
			// Replacing the lan view leaves the default and public data alone.
			post(viewEP("www", "A", "lan", "10.0.0.11"), http.StatusOK)
			post(viewEP("www", "A", "", "203.0.113.11"), http.StatusOK)
			if got := views(); !slices.Equal(got, []string{"=203.0.113.11", "lan=10.0.0.11", "public=198.51.100.10"}) {
				t.Fatalf("replace touched the wrong view: %v", got)
			}
			// The record list carries the view of every row (the command
			// harness answers reads from another store).
			if mode == "store" {
				var list struct {
					Items []ZoneRecord `json:"items"`
				}
				h.do(http.MethodGet, "/api/v1/zones/example/records", "viewer", nil).want(http.StatusOK).json(&list)
				listed := map[string]bool{}
				for _, it := range list.Items {
					v := ""
					if it.Kudurru != nil {
						v = it.Kudurru.View
					}
					listed[v] = true
				}
				if len(list.Items) != 3 || !listed[""] || !listed["lan"] || !listed["public"] {
					t.Fatalf("listed %+v", list.Items)
				}
			}
			// DELETE ?view=lan removes only the lan view; ?view=- only the
			// default data; a view that has no endpoint is 404.
			h.do(http.MethodDelete, "/api/v1/zones/example/records/www/A?view=lan", "operator", nil).want(http.StatusNoContent)
			if got := views(); !slices.Equal(got, []string{"=203.0.113.11", "public=198.51.100.10"}) {
				t.Fatalf("view delete: %v", got)
			}
			h.do(http.MethodDelete, "/api/v1/zones/example/records/www/A?view=lan", "operator", nil).want(http.StatusNotFound)
			h.do(http.MethodDelete, "/api/v1/zones/example/records/www/A?view=-", "operator", nil).want(http.StatusNoContent)
			if got := views(); !slices.Equal(got, []string{"public=198.51.100.10"}) {
				t.Fatalf("default delete: %v", got)
			}
			// Without view, every view of the name and type is removed (as before).
			post(viewEP("www", "A", "kids", "10.0.0.12"), http.StatusCreated)
			h.do(http.MethodDelete, "/api/v1/zones/example/records/www/A", "operator", nil).want(http.StatusNoContent)
			if got := read(); len(got) != 0 {
				t.Fatalf("delete without view kept %v", got)
			}
		})
	}
}
