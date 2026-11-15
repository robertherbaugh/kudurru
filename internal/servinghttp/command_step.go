package servinghttp

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/serving"
)

// WithCommandStep captures the initial lease of a distinct background command.
// It does not authenticate, transfer a request credential, or retry a command.
// The capture is bounded by capture; fn runs on ctx, so a bound on waiting
// for a cut does not also bound the command. The root must bind its original
// job proof and writer and reauthorize in the concrete transaction. A step
// refused after its preparation began must never call this to replace its cut.
func WithCommandStep(ctx, capture context.Context, publisher *serving.Publisher, fn func(context.Context) error) error {
	if ctx == nil || capture == nil || publisher == nil || fn == nil {
		return serving.ErrComponents
	}
	lease, err := publisher.CaptureHTTP(capture)
	if err != nil {
		return err
	}
	defer lease.Release()
	return fn(context.WithValue(ctx, leaseKey{}, lease))
}
