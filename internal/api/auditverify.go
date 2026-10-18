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
	result AuditVerification
	err    error
}

type auditVerificationCache struct {
	mu    sync.Mutex
	have  bool
	value auditVerificationResult
	group singleflight.Group
}

func (c *auditVerificationCache) cached(now time.Time) (auditVerificationResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, c.have && now.Sub(c.value.result.CheckedAt) < auditVerificationTTL
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
	c := &s.auditVerification
	if value, ok := c.cached(s.opts.Now()); ok {
		return value, nil
	}
	result := c.group.DoChan("audit", func() (any, error) {
		// Another caller may have filled the cache between the first lookup and
		// joining the shared scan. Recheck it inside the single-flight group.
		if value, ok := c.cached(s.opts.Now()); ok {
			return value, nil
		}
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditVerificationTimeout)
		defer cancel()
		err := v.Verify(checkCtx)
		value := auditVerificationResult{result: AuditVerification{Verified: err == nil, CheckedAt: s.opts.Now()}}
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
	case <-ctx.Done():
		return auditVerificationResult{}, ctx.Err()
	case shared := <-result:
		value, _ := shared.Val.(auditVerificationResult)
		return value, shared.Err
	}
}
