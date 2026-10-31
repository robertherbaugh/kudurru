package host

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/robertherbaugh/kudurru/ee/integration"
)

// Check performs a bounded read-only provider check under its active lease.
func (h *Host) Check(ctx context.Context, id InstanceID) (integration.Status, error) {
	a := h.submit(ctx, id, command{kind: "check"})
	return a.check, a.err
}

// Preview computes every complete destination plan without provider mutations.
func (h *Host) Preview(ctx context.Context, id InstanceID) (*Preview, error) {
	a := h.submit(ctx, id, command{kind: "preview"})
	return a.preview, a.err
}

// Sync rejects a changed aggregate preview before any external mutation.
func (h *Host) Sync(ctx context.Context, id InstanceID, expected string) (*Result, error) {
	a := h.submit(ctx, id, command{kind: "sync", expected: expected})
	return a.result, a.err
}

// Invoke invokes only an advertised action; root owns caller authorization.
func (h *Host) Invoke(ctx context.Context, id InstanceID, action string, params json.RawMessage) (json.RawMessage, error) {
	if action == "" || len(action) > 128 || len(params) > 64<<10 || !json.Valid(params) {
		return nil, ErrInvalid
	}
	a := h.submit(ctx, id, command{kind: "invoke", action: action, params: append(json.RawMessage(nil), params...)})
	return a.value, a.err
}
func (h *Host) submit(ctx context.Context, id InstanceID, c command) answer {
	if id.Validate() != nil {
		return answer{err: ErrInvalid}
	}
	ctx, cancel := context.WithTimeout(ctx, h.opts.OperationTimeout)
	defer cancel()
	h.mu.RLock()
	j := h.jobs[instanceKey(id)]
	valid := j != nil && j.active && j.frame.instance.owner == id && j.ctx.Err() == nil
	h.mu.RUnlock()
	if !valid {
		return answer{err: ErrUnavailable}
	}
	c.ctx = ctx
	c.reply = make(chan answer, 1)
	select {
	case j.commands <- c:
	case <-ctx.Done():
		return answer{err: ctx.Err()}
	case <-j.ctx.Done():
		return answer{err: ErrUnavailable}
	default:
		return answer{err: ErrBusy}
	}
	select {
	case a := <-c.reply:
		return a
	case <-ctx.Done():
		return answer{err: ctx.Err()}
	case <-j.ctx.Done():
		return answer{err: ErrUnavailable}
	}
}
func (h *Host) runLease(leaseCtx context.Context, j *job) {
	ctx, cancel := context.WithCancel(leaseCtx)
	stopJob := context.AfterFunc(j.ctx, cancel)
	defer stopJob()
	defer cancel()
	h.mu.Lock()
	if h.jobs[j.key] != j || j.active || j.ctx.Err() != nil || leaseCtx.Err() != nil {
		h.mu.Unlock()
		return
	}
	instance := j.frame.instance
	g, err := instance.Guard(h.leaseID(j))
	if err != nil {
		h.mu.Unlock()
		return
	}
	j.active = true
	j.guard = g
	j.leaseCtx = ctx
	h.mu.Unlock()
	defer func() { h.mu.Lock(); j.active = false; j.provider = nil; h.mu.Unlock() }()
	provider, err := h.opts.Factory(instance.definition.Spec.Type)
	if err != nil || provider == nil {
		h.setLocal(j, "Degraded", "ProviderUnavailable", Result{}, true)
		return
	}
	h.mu.Lock()
	j.provider = provider
	h.mu.Unlock()
	providerHost := providerHost{h: h, j: j}
	setupCtx, setupCancel := context.WithTimeout(ctx, h.opts.OperationTimeout)
	setupCtx = scoped(setupCtx, j, ExecutionGuard{Ledger: g, BundleID: instance.BundleID()})
	if h.fence(setupCtx, j, ExecutionGuard{Ledger: g, BundleID: instance.BundleID()}) == nil {
		err = provider.Provision(setupCtx, providerHost, instance.ProviderSpec(), instance.Secrets())
		if err == nil {
			err = provider.Validate(setupCtx)
		}
		if err == nil {
			var checked integration.Status
			checked, err = provider.Check(setupCtx)
			if err == nil && (!checked.Authenticated || !checked.Reachable) {
				err = ErrProvider
			}
		}
	} else {
		err = ErrFenced
	}
	setupCancel()
	var startDone chan struct{}
	defer func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), h.opts.StopTimeout)
		defer stopCancel()
		if err := provider.Stop(stopCtx); err != nil {
			h.opts.Logger.Warn("integration provider stop failed", "integration", j.key)
		}
		// Never hand a name/slot to a replacement while the previous Start
		// callback can still execute. Providers must quiesce on cancellation.
		if startDone != nil {
			<-startDone
		}
	}()
	if err != nil {
		h.setLocal(j, "Degraded", "ProvisionFailed", Result{}, true)
		return
	}
	started := make(chan error, 1)
	startDone = make(chan struct{})
	startCtx := scoped(ctx, j, ExecutionGuard{Ledger: g, BundleID: instance.BundleID()})
	go func() { defer close(startDone); started <- provider.Start(startCtx) }()
	h.setLocal(j, "Running", "Provisioned", Result{}, true)
	// Start must honor cancellation; Stop quiesces rather than spawning writes.
	interval := instance.definition.Spec.Resync.Duration
	if interval == 0 {
		interval = 10 * time.Minute
	}
	if u := instance.definition.Spec.UniFi; u != nil {
		interval = u.Poll.Duration
		if interval == 0 {
			interval = time.Minute
		}
	}
	if interval < time.Second {
		interval = time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	var eventTick <-chan time.Time
	if _, ok := provider.(DeliveryHook); ok {
		events := time.NewTicker(h.opts.PollInterval)
		defer events.Stop()
		eventTick = events.C
	}
	var failures uint32
	reset := func(delay time.Duration) {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case startErr := <-started:
			if startErr != nil && ctx.Err() == nil {
				h.setLocal(j, "Degraded", "StartFailed", Result{}, true)
				return
			}
			started = nil
		case c := <-j.commands:
			operationCtx, operationCancel := context.WithTimeout(c.ctx, h.opts.OperationTimeout)
			stopLease := context.AfterFunc(ctx, operationCancel)
			a := h.execute(operationCtx, j, c)
			stopLease()
			operationCancel()
			c.reply <- a
			if c.kind == "sync" && a.err == nil {
				failures = 0
				reset(interval)
			}
		case <-j.reconcile:
			failures = 0
			reset(0)
		case <-eventTick:
			operationCtx, operationCancel := context.WithTimeout(ctx, h.opts.OperationTimeout)
			if err := h.drainEvents(operationCtx, j); err != nil {
				h.opts.Logger.Warn("integration event drain failed", "integration", j.key)
			}
			operationCancel()
		case <-timer.C:
			operationCtx, operationCancel := context.WithTimeout(ctx, h.opts.OperationTimeout)
			a := h.execute(operationCtx, j, command{kind: "sync"})
			operationCancel()
			if a.err == nil {
				failures = 0
				timer.Reset(interval)
			} else {
				if failures < 32 {
					failures++
				}
				if delay, retry := h.retryDelay(a.err, failures); retry {
					timer.Reset(delay)
				}
			}
		}
	}
}
func (h *Host) execute(ctx context.Context, j *job, c command) answer {
	ctx, cancel := context.WithTimeout(ctx, h.opts.OperationTimeout)
	defer cancel()
	f, g, err := h.captured(j)
	if err != nil {
		return answer{err: err}
	}
	ctx, err = h.scopedOriginal(ctx, j, f, g)
	if err != nil {
		return answer{err: err}
	}
	if err := h.fence(ctx, j, g); err != nil {
		return answer{err: err}
	}
	switch c.kind {
	case "deliver":
		return answer{err: h.queueEvent(ctx, j, f, g, c)}
	case "check":
		checked, err := j.provider.Check(ctx)
		if err != nil {
			return answer{err: safeError(ctx, err)}
		}
		if err := h.fence(ctx, j, g); err != nil {
			return answer{err: err}
		}
		checked.Provider = nil
		switch {
		case !checked.Reachable:
			checked.Message = "Provider is unavailable."
		case !checked.Authenticated:
			checked.Message = "Provider authentication failed."
		default:
			checked.Message = "Provider is reachable and authenticated."
		}
		return answer{check: checked}
	case "preview":
		p, err := h.preview(ctx, j, f, g)
		return answer{preview: p, err: err}
	case "sync":
		var result *Result
		_, observer := j.provider.(Observer)
		if _, publisher := j.provider.(Publisher); publisher {
			result, err = h.sync(ctx, j, f, g, c.expected)
			if err == nil && observer {
				var observation *Result
				observation, err = h.observe(ctx, j, g)
				result = combineResult(result, observation)
			}
		} else if observer {
			if c.expected != "" {
				return answer{err: ErrStalePreview}
			}
			result, err = h.observe(ctx, j, g)
		} else {
			return answer{err: ErrUnsupported}
		}
		if stale := h.fence(ctx, j, g); stale != nil {
			if result != nil && result.Source != nil && result.Source.Committed && !result.Source.Published && result.Pending == 0 {
				result.Pending = 1
			}
			h.setLocal(j, "Degraded", "ReconcileFenced", valueResult(result), false)
			return answer{result: result, err: stale}
		}
		if err != nil {
			h.setLocal(j, "Degraded", "ReconcileFailed", valueResult(result), true)
		} else {
			phase, reason := "Ready", "Synced"
			if result == nil {
				return answer{err: ErrInvalid}
			}
			if result.Pending > 0 || result.Conflicts > 0 {
				phase, reason = "Degraded", "ConflictsOrPending"
			}
			h.setLocal(j, phase, reason, *result, true)
		}
		h.publishStatus(ctx, j, g)
		return answer{result: result, err: err}
	case "invoke":
		actions, ok := j.provider.(integration.ActionProvider)
		if !ok {
			return answer{err: ErrUnsupported}
		}
		advertised := false
		if len(actions.Actions()) > 128 {
			return answer{err: ErrCapacity}
		}
		for _, a := range actions.Actions() {
			if a.Name == c.action {
				if advertised {
					return answer{err: ErrInvalid}
				}
				if err := validateParams(a.Params, c.params); err != nil {
					return answer{err: err}
				}
				advertised = true
			}
		}
		if !advertised {
			return answer{err: ErrUnsupported}
		}
		if err := h.fence(ctx, j, g); err != nil {
			return answer{err: err}
		}
		result, err := actions.Invoke(ctx, c.action, c.params)
		if err != nil {
			return answer{err: safeError(ctx, err)}
		}
		if len(result) > 64<<10 || !json.Valid(result) {
			return answer{err: ErrProvider}
		}
		// Defense in depth for the resolved named capabilities; wrappers still
		// sanitize authenticated responses before returning them.
		if containsSecret(result, f.instance.secrets) {
			return answer{err: ErrProvider}
		}
		if err := h.fence(ctx, j, g); err != nil {
			return answer{err: err}
		}
		return answer{value: result}
	default:
		return answer{err: ErrInvalid}
	}
}
func valueResult(r *Result) Result {
	if r == nil {
		return Result{}
	}
	out := *r
	if r.Source != nil {
		s := *r.Source
		out.Source = &s
	}
	return out
}

func combineResult(outbound, source *Result) *Result {
	if outbound == nil && source == nil {
		return nil
	}
	out := valueResult(outbound)
	if source != nil {
		observation := valueResult(source)
		out.Applied += observation.Applied
		out.Pending += observation.Pending
		out.Conflicts += observation.Conflicts
		out.Unchanged += observation.Unchanged
		out.Clients = observation.Clients
		out.Source = observation.Source
	}
	return &out
}
func safeError(ctx context.Context, err error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f := retryFailure(err); f != nil {
		return f
	}
	return ErrProvider
}
func (h *Host) publishStatus(ctx context.Context, j *job, g ExecutionGuard) {
	if h.opts.Status == nil {
		return
	}
	if h.fence(ctx, j, g) != nil {
		return
	}
	h.mu.RLock()
	s := h.statuses[j.key]
	s.Result = valueResult(&s.Result)
	s.CredentialFingerprints = j.frame.instance.CredentialFingerprints()
	h.mu.RUnlock()
	if h.opts.Status(ctx, g, s) != nil {
		h.opts.Logger.Warn("integration status publication failed", "integration", j.key)
	}
}

type providerHost struct {
	h *Host
	j *job
}

func (p providerHost) Logger() *slog.Logger           { return p.h.opts.Logger.With("integration", p.j.key) }
func (p providerHost) Metrics() prometheus.Registerer { return p.h.opts.Metrics }
func (p providerHost) Resolve(ctx context.Context, q dns.Question) (*dns.Msg, error) {
	if p.h.opts.Resolve == nil {
		return nil, ErrUnsupported
	}
	g, err := p.guard(ctx)
	if err != nil {
		return nil, err
	}
	if err := p.h.fence(ctx, p.j, g); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.h.opts.OperationTimeout)
	defer cancel()
	return p.h.opts.Resolve(ctx, q)
}
func (p providerHost) SetCondition(ctx context.Context, _, _, _ string, ok bool) error {
	g, err := p.guard(ctx)
	if err != nil {
		return err
	}
	if err := p.h.fence(ctx, p.j, g); err != nil {
		return err
	}
	phase, reason := "Degraded", "ProviderCondition"
	if ok {
		phase, reason = "Running", "ProviderCondition"
	}
	p.h.setLocal(p.j, phase, reason, Result{}, true)
	p.h.publishStatus(ctx, p.j, g)
	return nil
}
