package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type clusterOutputRequestKey struct{}

// This middleware is INSIDE the serving lease adapter but BEFORE Huma can
// acquire a body. Source capture and original permission/CSRF admission therefore
// cannot be replaced after a slow or interposed body read.
func (h *servingHTTP) captureClusterOutput(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != h.opts.RoutePrefix+"/cluster/join-token" {
			next.ServeHTTP(w, r)
			return
		}
		capture := func() (*sqlite.CommandOutputRequest, error) {
			v, _, err := h.permission(r.Context(), "cluster", "join")
			if err != nil {
				return nil, err
			}
			proof, err := servinghttp.Credential(r.Context())
			if err != nil {
				return nil, err
			}
			if err := h.factorCSRF(r, v, proof); err != nil {
				return nil, err
			}
			if !h.opts.Runtime.ClusterEnrollmentAvailable() {
				return nil, &auth.Error{Status: 503, Key: "unavailable", Message: "cluster enrollment requires the complete nonstatic SQLite runtime"}
			}
			token, err := servinghttp.SourceToken(r.Context())
			if err != nil {
				return nil, err
			}
			if token.Authority().Mode != replication.Primary {
				return nil, clusterIssueConflict()
			}
			return h.opts.Runtime.opts.Store.CaptureCommandOutput(r.Context(), token)
		}
		request, err := capture()
		if err != nil {
			servingHTTPError(w, r, err)
			return
		}
		deadline, _ := r.Context().Deadline()
		bounded := &clusterBoundedWriter{ResponseWriter: w, ctx: r.Context(), deadline: deadline}
		if err := bounded.SetReadDeadline(deadline); err != nil {
			servingHTTPError(w, r, err)
			return
		}
		if err := bounded.SetWriteDeadline(deadline); err != nil {
			servingHTTPError(w, r, err)
			return
		}
		done := make(chan struct{})
		stop := context.AfterFunc(r.Context(), func() { _ = bounded.SetReadDeadline(time.Now()); close(done) })
		defer func() {
			if !stop() {
				<-done
			}
		}()
		next.ServeHTTP(bounded, r.WithContext(context.WithValue(r.Context(), clusterOutputRequestKey{}, request)))
	})
}

// Huma sets and clears relative body deadlines. Neither operation may extend
// this request's earlier absolute bound or undo a cancellation interrupt.
type clusterBoundedWriter struct {
	http.ResponseWriter
	ctx         context.Context
	deadline    time.Time
	mu          sync.Mutex
	read, write time.Time
}

func (w *clusterBoundedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *clusterBoundedWriter) setDeadline(at time.Time, read bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if at.IsZero() || w.deadline.Before(at) {
		at = w.deadline
	}
	previous := &w.write
	if read {
		previous = &w.read
	}
	if !previous.IsZero() && previous.Before(at) {
		at = *previous
	}
	if w.ctx.Err() != nil && time.Now().Before(at) {
		at = time.Now()
	}
	*previous = at
	c := http.NewResponseController(w.ResponseWriter)
	if read {
		return c.SetReadDeadline(at)
	}
	return c.SetWriteDeadline(at)
}
func (w *clusterBoundedWriter) SetReadDeadline(at time.Time) error  { return w.setDeadline(at, true) }
func (w *clusterBoundedWriter) SetWriteDeadline(at time.Time) error { return w.setDeadline(at, false) }

func clusterIssueConflict() error {
	return &auth.Error{Status: 409, Key: "conflict", Message: "cluster source changed or is not an enrolled primary; review enrollment before a new decision"}
}

func (h *servingHTTP) issueClusterJoinToken(w http.ResponseWriter, r *http.Request) error {
	request, ok := r.Context().Value(clusterOutputRequestKey{}).(*sqlite.CommandOutputRequest)
	if !ok || request == nil {
		return clusterIssueConflict()
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	token, err := servinghttp.SourceToken(r.Context())
	if err != nil {
		return err
	}
	if r.URL.RawQuery != "" {
		return servingHTTPInvalid("join-token does not accept query parameters")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return servingHTTPInvalid("invalid join-token command")
	}
	var in api.ClusterJoinTokenCommand
	if err := decodeReviewedCommandJSON(raw, &in, "invalid join-token command"); err != nil {
		return err
	}
	if !validClusterReview(in.ClusterEnrollmentReview) || in.Name == "" || len(in.Name) > 63 || in.Address == "" || len(in.Address) > 320 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 512 || in.ExpiresAt.IsZero() {
		return servingHTTPInvalid("exact reviewed identities, target name/address, absolute expiresAt and audit reason required")
	}
	a := token.Authority()
	if a.Mode != replication.Primary || a.Term.ClusterID != in.ClusterID || a.MemberID != in.MemberID || token.Generation().Digest != in.SourceDigest {
		return clusterIssueConflict()
	}
	s := h.opts.Runtime
	var invitation enrollment.Invitation
	var lifetime *servingauth.CredentialLifetime
	started := false
	committed, err := s.currentAuthCommandOutput(r.Context(), request, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		principal, decision, bound, err := servingauth.ReauthorizeCurrentForOutput(ctx, tx, deps, proof, servingHTTPCluster("cluster", "join"), s.opts.Now)
		if err != nil {
			return err
		}
		lifetime = bound
		owner, err := persistence.Load(ctx, tx)
		if err != nil {
			return err
		}
		if owner.State() == nil || owner.Digest() != in.OwnerDigest {
			return clusterIssueConflict()
		}
		original, err := persistence.Capture(ctx, owner, ctx.Err)
		if err != nil {
			return err
		}
		_, invitation, err = persistence.Issue(ctx, tx, original, in.Name, in.Address, in.ExpiresAt)
		if err != nil {
			return err
		}
		return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "cluster.join-token", Actor: string(principal.Kind) + ":" + principal.Name, Authz: decision.Path, Key: store.Key{Kind: "Cluster", Name: in.ClusterID}, Reason: in.Reason, RequestMethod: r.Method, RequestPath: r.URL.Path, ClientIP: h.clientKey(r)})
	}, func(scope context.Context) error {
		return lifetime.WithOutput(scope, func(output context.Context) error {
			// The invitation itself may expire before the original request.
			_, _, trust := invitation.IssuingPeer()
			until := in.ExpiresAt
			if trust.ExpiresAt().Before(until) {
				until = trust.ExpiresAt()
			}
			output, cancel := context.WithDeadline(output, until)
			defer cancel()
			if err := request.CheckOutput(output); err != nil {
				return err
			}
			wire, err := invitation.Encode()
			if err != nil {
				return err
			}
			body, err := json.Marshal(api.ClusterJoinTokenReceipt{Committed: true, Invitation: wire, ExpiresAt: in.ExpiresAt})
			if err != nil {
				return err
			}
			return emitClusterSecret(output, w, request, body, &started)
		})
	})
	if err != nil && started {
		panic(http.ErrAbortHandler)
	}
	if errors.Is(err, sqlite.ErrCommandCompletionUnknown) {
		return &auth.Error{Status: 503, Key: "completion_unknown", Message: "invitation completion is unknown; inspect enrollment before a new decision; do not replay this command"}
	}
	if err != nil && committed {
		return &auth.Error{Status: 503, Key: "committed_output_unavailable", Message: "invitation committed but its secret response is unavailable; do not replay this command"}
	}
	if errors.Is(err, replication.ErrCAS) || errors.Is(err, enrollment.ErrRefused) {
		return clusterIssueConflict()
	}
	if errors.Is(err, enrollment.ErrInvalid) {
		return servingHTTPInvalid("invalid target or absolute invitation expiry (maximum ten minutes)")
	}
	return err
}

// All writes, header/body flush and cancellation callback joining occur under
// the exact Store-issued output scope. No secret is emitted by deferred cleanup.
func emitClusterSecret(ctx context.Context, w http.ResponseWriter, request *sqlite.CommandOutputRequest, body []byte, started *bool) error {
	if err := request.CheckOutput(ctx); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(deadline); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = controller.SetWriteDeadline(time.Now()); close(done) })
	defer func() {
		if !stop() {
			<-done
		}
	}()
	if err := request.CheckOutput(ctx); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	*started = true
	w.WriteHeader(http.StatusOK)
	n, err := w.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	if err := request.CheckOutput(ctx); err != nil {
		return err
	}
	if err := controller.Flush(); err != nil {
		return err
	}
	return request.CheckOutput(ctx)
}
