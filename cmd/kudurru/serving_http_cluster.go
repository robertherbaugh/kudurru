package main

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func (s *servingRuntime) ClusterEnrollmentAvailable() bool {
	return s != nil && s.opts.Store != nil && s.opts.Config.Cluster.Role != "static"
}

// Install the original bound before lease capture, proof verification and body
// acquisition. The handler never starts a replacement deadline after decoding.
func (h *servingHTTP) clusterEnrollmentLifetime(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == h.opts.RoutePrefix+"/dns/blocking" || r.URL.Path == h.opts.RoutePrefix+"/cluster/promotion" || r.URL.Path == h.opts.RoutePrefix+"/cluster/promote" || r.URL.Path == h.opts.RoutePrefix+"/cluster/activate" || r.URL.Path == h.opts.RoutePrefix+"/cluster/enrollment" || r.URL.Path == h.opts.RoutePrefix+"/cluster/anchors" || r.URL.Path == h.opts.RoutePrefix+"/cluster/join-token" {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

func (h *servingHTTP) registerClusterEnrollment() {
	h.route(http.MethodGet, "/cluster/promotion", h.getClusterPromotion)
	h.route(http.MethodPost, "/cluster/promote", h.promoteCluster)
	h.route(http.MethodGet, "/cluster/enrollment", h.getClusterEnrollment)
	h.route(http.MethodGet, "/cluster/anchors", h.getClusterAnchors)
	h.route(http.MethodPost, "/cluster/activate", h.activateCluster)
	h.route(http.MethodPost, "/cluster/join-token", h.issueClusterJoinToken)
}

func clusterReviewConflict() error {
	return &auth.Error{Status: http.StatusConflict, Key: "conflict", Message: "cluster source changed or is not dormant standalone; review enrollment before a new decision"}
}

func (h *servingHTTP) getClusterEnrollment(w http.ResponseWriter, r *http.Request) error {
	if !h.opts.Runtime.ClusterEnrollmentAvailable() {
		return &auth.Error{Status: http.StatusServiceUnavailable, Key: "unavailable", Message: "cluster enrollment requires the complete nonstatic SQLite runtime", NoRetryAfter: true,
			Hint: "enrollment needs a SQLite node that is not static; see docs/deploy/sqlite-cluster.md"}
	}
	if r.URL.RawQuery != "" {
		return servingHTTPInvalid("enrollment review does not accept query parameters")
	}
	if _, _, err := h.permission(r.Context(), "cluster", "read"); err != nil {
		return err
	}
	lease, err := servinghttp.CloneLease(r.Context())
	if err != nil {
		return err
	}
	defer lease.Release()
	token, err := lease.SourceToken()
	if err != nil {
		return err
	}
	s := h.opts.Runtime
	if err := s.publisher.CheckReadLease(r.Context(), lease); err != nil {
		return err
	}
	a, owner, err := s.opts.Store.ReadMemberEnrollment(r.Context())
	if err != nil {
		return err
	}
	if !token.Matches(a) {
		return clusterReviewConflict()
	}
	if err := s.publisher.CheckReadLease(r.Context(), lease); err != nil {
		return err
	}
	return servingHTTPJSON(w, http.StatusOK, api.ClusterEnrollmentInfo{
		ClusterEnrollmentReview: api.ClusterEnrollmentReview{ClusterID: owner.ClusterID(), MemberID: a.MemberID, SourceDigest: token.Generation().Digest, OwnerDigest: owner.Digest()},
		Mode:                    string(a.Mode), Dormant: owner.State() == nil,
	})
}

func validClusterReview(q api.ClusterEnrollmentReview) bool {
	for _, value := range []string{q.ClusterID, q.MemberID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return false
		}
	}
	for _, value := range []string{q.SourceDigest, q.OwnerDigest} {
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != value {
			return false
		}
	}
	return true
}

// Local identity I/O follows the original permission and exact dormant-source
// review, outside Command. The command independently reauthorizes and rechecks
// that source. A prior failed attempt may leave this file; retain its exact
// identity and never replace or repair it. This file alone grants no membership.
func activationIdentity(path, member string) (*memberidentity.Identity, error) {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return memberidentity.Create(path, member)
	} else if err != nil {
		return nil, err
	}
	return memberidentity.Load(path, member)
}

func (h *servingHTTP) activateCluster(w http.ResponseWriter, r *http.Request) error {
	s := h.opts.Runtime
	if !s.ClusterEnrollmentAvailable() {
		return &auth.Error{Status: http.StatusServiceUnavailable, Key: "unavailable", Message: "cluster enrollment requires the complete nonstatic SQLite runtime", NoRetryAfter: true,
			Hint: "enrollment needs a SQLite node that is not static; see docs/deploy/sqlite-cluster.md"}
	}
	v, _, err := h.permission(r.Context(), "cluster", "join")
	if err != nil {
		return err
	}
	proof, err := servinghttp.Credential(r.Context())
	if err != nil {
		return err
	}
	if err := h.factorCSRF(r, v, proof); err != nil {
		return err
	}
	token, err := servinghttp.SourceToken(r.Context())
	if err != nil {
		return err
	}
	if r.URL.RawQuery != "" {
		return servingHTTPInvalid("activation does not accept query parameters")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return servingHTTPInvalid("invalid cluster activation command")
	}
	var in api.ActivateClusterCommand
	if err := decodeReviewedCommandJSON(raw, &in, "invalid cluster activation command"); err != nil {
		return err
	}
	if !validClusterReview(in.ClusterEnrollmentReview) || len(in.Name) == 0 || len(in.Name) > 63 || len(in.Address) == 0 || len(in.Address) > 320 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 512 {
		return servingHTTPInvalid("exact reviewed source, member name/address and audit reason required")
	}
	a := token.Authority()
	if a.Mode != replication.Standalone || in.ClusterID != a.Term.ClusterID || in.MemberID != a.MemberID || in.SourceDigest != token.Generation().Digest {
		return clusterReviewConflict()
	}
	// Filesystem effects cannot run inside a Command callback. Review the same
	// original publication and complete owner before creating a non-granting
	// private identity; the actual command below independently repeats admission.
	lease, err := servinghttp.CloneLease(r.Context())
	if err != nil {
		return err
	}
	defer lease.Release()
	actual, owner, err := s.opts.Store.ReadMemberEnrollment(r.Context())
	if err != nil {
		return err
	}
	if !token.Matches(actual) || owner.State() != nil || owner.Digest() != in.OwnerDigest {
		return clusterReviewConflict()
	}
	if err := s.publisher.CheckReadLease(r.Context(), lease); err != nil {
		return err
	}
	identity, err := activationIdentity(filepath.Join(s.opts.Config.Node.DataDir, memberIdentityFile), a.MemberID)
	if err != nil {
		return err
	}
	err = s.currentAuthCommand(r.Context(), func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		principal, decision, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, servingHTTPCluster("cluster", "join"), s.opts.Now)
		if err != nil {
			return err
		}
		owner, err := persistence.Load(ctx, tx)
		if err != nil {
			return err
		}
		if owner.State() != nil || owner.Digest() != in.OwnerDigest || !token.Matches(s.opts.Store.Authority()) {
			return clusterReviewConflict()
		}
		if err := tx.StageEnrollmentPrimary(ctx, token, identity, in.Name, in.Address); err != nil {
			return err
		}
		return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "cluster.activate", Actor: string(principal.Kind) + ":" + principal.Name, Authz: decision.Path, Key: store.Key{Kind: "Cluster", Name: in.ClusterID}, Reason: in.Reason, RequestMethod: r.Method, RequestPath: r.URL.Path, ClientIP: h.clientKey(r)})
	})
	if errors.Is(err, sqlite.ErrEnrollmentCompletionUnknown) {
		return &auth.Error{Status: http.StatusServiceUnavailable, Key: "completion_unknown", Message: "activation completion is unknown; inspect retained enrollment before a new decision; do not replay this command"}
	}
	if errors.Is(err, replication.ErrCAS) || errors.Is(err, enrollment.ErrRefused) {
		return clusterReviewConflict()
	}
	if errors.Is(err, enrollment.ErrInvalid) {
		return servingHTTPInvalid("invalid member name or internal host:port address")
	}
	if err != nil {
		return err
	}
	s.publisher.Wake()
	// This public receipt reports only the completed effect. The old writer is
	// intentionally fenced; no late capture or readiness claim rescues it.
	if err := r.Context().Err(); err != nil {
		return err
	}
	return servingHTTPJSON(w, http.StatusOK, api.ClusterActivationReceipt{Committed: true, ClusterID: in.ClusterID, MemberID: in.MemberID, RestartRequired: true})
}
