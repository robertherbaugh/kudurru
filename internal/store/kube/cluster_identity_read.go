package kube

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// StartupClusterID returns an actually observed authenticated cluster identity.
// It is an optional capability prerequisite, not a general startup/readiness
// barrier. Unknown identity is never a successful empty or configured result.
// Unpinned callers share existing discovery and do not start another API read.
func (s *Store) StartupClusterID(ctx context.Context) (string, error) {
	if s == nil || ctx == nil {
		return "", errIdentityUntrusted
	}
	if s.isClosed() {
		return "", store.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.bgCtx == nil || s.opts.StartupTimeout <= 0 {
		return "", errIdentityUnavailable
	}
	original, cancel := context.WithTimeout(ctx, s.opts.StartupTimeout)
	defer cancel()
	stop := context.AfterFunc(s.bgCtx, cancel)
	defer stop()
	var err error
	if s.identity != nil {
		err = s.CheckClusterIdentity(original)
	} else if s.clusterIDReady == nil {
		err = errIdentityUnavailable
	} else {
		select {
		case <-s.clusterIDReady:
		case <-original.Done():
			err = original.Err()
		}
	}
	if s.isClosed() {
		return "", store.ErrClosed
	}
	if err == nil {
		err = original.Err()
	}
	if err != nil {
		return "", err
	}
	var id string
	if s.identity != nil {
		id = s.identity.id()
	} else if observed := s.clusterID.Load(); observed != nil {
		id = *observed
	}
	if id == "" {
		return "", errIdentityUnavailable
	}
	return id, nil
}
