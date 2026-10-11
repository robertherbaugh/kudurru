package dnsdist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// APIClient talks to dnsdist's built-in webserver (X-API-Key). Only the
// read endpoints and the cache flush are used: the API cannot manage
// backends (docs/research/dnsdist-keepalived.md §1.12).
type APIClient struct {
	// BaseURL is http://host:port of the webserver.
	BaseURL string
	// Key is the API key; when empty, KeyFile is read on every request
	// (the key changes on every dnsdist start).
	Key     string
	KeyFile string
	// HTTP is the client; default a 2 s timeout client.
	HTTP *http.Client
}

// BackendStatus is one entry of servers[] in /api/v1/servers/localhost.
type BackendStatus struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Address     string   `json:"address"`
	State       string   `json:"state"`
	Pools       []string `json:"pools"`
	Queries     float64  `json:"queries"`
	Outstanding float64  `json:"outstanding"`
	Latency     float64  `json:"latency"`
}

// Up reports whether dnsdist considers the backend usable: "up" (health
// checked) or "UP" (forced).
func (b BackendStatus) Up() bool { return strings.EqualFold(b.State, "up") }

// PoolStatus is one entry of pools[].
type PoolStatus struct {
	Name         string  `json:"name"`
	ServersCount float64 `json:"serversCount"`
	CacheSize    float64 `json:"cacheSize"`
	CacheEntries float64 `json:"cacheEntries"`
	CacheHits    float64 `json:"cacheHits"`
	CacheMisses  float64 `json:"cacheMisses"`
}

// ServerStatus is the subset of GET /api/v1/servers/localhost kudurru
// reads (the `/edge` page, readiness and the VRRP probe).
type ServerStatus struct {
	DaemonType string          `json:"daemon_type"`
	Version    string          `json:"version"`
	Servers    []BackendStatus `json:"servers"`
	Pools      []PoolStatus    `json:"pools"`
}

// UpCount returns the number of backends that are up.
func (s *ServerStatus) UpCount() int {
	n := 0
	for _, b := range s.Servers {
		if b.Up() {
			n++
		}
	}
	return n
}

// ErrUnauthorized is returned when dnsdist rejects the API key (it changed
// with a restart the caller has not seen yet).
var ErrUnauthorized = errors.New("dnsdist: API key rejected")

func (c *APIClient) key() (string, error) {
	if c.Key != "" {
		return c.Key, nil
	}
	if c.KeyFile == "" {
		return "", errors.New("dnsdist: no API key or key file")
	}
	b, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return "", fmt.Errorf("dnsdist: API key: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *APIClient) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 2 * time.Second}
}

func (c *APIClient) do(ctx context.Context, method, path string, q url.Values) (*http.Response, error) {
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	u := strings.TrimSuffix(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		_ = resp.Body.Close()
		return nil, ErrUnauthorized
	case resp.StatusCode >= 300:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("dnsdist: %s %s: %s %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

// Status fetches GET /api/v1/servers/localhost.
func (c *APIClient) Status(ctx context.Context) (*ServerStatus, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/v1/servers/localhost", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var st ServerStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("dnsdist: decode server status: %w", err)
	}
	return &st, nil
}

// FlushCache expunges name (and, with suffix, everything below it) from
// the packet cache of pool: DELETE /api/v1/cache, allowed even when the
// API is read-only.
func (c *APIClient) FlushCache(ctx context.Context, pool, name string, suffix bool) error {
	q := url.Values{"pool": {pool}, "name": {name}}
	if suffix {
		q.Set("suffix", "true")
	}
	resp, err := c.do(ctx, http.MethodDelete, "/api/v1/cache", q)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.Body.Close()
}
