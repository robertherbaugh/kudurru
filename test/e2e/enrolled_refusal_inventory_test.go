package e2e

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/refusalinventory"
)

// enrolledOpenAPI reads the served OpenAPI document of an open member.
func enrolledOpenAPI(t *testing.T, c *staticProcessClient) []byte {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.base+"/openapi.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		t.Fatal("served OpenAPI document:", err)
	}
	defer func() { _ = response.Body.Close() }()
	document, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("served OpenAPI document unavailable", response.StatusCode, err)
	}
	return document
}

// enrolledRefusalSession signs the admin in on a member with a session of its
// own, so that the walk's session-ending probes end only that session. On a
// follower the sign-in is forwarded to the primary (D363).
func enrolledRefusalSession(t *testing.T, base, password string) *staticProcessClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	c := &staticProcessClient{client: &http.Client{Jar: jar, Transport: transport, Timeout: 35 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: base}
	var login api.SessionInfo
	c.request(t, http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: password}, http.StatusOK, &login)
	c.csrf = login.CSRF
	return c
}

// enrolledRefusalInventory walks every operation of the document on one
// member process and reports each answer that the refusal inventory of
// internal/api does not explain (G21-11): an error without the envelope, or a
// 501/503 whose key the inventory does not declare for the shape, has no
// allowlist row, has no hint or carries the wrong Retry-After.
func enrolledRefusalInventory(t *testing.T, c *staticProcessClient, shape api.RuntimeShape, document []byte) {
	t.Helper()
	operations, err := refusalinventory.Operations(document)
	if err != nil {
		t.Fatal(err)
	}
	csrf := c.csrf
	parallel := 1
	if shape == api.ShapeFenced || shape == api.ShapeConflicted {
		// A closed member runs nothing and answers each request after the
		// serving gate's bounded capture wait.
		parallel = 16
	}
	// The enrolled fixtures run without the query log, DHCP and a VRRP
	// address.
	walker := refusalinventory.Client{HTTP: c.client, Parallel: parallel, FeaturesOff: []api.Feature{api.FeatureQueryLog, api.FeatureDHCP, api.FeatureVIP}, Base: strings.TrimSuffix(c.base, api.BasePath), Prepare: func(r *http.Request) {
		if csrf != "" {
			r.Header.Set("X-Kudurru-CSRF", csrf)
		}
	}}
	results, err := walker.Walk(t.Context(), shape, operations)
	if err != nil {
		t.Fatal(err)
	}
	refused, dump := 0, os.Getenv("KUDURRU_REFUSAL_INVENTORY_DUMP") != ""
	for _, r := range results {
		if dump {
			t.Logf("DUMP %s %s %s %d %s %q %q", shape, r.Operation.ID, r.Operation.Method, r.Answer.Status, r.Answer.Key, r.Answer.Message, r.Answer.Hint)
		}
		if r.Answer.Status == http.StatusNotImplemented || r.Answer.Status == http.StatusServiceUnavailable {
			refused++
		}
		for _, problem := range r.Problems {
			t.Error(problem)
		}
	}
	t.Logf("%s: %d operations walked, %d answered a declared 501 or 503", shape, len(results), refused)
}
