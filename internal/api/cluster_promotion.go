package api

import (
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ClusterPromotionReview identifies the separately reviewed complete local image.
// It is a comparison operand, never proof of permission or promotion eligibility.
type ClusterPromotionReview struct {
	MemberID  string                 `json:"memberID" minLength:"36" maxLength:"36"`
	Installed replication.Generation `json:"installed"`
}

// ClusterPromotionInfo reports current local eligibility without granting it.
type ClusterPromotionInfo struct {
	ClusterPromotionReview
	Node               string  `json:"node"`
	Mode               string  `json:"mode"`
	Eligible           bool    `json:"eligible"`
	Reason             string  `json:"reason"`
	MissedPulls        int     `json:"missedPulls"`
	UnreachableSeconds float64 `json:"unreachableSeconds"`
}

// ClusterPromoteCommand carries one reviewed operator decision. Never replay it
// automatically after a lost response or refresh its source after a refusal.
type ClusterPromoteCommand struct {
	ClusterPromotionReview
	ConfirmNode string `json:"confirmNode" minLength:"1" maxLength:"63"`
	RequestID   string `json:"requestID" minLength:"1" maxLength:"256"`
	Reason      string `json:"reason" minLength:"1" maxLength:"512"`
}

// ClusterPromotionReceipt reports the confirmed durable transition. It does not
// assert that serving publication, writer work or remote convergence completed.
type ClusterPromotionReceipt struct {
	Committed bool             `json:"committed"`
	MemberID  string           `json:"memberID"`
	RequestID string           `json:"requestID"`
	Term      replication.Term `json:"term"`
}

// ClusterPromotionRuntime excludes unmounted, legacy and static runtimes.
type ClusterPromotionRuntime interface{ ClusterPromotionAvailable() bool }

func (s *Server) registerClusterPromotion() {
	for _, route := range []struct {
		method, path, id, summary, verb string
		request, response               any
	}{
		{http.MethodGet, "/cluster/promotion", "getClusterPromotion", "Review local follower promotion eligibility", "read", nil, ClusterPromotionInfo{}},
		{http.MethodPost, "/cluster/promote", "promoteCluster", "Promote an explicitly reviewed enrolled follower", "promote", ClusterPromoteCommand{}, ClusterPromotionReceipt{}},
	} {
		op := operation(route.method, route.path, route.id, route.summary, "cluster")
		op.BodyReadTimeout = 30 * time.Second
		op.Description = "Complete enrolled SQLite runtime only. Promotion requires current cluster.promote permission, cookie CSRF when applicable, the separately reviewed complete installed generation and member ID, exact local node confirmation, a request ID and an audit reason. The original request is bounded to thirty seconds including proof, body, queue and commit. Requires continuous primary unreachability across two pull intervals and agreement with the served image. Audit and promotion commit together. A lost or unknown response does not prove rollback; inspect current state before a new decision. No automatic refresh, redirect or replay. A committed receipt does not assert new-term publication or remote convergence."
		available := func() error {
			runtime, ok := s.opts.CanonicalSettings.(ClusterPromotionRuntime)
			if s.staticMode() || !ok || !runtime.ClusterPromotionAvailable() || s.opts.AuthHandler == nil {
				return newError(http.StatusServiceUnavailable, KeyUnavailable, "cluster promotion unavailable", "requires the complete enrolled SQLite runtime")
			}
			return nil
		}
		s.forwardWithCapability(op, perm{resource: "cluster", verb: route.verb}, route.request, route.response, available)
	}
}
