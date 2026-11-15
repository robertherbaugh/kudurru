package kube

import (
	"bytes"
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/internal/auth/requestlimit"
	"github.com/robertherbaugh/kudurru/internal/store"
)

const requestLimitName = "kudurru-browser-admission"
const requestLimitType corev1.SecretType = "kudurru.io/browser-admission"

// UpdateRequestLimit uses the original guarded Kubernetes connection and one
// UID/resourceVersion create/update. It neither retries nor watches a cache.
// Confirmed absence initializes; privileged deletion explicitly resets history.
func (s *Store) UpdateRequestLimit(ctx context.Context, change func([]byte) ([]byte, error)) error {
	if s == nil || s.isClosed() {
		return store.ErrClosed
	}
	if ctx == nil || change == nil {
		return requestlimit.ErrState
	}
	if _, ok := ctx.Deadline(); !ok {
		return requestlimit.ErrState
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var current corev1.Secret
	err := s.coreREST.Get().Namespace(s.opts.Namespace).Resource("secrets").Name(requestLimitName).MaxRetries(0).Do(ctx).Into(&current)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return err
	}
	var raw []byte
	if create {
		current = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: requestLimitName, Namespace: s.opts.Namespace}, Type: requestLimitType}
	} else {
		if current.Type != requestLimitType || current.UID == "" || current.ResourceVersion == "" || current.DeletionTimestamp != nil || len(current.OwnerReferences) != 0 || len(current.Data) != 1 || requestlimit.Validate(current.Data["state"]) != nil {
			return requestlimit.ErrState
		}
		raw = current.Data["state"]
	}
	next, err := change(raw)
	if err != nil {
		return err
	}
	if err := requestlimit.Validate(next); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current.Data = map[string][]byte{"state": next}
	req := s.coreREST.Put().Namespace(s.opts.Namespace).Resource("secrets").Name(requestLimitName)
	if create {
		req = s.coreREST.Post().Namespace(s.opts.Namespace).Resource("secrets")
	}
	var saved corev1.Secret
	if err := req.Body(&current).MaxRetries(0).Do(ctx).Into(&saved); err != nil {
		return err
	}
	if saved.Name != requestLimitName || saved.Namespace != s.opts.Namespace || saved.UID == "" || saved.ResourceVersion == "" || (!create && (saved.UID != current.UID || saved.ResourceVersion == current.ResourceVersion)) || saved.DeletionTimestamp != nil || len(saved.OwnerReferences) != 0 || saved.Type != requestLimitType || len(saved.Data) != 1 || !bytes.Equal(saved.Data["state"], next) {
		return errors.New("store/kube: browser admission response mismatch")
	}
	return nil
}
