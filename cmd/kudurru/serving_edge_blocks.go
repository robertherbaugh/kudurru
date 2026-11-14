package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/edgeblock"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Operator edge blocks on the complete SQLite runtime (D388). The set is one
// replicated runtime row changed by the original authorized current command
// with its audit row in the same transaction; every member serves the set of
// its current publication to the edges on GET /internal/edge/blocks.

var edgeBlockWriteAttrs = rbac.Attributes{Resource: "edgeblocks", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}

// loadEdgeBlocksTx reads the owner row inside the command; absence is the
// never-written empty set.
func loadEdgeBlocksTx(ctx context.Context, tx *sqlite.CommandTx) (edgeblock.State, error) {
	if err := tx.CheckSectionPrefixBounds(ctx, edgeblock.Section, edgeblock.Prefix, 1, edgeblock.MaxDocumentBytes); err != nil {
		return edgeblock.State{}, err
	}
	rows, err := tx.ListSectionPrefix(ctx, edgeblock.Section, edgeblock.Prefix)
	if err != nil {
		return edgeblock.State{}, err
	}
	return edgeblock.FromRows(rows)
}

func stageEdgeBlocksTx(ctx context.Context, tx *sqlite.CommandTx, next edgeblock.State) error {
	data, err := edgeblock.Encode(next)
	if err != nil {
		return err
	}
	return tx.PutSection(ctx, edgeblock.Section, replication.Entry{Key: edgeblock.Key, Data: data})
}

// edgeBlockCommand runs one change under the original raw proof and writer.
// change returns the next state and the block to audit.
func (s *servingRuntime) edgeBlockCommand(ctx context.Context, action string, change func(edgeblock.State, string, time.Time) (edgeblock.State, edgeblock.Block, error)) (*api.EdgeBlock, error) {
	if s == nil || ctx == nil {
		return nil, currentCommandAPIError(serving.ErrComponents)
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	var result edgeblock.Block
	err = s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		principal, decision, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, edgeBlockWriteAttrs, s.opts.Now)
		if err != nil {
			return err
		}
		current, err := loadEdgeBlocksTx(ctx, tx)
		if err != nil {
			return err
		}
		actor := string(principal.Kind) + ":" + principal.Name
		next, block, err := change(current, actor, s.opts.Now())
		if err != nil {
			return err
		}
		if err := stageEdgeBlocksTx(ctx, tx, next); err != nil {
			return err
		}
		entry := sqlite.AuditEntry{Action: action, Actor: actor, Authz: decision.Path, Manager: string(store.ManagerAPI),
			Key: store.Key{Kind: "EdgeBlock", Name: block.ID}, ResourceVersion: strconv.FormatUint(next.Revision, 10), Reason: block.Reason}
		doc, _ := json.Marshal(block)
		if action == "edgeblock.revoke" {
			entry.Before = doc
		} else {
			entry.After = doc
		}
		result = block
		return tx.RecordAudit(ctx, entry)
	})
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	s.publisher.Wake()
	out := api.EdgeBlockFromStored(result)
	return &out, nil
}

// CreateEdgeBlock stores one new block.
func (s *servingRuntime) CreateEdgeBlock(ctx context.Context, c api.EdgeBlockChange) (*api.EdgeBlock, error) {
	id, err := edgeblock.NewID()
	if err != nil {
		return nil, err
	}
	return s.edgeBlockCommand(ctx, "edgeblock.create", func(st edgeblock.State, actor string, now time.Time) (edgeblock.State, edgeblock.Block, error) {
		return edgeblock.Add(st, edgeblock.Request{Prefix: c.Prefix, Duration: c.Duration, Until: c.Until, Reason: c.Reason, Actor: actor}, id, now)
	})
}

// RevokeEdgeBlock removes one active block.
func (s *servingRuntime) RevokeEdgeBlock(ctx context.Context, id string) (*api.EdgeBlock, error) {
	return s.edgeBlockCommand(ctx, "edgeblock.revoke", func(st edgeblock.State, _ string, now time.Time) (edgeblock.State, edgeblock.Block, error) {
		return edgeblock.Revoke(st, id, now)
	})
}

// ListEdgeBlocks reads the set of the request's original publication.
func (s *servingRuntime) ListEdgeBlocks(ctx context.Context) (*api.EdgeBlockList, error) {
	owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](ctx, "runtime")
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	state, _ := owner.EdgeBlocks()
	return api.EdgeBlockListFromState(state, s.opts.Now()), nil
}

// InternalEdgeBlocks serves GET /internal/edge/blocks from one protected
// lease of the current publication, on the primary and on followers alike.
func (s *servingRuntime) InternalEdgeBlocks(ctx context.Context, node string) (*internalapi.EdgeBlockList, error) {
	if s == nil || s.publisher == nil || ctx == nil {
		return nil, serving.ErrComponents
	}
	lease, err := s.publisher.CaptureHTTP(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	bundle := lease.Bundle()
	if bundle == nil {
		return nil, serving.ErrReleased
	}
	value, _ := bundle.Component("runtime")
	owner, ok := value.(*servingbuild.RuntimeOwner)
	if !ok || owner == nil {
		return nil, serving.ErrComponents
	}
	state, clusterID := owner.EdgeBlocks()
	if clusterID == "" {
		return nil, errors.New("edge blocks: publication has no cluster identity")
	}
	list := internalapi.EdgeBlockListFrom(node, clusterID, state, s.opts.Now())
	if err := s.publisher.CheckReadLease(ctx, lease); err != nil {
		return nil, err
	}
	return &list, nil
}

var _ api.EdgeBlocks = (*servingRuntime)(nil)
