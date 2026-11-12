package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

// forbiddenMissing decodes the missing permission of a 403 envelope.
func forbiddenMissing(t *testing.T, r response) Missing {
	t.Helper()
	var e struct {
		Error ErrorBody `json:"error"`
	}
	r.json(&e)
	if e.Error.Key != KeyForbidden || e.Error.Missing == nil {
		t.Fatalf("not a forbidden envelope with missing: %s", r.Body.String())
	}
	return *e.Error.Missing
}

// TestQueryExportRoleMatrix pins the D31 role table on the query-log
// download (D393): GET /queries/export needs querylog.export, which the
// auditor and admin roles hold and the operator role does not, while GET
// /queries stays on querylog.read.
func TestQueryExportRoleMatrix(t *testing.T) {
	log, _ := querylogFixture(t)
	h := newHarness(t, func(o *Options) { o.Querylog = log.Reader() })
	for _, c := range []struct {
		user         string
		list, export int
	}{
		{"viewer", http.StatusForbidden, http.StatusForbidden},
		{"zoneadmin", http.StatusForbidden, http.StatusForbidden},
		{"operator", http.StatusOK, http.StatusForbidden},
		{"auditor", http.StatusOK, http.StatusOK},
		{"admin", http.StatusOK, http.StatusOK},
	} {
		t.Run(c.user, func(t *testing.T) {
			h.do(http.MethodGet, BasePath+"/queries", c.user, nil).want(c.list)
			r := h.do(http.MethodGet, BasePath+"/queries/export", c.user, nil).want(c.export)
			if c.export == http.StatusOK {
				var page QueriesBody
				r.json(&page)
				if len(page.Queries) != 4 || r.Header().Get("Content-Disposition") == "" {
					t.Fatalf("export page %d rows, disposition %q", len(page.Queries), r.Header().Get("Content-Disposition"))
				}
				return
			}
			if r.Header().Get("Content-Disposition") != "" {
				t.Fatal("refused export sent attachment headers")
			}
			if m := forbiddenMissing(t, r); m.Resource != "querylog" || m.Verb != "export" || m.Scope != "cluster" {
				t.Fatalf("missing %+v, want querylog.export at cluster", m)
			}
		})
	}
}

// listsOnly grants the user "lister" every blocklists verb and nothing
// else, the way a custom role could; everyone else gets the standard
// bindings.
type listsOnly struct{ standard rbac.Authorizer }

func (a listsOnly) Authorize(ctx context.Context, p rbac.Principal, attrs rbac.Attributes) (rbac.Decision, error) {
	if p.Name == "lister" {
		if attrs.Resource == "blocklists" {
			return rbac.Decision{Allowed: true, Path: "test"}, nil
		}
		return rbac.Decision{Path: "test", Missing: &attrs, Reason: "not granted"}, nil
	}
	return a.standard.Authorize(ctx, p, attrs)
}

// TestQueryActionNeedsQuerylogRead: POST /queries/{id}/actions reads a
// logged query by ID before it edits a list, so blocklists.write alone is
// not enough (D393). A caller without querylog.read is refused with the
// missing permission before the row is read, for an existing and a missing
// ID alike (no existence oracle), on the legacy and the command path; the
// denial is audited and no list changes.
func TestQueryActionNeedsQuerylogRead(t *testing.T) {
	standard := apitest.Authorizer(apitest.StandardBindings()...)
	deniedRead := func(t *testing.T, h *harness) {
		t.Helper()
		for _, id := range []string{"2", "9999"} {
			r := h.do(http.MethodPost, BasePath+"/queries/"+id+"/actions", "lister", map[string]any{"action": "allow"}).want(http.StatusForbidden)
			if m := forbiddenMissing(t, r); m.Resource != "querylog" || m.Verb != "read" || m.Scope != "cluster" {
				t.Fatalf("query %s: missing %+v, want querylog.read at cluster", id, m)
			}
		}
		denied := 0
		for _, ev := range h.audit.Events() {
			if ev.Action == "querylog.read" && ev.Result == audit.ResultDenied {
				denied++
			}
		}
		if denied != 2 {
			t.Fatalf("%d audited querylog.read denials, want 2", denied)
		}
	}

	t.Run("legacy", func(t *testing.T) {
		log, _ := querylogFixture(t)
		h := newHarness(t, func(o *Options) {
			o.Querylog = log.Reader()
			o.Authorizer = listsOnly{standard: standard}
		})
		// The caller may still edit lists directly.
		h.do(http.MethodPost, BasePath+"/allowlist", "lister", map[string]any{"values": []string{"direct.example"}}).want(http.StatusOK)
		deniedRead(t, h)
		var p Processed
		h.do(http.MethodPost, BasePath+"/queries/2/actions", "operator", map[string]any{"action": "allow"}).want(http.StatusOK).json(&p)
		if len(p.Processed.Success) != 1 || p.Processed.Success[0].Item != "ads.example" {
			t.Fatalf("operator action %+v", p)
		}
	})

	t.Run("command", func(t *testing.T) {
		commands := 0
		h, st := domainCommandHarness(t, func([]rbac.Attributes) error { commands++; return nil })
		log, _ := querylogFixture(t)
		h.srv.opts.Querylog = log.Reader()
		h.srv.opts.Authorizer = listsOnly{standard: standard}
		deniedRead(t, h)
		if commands != 0 {
			t.Fatalf("a refused query action reached %d domain commands", commands)
		}
		h.do(http.MethodPost, BasePath+"/queries/2/actions", "operator", map[string]any{"action": "allow"}).want(http.StatusOK)
		if got := domainListAt(t, st, "ui-allow"); len(got.Spec.Sources[0].Entries) != 1 || got.Spec.Sources[0].Entries[0].Value != "ads.example" {
			t.Fatal("operator query action did not edit the list", got)
		}
	})
}
