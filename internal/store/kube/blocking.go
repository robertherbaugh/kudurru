package kube

import (
	"context"
	"errors"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/internal/store"
)

const (
	// BlockingRuntimeName is release-namespace state, separate from settings.
	BlockingRuntimeName = "kudurru-runtime"
	blockingUntilKey    = "kudurru.io/blocking-disabled-until"
)

var errBlockingRuntime = errors.New("store/kube: invalid blocking runtime state")

// BlockingRuntimeState is an actual ConfigMap observation. Empty UID/version
// represents confirmed absence (blocking enabled), not an unavailable read.
type BlockingRuntimeState struct {
	UID, ResourceVersion string
	Until                time.Time
}

// BlockingRuntimeReceipt preserves dispatch and commit separately: an I/O
// failure after dispatch must never be presented as a rollback or retried.
type BlockingRuntimeReceipt struct {
	Before, After         BlockingRuntimeState
	Dispatched, Committed bool
}

func decodeBlockingRuntime(cm *corev1.ConfigMap) (BlockingRuntimeState, error) {
	if cm == nil {
		return BlockingRuntimeState{}, nil
	}
	if cm.Name != BlockingRuntimeName || cm.UID == "" || cm.ResourceVersion == "" || cm.DeletionTimestamp != nil {
		return BlockingRuntimeState{}, errBlockingRuntime
	}
	out := BlockingRuntimeState{UID: string(cm.UID), ResourceVersion: cm.ResourceVersion}
	value := cm.Annotations[blockingUntilKey]
	switch value {
	case "":
	case "forever":
		out.Until = time.Unix(0, math.MaxInt64).UTC()
	default:
		if len(value) > 64 {
			return BlockingRuntimeState{}, errBlockingRuntime
		}
		until, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || !until.After(time.Unix(0, 0)) || !until.Before(time.Unix(0, math.MaxInt64)) {
			return BlockingRuntimeState{}, errBlockingRuntime
		}
		out.Until = until.UTC()
	}
	return out, nil
}

func (s *Store) blockingRuntime(ctx context.Context) (*corev1.ConfigMap, error) {
	if s == nil || s.isClosed() {
		return nil, store.ErrClosed
	}
	if ctx == nil {
		return nil, errBlockingRuntime
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var cm corev1.ConfigMap
	err := s.coreREST.Get().Namespace(s.opts.Namespace).Resource("configmaps").Name(BlockingRuntimeName).MaxRetries(0).Do(ctx).Into(&cm)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return &cm, err
}

// ReadBlockingRuntime uses the driver's original cluster-identity guard. It
// never initializes missing state or converts an unavailable read to enabled.
func (s *Store) ReadBlockingRuntime(ctx context.Context) (BlockingRuntimeState, error) {
	cm, err := s.blockingRuntime(ctx)
	if err != nil {
		return BlockingRuntimeState{}, err
	}
	return decodeBlockingRuntime(cm)
}

// ChangeBlockingRuntime makes one create or resourceVersion/UID-bound update.
// The caller supplies its original finite context and final authorization. No
// conflict, unavailable reply or unknown completion is retried. Other ConfigMap
// fields survive; neither a refused write nor a failed read changes local DNS.
func (s *Store) ChangeBlockingRuntime(ctx context.Context, until time.Time, admit func(context.Context) error) (receipt BlockingRuntimeReceipt, err error) {
	if ctx == nil || admit == nil {
		return receipt, errBlockingRuntime
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return receipt, errBlockingRuntime
	}
	value := ""
	if !until.IsZero() {
		if until.Equal(time.Unix(0, math.MaxInt64)) {
			value = "forever"
		} else if until.After(time.Unix(0, 0)) && until.Before(time.Unix(0, math.MaxInt64)) {
			value = until.UTC().Format(time.RFC3339Nano)
		} else {
			return receipt, errBlockingRuntime
		}
	}
	if err = ctx.Err(); err != nil {
		return receipt, err
	}
	if err = admit(ctx); err != nil {
		return receipt, err
	}
	cm, err := s.blockingRuntime(ctx)
	if err != nil {
		return receipt, err
	}
	receipt.Before, err = decodeBlockingRuntime(cm)
	if err != nil {
		return receipt, err
	}
	if err = admit(ctx); err != nil {
		return receipt, err
	}
	if err = ctx.Err(); err != nil {
		return receipt, err
	}
	// A no-op still uses a guarded write, so the original read cannot authorize
	// success after a competing update or deletion/recreation.
	create := cm == nil
	if create {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: BlockingRuntimeName, Namespace: s.opts.Namespace}}
	}
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	if value == "" {
		delete(cm.Annotations, blockingUntilKey)
	} else {
		cm.Annotations[blockingUntilKey] = value
	}
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
	receipt.After, err = decodeBlockingRuntime(&saved)
	return receipt, err
}
