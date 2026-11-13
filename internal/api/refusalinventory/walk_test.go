package refusalinventory

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/api"
)

const document = `{"paths":{
 "/auth/logout":{"post":{"operationId":"logout"}},
 "/auth/sessions":{"get":{"operationId":"listSessions"}},
 "/settings/sections/{section}/yaml":{"post":{"operationId":"exportSettingsSectionYAML"}},
 "/auth/tokens":{"post":{"operationId":"createToken"},"get":{"operationId":"listTokens"},"parameters":[]}
}}`

func TestOperationsSortsSessionEndingLast(t *testing.T) {
	ops, err := Operations([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, op := range ops {
		ids = append(ids, op.ID)
	}
	if got := strings.Join(ids, " "); got != "listSessions listTokens createToken exportSettingsSectionYAML logout" {
		t.Fatal("operation order:", got)
	}
	if _, err := Operations([]byte(`{"paths":{"/x":{"get":{}}}}`)); err == nil {
		t.Fatal("an operation without an operationId was accepted")
	}
	if got := ProbePath("/objects/{kind}/{namespace}/{name}"); got != api.BasePath+"/objects/dnszones/default/refusal-probe" {
		t.Fatal("probe path:", got)
	}
}

func TestProblemsNameWhatToAdd(t *testing.T) {
	envelope := func(status int, key, hint, retry string) Answer {
		return Answer{Status: status, ContentType: "application/json", Envelope: true, Key: key, Message: "m", Hint: hint, RetryAfter: retry}
	}
	get := Operation{Method: http.MethodGet, Path: "/auth/sessions", ID: "listSessions"}
	post := Operation{Method: http.MethodPost, Path: "/auth/tokens", ID: "createToken"}
	for _, tc := range []struct {
		name  string
		shape api.RuntimeShape
		op    Operation
		a     Answer
		want  string
	}{
		{"declared refusal", api.ShapeKubernetes, get, envelope(503, "sessions_unavailable", "h", ""), ""},
		{"no refusal", api.ShapeStandalone, get, Answer{Status: 200}, ""},
		{"plain-text error", api.ShapeStandalone, get, Answer{Status: 404, ContentType: "text/plain"}, "without the JSON error envelope"},
		{"undeclared refusal", api.ShapeStandalone, get, envelope(503, "unavailable", "h", ""), "declares no refusal for it there"},
		{"other key", api.ShapeKubernetes, get, envelope(503, "unavailable", "h", ""), `declares "sessions_unavailable" there`},
		{"declared but absent", api.ShapeKubernetes, get, Answer{Status: 200}, "remove the declaration or restore the refusal"},
		{"no hint", api.ShapeKubernetes, get, envelope(503, "sessions_unavailable", "", ""), "without a hint"},
		{"durable with Retry-After", api.ShapeKubernetes, get, envelope(503, "sessions_unavailable", "h", "1"), "Retry-After"},
		{"transient without Retry-After", api.ShapeStandalone, post, envelope(503, "generation_pending", "h", ""), "declares no refusal"},
		{"follower refusal", api.ShapeFollower, post, envelope(503, api.KeyPrimaryRequired, "h", ""), ""},
		{"unknown key", api.ShapeStandalone, Operation{Method: http.MethodPost, Path: "/x", ID: "exportSettingsSectionYAML"}, envelope(503, "something_new", "h", ""), `declares "unavailable" there`},
		{"succeeded mutation", api.ShapeStandalone, Operation{Method: http.MethodDelete, Path: "/objects/{kind}/{namespace}/{name}", ID: "deleteObject"}, Answer{Status: 200}, "add it to probeMutations"},
		{"reviewed mutation", api.ShapeStandalone, post, Answer{Status: 201}, ""},
		{"new route", api.ShapeStandalone, Operation{Method: http.MethodGet, Path: "/new", ID: "aRouteAddedLater"}, Answer{Status: 200}, "has no line in operationRefusals"},
	} {
		problems := strings.Join(Problems(tc.shape, tc.op, tc.a), "\n")
		if (tc.want == "") != (problems == "") || !strings.Contains(problems, tc.want) {
			t.Errorf("%s: problems %q, want one containing %q", tc.name, problems, tc.want)
		}
	}
	// A key outside the allowlist is reported with what to add.
	if problems := strings.Join(Problems(api.ShapeFenced, get, envelope(503, "something_new", "h", "")), "\n"); !strings.Contains(problems, "without a row in refusalKeys") {
		t.Errorf("unknown key: %q", problems)
	}
}

// Walk probes every operation once (generation_pending excepted), sends the
// credential, and in parallel mode still runs the session-ending operations
// after every other one.
func TestWalkSequentialAndParallel(t *testing.T) {
	ops, err := Operations([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	for _, parallel := range []int{1, 4} {
		var pending, others, readiness atomic.Int64
		var endedEarly atomic.Bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Test") != "credential" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path == api.BasePath+"/auth/logout" {
				if others.Load() != int64(len(ops)-1) {
					endedEarly.Store(true)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.URL.Path == api.BasePath+"/auth/sessions" && pending.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"key":"generation_pending","message":"m","hint":"h"}}`))
				return
			}
			if r.URL.Path == api.BasePath+"/auth/me" {
				// The readiness probe after a pending answer.
				readiness.Add(1)
				w.WriteHeader(http.StatusOK)
				return
			}
			others.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		c := Client{HTTP: server.Client(), Base: server.URL, Parallel: parallel, Prepare: func(r *http.Request) { r.Header.Set("X-Test", "credential") }}
		results, err := c.Walk(t.Context(), api.ShapeStandalone, ops)
		server.Close()
		if err != nil || len(results) != len(ops) {
			t.Fatal(parallel, err, len(results))
		}
		// The pending operation is sent again only after one readiness probe.
		if endedEarly.Load() || pending.Load() != 2 || readiness.Load() != 1 {
			t.Fatalf("parallel=%d: logout ran early=%t, pending probes=%d, readiness probes=%d", parallel, endedEarly.Load(), pending.Load(), readiness.Load())
		}
		for i, r := range results {
			if r.Operation != ops[i] {
				t.Fatalf("parallel=%d: result %d is %s, want %s", parallel, i, r.Operation, ops[i])
			}
		}
	}
}

// A probe of an operation with a probeRequests entry sends its query, body
// and If-Match, so that it passes request validation.
func TestProbeSendsTheRequestThatReachesTheHandler(t *testing.T) {
	var query, body, ifMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		query, body, ifMatch = r.URL.RawQuery, string(raw), r.Header.Get("If-Match")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := Client{HTTP: server.Client(), Base: server.URL}
	for _, tc := range []struct {
		op                   Operation
		query, body, ifMatch string
	}{
		{Operation{http.MethodGet, "/integrations", "listIntegrations"}, "namespace=default", "", ""},
		{Operation{http.MethodPost, "/integrations/{namespace}/{name}/check", "checkIntegration"}, "", integrationReview, ""},
		{Operation{http.MethodDelete, "/dhcp/scopes/{scope}/reservations/{mac}", "deleteDHCPReservation"}, "", "{}", `"refusal-probe"`},
		{Operation{http.MethodPost, "/auth/tokens", "createToken"}, "", "{}", ""},
	} {
		if _, err := c.Probe(t.Context(), tc.op); err != nil {
			t.Fatal(err)
		}
		if query != tc.query || body != tc.body || ifMatch != tc.ifMatch {
			t.Errorf("%s: query %q body %q If-Match %q", tc.op, query, body, ifMatch)
		}
	}
	if !StoppedAtValidation(Answer{Status: 400, Envelope: true, Key: api.KeyInvalid, Message: "validation failed: body: expected required property uid to be present"}) ||
		StoppedAtValidation(Answer{Status: 400, Envelope: true, Key: api.KeyInvalid, Message: "action must be adopt or release"}) {
		t.Error("StoppedAtValidation does not tell request validation from a handler's answer")
	}
}

// While a node stays pending, only the readiness probe is repeated: the
// operation itself, which a rate limit may charge, is sent once more after
// the pending state ends.
func TestProbeWaitsForReadinessBeforeResending(t *testing.T) {
	var starts, readiness atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pending := false
		switch r.URL.Path {
		case api.BasePath + "/auth/oidc/refusal-probe/start":
			pending = starts.Add(1) == 1
		case api.BasePath + "/auth/me":
			pending = readiness.Add(1) < 10
		}
		if pending {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"key":"generation_pending","message":"m","hint":"h"}}`))
			return
		}
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	c := Client{HTTP: server.Client(), Base: server.URL}
	answer, err := c.Probe(t.Context(), Operation{Method: http.MethodGet, Path: "/auth/oidc/{idp}/start", ID: "oidcStart"})
	if err != nil || answer.Status != http.StatusFound {
		t.Fatal(answer, err)
	}
	if starts.Load() != 2 || readiness.Load() != 10 {
		t.Fatalf("operation sent %d times, readiness probed %d times", starts.Load(), readiness.Load())
	}
}
