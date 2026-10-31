package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/robertherbaugh/kudurru/internal/audit"
)

const (
	auditVerificationTTL     = 30 * time.Second
	auditVerificationTimeout = 30 * time.Second
)

type auditVerificationResult struct {
	identity string
	result   AuditVerification
	err      error
}

type auditVerificationCache struct {
	mu    sync.Mutex
	have  bool
	value auditVerificationResult
	group singleflight.Group
}

func (c *auditVerificationCache) cached(now time.Time, identity string) (auditVerificationResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, c.have && c.value.identity == identity && now.Sub(c.value.result.CheckedAt) < auditVerificationTTL
}

func (s *Server) verifyAudit(ctx context.Context, _ *struct{}) (*auditVerifyOutput, error) {
	// Always apply the license gate before returning a cached result.
	rd, err := s.auditReader()
	if err != nil {
		return nil, err
	}
	v, ok := rd.(audit.Verifier)
	if !ok {
		return nil, verificationUnavailable()
	}
	value, err := s.cachedAuditVerification(ctx, v)
	if err != nil {
		return nil, verificationUnavailable()
	}
	return &auditVerifyOutput{Body: &value.result}, value.err
}

func verificationUnavailable() *Error {
	return newError(http.StatusServiceUnavailable, KeyUnavailable, "audit verification is temporarily unavailable", "try again later")
}

func (s *Server) cachedAuditVerification(ctx context.Context, v audit.Verifier) (auditVerificationResult, error) {
	opCtx, cancel := context.WithTimeout(ctx, auditVerificationTimeout)
	defer cancel()
	snapshot := audit.VerificationSnapshot{Verifier: v, Current: func(context.Context) error { return nil }}
	if source, ok := v.(audit.SnapshotVerifier); ok {
		var err error
		snapshot, err = source.SnapshotVerification(opCtx)
		if err != nil {
			return auditVerificationResult{}, err
		}
		if snapshot.Identity == "" || snapshot.Verifier == nil || snapshot.Current == nil {
			return auditVerificationResult{}, errors.New("audit trust snapshot unavailable")
		}
	}
	c := &s.auditVerification
	if value, ok := c.cached(s.opts.Now(), snapshot.Identity); ok {
		return value, snapshot.Current(opCtx)
	}
	result := c.group.DoChan("audit:"+snapshot.Identity, func() (any, error) {
		// Another caller may have filled the cache between the first lookup and
		// joining the shared scan. Recheck it inside the single-flight group.
		if value, ok := c.cached(s.opts.Now(), snapshot.Identity); ok {
			return value, nil
		}
		deadline, _ := opCtx.Deadline()
		checkCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		defer cancel()
		err := snapshot.Verifier.Verify(checkCtx)
		if currentErr := snapshot.Current(checkCtx); currentErr != nil {
			return auditVerificationResult{}, currentErr
		}
		value := auditVerificationResult{identity: snapshot.Identity, result: AuditVerification{Verified: err == nil, CheckedAt: s.opts.Now()}}
		if err != nil {
			s.log.Warn("audit verification failed", "err", err)
			if errors.Is(err, audit.ErrTampered) {
				value.result.Reason = "The retained audit trail contains changed records, invalid seals, or broken links."
			} else {
				value.err = verificationUnavailable()
			}
		}
		// Cache unavailable results too, so outages cannot cause repeated scans.
		c.mu.Lock()
		c.value, c.have = value, true
		c.mu.Unlock()
		return value, nil
	})
	select {
	case <-opCtx.Done():
		return auditVerificationResult{}, opCtx.Err()
	case shared := <-result:
		value, _ := shared.Val.(auditVerificationResult)
		if shared.Err != nil {
			return value, shared.Err
		}
		return value, snapshot.Current(opCtx)
	}
}
