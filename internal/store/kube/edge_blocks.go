package kube

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/internal/edgeblock"
)

// EdgeBlockState is an actual observation of the operator edge block set
// (D388) in the release namespace's runtime ConfigMap. Empty UID/version is
// confirmed absence of the ConfigMap (the empty set), not a failed read.
type EdgeBlockState struct {
	UID, ResourceVersion string
	State                edgeblock.State
}

// EdgeBlockReceipt keeps dispatch and commit apart, as BlockingRuntimeReceipt
// does: an error after dispatch is an unknown outcome, never a rollback.
type EdgeBlockReceipt struct {
	Before, After         EdgeBlockState
	Dispatched, Committed bool
}

var errEdgeBlocks = errors.New("store/kube: invalid edge block runtime state")

func decodeEdgeBlocks(cm *corev1.ConfigMap) (EdgeBlockState, error) {
	if cm == nil {
		return EdgeBlockState{State: edgeblock.Empty()}, nil
	}
	if cm.Name != BlockingRuntimeName || cm.UID == "" || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil {
		return EdgeBlockState{}, errEdgeBlocks
	}
	out := EdgeBlockState{UID: string(cm.UID), ResourceVersion: cm.ResourceVersion, State: edgeblock.Empty()}
	if raw, ok := cm.Data[edgeblock.ConfigMapKey]; ok {
		state, err := edgeblock.Decode([]byte(raw))
		if err != nil {
			return EdgeBlockState{}, err
		}
		out.State = state
	}
	return out, nil
}

// ReadEdgeBlocks reads the set without initializing anything.
func (s *Store) ReadEdgeBlocks(ctx context.Context) (EdgeBlockState, error) {
	cm, err := s.blockingRuntime(ctx)
	if err != nil {
		return EdgeBlockState{}, err
	}
	return decodeEdgeBlocks(cm)
}

// ChangeEdgeBlocks makes one create or UID/resourceVersion-bound update of
// the runtime ConfigMap's edge block key. change computes the next state
// from the observed one; admit is the caller's final authorization, checked
// before the read and again before dispatch. Nothing is retried: a conflict
// or an unknown completion is returned with the receipt. Other keys and the
// blocking annotation survive.
func (s *Store) ChangeEdgeBlocks(ctx context.Context, change func(edgeblock.State) (edgeblock.State, error), admit func(context.Context) error) (receipt EdgeBlockReceipt, err error) {
	if ctx == nil || admit == nil || change == nil {
		return receipt, errEdgeBlocks
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return receipt, errEdgeBlocks
	}
	if err = admit(ctx); err != nil {
		return receipt, err
	}
	cm, err := s.blockingRuntime(ctx)
	if err != nil {
		return receipt, err
	}
	receipt.Before, err = decodeEdgeBlocks(cm)
	if err != nil {
		return receipt, err
	}
	next, err := change(receipt.Before.State)
	if err != nil {
		return receipt, err
	}
	data, err := edgeblock.Encode(next)
	if err != nil {
		return receipt, err
	}
	if err = admit(ctx); err != nil {
		return receipt, err
	}
	if err = ctx.Err(); err != nil {
		return receipt, err
	}
	create := cm == nil
	if create {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: BlockingRuntimeName, Namespace: s.opts.Namespace}}
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[edgeblock.ConfigMapKey] = string(data)
	req := s.coreREST.Put().Namespace(s.opts.Namespace).Resource("configmaps").Name(BlockingRuntimeName)
	if create {
		req = s.coreREST.Post().Namespace(s.opts.Namespace).Resource("configmaps")
	}
	var saved corev1.ConfigMap
	receipt.Dispatched = true
	err = req.Body(cm).MaxRetries(0).Do(ctx).Into(&saved)
	if err != nil {
		return receipt, err
	}
	receipt.Committed = true
	receipt.After, err = decodeEdgeBlocks(&saved)
	return receipt, err
}

// Namespace is the release namespace of the runtime ConfigMap.
func (s *Store) Namespace() string { return s.opts.Namespace }
