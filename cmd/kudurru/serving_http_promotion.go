package main

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func (s *servingRuntime) ClusterPromotionAvailable() bool {
	return s != nil && s.ClusterEnrollmentAvailable() && s.member != nil
}

func promotionUnavailable() error {
	return &auth.Error{Status: 503, Key: "unavailable", Message: "cluster promotion requires the complete enrolled SQLite runtime"}
}
func promotionConflict() error {
	return &auth.Error{Status: 409, Key: "conflict", Message: "promotion source changed or follower is ineligible; review current state before a new decision"}
}

func (h *servingHTTP) getClusterPromotion(w http.ResponseWriter, r *http.Request) error {
	s := h.opts.Runtime
	if !s.ClusterPromotionAvailable() {
		return promotionUnavailable()
	}
	if r.URL.RawQuery != "" {
		return servingHTTPInvalid("promotion review does not accept query parameters")
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
	status := s.member.Status(r.Context())
	if !token.Matches(status.Authority) || !token.Generation().Same(status.Installed) {
		return promotionConflict()
	}
	if err := s.publisher.CheckReadLease(r.Context(), lease); err != nil {
		return err
	}
	return servingHTTPJSON(w, http.StatusOK, api.ClusterPromotionInfo{
		ClusterPromotionReview: api.ClusterPromotionReview{MemberID: status.Authority.MemberID, Installed: status.Installed},
		Node:                   s.member.NodeName(), Mode: string(status.Authority.Mode), Eligible: status.Eligible,
		Reason: status.Reason, MissedPulls: status.MissedPulls, UnreachableSeconds: status.UnreachableFor.Seconds(),
	})
}

func validPromotionCommand(in api.ClusterPromoteCommand) bool {
	id, err := uuid.Parse(in.MemberID)
	return err == nil && id != uuid.Nil && id.String() == in.MemberID && in.Installed.Validate() == nil &&
		in.ConfirmNode != "" && len(in.ConfirmNode) <= 63 && strings.TrimSpace(in.RequestID) != "" && len(in.RequestID) <= 256 &&
		strings.TrimSpace(in.Reason) != "" && len(in.Reason) <= 512
}

func (h *servingHTTP) promoteCluster(w http.ResponseWriter, r *http.Request) error {
	s := h.opts.Runtime
	if !s.ClusterPromotionAvailable() {
		return promotionUnavailable()
	}
	v, _, err := h.permission(r.Context(), "cluster", "promote")
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
		return servingHTTPInvalid("promotion does not accept query parameters")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		return servingHTTPInvalid("invalid cluster promotion command")
	}
	var in api.ClusterPromoteCommand
	if err := decodeReviewedCommandJSON(raw, &in, "invalid cluster promotion command"); err != nil {
		return err
	}
	if !validPromotionCommand(in) {
		return servingHTTPInvalid("exact reviewed member/generation, node confirmation, request ID and audit reason required")
	}
	if in.MemberID != token.Authority().MemberID || !in.Installed.Same(token.Generation()) {
		return promotionConflict()
	}
	result, err := s.member.Promote(r.Context(), clusterreplica.PromoteRequest{ConfirmNode: in.ConfirmNode,
		ExpectedTerm: in.Installed.Term, ExpectedInstalled: in.Installed, RequestID: in.RequestID, Reason: in.Reason})
	if errors.Is(err, sqlite.ErrPromotionCompletionUnknown) {
		return &auth.Error{Status: 503, Key: "completion_unknown", Message: "promotion completion is unknown; inspect current state before a new decision; do not replay this command"}
	}
	if errors.Is(err, clusterreplica.ErrIneligible) || errors.Is(err, replication.ErrCAS) || errors.Is(err, replication.ErrFenced) {
		return promotionConflict()
	}
	if err != nil {
		return err
	}
	s.publisher.Wake()
	if err := r.Context().Err(); err != nil {
		return err
	}
	return servingHTTPJSON(w, http.StatusOK, api.ClusterPromotionReceipt{Committed: true, MemberID: result.MemberID, RequestID: in.RequestID, Term: result.Term})
}
