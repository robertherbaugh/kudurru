package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
)

// enrolledAnswer is one answer of a real server process, kept whole so that
// a refusal's key, hint and headers can be checked together.
type enrolledAnswer struct {
	status     int
	retryAfter string
	raw        []byte
	Error      struct {
		Key     string `json:"key"`
		Message string `json:"message"`
		Hint    any    `json:"hint"`
	} `json:"error"`
}

func (a enrolledAnswer) hint() string { s, _ := a.Error.Hint.(string); return s }

// enrolledSend sends one request once. It never retries.
func enrolledSend(t *testing.T, c *staticProcessClient, method, path string, body any) enrolledAnswer {
	t.Helper()
	answer, err := enrolledTrySend(t, c, method, path, body)
	if err != nil {
		t.Fatal("enrolled request failed", method, path, err)
	}
	return answer
}

// enrolledTrySend is enrolledSend for a process that may not listen yet: a
// transport error is returned instead of failing the test.
func enrolledTrySend(t *testing.T, c *staticProcessClient, method, path string, body any) (enrolledAnswer, error) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r.GetBody = nil
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		r.Header.Set("X-Kudurru-CSRF", c.csrf)
	}
	response, err := c.client.Do(r)
	if err != nil {
		return enrolledAnswer{}, err
	}
	defer func() { _ = response.Body.Close() }()
	answer := enrolledAnswer{status: response.StatusCode, retryAfter: response.Header.Get("Retry-After")}
	if answer.raw, err = io.ReadAll(io.LimitReader(response.Body, 128<<10)); err != nil {
		return enrolledAnswer{}, err
	}
	_ = json.Unmarshal(answer.raw, &answer)
	return answer, nil
}

// commandRefusedMeanwhile is the message of the effect-free 503 unavailable
// that a command gets when the node published another commit while its
// request ran. It must equal currentCommandAPIError's message in
// cmd/kudurru/serving_settings_api.go.
const commandRefusedMeanwhile = "current writable serving generation unavailable"

// enrolledPrimaryDelete deletes one object on the primary and returns once
// the delete answered 204. The primary commits its own work on its own
// schedule (zone status and lifecycle writes, admission), and each commit
// replaces the publication that a request in flight captured. A delete is
// bound to what its request saw, so it is then refused at once: 503
// unavailable with commandRefusedMeanwhile and no Retry-After, nothing
// applied (D399, D405). No wait for a zone's status proves that none of
// those commits is still to come: lifecycle and admission writes are not
// visible through the API. As docs/deploy/api-contracts.md ("Commands
// refused because the node published meanwhile") tells a client, the test
// then reads the current state and decides again: the object must still be
// there at the resource version it had before the refused attempt, and only
// then is the same delete sent once more, at the 50 ms cadence of the other
// e2e polls. Any other answer, a refusal with Retry-After, a changed or
// missing object, or ten seconds of refusals (the reads included) fails
// the test.
func enrolledPrimaryDelete(t *testing.T, c *staticProcessClient, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	pause := func() {
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	// version reads the object within the same bound. A read can meet the
	// node's own publication as well (503); a 404 after a refused delete is
	// a refusal that had an effect.
	version := func(afterRefusal bool) string {
		for {
			answer := enrolledSend(t, c, http.MethodGet, path, nil)
			if answer.status == http.StatusNotFound && afterRefusal {
				t.Fatalf("primary DELETE %s answered the effect-free 503 %s %q, but the object is gone", path, api.KeyUnavailable, commandRefusedMeanwhile)
			}
			var object struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			if answer.status == http.StatusOK && json.Unmarshal(answer.raw, &object) == nil && object.Metadata.ResourceVersion != "" {
				return object.Metadata.ResourceVersion
			}
			if answer.status != http.StatusServiceUnavailable {
				t.Fatalf("primary GET %s: status=%d key=%q message=%q", path, answer.status, answer.Error.Key, answer.Error.Message)
			}
			if time.Now().After(deadline) {
				t.Fatalf("primary GET %s: still 503 %s %q after ten seconds", path, answer.Error.Key, answer.Error.Message)
			}
			pause()
		}
	}
	before := version(false)
	for refused := 1; ; refused++ {
		answer := enrolledSend(t, c, http.MethodDelete, path, nil)
		if answer.status == http.StatusNoContent {
			return
		}
		if answer.status != http.StatusServiceUnavailable || answer.Error.Key != api.KeyUnavailable || answer.Error.Message != commandRefusedMeanwhile || answer.retryAfter != "" {
			t.Fatalf("primary DELETE %s: want 204, or 503 %s %q without Retry-After; got status=%d key=%q message=%q retry-after=%q",
				path, api.KeyUnavailable, commandRefusedMeanwhile, answer.status, answer.Error.Key, answer.Error.Message, answer.retryAfter)
		}
		if after := version(true); after != before {
			t.Fatalf("primary DELETE %s answered the effect-free 503 %s, but the object moved from resource version %s to %s", path, api.KeyUnavailable, before, after)
		}
		if time.Now().After(deadline) {
			t.Fatalf("primary DELETE %s: refused %d times in ten seconds (503 %s %q)", path, refused, api.KeyUnavailable, commandRefusedMeanwhile)
		}
		t.Logf("primary DELETE %s: refused (503 %s %q, no Retry-After) while the primary published its own commit; the object is unchanged at resource version %s, so the delete is sent again (refusal %d)",
			path, api.KeyUnavailable, commandRefusedMeanwhile, before, refused)
		pause()
	}
}
