package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/apiclient/openapi"
)

// TestGeneratedClient drives the server through the generated Go client
// (internal/apiclient/openapi), so a contract change that breaks the
// client fails here as well as in the drift test.
func TestGeneratedClient(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.srv)
	defer ts.Close()
	as := func(user string) openapi.ClientOption {
		return openapi.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
			r.Header.Set("Authorization", apitest.Bearer(user))
			return nil
		})
	}
	c, err := openapi.NewClientWithResponses(ts.URL+BasePath, as("admin"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	v, err := c.GetVersionWithResponse(ctx)
	if err != nil || v.JSON200 == nil || v.JSON200.Version != "v0.1.0-test" {
		t.Fatalf("version %v %+v", err, v)
	}
	put, err := c.PutClusterObjectWithResponse(ctx, "dnszones", "example", &openapi.PutClusterObjectParams{}, zoneDoc("example", "example.com."))
	if err != nil || put.StatusCode() != http.StatusCreated || put.JSON201 == nil {
		t.Fatalf("put %v %d %s", err, put.StatusCode(), put.Body)
	}
	zones, err := c.ListZonesWithResponse(ctx)
	if err != nil || zones.JSON200 == nil || len(zones.JSON200.Items) != 1 || zones.JSON200.Items[0].ZoneName != "example.com." {
		t.Fatalf("zones %v %s", err, zones.Body)
	}
	stale := "999"
	conflict, err := c.PutClusterObjectWithResponse(ctx, "dnszones", "example", &openapi.PutClusterObjectParams{IfMatch: &stale}, zoneDoc("example", "example.com."))
	if err != nil || conflict.StatusCode() != http.StatusPreconditionFailed || conflict.JSON412 == nil || conflict.JSON412.Error.Key != KeyPrecondition {
		t.Fatalf("If-Match %v %d %s", err, conflict.StatusCode(), conflict.Body)
	}

	viewer, err := openapi.NewClientWithResponses(ts.URL+BasePath, as("viewer"))
	if err != nil {
		t.Fatal(err)
	}
	denied, err := viewer.DeleteClusterObjectWithResponse(ctx, "dnszones", "example", &openapi.DeleteClusterObjectParams{})
	if err != nil || denied.StatusCode() != http.StatusForbidden || denied.JSON403 == nil || denied.JSON403.Error.Key != KeyForbidden {
		t.Fatalf("viewer delete %v %d %s", err, denied.StatusCode(), denied.Body)
	}
}
