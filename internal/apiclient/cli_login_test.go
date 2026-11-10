package apiclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// The provider list keeps the stable refusal key of a server without public CLI
// login, so the CLI can explain it, and still drops the server's own text.
func TestCLILoginProvidersKeepsUnavailableKeyWithoutServerText(t *testing.T) {
	const reflected = "sensitive-proof-marker"
	var calls atomic.Int32
	c, err := New("https://fixture.invalid", Options{HTTPClient: &http.Client{Transport: totpTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		body := `{"error":{"key":"cli_login_unavailable","message":"` + reflected + `","hint":"` + reflected + `"}}`
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	providers, err := c.CLILoginProviders(t.Context())
	var refusal *Error
	if !errors.As(err, &refusal) || !IsKey(err, "cli_login_unavailable") || refusal.StatusCode != http.StatusServiceUnavailable || len(providers) != 0 {
		t.Fatal("stable refusal key lost", err)
	}
	if strings.Contains(err.Error(), reflected) || refusal.Hint != "" || calls.Load() != 1 {
		t.Fatal("server text retained or request repeated")
	}
}
