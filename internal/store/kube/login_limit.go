package kube

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/store"
)

const loginLimitName = "kudurru-login-lockouts"
const loginLimitType corev1.SecretType = "kudurru.io/login-lockouts"

// UpdateLoginLimit reserves or completes one password attempt through the
// original guarded Kubernetes connection. Both keys live in one private Secret;
// no cache, background watch, write retry or local fallback grants admission.
func (s *Store) UpdateLoginLimit(ctx context.Context, change func([]byte) ([]byte, error)) error {
	if s == nil || s.isClosed() {
		return store.ErrClosed
	}
	if ctx == nil || change == nil {
		return loginlimit.ErrState
	}
	if _, ok := ctx.Deadline(); !ok {
		return loginlimit.ErrState
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var current corev1.Secret
	err := s.coreREST.Get().Namespace(s.opts.Namespace).Resource("secrets").Name(loginLimitName).MaxRetries(0).Do(ctx).Into(&current)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return err
	}
	var raw []byte
	if create {
		current = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: loginLimitName, Namespace: s.opts.Namespace}, Type: loginLimitType}
	} else {
		if current.Type != loginLimitType || current.UID == "" || current.ResourceVersion == "" || current.DeletionTimestamp != nil || len(current.OwnerReferences) != 0 || len(current.Data) != 1 || len(current.Data["state"]) == 0 || len(current.Data["state"]) > loginlimit.MaxBytes {
			return loginlimit.ErrState
		}
		raw = current.Data["state"]
	}
	next, err := change(raw)
	if err != nil {
		return err
	}
	if len(next) == 0 || len(next) > loginlimit.MaxBytes {
		return loginlimit.ErrCapacity
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	current.Data = map[string][]byte{"state": next}
	req := s.coreREST.Put().Namespace(s.opts.Namespace).Resource("secrets").Name(loginLimitName)
	if create {
		req = s.coreREST.Post().Namespace(s.opts.Namespace).Resource("secrets")
	}
	var saved corev1.Secret
	if err = req.Body(&current).MaxRetries(0).Do(ctx).Into(&saved); err != nil {
		return err
	}
	if saved.UID == "" || saved.ResourceVersion == "" || (!create && saved.UID != current.UID) || saved.Type != loginLimitType || string(saved.Data["state"]) != string(next) {
		return errors.New("store/kube: login reservation response mismatch")
	}
	return nil
}
