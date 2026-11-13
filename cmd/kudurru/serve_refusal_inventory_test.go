package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/api/refusalinventory"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// refusalInventorySession signs the admin in on a mounted API with a cookie
// session, the credential the dashboard uses, and returns a walk client. The
// mounted fixtures run without DHCP and without a VRRP address.
func refusalInventorySession(t *testing.T, address, password string) refusalinventory.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	c := &http.Client{Transport: transport, Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + address + api.BasePath
	tsigMountAwaitRead(t, c, base+"/auth/status", nil)
	var session api.SessionInfo
	controlMountedRequest(t, c, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: password}, http.StatusOK, &session)
	if session.CSRF == "" {
		t.Fatal("admin session lacks CSRF")
	}
	return refusalinventory.Client{HTTP: c, Base: "http://" + address, FeaturesOff: []api.Feature{api.FeatureDHCP, api.FeatureVIP}, Prepare: func(r *http.Request) { r.Header.Set("X-Kudurru-CSRF", session.CSRF) }}
}

// refusalInventoryWalk walks every operation of the served OpenAPI document
// on one shape and reports each answer that the inventory does not explain.
func refusalInventoryWalk(t *testing.T, shape api.RuntimeShape, c refusalinventory.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+api.BasePath+"/openapi.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		t.Fatal("served OpenAPI document:", err)
	}
	document, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("served OpenAPI document unavailable", response.StatusCode, err)
	}
	operations, err := refusalinventory.Operations(document)
	if err != nil {
		t.Fatal(err)
	}
	refusalInventoryReport(t, shape, c, operations)
}

// refusalInventoryReport walks the operations on one shape and reports each
// answer that the inventory does not explain.
func refusalInventoryReport(t *testing.T, shape api.RuntimeShape, c refusalinventory.Client, operations []refusalinventory.Operation) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	results, err := c.Walk(ctx, shape, operations)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(operations) {
		t.Fatalf("%s: walked %d of %d operations", shape, len(results), len(operations))
	}
	dump := os.Getenv("KUDURRU_REFUSAL_INVENTORY_DUMP") != ""
	refused := 0
	for _, r := range results {
		if r.Answer.Status == http.StatusNotImplemented || r.Answer.Status == http.StatusServiceUnavailable {
			refused++
		}
		if dump {
			t.Logf("DUMP %s %s %s %d %s %q %q", shape, r.Operation.ID, r.Operation.Method, r.Answer.Status, r.Answer.Key, r.Answer.Message, r.Answer.Hint)
		}
		for _, problem := range r.Problems {
			t.Error(problem)
		}
	}
	t.Logf("%s: %d operations walked, %d answered a declared 501 or 503", shape, len(results), refused)
}

// refusalInventoryConfig is the mounted SQLite fixture with what an operator
// of a real node has: a license key and the SQLite query log. Refusals that
// only an unlicensed node or a disabled query log answers are not shape
// limits. OIDC providers stay unconfigured, as on a node without one.
func refusalInventoryConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv(license.EnvKey, trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3}))
	t.Setenv(license.EnvKeyFile, "")
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.Querylog.Enabled = true
	return cfg
}

// The complete SQLite runtime of a standalone node.
func TestRunServeRefusalInventoryStandalone(t *testing.T) {
	cfg := refusalInventoryConfig(t)
	startStaticMount(t, cfg)
	refusalInventoryWalk(t, api.ShapeStandalone, refusalInventorySession(t, cfg.Listeners.API.Address, "mounted-test-password-1"))
}

// A static twin. Its peer is not running: the inventory is about this node's
// own answers.
func TestRunServeRefusalInventoryStatic(t *testing.T) {
	cfg := refusalInventoryConfig(t)
	cfg.Node.Name = "node-a"
	cfg.Listeners.Internal.Address = offlineFreeAddress(t, "tcp")
	cfg.Cluster = config.ClusterConfig{Role: "static", Peers: []config.StaticPeer{{Name: "node-a", Address: cfg.Listeners.Internal.Address}, {Name: "node-b", Address: offlineFreeAddress(t, "tcp")}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	startStaticMount(t, cfg)
	refusalInventoryWalk(t, api.ShapeStatic, refusalInventorySession(t, cfg.Listeners.API.Address, "mounted-test-password-1"))
}

// A node of the Kubernetes store driver, against an envtest control plane.
func TestRunServeRefusalInventoryKubernetes(t *testing.T) {
	t.Setenv(license.EnvKey, trustTestKey(t)(license.Claims{Plan: license.PlanPersonal, Nodes: 3}))
	t.Setenv(license.EnvKeyFile, "")
	cfg := startKubeRuntimeMount(t, func(client.Client) {})
	refusalInventoryWalk(t, api.ShapeKubernetes, refusalInventorySession(t, cfg.Listeners.API.Address, "offline-test-password-1"))
}

// A fenced former primary and a member that recorded conflicting promotions
// close every API operation with the key of their durable state, through the
// production serving gate of the member fixture's follower. The fixture
// mounts fewer API options than runServe; no answer here reaches them. Each
// answer comes after the gate's bounded capture wait (two seconds), so the
// probes run concurrently.
func TestRootMemberRefusalInventoryClosedMembers(t *testing.T) {
	for _, tc := range []struct {
		shape  api.RuntimeShape
		epochs uint64
		want   error
		mode   replication.Mode
	}{
		{api.ShapeFenced, 1, nil, replication.Fenced},
		{api.ShapeConflicted, 0, replication.ErrPromotionConflict, replication.Conflict},
	} {
		t.Run(string(tc.shape), func(t *testing.T) {
			f := newMemberForwardHTTPFixture(t, false)
			document, err := json.Marshal(f.api.OpenAPI())
			if err != nil {
				t.Fatal(err)
			}
			operations, err := refusalinventory.Operations(document)
			if err != nil {
				t.Fatal(err)
			}
			followerStore := f.follower.opts.Store
			term := followerStore.Authority().Term
			term.Epoch += tc.epochs
			term.PrimaryID, term.PromotionID, term.PrimaryName = "other-member", "other-promotion", "node-z"
			if _, err := followerStore.ObserveAuthority(t.Context(), term); !errors.Is(err, tc.want) {
				t.Fatal("authority observation", err)
			}
			awaitServingRuntime(t, f.follower, func() bool { return f.follower.publisher.Status().Authority.Mode == tc.mode })
			token := f.primary.token
			c := refusalinventory.Client{HTTP: f.live.Client(), Base: f.live.URL, Parallel: 16, Prepare: func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }}
			refusalInventoryReport(t, tc.shape, c, operations)
		})
	}
}
