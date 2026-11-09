package sqlite

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// ErrPromotionCompletionUnknown means the driver did not confirm COMMIT. It
// grants neither success nor rollback, and never permits automatic replay.
var ErrPromotionCompletionUnknown = errors.New("store/sqlite: promotion completion unknown")

// PromoteAudited retains the original operator check through the existing
// installed-generation CAS and commits attributed audit with the authority
// transition. Root must hold the original publication and all policy dependencies
// through completion. A duplicate confirmed intent rechecks authority but emits
// no second audit event. check must use only pinned memory and the actual clock.
func (s *Store) PromoteAudited(ctx context.Context, req replication.PromoteRequest, check func(context.Context) error, attribution AuditEntry) (replication.Authority, error) {
	if ctx == nil || check == nil || strings.TrimSpace(attribution.Actor) == "" || len(attribution.Actor) > 1024 ||
		strings.TrimSpace(attribution.Reason) == "" || len(attribution.Reason) > 512 || attribution.Authz == "" {
		return replication.Authority{}, replication.ErrInvalid
	}
	deadline, finite := ctx.Deadline()
	if !finite || time.Until(deadline) > 30*time.Second {
		return replication.Authority{}, replication.ErrInvalid
	}
	// Never inherit caller-supplied audit hashes, documents, results or times.
	audit := AuditEntry{Actor: attribution.Actor, Authz: attribution.Authz, Reason: attribution.Reason,
		ClientIP: attribution.ClientIP, UserAgent: attribution.UserAgent,
		RequestMethod: attribution.RequestMethod, RequestPath: attribution.RequestPath,
		Action: "cluster.promote", RequestID: req.RequestID, Result: AuditSuccess,
		Key: store.Key{Kind: "Cluster", Name: req.ExpectedTerm.ClusterID}}
	return s.promote(ctx, req, check, &audit)
}
