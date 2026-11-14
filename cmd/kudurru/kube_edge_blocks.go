package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/edgeblock"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

const (
	kubeEdgeBlocksTimeout = 5 * time.Second
	kubeEdgeBlocksRefresh = 2 * time.Second
	// The internal route refuses an observation older than this rather than
	// serve a set the apiserver may have changed long ago.
	kubeEdgeBlocksStale = 15 * time.Second
)

// kubeEdgeBlocks keeps operator edge blocks (D388) in the release
// namespace's runtime ConfigMap next to the blocking deadline. Commands make
// one authorized CAS write with no retry; every replica refreshes its
// observation for the edges every two seconds. The API audits the commands
// (Options.AuditEdgeBlocks), because a ConfigMap write has no audit row.
type kubeEdgeBlocks struct {
	store      *kube.Store
	authorizer rbac.Authorizer
	node       string
	observed   atomic.Pointer[kubeEdgeBlockObservation]
}

type kubeEdgeBlockObservation struct {
	state kube.EdgeBlockState
	at    time.Time
}

func newKubeEdgeBlocks(st *kube.Store, authorizer rbac.Authorizer, node string) *kubeEdgeBlocks {
	return &kubeEdgeBlocks{store: st, authorizer: authorizer, node: node}
}

func (k *kubeEdgeBlocks) owner(st kube.EdgeBlockState) string {
	if st.UID == "" {
		return "kubernetes:" + k.store.Namespace()
	}
	return "kubernetes:" + k.store.Namespace() + ":" + st.UID
}

func (k *kubeEdgeBlocks) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, kubeEdgeBlocksTimeout)
	defer cancel()
	st, err := k.store.ReadEdgeBlocks(ctx)
	if err == nil && ctx.Err() == nil {
		k.observed.Store(&kubeEdgeBlockObservation{state: st, at: time.Now()})
	}
}

// Run refreshes the observation until ctx ends.
func (k *kubeEdgeBlocks) Run(ctx context.Context) error {
	for {
		k.refresh(ctx)
		timer := time.NewTimer(kubeEdgeBlocksRefresh)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// Internal serves GET /internal/edge/blocks from a fresh observation.
func (k *kubeEdgeBlocks) Internal(context.Context) (*internalapi.EdgeBlockList, error) {
	o := k.observed.Load()
	now := time.Now()
	if o == nil || now.Sub(o.at) > kubeEdgeBlocksStale {
		return nil, edgeblock.ErrState
	}
	list := internalapi.EdgeBlockListFrom(k.node, k.owner(o.state), o.state.State, now)
	return &list, nil
}

// ListEdgeBlocks reads the ConfigMap once.
func (k *kubeEdgeBlocks) ListEdgeBlocks(ctx context.Context) (*api.EdgeBlockList, error) {
	ctx, cancel := context.WithTimeout(ctx, kubeEdgeBlocksTimeout)
	defer cancel()
	st, err := k.store.ReadEdgeBlocks(ctx)
	if err != nil {
		return nil, kubeEdgeBlocksUnavailable(nil)
	}
	return api.EdgeBlockListFromState(st.State, time.Now()), nil
}

func kubeEdgeBlocksUnavailable(hint any) error {
	return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "cluster edge block state unavailable", Hint: hint}}
}

func (k *kubeEdgeBlocks) command(ctx context.Context, change func(edgeblock.State, string, time.Time) (edgeblock.State, edgeblock.Block, error)) (*api.EdgeBlock, error) {
	ctx, cancel := context.WithTimeout(ctx, kubeEdgeBlocksTimeout)
	defer cancel()
	actor := ""
	if p, ok := auth.PrincipalFrom(ctx); ok {
		actor = string(p.Kind) + ":" + p.Name
	}
	admit := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := auth.Authorize(ctx, k.authorizer, edgeBlockWriteAttrs); err != nil {
			return &api.Error{Status: err.Status, Body: api.ErrorBody{Key: err.Key, Message: err.Message, Hint: err.Hint}}
		}
		return ctx.Err()
	}
	var block edgeblock.Block
	// nosemgrep: trailofbits.go.invalid-usage-of-modified-variable.invalid-usage-of-modified-variable -- Receipt is a value, valid on error; its dispatch/commit fields distinguish refused, unknown and committed writes.
	receipt, err := k.store.ChangeEdgeBlocks(ctx, func(st edgeblock.State) (edgeblock.State, error) {
		next, b, err := change(st, actor, time.Now())
		block = b
		return next, err
	}, admit)
	if err != nil {
		if !receipt.Dispatched {
			return nil, err // model, admission and read refusals map in the API
		}
		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			return nil, &api.Error{Status: http.StatusConflict, Body: api.ErrorBody{Key: api.KeyConflict, Message: "cluster edge block state changed; this command was not applied"}}
		}
		hint := map[string]any{"dispatched": true, "outcome": "unknown"}
		if receipt.Committed {
			hint["outcome"], hint["committed"] = "committed", true
		}
		return nil, kubeEdgeBlocksUnavailable(hint)
	}
	k.observed.Store(&kubeEdgeBlockObservation{state: receipt.After, at: time.Now()})
	out := api.EdgeBlockFromStored(block)
	return &out, nil
}

func (k *kubeEdgeBlocks) CreateEdgeBlock(ctx context.Context, c api.EdgeBlockChange) (*api.EdgeBlock, error) {
	id, err := edgeblock.NewID()
	if err != nil {
		return nil, err
	}
	return k.command(ctx, func(st edgeblock.State, actor string, now time.Time) (edgeblock.State, edgeblock.Block, error) {
		return edgeblock.Add(st, edgeblock.Request{Prefix: c.Prefix, Duration: c.Duration, Until: c.Until, Reason: c.Reason, Actor: actor}, id, now)
	})
}

func (k *kubeEdgeBlocks) RevokeEdgeBlock(ctx context.Context, id string) (*api.EdgeBlock, error) {
	return k.command(ctx, func(st edgeblock.State, _ string, now time.Time) (edgeblock.State, edgeblock.Block, error) {
		return edgeblock.Revoke(st, id, now)
	})
}

var _ api.EdgeBlocks = (*kubeEdgeBlocks)(nil)
