package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
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
		t.Fatal("enrolled request failed", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	answer := enrolledAnswer{status: response.StatusCode, retryAfter: response.Header.Get("Retry-After")}
	if answer.raw, err = io.ReadAll(io.LimitReader(response.Body, 128<<10)); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(answer.raw, &answer)
	return answer
}
