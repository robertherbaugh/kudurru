package internalapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"
)

// PathHealth serves current operational readiness detail, never canonical objects.
const PathHealth = "/internal/health"

const (
	maxHealthBytes = 1 << 20
	healthTimeout  = 2 * time.Second
)

// Health client errors contain no peer addresses or response bodies.
var (
	ErrHealthUnsupported = errors.New("internalapi: peer health is unsupported")
	ErrHealthInvalid     = errors.New("internalapi: invalid peer health response")
	ErrHealthTooLarge    = errors.New("internalapi: peer health exceeds the size cap")
	ErrHealthIdentity    = errors.New("internalapi: peer health identity mismatch")
)

// Health is the peer's readiness and its original /healthz document.
type Health struct {
	Node   string         `json:"node"`
	Ready  bool           `json:"ready"`
	Health map[string]any `json:"health"`
}

// LocalHealth uses the same checker handler as the ops listener. A checker 503
// is a successful observation of an unhealthy node, not a transport failure.
func LocalHealth(ctx context.Context, node string, handler http.Handler) (Health, error) {
	out := Health{Node: node, Ready: true, Health: map[string]any{}}
	if err := ctx.Err(); err != nil {
		return Health{}, err
	}
	if handler == nil {
		return out, nil
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/healthz", nil))
	if rec.Body.Len() > maxHealthBytes {
		return Health{}, ErrHealthTooLarge
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		return Health{}, ErrHealthInvalid
	}
	out.Ready = rec.Code == http.StatusOK
	if err := json.Unmarshal(rec.Body.Bytes(), &out.Health); err != nil || out.Health == nil {
		return Health{}, ErrHealthInvalid
	}
	if err := ctx.Err(); err != nil {
		return Health{}, err
	}
	return out, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(healthTimeout))
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	out, err := s.opts.Health(ctx)
	if err != nil || ctx.Err() != nil {
		http.Error(w, "health unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := json.Marshal(out)
	if err != nil || len(body) > maxHealthBytes {
		http.Error(w, "health unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// Health fetches a trusted member's health with a two-second total deadline and
// a 1 MiB response cap. node must be its membership identity, not user input
// interpreted as an address. Redirects and public caller credentials are never
// forwarded by the internal transport.
func (c *Client) Health(ctx context.Context, addr, node string) (Health, error) {
	host, port, err := net.SplitHostPort(addr)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || host == "" || n < 1 || n > 65535 || strings.ContainsAny(host, "/?#@\\%") || node == "" {
		return Health{}, ErrHealthInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	resp, err := c.get(ctx, addr, PathHealth)
	if errors.Is(err, ErrListNotFound) {
		return Health{}, ErrHealthUnsupported
	}
	if err != nil {
		return Health{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		cert := resp.TLS.PeerCertificates[0]
		// Derived certificates carry the member identity. Configured certificates
		// may identify a shared service; those still use the envelope identity.
		if cert.Issuer.CommonName == "kudurru internal CA" && cert.Subject.CommonName != node {
			return Health{}, ErrHealthIdentity
		}
	}
	if resp.ContentLength > maxHealthBytes {
		return Health{}, ErrHealthTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBytes+1))
	if err != nil {
		return Health{}, err
	}
	if len(body) > maxHealthBytes {
		return Health{}, ErrHealthTooLarge
	}
	var wire struct {
		Node   string         `json:"node"`
		Ready  *bool          `json:"ready"`
		Health map[string]any `json:"health"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.Ready == nil || wire.Health == nil {
		return Health{}, ErrHealthInvalid
	}
	if wire.Node != node {
		return Health{}, ErrHealthIdentity
	}
	status := "not-ready"
	if *wire.Ready {
		status = "ready"
	}
	if components, ok := wire.Health["components"].(map[string]any); !ok || components == nil || wire.Health["status"] != status {
		return Health{}, ErrHealthInvalid
	}
	return Health{Node: wire.Node, Ready: *wire.Ready, Health: wire.Health}, nil
}
