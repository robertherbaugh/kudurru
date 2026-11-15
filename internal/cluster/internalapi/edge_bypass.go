package internalapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PathEdgeBypass is a bounded observation of an installed managed edge policy.
// It grants no backend activation or canonical mutation authority.
const PathEdgeBypass = "/internal/edge/managed-bypass"

const (
	// MaxEdgeBypassBackends bounds one installed membership observation.
	MaxEdgeBypassBackends = 128
	maxEdgeBypassBytes    = 128 << 10
	edgeBypassTimeout     = 2 * time.Second
)

// EdgeBypassBackend is detached membership from the same installed edge cut.
// None of its fields are instructions for the receiver to dial an endpoint.
type EdgeBypassBackend struct {
	Name            string `json:"name"`
	Address         string `json:"address"`
	InternalAddress string `json:"internalAddress"`
}

// EdgeBypassState separates durable deployment policy from one running child
// and its installed membership. A recipient must retain its original attempt,
// configured deployment/node identities and complete local activation checks.
// These public data fields cannot mint a local activation permit.
type EdgeBypassState struct {
	Version     int                 `json:"version"`
	Node        string              `json:"node"`
	Deployment  string              `json:"deployment"`
	Run         string              `json:"run"`
	SourceEpoch uint64              `json:"sourceEpoch,string"`
	ChildEpoch  uint64              `json:"childEpoch,string"`
	Backends    []EdgeBypassBackend `json:"backends"`
	Challenge   string              `json:"challenge"`
}

// Managed edge observation errors distinguish absent capability, invalid input
// and unavailable installed state without exposing private peer diagnostics.
var (
	ErrEdgeBypassUnsupported = errors.New("internalapi: managed edge observation is unsupported")
	ErrEdgeBypassInvalid     = errors.New("internalapi: invalid managed edge observation")
	ErrEdgeBypassUnavailable = errors.New("internalapi: managed edge observation is unavailable")
)

func nonzeroEdgeDigest(s string) bool { return ValidDigest(s) && s != strings.Repeat("0", 64) }

func edgeBypassAddress(s string) bool {
	if len(s) > 512 {
		return false
	}
	host, port, err := net.SplitHostPort(s)
	n, portErr := strconv.Atoi(port)
	return err == nil && portErr == nil && host != "" && n > 0 && n <= 65535 &&
		!strings.ContainsAny(host, "/?#@\\% \t\r\n")
}

func validEdgeBypass(s EdgeBypassState) bool {
	if s.Version != 1 || s.Node == "" || len(s.Node) > 256 ||
		!nonzeroEdgeDigest(s.Deployment) || !nonzeroEdgeDigest(s.Run) || !nonzeroEdgeDigest(s.Challenge) ||
		s.SourceEpoch == 0 || s.ChildEpoch == 0 || len(s.Backends) == 0 || len(s.Backends) > MaxEdgeBypassBackends {
		return false
	}
	addresses, names := map[string]bool{}, map[string]bool{}
	for _, b := range s.Backends {
		if len(b.Name) > 256 || !edgeBypassAddress(b.Address) ||
			b.InternalAddress != "" && !edgeBypassAddress(b.InternalAddress) ||
			addresses[b.Address] || b.Name != "" && names[b.Name] {
			return false
		}
		addresses[b.Address], names[b.Name] = true, true
	}
	return true
}

func (s *Server) handleEdgeBypass(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 2 || len(query["challenge"]) != 1 || len(query["deployment"]) != 1 ||
		!nonzeroEdgeDigest(query.Get("challenge")) || !nonzeroEdgeDigest(query.Get("deployment")) {
		http.Error(w, "invalid managed edge request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), edgeBypassTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	out, err := s.opts.EdgeBypass(ctx, query.Get("deployment"))
	if err != nil || ctx.Err() != nil {
		http.Error(w, "managed edge unavailable", http.StatusServiceUnavailable)
		return
	}
	out.Node, out.Version, out.Challenge = s.opts.Credentials.nodeName, 1, query.Get("challenge")
	if out.Deployment != query.Get("deployment") || !validEdgeBypass(out) {
		http.Error(w, "managed edge unavailable", http.StatusServiceUnavailable)
		return
	}
	body, err := json.Marshal(out)
	if err != nil || len(body) > maxEdgeBypassBytes || ctx.Err() != nil {
		http.Error(w, "managed edge unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// EdgeBypass observes one operator-configured peer over authenticated TLS. The
// challenge is generated before the first request; token rotation keeps the
// same challenge and the same original total deadline. No retry is made after
// a response, source mismatch, parse error or expiry. Returned data is evidence
// for the caller's original attempt, not a reusable/renewable activation grant.
func (c *Client) EdgeBypass(ctx context.Context, addr, node, deployment string) (EdgeBypassState, error) {
	if !edgeBypassAddress(addr) || node == "" || len(node) > 256 || !nonzeroEdgeDigest(deployment) {
		return EdgeBypassState{}, ErrEdgeBypassInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, edgeBypassTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return EdgeBypassState{}, err
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return EdgeBypassState{}, err
	}
	challenge := hex.EncodeToString(nonce[:])
	resp, err := c.get(ctx, addr, PathEdgeBypass+"?deployment="+deployment+"&challenge="+challenge)
	if errors.Is(err, ErrListNotFound) {
		return EdgeBypassState{}, ErrEdgeBypassUnsupported
	}
	if err != nil {
		return EdgeBypassState{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		cert := resp.TLS.PeerCertificates[0]
		if cert.Issuer.CommonName == "kudurru internal CA" && cert.Subject.CommonName != node {
			return EdgeBypassState{}, ErrEdgeBypassInvalid
		}
	}
	if resp.ContentLength > maxEdgeBypassBytes {
		return EdgeBypassState{}, ErrTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxEdgeBypassBytes+1))
	if err != nil {
		return EdgeBypassState{}, err
	}
	if len(body) > maxEdgeBypassBytes {
		return EdgeBypassState{}, ErrTooLarge
	}
	var out EdgeBypassState
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil || decoder.Decode(new(any)) != io.EOF || !validEdgeBypass(out) ||
		out.Deployment != deployment || out.Node != node || out.Challenge != challenge {
		return EdgeBypassState{}, ErrEdgeBypassInvalid
	}
	if err := ctx.Err(); err != nil {
		return EdgeBypassState{}, err
	}
	return out, nil
}
