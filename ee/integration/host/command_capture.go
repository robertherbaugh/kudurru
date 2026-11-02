package host

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/ee/integration"
)

// CommandCapture retains the exact integration job, frame and elected lease
// selected before an explicit command is queued. It conveys no caller authority:
// the root must retain and reauthorize the original raw proof at each effect.
// A capture permits one submission, including when that submission is refused.
type CommandCapture struct {
	host   *Host
	job    *job
	frame  frame
	guard  ExecutionGuard
	lease  context.Context
	origin context.Context
	used   atomic.Bool
}

type commandCaptureKey struct{}

func (*CommandCapture) String() string { return "private integration command capture" }

// GoString keeps private inputs and credentials out of diagnostic formatting.
func (c *CommandCapture) GoString() string { return c.String() }

// CaptureCommand requires the exact reviewed owner and resource version. The
// immutable input is retained here; execute must not capture a successor frame.
func (h *Host) CaptureCommand(ctx context.Context, id InstanceID, revision string) (*CommandCapture, error) {
	if h == nil || ctx == nil || id.Validate() != nil || revision == "" || len(revision) > 256 {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	j := h.jobs[instanceKey(id)]
	if j == nil || j.frame.instance == nil || j.frame.instance.owner != id || !j.active || j.ctx.Err() != nil || j.leaseCtx == nil || j.leaseCtx.Err() != nil || !h.opts.Allowed() {
		return nil, ErrUnavailable
	}
	if j.frame.instance.definition.ResourceVersion != revision || j.guard.IntegrationResourceVersion != revision {
		return nil, ErrFenced
	}
	return &CommandCapture{host: h, job: j, frame: j.frame, guard: ExecutionGuard{Ledger: j.guard, BundleID: j.frame.input.BundleID}, lease: j.leaseCtx, origin: ctx}, nil
}

// Current is a rejection check over the original capture, never a refresh.
// Root performs its own current caller, dependency, license and writer checks.
func (c *CommandCapture) Current(ctx context.Context) error {
	if c == nil || c.host == nil || ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.origin == nil {
		return ErrInvalid
	}
	if err := c.origin.Err(); err != nil {
		return err
	}
	c.host.mu.RLock()
	defer c.host.mu.RUnlock()
	return c.currentLocked()
}

func (c *CommandCapture) currentLocked() error {
	j := c.job
	if j == nil || c.host.jobs[j.key] != j || !j.active || j.ctx.Err() != nil || c.lease == nil || c.lease.Err() != nil || j.leaseCtx != c.lease || j.frame.instance != c.frame.instance || j.guard != c.guard.Ledger || j.frame.input.BundleID != c.guard.BundleID || !c.host.opts.Allowed() {
		return ErrFenced
	}
	return nil
}

func (c *CommandCapture) submit(ctx context.Context, request command) answer {
	if c == nil || !c.used.CompareAndSwap(false, true) {
		return answer{err: ErrInvalid}
	}
	if err := c.Current(ctx); err != nil {
		return answer{err: err}
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.origin, cancel)
	defer stop()
	defer cancel()
	request.original = c
	return c.host.submit(context.WithValue(ctx, commandCaptureKey{}, c), c.guard.Ledger.Owner, request)
}

// Check performs one bounded provider check using the original capture.
func (c *CommandCapture) Check(ctx context.Context) (integration.Status, error) {
	a := c.submit(ctx, command{kind: "check"})
	return a.check, a.err
}

// Preview computes complete plans without replacing the original input.
func (c *CommandCapture) Preview(ctx context.Context) (*Preview, error) {
	a := c.submit(ctx, command{kind: "preview"})
	return a.preview, a.err
}

// Sync requires an exact reviewed aggregate preview for a publisher. Observer
// sync uses an empty digest; root must distinguish that supported capability.
func (c *CommandCapture) Sync(ctx context.Context, expected string) (*Result, error) {
	if len(expected) > 64 {
		return nil, ErrInvalid
	}
	a := c.submit(ctx, command{kind: "sync", expected: expected})
	return a.result, a.err
}

// Invoke submits one advertised action with a detached bounded input.
func (c *CommandCapture) Invoke(ctx context.Context, action string, params json.RawMessage) (json.RawMessage, error) {
	if action == "" || len(action) > 128 || len(params) > 64<<10 || !json.Valid(params) {
		return nil, ErrInvalid
	}
	a := c.submit(ctx, command{kind: "invoke", action: action, params: append(json.RawMessage(nil), params...)})
	return a.value, a.err
}
