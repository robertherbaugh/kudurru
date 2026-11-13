// Package refusalinventory walks every operation of a served OpenAPI document
// against a running kudurru API and holds each answer to the refusal
// inventory of internal/api (api.ExpectedRefusal). Mounted tests in
// cmd/kudurru (complete SQLite, static twin, Kubernetes) and the process
// tests in test/e2e (enrolled primary, follower, fenced and conflicted
// members) share it, so every runtime shape is judged by the same rules.
package refusalinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// Operation is one operation of the served OpenAPI document.
type Operation struct {
	Method, Path, ID string
}

func (o Operation) String() string { return o.Method + " " + o.Path + " (" + o.ID + ")" }

// Operations returns every operation of an OpenAPI document, sorted by path
// and method, with the operations that end the caller's session last.
func Operations(document []byte) ([]Operation, error) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(document, &doc); err != nil {
		return nil, fmt.Errorf("decode OpenAPI document: %w", err)
	}
	var out []Operation
	for path, item := range doc.Paths {
		for method, raw := range item {
			upper := strings.ToUpper(method)
			switch upper {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				continue
			}
			var op struct {
				OperationID string `json:"operationId"`
			}
			if err := json.Unmarshal(raw, &op); err != nil || op.OperationID == "" {
				return nil, fmt.Errorf("%s %s has no operationId", upper, path)
			}
			out = append(out, Operation{Method: upper, Path: path, ID: op.OperationID})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the OpenAPI document has no operations")
	}
	slices.SortFunc(out, func(a, b Operation) int {
		if ea, eb := endsSession(a), endsSession(b); ea != eb {
			if ea {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Path+" "+a.Method, b.Path+" "+b.Method)
	})
	return out, nil
}

// endsSession reports whether a probe of the operation may end the session
// the walk uses, so that it must run after every other probe.
func endsSession(o Operation) bool {
	return slices.Contains(api.SessionEndingOperations(), o.ID)
}

var pathParameter = regexp.MustCompile(`\{[^}]+\}`)

// ProbePath fills the path parameters of an operation with values that the
// router and the permission checks accept and that name no existing object.
func ProbePath(path string) string {
	path = strings.NewReplacer("{kind}", "dnszones", "{namespace}", "default", "{type}", "A", "{zone}", "refusal-probe",
		"{section}", "upstreams", "{scope}", "refusal-probe", "{mac}", "02:00:00:00:00:01").Replace(path)
	return api.BasePath + pathParameter.ReplaceAllString(path, "refusal-probe")
}

// Answer is what one probe observed.
type Answer struct {
	Status      int
	ContentType string
	RetryAfter  string
	// Envelope is true when the body decoded as the error envelope with a key.
	Envelope bool
	Key      string
	Message  string
	Hint     string
}

func (a Answer) String() string {
	if a.Envelope {
		return fmt.Sprintf("%d %s (message %q, hint %q)", a.Status, a.Key, a.Message, a.Hint)
	}
	return fmt.Sprintf("%d with content type %q", a.Status, a.ContentType)
}

// Client sends the probes of one walk.
type Client struct {
	HTTP *http.Client
	// Base is the API's address including BasePath's host part, for
	// example "http://127.0.0.1:8080".
	Base string
	// Prepare adds the walk's credential to a request (a CSRF header for a
	// cookie session, or a bearer token).
	Prepare func(*http.Request)
	// Timeout bounds one probe. Zero means ten seconds.
	Timeout time.Duration
	// Parallel is the number of concurrent probes of Walk; see Walk.
	Parallel int
	// FeaturesOff names the features that the node's configuration turns
	// off (api.ExpectedRefusal).
	FeaturesOff []api.Feature
}

// probeRequest is what a probe sends beyond the filled path: a query string,
// a JSON body (the empty object when Body is ""), and an If-Match value.
type probeRequest struct {
	Query, Body, IfMatch string
}

// probeRequests lets the probe of an operation whose refusal is decided in
// its handler pass request validation, so that the walk reaches the handler.
// Every value names no existing object and no current revision, so no probe
// here can change anything on a shape that accepts it: an If-Match or
// review revision that matches nothing is refused, and the node log level is
// the one the fixtures already use.
var probeRequests = map[string]probeRequest{
	"patchNodeConfig":         {Body: `{"log":{"level":"error"}}`},
	"changeSettingsOwnership": {Body: `{"revision":"refusal-probe"}`},
	// Integrations: the namespace, the exact uid and revision of a review.
	"listIntegrations":        {Query: "namespace=default"},
	"listIntegrationActions":  {Query: "uid=refusal-probe&revision=refusal-probe"},
	"checkIntegration":        {Body: integrationReview},
	"previewIntegration":      {Body: integrationReview},
	"syncIntegration":         {Body: integrationReview},
	"releaseIntegration":      {Body: integrationReview},
	"invokeIntegrationAction": {Body: integrationReview},
	// DHCP mutations reach the scope lookup, which refuses without DHCP.
	"putDHCPReservation":    {Body: `{"ip":"192.0.2.10"}`, IfMatch: `"refusal-probe"`},
	"deleteDHCPReservation": {IfMatch: `"refusal-probe"`},
	"putDHCPScopeConfig":    {Body: `{"rangeStart":"192.0.2.100","rangeEnd":"192.0.2.200","router":"192.0.2.1","dnsServers":["192.0.2.1"],"domainName":"","leaseDuration":"1h","registerDNS":false,"dnsTTL":"60s"}`, IfMatch: `"refusal-probe"`},
	"importDHCPLeases":      {Body: `{"format":"native","data":""}`},
}

const integrationReview = `{"uid":"refusal-probe","revision":"refusal-probe"}`

// probeMutations are the operations whose probe a shape may accept with a
// 2xx answer. Each one is reviewed: it ends the walk's own session, changes
// nothing that a later probe or test step reads (an empty apply, a cache
// flush, a log level the fixture already has, a YAML export), or is
// harmless on a test node (an unnamed token, a query-log purge). A probe of
// any other mutation that succeeds fails the walk, so that a body that
// becomes valid cannot change a shared fixture silently.
var probeMutations = []string{
	"apply", "createToken", "flushCache", "purgeQuerylog", "logout", "browserLogout",
	"patchNodeConfig", "exportSettingsSectionYAML",
}

// Probe sends one request for an operation, with an empty JSON object (or
// the body of the operation's probeRequests entry) as the body of a method
// that may carry one, and the entry's query and If-Match. A successful
// streaming answer is not read: its status and content type are the
// observation.
//
// A 503 generation_pending is answered before any command runs, so only that
// answer is observed again, for at most a minute (a node under the race
// detector can take that long to prepare its first provider generation).
// The walk waits for the end of the pending state with readinessProbe, which
// no rate limit charges, and only then sends the operation again: provider
// sign-in admission charges its lane before the serving generation is
// captured, and five quick resends of the browser start exhaust it.
func (c Client) Probe(ctx context.Context, op Operation) (Answer, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(time.Minute)
	pending := func(a Answer) bool {
		return a.Status == http.StatusServiceUnavailable && a.Key == "generation_pending"
	}
	for {
		answer, err := c.probeOnce(ctx, op, timeout)
		if err != nil || !pending(answer) {
			return answer, err
		}
		for {
			if !time.Now().Before(deadline) {
				return answer, nil
			}
			select {
			case <-ctx.Done():
				return answer, ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
			ready, err := c.probeOnce(ctx, readinessProbe, timeout)
			if err != nil {
				return answer, err
			}
			if !pending(ready) {
				break
			}
		}
	}
}

// readinessProbe is the request that tells when a node's pending serving
// generation is complete: it passes the same serving gate as every
// operation, reads only the caller's own principal and charges no limit.
var readinessProbe = Operation{Method: http.MethodGet, Path: "/auth/me", ID: "me"}

func (c Client) probeOnce(ctx context.Context, op Operation, timeout time.Duration) (Answer, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	probe := probeRequests[op.ID]
	var body io.Reader = http.NoBody
	if op.Method != http.MethodGet {
		raw := probe.Body
		if raw == "" {
			raw = "{}"
		}
		body = strings.NewReader(raw)
	}
	target := c.Base + ProbePath(op.Path)
	if probe.Query != "" {
		target += "?" + probe.Query
	}
	request, err := http.NewRequestWithContext(ctx, op.Method, target, body)
	if err != nil {
		return Answer{}, err
	}
	if op.Method != http.MethodGet {
		request.Header.Set("Content-Type", "application/json")
	}
	if probe.IfMatch != "" {
		request.Header.Set("If-Match", probe.IfMatch)
	}
	if c.Prepare != nil {
		c.Prepare(request)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return Answer{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer := Answer{Status: response.StatusCode, ContentType: response.Header.Get("Content-Type"), RetryAfter: response.Header.Get("Retry-After")}
	if response.StatusCode < http.StatusBadRequest {
		return answer, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return answer, fmt.Errorf("%s: read the answer: %w", op, err)
	}
	var envelope struct {
		Error *struct {
			Key     string `json:"key"`
			Message string `json:"message"`
			Hint    any    `json:"hint"`
		} `json:"error"`
	}
	if strings.HasPrefix(answer.ContentType, "application/json") && json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil && envelope.Error.Key != "" {
		answer.Envelope = true
		answer.Key, answer.Message = envelope.Error.Key, envelope.Error.Message
		answer.Hint, _ = envelope.Error.Hint.(string)
	}
	return answer, nil
}

// Problems holds one answer to the inventory for a shape and returns what is
// wrong with it, each line saying what to change.
func Problems(shape api.RuntimeShape, op Operation, a Answer, off ...api.Feature) []string {
	var out []string
	if a.Status >= http.StatusBadRequest && !a.Envelope {
		out = append(out, fmt.Sprintf("%s on %s answered %s without the JSON error envelope", op, shape, a))
	}
	want, declared := api.ExpectedRefusal(shape, op.ID, op.Method, off...)
	refusal := a.Status == http.StatusNotImplemented || a.Status == http.StatusServiceUnavailable
	switch {
	case !declared:
		out = append(out, fmt.Sprintf("%s has no line in operationRefusals (internal/api/refusals.go): add one that names the 501/503 key it answers on each runtime shape, or an empty line if it answers none; on %s it answered %s", op, shape, a))
	case refusal && want == "":
		out = append(out, fmt.Sprintf("%s on %s answered %s, but operationRefusals (internal/api/refusals.go) declares no refusal for it there: declare the key on that shape (and give the key a row in refusalKeys with its operator doc anchor) or fix the route", op, shape, a))
	case refusal && a.Key != want:
		out = append(out, fmt.Sprintf("%s on %s answered %s, but operationRefusals (internal/api/refusals.go) declares %q there", op, shape, a, want))
	case !refusal && want != "":
		out = append(out, fmt.Sprintf("%s on %s answered %s, but operationRefusals (internal/api/refusals.go) declares the refusal %q there: remove the declaration or restore the refusal", op, shape, a, want))
	}
	if refusal && a.Envelope {
		doc, known := api.RefusalKeyDoc(a.Key, a.Status)
		switch {
		case !known:
			out = append(out, fmt.Sprintf("%s on %s answered %s, a key and status without a row in refusalKeys (internal/api/refusals.go): add the row with the operator doc anchor that explains it", op, shape, a))
		case a.Hint == "":
			out = append(out, fmt.Sprintf("%s on %s answered %s without a hint (explained at %s)", op, shape, a, doc))
		}
		if allowed, required := api.RefusalRetryAfterAllowed(op.ID, a.Key, a.Status); known && (a.RetryAfter != "" && !allowed || a.RetryAfter == "" && required) {
			out = append(out, fmt.Sprintf("%s on %s answered %s with Retry-After %q: only a transient refusal carries one, and it always does", op, shape, a, a.RetryAfter))
		}
	}
	if op.Method != http.MethodGet && a.Status >= 200 && a.Status < 300 && !slices.Contains(probeMutations, op.ID) {
		out = append(out, fmt.Sprintf("%s on %s answered %d: the probe of a mutation succeeded and may have changed the node; give the operation a probeRequests entry that names no object or revision, or review it and add it to probeMutations (internal/api/refusalinventory/walk.go)", op, shape, a.Status))
	}
	return out
}

// StoppedAtValidation reports whether a probe was refused by request
// validation (a missing or invalid parameter or body property) before the
// operation's handler ran, so that a refusal the handler decides was not
// observed.
func StoppedAtValidation(a Answer) bool {
	return a.Status == http.StatusBadRequest && a.Key == api.KeyInvalid && strings.HasPrefix(a.Message, "validation failed")
}

// Result is one probe and its problems.
type Result struct {
	Operation Operation
	Answer    Answer
	Problems  []string
}

// Walk probes every operation and returns each answer with its problems, in
// the order of operations. A transport failure stops the walk.
//
// With Parallel above one the probes run concurrently, at most Parallel at a
// time, and the session-ending operations still run after every other one.
// Use it only for a shape that runs no operation (a fenced or conflicted
// member, which answers each request after a bounded wait).
func (c Client) Walk(ctx context.Context, shape api.RuntimeShape, operations []Operation) ([]Result, error) {
	out := make([]Result, len(operations))
	probe := func(i int) error {
		answer, err := c.Probe(ctx, operations[i])
		if err != nil {
			return err
		}
		out[i] = Result{Operation: operations[i], Answer: answer, Problems: Problems(shape, operations[i], answer, c.FeaturesOff...)}
		return nil
	}
	if c.Parallel <= 1 {
		for i := range operations {
			if err := probe(i); err != nil {
				return out[:i], err
			}
		}
		return out, nil
	}
	var first, ending []int
	for i, op := range operations {
		if endsSession(op) {
			ending = append(ending, i)
		} else {
			first = append(first, i)
		}
	}
	for _, group := range [][]int{first, ending} {
		errs := make(chan error, len(group))
		slots := make(chan struct{}, c.Parallel)
		for _, i := range group {
			slots <- struct{}{}
			go func() {
				defer func() { <-slots }()
				errs <- probe(i)
			}()
		}
		var failed error
		for range group {
			if err := <-errs; err != nil && failed == nil {
				failed = err
			}
		}
		if failed != nil {
			return nil, failed
		}
	}
	return out, nil
}
