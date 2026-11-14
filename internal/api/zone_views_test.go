package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
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

// TestZoneRecordDeleteViewInOneNamespace: DELETE ?view=&namespace= removes
// the view of record sets in that namespace only (a view names the
// ClientGroup of its record set's namespace, D403), on the direct store path
// and on the SQLite command path. A served view row has no record set, so
// this is how the dashboard deletes the kids/lan view and keeps default/lan.
func TestZoneRecordDeleteViewInOneNamespace(t *testing.T) {
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
			views := func(ns string) []string {
				var rs v1alpha1.DNSRecordSet
				if err := objects.Get(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: ns, Name: "api-example"}, &rs); err != nil {
					return nil
				}
				var out []string
				for _, e := range rs.Spec.Endpoints {
					out = append(out, endpointView(e)+"="+strings.Join(e.Targets, ","))
				}
				slices.Sort(out)
				return out
			}
			h.do(http.MethodPost, "/api/v1/zones/example/records?recordSet=default/api-example", "operator", viewEP("www", "A", "lan", "10.0.0.10")).want(http.StatusCreated)
			h.do(http.MethodPost, "/api/v1/zones/example/records?recordSet=kids/api-example", "operator", viewEP("www", "A", "lan", "10.9.0.10")).want(http.StatusCreated)
			h.do(http.MethodDelete, "/api/v1/zones/example/records/www/A?view=lan&namespace=kids", "operator", nil).want(http.StatusNoContent)
			if got := views("kids"); len(got) != 0 {
				t.Fatalf("kids/lan survived: %v", got)
			}
			if got := views("default"); !slices.Equal(got, []string{"lan=10.0.0.10"}) {
				t.Fatalf("the delete of kids/lan touched default/lan: %v", got)
			}
			h.do(http.MethodDelete, "/api/v1/zones/example/records/www/A?view=lan&namespace=kids", "operator", nil).want(http.StatusNotFound)
		})
	}
}

// TestCapturedZoneRecordsListServedViews: on the complete SQLite runtime the
// record list is the served model, which also lists every served
// client-group view with its group's name and namespace (D403), never the
// publish-only public endpoint, which is not served. Views of one name in two
// namespaces are two rows.
func TestCapturedZoneRecordsListServedViews(t *testing.T) {
	h, f := newZoneReadFixture(t)
	actual := zoneObj("example", "example.com.")
	rs := recordSetObj("default", "actual", "example",
		viewEP("www.example.com", "A", "", "203.0.113.10"),
		viewEP("www.example.com", "A", "lan", "10.0.0.10"),
		viewEP("nas.example.com", "A", "lan", "10.0.0.20"),
		viewEP("www.example.com", "A", "public", "198.51.100.10"))
	kids := recordSetObj("kids", "actual", "example", viewEP("nas.example.com", "A", "lan", "10.9.0.20"))
	model, errs := zone.Build(actual, []*v1alpha1.DNSRecordSet{rs, kids}, zone.BuildOptions{})
	if model == nil || !model.HasViews() || !errs.Empty() {
		t.Fatalf("fixture model with a view did not build: %v", errs)
	}
	f.model = model
	var records List[ZoneRecord]
	h.do(http.MethodGet, "/api/v1/zones/example/records", "viewer", nil).want(http.StatusOK).json(&records)
	var got []string
	for _, row := range records.Items {
		if row.RecordType != "A" {
			continue
		}
		v := ""
		if row.Kudurru != nil {
			v = row.Namespace + "/" + row.Kudurru.View
		} else if row.Namespace != "" {
			t.Fatalf("default row %+v names a namespace", row)
		}
		got = append(got, v+" "+row.DNSName+"="+strings.Join(row.Targets, ","))
	}
	slices.Sort(got)
	if want := []string{" www.example.com=203.0.113.10", "default/lan nas.example.com=10.0.0.20", "default/lan www.example.com=10.0.0.10", "kids/lan nas.example.com=10.9.0.20"}; !slices.Equal(got, want) {
		t.Fatalf("served rows %q, want %q", got, want)
	}
}

// TestCapturedZoneRecordsNameTheProvenRecordSet: a served row names the
// desired record set (and its manager, set identifier, comment and labels)
// when exactly one live endpoint of the request's captured objects has its
// name, type and view, a view only from its group's namespace. Otherwise
// (system data, an endpoint the objects do not hold, two candidates) the row
// names none. The list says which list it is: served here, desired on the
// store path.
func TestCapturedZoneRecordsNameTheProvenRecordSet(t *testing.T) {
	h, f := newZoneReadFixture(t)
	z := zoneObj("example", "example.com.")
	commented := viewEP("www.example.com", "A", "", "203.0.113.10")
	commented.Kudurru = &v1alpha1.EndpointExt{Comment: "front door"}
	commented.Labels = map[string]string{"team": "web"}
	def := recordSetObj("default", "api-example", "example", commented,
		viewEP("www.example.com", "A", "lan", "10.0.0.10"),
		viewEP("mail.example.com", "A", "", "203.0.113.25"))
	kids := recordSetObj("kids", "kids-views", "example", viewEP("nas.example.com", "A", "lan", "10.9.0.20"))
	store.SetManagedBy(kids, store.ManagerFile("objects.d/kids.yaml"))
	// A second desired candidate for mail: the build serves one of them,
	// and the row cannot say which.
	other := recordSetObj("default", "other", "example", viewEP("mail.example.com", "A", "", "203.0.113.25"))
	model, _ := zone.Build(z, []*v1alpha1.DNSRecordSet{def, kids, recordSetObj("default", "dynamic", "example", ep("dyn.example.com", "A", "192.0.2.7"))}, zone.BuildOptions{})
	if model == nil || !model.HasViews() {
		t.Fatal("fixture model with views did not build")
	}
	snapshot, err := zonecache.Prepare(zonecache.PrepareOptions{}, []store.Object{z, def, kids, other}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.objects, f.model = snapshot.Objects, model

	var records ZoneRecordList
	h.do(http.MethodGet, "/api/v1/zones/example/records", "viewer", nil).want(http.StatusOK).json(&records)
	if records.Source != RecordSourceServed {
		t.Fatalf("source %q, want served", records.Source)
	}
	got := map[string]ZoneRecord{}
	for _, row := range records.Items {
		key := row.DNSName + " " + row.RecordType
		if row.Kudurru != nil && row.Kudurru.View != "" {
			key += " " + row.Namespace + "/" + row.Kudurru.View
		}
		got[key] = row
	}
	www := got["www.example.com A"]
	if www.RecordSet != "default/api-example" || www.ManagedBy != "" || www.Kudurru == nil || www.Kudurru.Comment != "front door" || www.Labels["team"] != "web" {
		t.Fatalf("default row did not name its proven record set: %+v", www)
	}
	if lan := got["www.example.com A default/lan"]; lan.RecordSet != "default/api-example" || lan.Kudurru.View != "lan" {
		t.Fatalf("default/lan row: %+v", lan)
	}
	if lan := got["nas.example.com A kids/lan"]; lan.RecordSet != "kids/kids-views" || lan.Namespace != "kids" || lan.ManagedBy != "file:objects.d/kids.yaml" || lan.Kudurru.View != "lan" {
		t.Fatalf("kids/lan row did not name the kids record set: %+v", lan)
	}
	for _, key := range []string{"mail.example.com A", "dyn.example.com A", "example.com SOA"} {
		row, ok := got[key]
		if !ok {
			t.Fatalf("row %s missing: %v", key, records.Items)
		}
		if row.RecordSet != "" || row.ManagedBy != "" {
			t.Fatalf("row %s names an unproven record set: %+v", key, row)
		}
	}

	var desired ZoneRecordList
	hd := newHarness(t)
	hd.seed(store.ManagerAPI, zoneObj("example", "example.com."))
	hd.seed(store.ManagerAPI, recordSetObj("default", "api-example", "example", ep("www.example.com", "A", "192.0.2.1")))
	hd.do(http.MethodGet, "/api/v1/zones/example/records", "viewer", nil).want(http.StatusOK).json(&desired)
	if desired.Source != RecordSourceDesired || len(desired.Items) != 1 || desired.Items[0].RecordSet != "default/api-example" {
		t.Fatalf("desired list: %+v", desired)
	}
}
