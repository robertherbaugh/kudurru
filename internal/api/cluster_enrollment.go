package api

import (
	"net/http"
)

// ClusterEnrollmentReview is an explicit observation for operator review. Its
// digests are comparison operands, never membership or authorization grants.
type ClusterEnrollmentReview struct {
	ClusterID    string `json:"clusterID" minLength:"36" maxLength:"36"`
	MemberID     string `json:"memberID" minLength:"36" maxLength:"36"`
	SourceDigest string `json:"sourceDigest" minLength:"64" maxLength:"64"`
	OwnerDigest  string `json:"ownerDigest" minLength:"64" maxLength:"64"`
}

// ClusterEnrollmentInfo describes the current complete local enrollment owner.
type ClusterEnrollmentInfo struct {
	ClusterEnrollmentReview
	Mode    string `json:"mode"`
	Dormant bool   `json:"dormant"`
}

// ActivateClusterCommand carries a separately reviewed source and local member
// intent. A refused request is never automatically refreshed or replayed.
type ActivateClusterCommand struct {
	ClusterEnrollmentReview
	Name    string `json:"name" minLength:"1" maxLength:"63"`
	Address string `json:"address" minLength:"1" maxLength:"320"`
	Reason  string `json:"reason" minLength:"1" maxLength:"512"`
}

// ClusterActivationReceipt reports actual durable completion, not listener,
// publication, peer connectivity or quorum readiness. No secret is returned.
type ClusterActivationReceipt struct {
	Committed       bool   `json:"committed"`
	ClusterID       string `json:"clusterID"`
	MemberID        string `json:"memberID"`
	RestartRequired bool   `json:"restartRequired"`
}

// ClusterEnrollmentRuntime excludes legacy, central and Kubernetes fallbacks.
type ClusterEnrollmentRuntime interface{ ClusterEnrollmentAvailable() bool }

func (s *Server) registerClusterEnrollment() {
	for _, route := range []struct {
		method, path, id, summary, verb string
		request, response               any
	}{
		{http.MethodGet, "/cluster/enrollment", "getClusterEnrollment", "Review the complete local enrollment source", "read", nil, ClusterEnrollmentInfo{}},
		{http.MethodPost, "/cluster/activate", "activateCluster", "Activate an explicitly reviewed standalone SQLite primary", "join", ActivateClusterCommand{}, ClusterActivationReceipt{}},
	} {
		op := operation(route.method, route.path, route.id, route.summary, "cluster")
		op.Description = "Complete nonstatic SQLite runtime only. Activation requires current cluster.join authority, cookie CSRF when applicable, and exact separately reviewed cluster/member/source/owner identities. One original 30-second request; no automatic refresh or retry. A confirmed commit requires process restart to mount the enrolled member listener. A lost or unknown response does not prove rollback; inspect enrollment before any new decision."
		available := func() error {
			runtime, ok := s.opts.CanonicalSettings.(ClusterEnrollmentRuntime)
			if s.staticMode() || !ok || !runtime.ClusterEnrollmentAvailable() || s.opts.AuthHandler == nil {
				return newError(http.StatusServiceUnavailable, KeyUnavailable, "cluster enrollment unavailable", "requires the complete nonstatic SQLite runtime")
			}
			return nil
		}
		s.forwardWithCapability(op, perm{resource: "cluster", verb: route.verb}, route.request, route.response, available)
	}
}
