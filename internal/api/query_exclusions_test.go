package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/rbac"
)

type queryExclusionAuthorizer struct{ lists bool }

func (a queryExclusionAuthorizer) Authorize(_ context.Context, _ rbac.Principal, attrs rbac.Attributes) (rbac.Decision, error) {
	return rbac.Decision{Allowed: attrs.Resource == "stats" || (a.lists && attrs.Resource == "blocklists"), Path: rbac.PathBinding}, nil
}

func TestDNSQueryExclusionsRequireListRead(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		h := newHarness(t, func(o *Options) {
			o.Authorizer = queryExclusionAuthorizer{lists: allowed}
			o.Chain = pipeline.MustNew(queryStage(func(req *pipeline.Request) (*pipeline.Response, error) {
				if req.CollectExclusions != allowed {
					t.Error("diagnostic permission was not checked")
				}
				// Even a stage supplying unsolicited evidence cannot bypass the
				// API's projection permission gate.
				req.Exclusions = []pipeline.RuleExclusion{{List: "default/feed", Source: 0, Rule: "example.", Effect: "block"}}
				req.ExclusionsTruncated = true
				return answerStage{}.Serve(req, nil)
			}))
		})
		var result DNSQueryResult
		h.do(http.MethodPost, "/api/v1/dns/query", "viewer", map[string]any{"name": "example."}).want(http.StatusOK).json(&result)
		if (len(result.Exclusions) == 1) != allowed || result.ExclusionsTruncated != allowed {
			t.Fatal("exclusion evidence escaped its permission or was lost")
		}
	}
}
