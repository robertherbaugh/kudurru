package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

func TestQueryExportFiltersCursorAndScope(t *testing.T) {
	f := newClusterLogFixture(t)
	for _, query := range []string{"?limit=2", "?status_class=blocked&domain=ads.*", "?scope=node&node=node-b", "?scope=cluster&limit=2"} {
		// Cluster cursors contain the original upper timestamp. Give both
		// independent reads the same bound before comparing their cursors.
		query += "&until=" + url.QueryEscape(time.Now().UTC().Format(time.RFC3339Nano))
		var listed, exported QueriesBody
		f.h.do("GET", BasePath+"/queries"+query, "operator", nil).want(200).json(&listed)
		response := f.h.do("GET", BasePath+"/queries/export"+query, "operator", nil).want(200)
		response.json(&exported)
		if !reflect.DeepEqual(listed.Queries, exported.Queries) || listed.CursorNext != exported.CursorNext || listed.RecordsFiltered != exported.RecordsFiltered || listed.Scope.Scope != exported.Scope.Scope || listed.Partial != exported.Partial {
			t.Fatal("export changed selected data, ordering, cursor or scope", query)
		}
		if response.Header().Get("Content-Type") != "application/json" || response.Header().Get("Content-Disposition") != `attachment; filename="kudurru-queries.json"` || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("export headers missing")
		}
	}
	var first, second QueriesBody
	f.h.do("GET", BasePath+"/queries/export?scope=cluster&limit=2", "operator", nil).want(200).json(&first)
	f.h.do("GET", BasePath+"/queries/export?scope=cluster&limit=2&cursor="+url.QueryEscape(first.CursorNext), "operator", nil).want(200).json(&second)
	if len(second.Queries) != 2 || reflect.DeepEqual(first.Queries, second.Queries) {
		t.Fatal("export continuation did not advance")
	}
	before := f.calls.Load()
	f.h.do("GET", BasePath+"/queries/export?scope=cluster", "viewer", nil).want(http.StatusForbidden)
	if f.calls.Load() != before {
		t.Fatal("denied export read peers")
	}
	for _, query := range []string{"?scope=unknown", "?scope=cluster&node=node-b", "?cursor=bad", "?status=not-a-status"} {
		response := f.h.do("GET", BasePath+"/queries/export"+query, "operator", nil).want(400)
		if response.Header().Get("Content-Disposition") != "" {
			t.Fatal("failed export sent attachment headers")
		}
	}
}

func TestQueryExportPartialAndBounds(t *testing.T) {
	f := newClusterLogFixture(t)
	f.h.srv.opts.Membership = staticMembership{peers: []cluster.Peer{{Name: "node-a", Self: true}, {Name: "offline"}}}
	var page QueriesBody
	f.h.do("GET", BasePath+"/queries/export?scope=cluster", "operator", nil).want(200).json(&page)
	if !page.Partial || len(page.Nodes) != 2 || page.Nodes[1].OK || len(page.Queries) != 3 {
		t.Fatal("export hid unavailable peer")
	}
	response := f.h.do("GET", BasePath+"/queries/export?limit=1001", "operator", nil)
	if response.Code < 400 || response.Header().Get("Content-Disposition") != "" {
		t.Fatal("oversize row limit was admitted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if out, err := f.h.srv.exportQueries(ctx, &QueryParams{}); err == nil || out != nil {
		t.Fatal("canceled export returned a download")
	}
	buf := &queryExportBuffer{ctx: t.Context()}
	if n, err := buf.Write(make([]byte, queryExportMaxBytes)); err != nil || n != queryExportMaxBytes {
		t.Fatal("output bound not inclusive")
	}
	if n, err := buf.Write([]byte{1}); n != 0 || !errors.Is(err, errQueryExportSize) || buf.Len() != queryExportMaxBytes {
		t.Fatal("oversize output appended partial data")
	}
	canceled := &queryExportBuffer{ctx: ctx}
	if n, err := canceled.Write([]byte("private")); n != 0 || !errors.Is(err, context.Canceled) || canceled.Len() != 0 {
		t.Fatal("canceled output retained data")
	}
}

func TestQueryExportUsesRetainedPrivacy(t *testing.T) {
	privacy, err := querylog.NewPrivacy(querylog.PrivacyOptions{Mode: querylog.PrivacyHideDomainsClients})
	if err != nil {
		t.Fatal(err)
	}
	event := qev(1, time.Now().Add(-time.Minute), "192.0.2.77", "private.example.", pipeline.StatusForwarded, "")
	privacy.Apply(event)
	w, _ := openNodeLog(t, "node-a", []*querylog.Event{event})
	h := newHarness(t, func(o *Options) { o.Querylog = w.Reader() })
	response := h.do("GET", BasePath+"/queries/export", "operator", nil).want(200)
	if strings.Contains(response.Body.String(), "private.example") || strings.Contains(response.Body.String(), "192.0.2.77") {
		t.Fatal("export disclosed pre-privacy values")
	}
	var page QueriesBody
	response.json(&page)
	if len(page.Queries) != 1 {
		t.Fatal("privacy test did not export a retained row")
	}
}
