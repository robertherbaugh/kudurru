package integrationconformance

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"
)

// Check contains only fixed protocol check names and sanitized outcomes.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Report describes observed wire acknowledgments, not durable provider effects.
// Complete is false whenever an advertised mutation has not been exercised.
type Report struct {
	Protocol string  `json:"protocol"`
	Passed   bool    `json:"passed"`
	Complete bool    `json:"complete"`
	Checks   []Check `json:"checks"`
}

// Run captures explicit files once, then probes the advertised protocol under a
// single nonrenewable deadline. It never fetches ambient credentials or retries.
func Run(ctx context.Context, c Config) (Report, error) {
	if c.Timeout == 0 {
		c.Timeout = time.Minute
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 10 * time.Second
	}
	if c.Timeout <= 0 || c.Timeout > 5*time.Minute || c.RequestTimeout <= 0 || c.RequestTimeout > c.Timeout {
		return Report{}, errInput
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	r, err := capture(ctx, func() (*runner, error) { return prepare(ctx, c) })
	if err != nil {
		return Report{}, err
	}
	defer r.client.CloseIdleConnections()
	r.report = Report{Passed: true, Complete: true, Checks: []Check{}}
	r.run(ctx)
	if err := ctx.Err(); err != nil {
		r.report.Passed = false
		r.report.Complete = false
		return r.report, err
	}
	if !r.report.Passed {
		return r.report, errors.New("webhook conformance failed; see sanitized checks")
	}
	return r.report, nil
}

func (r *runner) failed(name, reason string) {
	r.report.Passed, r.report.Complete = false, false
	r.report.Checks = append(r.report.Checks, Check{Name: name, Status: "failed", Reason: reason})
}

func (r *runner) skipped(name string) {
	r.report.Complete = false
	r.report.Checks = append(r.report.Checks, Check{Name: name, Status: "skipped", Reason: "explicit_probe_required"})
}

func (r *runner) probe(ctx context.Context, name, method, path string, body []byte, headers http.Header, mutation bool, validate func([]byte, int, string) bool) bool {
	b, status, media, err := r.request(ctx, method, path, body, headers)
	if err != nil {
		reason := err.Error()
		if mutation {
			reason = "effect_uncertain"
		}
		r.failed(name, reason)
		return false
	}
	if !validate(b, status, media) {
		reason := "protocol"
		if mutation {
			reason = "effect_uncertain"
		}
		r.failed(name, reason)
		return false
	}
	r.report.Checks = append(r.report.Checks, Check{Name: name, Status: "passed"})
	return true
}

func (r *runner) jsonCheck(valid func([]byte) bool) func([]byte, int, string) bool {
	return func(b []byte, status int, media string) bool {
		return status == http.StatusOK && media == r.media && valid(b)
	}
}

func (r *runner) run(ctx context.Context) {
	caps := map[string]bool{}
	if !r.probe(ctx, "negotiate", http.MethodGet, "/", nil, nil, false, func(b []byte, status int, media string) bool {
		if status != http.StatusOK {
			return false
		}
		r.media = media
		switch media {
		case ExternalDNSMedia:
			r.report.Protocol = "external-dns-v1"
			caps["records"] = true
			return validFilter(b)
		case IntegrationMedia:
			r.report.Protocol = "kudurru-integration-v1"
			var ok bool
			caps, ok = validDeclaration(b)
			return ok
		default:
			return false
		}
	}) {
		return
	}
	// Refuse operator-supplied probes for unadvertised capabilities before any
	// effects; a later protocol cannot lend authority to an earlier request.
	if (r.changes != nil || r.explicitAdjust) && !caps["records"] || r.event != nil && !caps["hooks"] || r.action != "" && !caps["actions"] {
		r.failed("probe_selection", "not_advertised")
		return
	}
	if caps["records"] {
		if !r.probe(ctx, "records", http.MethodGet, "/records", nil, nil, false, r.jsonCheck(validEndpoints)) {
			return
		}
		if !r.probe(ctx, "adjust_endpoints", http.MethodPost, "/adjustendpoints", r.adjust, nil, false, r.jsonCheck(validEndpoints)) {
			return
		}
	}
	if caps["clients"] {
		for _, item := range []struct {
			name    string
			clients bool
		}{{"clients", true}, {"networks", false}} {
			if !r.probe(ctx, item.name, http.MethodGet, "/"+item.name, nil, nil, false, r.jsonCheck(func(b []byte) bool { return validInventory(b, item.clients) })) {
				return
			}
		}
	}
	var actionCount int
	if caps["actions"] {
		found := r.action == ""
		if !r.probe(ctx, "actions", http.MethodGet, "/actions", nil, nil, false, r.jsonCheck(func(b []byte) bool {
			actions, ok := validActions(b)
			actionCount = len(actions)
			for _, a := range actions {
				if a.Name == r.action {
					found = true
				}
			}
			return ok
		})) {
			return
		}
		if !found {
			r.failed("invoke_action", "not_advertised")
			return
		}
	}
	if caps["records"] {
		if r.changes == nil {
			r.skipped("apply_records")
		} else if !r.probe(ctx, "apply_records", http.MethodPost, "/records", r.changes, nil, true, func(b []byte, status int, _ string) bool { return status == http.StatusNoContent && len(b) == 0 }) {
			return
		}
	}
	if caps["hooks"] {
		if r.event == nil {
			r.skipped("deliver_event")
		} else {
			id := make([]byte, 16)
			if _, err := rand.Read(id); err != nil {
				r.failed("deliver_event", "random_source")
				return
			}
			mac := hmac.New(sha256.New, r.hmacKey)
			_, _ = mac.Write(r.event)
			headers := http.Header{"X-Kudurru-Signature": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}, "X-Kudurru-Delivery": {hex.EncodeToString(id)}}
			if !r.probe(ctx, "deliver_event", http.MethodPost, "/events", r.event, headers, true, func(_ []byte, _ int, _ string) bool { return true }) {
				return
			}
		}
	}
	if caps["actions"] {
		if r.action == "" {
			r.skipped("invoke_action")
		} else if !r.probe(ctx, "invoke_action", http.MethodPost, "/actions/"+r.action, r.params, nil, true, func(b []byte, status int, media string) bool {
			return status == http.StatusNoContent && len(b) == 0 || status == http.StatusOK && media == r.media && validJSON(b)
		}) {
			return
		}
		if r.action != "" && actionCount > 1 {
			r.skipped("other_advertised_actions")
		}
	}
}
