package api

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func TestCapturedRegexImpactConcreteNameAndRefusals(t *testing.T) {
	for _, action := range []domainlist.Action{domainlist.Allow, domainlist.Deny} {
		t.Run(string(action), func(t *testing.T) {
			bl := convenienceList("ui-" + string(action))
			if action == domainlist.Deny {
				bl.Spec.Mode = "block"
			}
			off := false
			bl.Spec.Sources[0].Entries = []public.ListEntry{{Type: public.EntryRegex, Value: `^ALPHA\.example$`, Enabled: &off}}
			h, original := newConvenienceReadFixture(t, bl)
			id := domainlist.ID("default", public.EntryRegex, bl.Spec.Sources[0].Entries[0].Value)
			path := "/api/v1/" + string(action) + "list/" + url.PathEscape(id) + "/impact"
			var result DomainRegexImpact
			r := h.do(http.MethodGet, path+"?name=Alpha.Example.", "viewer", nil).want(http.StatusOK)
			r.json(&result)
			wantList := "default/captured-block"
			if action == domainlist.Deny {
				wantList = "default/captured-allow"
			}
			if !result.Matches || result.Enabled || !result.Everyone || result.Name != "alpha.example." || len(result.Overlaps) != 1 || result.Overlaps[0].List != wantList || result.Incomplete || r.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("incorrect concrete overlap", result)
			}
			h.do(http.MethodGet, path+"?name=beta.example", "viewer", nil).want(http.StatusOK).json(&result)
			if result.Matches || len(result.Overlaps) != 0 {
				t.Fatal("nonmatching name inherited impact", result)
			}
			for _, name := range []string{"", "*", "*.example", "a b.example", "."} {
				h.do(http.MethodGet, path+"?name="+url.QueryEscape(name), "viewer", nil).want(http.StatusBadRequest)
			}
			h.do(http.MethodGet, path+"?name=alpha.example", "zoneadmin", nil).want(http.StatusForbidden)
			original.err = serving.ErrReleased
			h.do(http.MethodGet, path+"?name=alpha.example", "viewer", nil).want(http.StatusServiceUnavailable)
			original.err = nil
			original.identityErr = serving.ErrComponents
			h.do(http.MethodGet, path+"?name=alpha.example", "viewer", nil).want(http.StatusServiceUnavailable)
		})
	}
}

func TestCapturedRegexImpactReportsBoundedPartialEvidence(t *testing.T) {
	bl := convenienceList("ui-allow")
	bl.Spec.Sources[0].Entries = []public.ListEntry{{Type: public.EntryRegex, Value: `^alpha\.example$`}}
	h, original := newConvenienceReadFixture(t, bl)
	var lists []*blocking.List
	for i := range 300 {
		list, err := blocking.Compile(blocking.Spec{Name: fmt.Sprintf("default/list-%03d", i), Mode: blocking.ModeBlock, Sources: []blocking.Source{blocking.LinesSource("test", parser.FormatDomains, []string{"alpha.example"})}}, nil, blocking.CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		lists = append(lists, list)
	}
	snapshot, err := blocking.NewSnapshot(1, lists...)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := zonecache.Prepare(zonecache.PrepareOptions{Lists: snapshot}, []store.Object{bl}, nil)
	if err != nil {
		t.Fatal(err)
	}
	original.objects = prepared.Objects
	id := domainlist.ID("default", public.EntryRegex, bl.Spec.Sources[0].Entries[0].Value)
	var result DomainRegexImpact
	h.do(http.MethodGet, "/api/v1/allowlist/"+url.PathEscape(id)+"/impact?name=alpha.example", "viewer", nil).want(http.StatusOK).json(&result)
	if !result.Matches || !result.Incomplete || len(result.Overlaps) != 64 || result.Overlaps[0].List != "default/list-000" {
		t.Fatal("overlap bounds or explicit partial state lost", result)
	}
}
