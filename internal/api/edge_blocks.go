package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/edgeblock"
)

// EdgeBlock is one operator-initiated edge block (D388): every dnsdist edge
// drops queries from Prefix until Until.
type EdgeBlock struct {
	ID        string    `json:"id" doc:"Block ID (16 lowercase hex characters)."`
	Prefix    string    `json:"prefix" doc:"Blocked client prefix, canonical CIDR."`
	CreatedAt time.Time `json:"createdAt"`
	Until     time.Time `json:"until" doc:"Absolute expiry; edges stop dropping at this time."`
	CreatedBy string    `json:"createdBy,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// EdgeBlockList is GET /edge/blocks.
type EdgeBlockList struct {
	Items []EdgeBlock `json:"items" nullable:"false"`
	// Revision increases with every change of the stored set.
	Revision string `json:"revision"`
	// MaxBlocks is the bound on active blocks.
	MaxBlocks int `json:"maxBlocks"`
	// Limits a client needs to build a valid request.
	MinSeconds  int64 `json:"minSeconds"`
	MaxSeconds  int64 `json:"maxSeconds"`
	MinIPv4Bits int   `json:"minIPv4Bits"`
	MinIPv6Bits int   `json:"minIPv6Bits"`
}

// EdgeBlockRequest is POST /edge/blocks. Exactly one of seconds and until.
type EdgeBlockRequest struct {
	Prefix  string     `json:"prefix" maxLength:"64" doc:"Client address or CIDR prefix (IPv4 /16 or longer, IPv6 /32 or longer)."`
	Seconds *int64     `json:"seconds,omitempty" doc:"Block for this many seconds (60 to 604800)."`
	Until   *time.Time `json:"until,omitempty" doc:"Block until this time (one minute to seven days from now)."`
	Reason  string     `json:"reason,omitempty" maxLength:"256" doc:"Operator note kept in the list and the audit trail; never sent to edges."`
}

// EdgeBlockChange is the detached create command.
type EdgeBlockChange struct {
	Prefix   string
	Duration time.Duration
	Until    time.Time
	Reason   string
}

// EdgeBlocks stores the canonical edge block set. Create and Revoke admit,
// change and audit in one driver command; a returned error that carries a
// committed hint is not a rollback.
type EdgeBlocks interface {
	ListEdgeBlocks(context.Context) (*EdgeBlockList, error)
	CreateEdgeBlock(context.Context, EdgeBlockChange) (*EdgeBlock, error)
	RevokeEdgeBlock(context.Context, string) (*EdgeBlock, error)
}

// EdgeBlockFromStored converts a stored block.
func EdgeBlockFromStored(b edgeblock.Block) EdgeBlock {
	return EdgeBlock{ID: b.ID, Prefix: b.Prefix, CreatedAt: b.CreatedAt, Until: b.Until, CreatedBy: b.CreatedBy, Reason: b.Reason}
}

// EdgeBlockListFromState projects the active blocks of a stored state.
func EdgeBlockListFromState(s edgeblock.State, now time.Time) *EdgeBlockList {
	out := &EdgeBlockList{Items: []EdgeBlock{}, Revision: strconv.FormatUint(s.Revision, 10), MaxBlocks: edgeblock.MaxBlocks,
		MinSeconds: int64(edgeblock.MinDuration / time.Second), MaxSeconds: int64(edgeblock.MaxDuration / time.Second),
		MinIPv4Bits: edgeblock.MinIPv4Bits, MinIPv6Bits: edgeblock.MinIPv6Bits}
	for _, b := range s.Active(now) {
		out.Items = append(out.Items, EdgeBlockFromStored(b))
	}
	return out
}

// EdgeBlockError maps the model's errors onto the envelope.
func EdgeBlockError(err error) error {
	var apiErr *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &apiErr):
		return apiErr
	case errors.Is(err, edgeblock.ErrInvalid):
		return newError(http.StatusBadRequest, KeyInvalid, err.Error(), "")
	case errors.Is(err, edgeblock.ErrExists), errors.Is(err, edgeblock.ErrBounds):
		return errConflict(err.Error(), "revoke the existing or an expiring block first")
	case errors.Is(err, edgeblock.ErrNotFound):
		return errNotFound("active edge block")
	case errors.Is(err, edgeblock.ErrState):
		return errUnavailable("the stored edge block set is invalid")
	}
	return err
}

type edgeBlockListOutput struct{ Body *EdgeBlockList }
type edgeBlockOutput struct{ Body *EdgeBlock }
type edgeBlockCreateInput struct{ Body EdgeBlockRequest }
type edgeBlockIDInput struct {
	ID string `path:"id" maxLength:"16" pattern:"^[0-9a-f]{16}$"`
}

func (s *Server) registerEdgeBlocks() {
	register(s, operation(http.MethodGet, "/edge/blocks", "listEdgeBlocks", "Active operator edge blocks", "cluster"),
		perm{resource: "edgeblocks", verb: "read"}, s.listEdgeBlocks)
	create := operation(http.MethodPost, "/edge/blocks", "createEdgeBlock", "Block a client prefix at every dnsdist edge until an expiry", "cluster")
	create.Description = "Stores the block in the canonical set and audits it. Edge supervisors pull the set from every backend about every five seconds and apply it with dnsdist's dynamic-block table, which also enforces the expiry; the response does not wait for edges."
	create.DefaultStatus = http.StatusCreated
	register(s, create, perm{resource: "edgeblocks", verb: "write"}, s.createEdgeBlock)
	register(s, operation(http.MethodDelete, "/edge/blocks/{id}", "revokeEdgeBlock", "Revoke an active edge block", "cluster"),
		perm{resource: "edgeblocks", verb: "write"}, s.revokeEdgeBlock)
}

func (s *Server) listEdgeBlocks(ctx context.Context, _ *struct{}) (*edgeBlockListOutput, error) {
	if s.opts.EdgeBlocks == nil {
		return nil, errNotImplemented("operator edge blocks")
	}
	out, err := s.opts.EdgeBlocks.ListEdgeBlocks(ctx)
	if err != nil {
		return nil, EdgeBlockError(err)
	}
	return &edgeBlockListOutput{Body: out}, nil
}

func (s *Server) createEdgeBlock(ctx context.Context, in *edgeBlockCreateInput) (*edgeBlockOutput, error) {
	if s.opts.EdgeBlocks == nil {
		return nil, errNotImplemented("operator edge blocks")
	}
	change := EdgeBlockChange{Prefix: in.Body.Prefix, Reason: in.Body.Reason}
	switch {
	case in.Body.Seconds != nil && in.Body.Until != nil:
		return nil, errInvalid("give seconds or until, not both")
	case in.Body.Seconds != nil:
		secs := *in.Body.Seconds
		if secs < int64(edgeblock.MinDuration/time.Second) || secs > int64(edgeblock.MaxDuration/time.Second) {
			return nil, errInvalid("seconds must be between %d and %d", int64(edgeblock.MinDuration/time.Second), int64(edgeblock.MaxDuration/time.Second))
		}
		change.Duration = time.Duration(secs) * time.Second
	case in.Body.Until != nil:
		change.Until = *in.Body.Until
	default:
		return nil, errInvalid("seconds or until is required")
	}
	block, err := s.opts.EdgeBlocks.CreateEdgeBlock(ctx, change)
	if s.opts.AuditEdgeBlocks {
		s.recordAction(ctx, "edgeblock.create", audit.Resource{Kind: "EdgeBlock", Name: auditBlockName(block)}, err, nil, auditBlock(block))
	}
	if err != nil {
		return nil, EdgeBlockError(err)
	}
	return &edgeBlockOutput{Body: block}, nil
}

func (s *Server) revokeEdgeBlock(ctx context.Context, in *edgeBlockIDInput) (*edgeBlockOutput, error) {
	if s.opts.EdgeBlocks == nil {
		return nil, errNotImplemented("operator edge blocks")
	}
	block, err := s.opts.EdgeBlocks.RevokeEdgeBlock(ctx, in.ID)
	if s.opts.AuditEdgeBlocks {
		s.recordAction(ctx, "edgeblock.revoke", audit.Resource{Kind: "EdgeBlock", Name: in.ID}, err, auditBlock(block), nil)
	}
	if err != nil {
		return nil, EdgeBlockError(err)
	}
	return &edgeBlockOutput{Body: block}, nil
}

func auditBlockName(b *EdgeBlock) string {
	if b == nil {
		return ""
	}
	return b.ID
}

// auditBlock keeps an absent block absent instead of a typed nil.
func auditBlock(b *EdgeBlock) any {
	if b == nil {
		return nil
	}
	return b
}
