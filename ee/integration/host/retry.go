package host

import (
	"errors"
	"fmt"
	"time"
)

// FailureKind is a sanitized provider response classification.
type FailureKind uint8

// Only transient failures and bounded Retry-After trigger automatic retries.
const (
	FailureTemporary FailureKind = iota + 1
	FailurePermanent
	FailureRateLimited
)

// Failure carries no URL, authenticated body, header, or credential data.
// Providers classify raw transport/API errors before returning to the host.
type Failure struct {
	Kind       FailureKind
	RetryAfter time.Duration
}

func (f *Failure) Error() string { return "integration host: provider request failed" }
func retryFailure(err error) *Failure {
	var f *Failure
	if errors.As(err, &f) && f != nil && f.Kind >= FailureTemporary && f.Kind <= FailureRateLimited && f.RetryAfter >= 0 && f.RetryAfter <= 15*time.Minute {
		return &Failure{Kind: f.Kind, RetryAfter: f.RetryAfter}
	}
	return nil
}
func (h *Host) retryDelay(err error, failures uint32) (time.Duration, bool) {
	f := retryFailure(err)
	if f == nil || f.Kind == FailurePermanent {
		return 0, false
	}
	delay := time.Second
	for i := uint32(1); i < failures && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	// Stable per-host jitter bounds bursts without unbounded retry workers.
	sum := kindsDigest(fmt.Sprintf("%s/%d", h.incarnation, h.sequence.Add(1)))
	delay += time.Duration(sum%1024) * (delay / 4) / 1024
	if f != nil && f.RetryAfter > delay {
		delay = f.RetryAfter
	}
	return delay, true
}
func kindsDigest(s string) uint64 {
	// FNV is used only for retry jitter, never identity/security.
	n := uint64(14695981039346656037)
	for i := range len(s) {
		n ^= uint64(s[i])
		n *= 1099511628211
	}
	return n
}
