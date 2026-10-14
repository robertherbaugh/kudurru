package host

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

// Wake coalesces root's background change notifications into one level trigger.
func (h *Host) Wake() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// Statuses returns stable private copies of sanitized local diagnostics.
func (h *Host) Statuses() []InstanceStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]InstanceStatus, 0, len(h.statuses))
	for _, s := range h.statuses {
		s.CredentialFingerprints = maps.Clone(s.CredentialFingerprints)
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b InstanceStatus) int { return strings.Compare(instanceKey(a.Owner), instanceKey(b.Owner)) })
	return out
}
func instanceKey(i InstanceID) string { return i.Namespace + "/" + i.Name }
func electionKey(i InstanceID) string {
	return "integration-" + kinds.Digest([]byte(i.Namespace + "/" + i.Name))[:32]
}
func sameInstance(a, b *Instance) bool {
	return a.owner == b.owner && a.configDigest == b.configDigest && a.credentialDigest == b.credentialDigest && a.term.Same(b.term) && a.credentialError == b.credentialError
}

// Run reconciles complete captured instance sets and keeps one bounded worker
// per name. Replaced workers are canceled and quiesced before their replacement
// can acquire capacity, including config A-B-A and same-name UID recreation.
func (h *Host) Run(ctx context.Context) error {
	if !h.running.CompareAndSwap(false, true) {
		return ErrInvalid
	}
	ticker := time.NewTicker(h.opts.PollInterval)
	defer ticker.Stop()
	defer func() {
		h.mu.Lock()
		var jobs []*job
		for _, j := range h.jobs {
			j.cancel()
			jobs = append(jobs, j)
		}
		h.mu.Unlock()
		for _, j := range jobs {
			<-j.done
		}
	}()
	for {
		h.refresh(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-h.wake:
		case <-ticker.C:
		}
	}
}
func (h *Host) refresh(ctx context.Context) {
	captureCtx, cancel := context.WithTimeout(ctx, h.opts.CaptureTimeout)
	input, err := h.opts.Capture(captureCtx)
	var instances []*Instance
	if err == nil {
		instances, err = CaptureInstances(captureCtx, input)
	}
	var sourceDigest string
	if err == nil {
		encoded, encodeErr := json.Marshal(input.Sources)
		if encodeErr != nil || len(encoded) > plan.MaxSnapshotBytes {
			err = ErrCapacity
		} else {
			sourceDigest = kinds.Digest(encoded)
		}
	}
	cancel()
	desired := map[string]*Instance{}
	if err == nil {
		for _, i := range instances {
			desired[instanceKey(i.owner)] = i
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, j := range h.jobs {
		select {
		case <-j.done:
			delete(h.jobs, key)
			continue
		default:
		}
		i, exists := desired[key]
		if !h.opts.Allowed() || !exists || !sameInstance(j.frame.instance, i) || i.definition.Spec.Suspend || !i.definition.DeletionTimestamp.IsZero() || i.credentialError {
			j.cancel()
			// Never replace a name while an old provider may still be quiescing.
			select {
			case <-j.done:
				delete(h.jobs, key)
			default:
			}
		} else if j.ctx.Err() == nil {
			// Runtime receipts/budgets/status may update a whole bundle without
			// changing desired sources. They must not trigger a self-sync loop.
			changed := j.sourceDigest != sourceDigest
			j.sourceDigest = sourceDigest
			j.frame = frame{i, input}
			if j.active {
				j.guard.IntegrationResourceVersion = i.definition.ResourceVersion
			}
			if changed {
				select {
				case j.reconcile <- struct{}{}:
				default:
				}
			}
		}
	}
	// Forget absent local status; durable cleanup/retain is root-owned admission.
	for key := range h.statuses {
		if _, ok := desired[key]; !ok {
			delete(h.statuses, key)
		}
	}
	for key, i := range desired {
		if _, exists := h.jobs[key]; exists {
			continue
		}
		if !h.opts.Allowed() || i.definition.Spec.Suspend || !i.definition.DeletionTimestamp.IsZero() || i.credentialError {
			reason := "Suspended"
			if !h.opts.Allowed() {
				reason = "LicenseUnavailable"
			}
			if i.credentialError {
				reason = "CredentialsUnavailable"
			}
			if !i.definition.DeletionTimestamp.IsZero() {
				reason = "Deleting"
			}
			h.statuses[key] = InstanceStatus{Owner: i.owner, Phase: "Paused", Reason: reason, CredentialFingerprints: i.CredentialFingerprints()}
			continue
		}
		jobCtx, cancel := context.WithCancel(ctx)
		j := &job{key: key, frame: frame{i, input}, sourceDigest: sourceDigest, ctx: jobCtx, cancel: cancel, done: make(chan struct{}), commands: make(chan command, 8), reconcile: make(chan struct{}, 1)}
		h.jobs[key] = j
		h.statuses[key] = InstanceStatus{Owner: i.owner, Phase: "Waiting", Reason: "Election", CredentialFingerprints: i.CredentialFingerprints()}
		go h.elect(j)
	}
}
func (h *Host) elect(j *job) {
	defer close(j.done)
	defer j.cancel()
	defer h.Wake()
	h.mu.RLock()
	id := j.frame.instance.owner
	h.mu.RUnlock()
	elector := h.opts.Elector(electionKey(id))
	if elector == nil {
		h.setLocal(j, "Degraded", "ElectionUnavailable", Result{}, false)
		return
	}
	// Kubernetes electors may invoke OnStarted asynchronously. Closing the
	// admission gate before Wait prevents a late callback from starting after
	// Run returned, and done remains open until every admitted callback stops.
	var callbacks sync.WaitGroup
	var gate sync.Mutex
	closed, admitted := false, false
	err := elector.Run(j.ctx, func(leaseCtx context.Context) {
		gate.Lock()
		if closed || admitted || j.ctx.Err() != nil {
			gate.Unlock()
			return
		}
		admitted = true
		callbacks.Add(1)
		gate.Unlock()
		defer func() {
			gate.Lock()
			admitted = false
			gate.Unlock()
			callbacks.Done()
		}()
		select {
		case h.slots <- struct{}{}:
		case <-leaseCtx.Done():
			return
		case <-j.ctx.Done():
			return
		}
		defer func() { <-h.slots }()
		h.runLease(leaseCtx, j)
	}, func() {})
	failed := err != nil && j.ctx.Err() == nil
	gate.Lock()
	closed = true
	j.cancel()
	gate.Unlock()
	callbacks.Wait()
	if failed {
		h.setLocal(j, "Degraded", "ElectionFailed", Result{}, false)
	}
}

// WithLease holds the active incarnation barrier through root's existing
// commit callback. Root still checks term/config/credential authority at the
// store boundary, never merely before entering this method.
func (h *Host) WithLease(ctx context.Context, g ledger.Guard, fn func(context.Context) error) error {
	if fn == nil {
		return ErrInvalid
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	j := h.jobs[instanceKey(g.Owner)]
	if j == nil || !j.active || j.ctx.Err() != nil || j.leaseCtx.Err() != nil || j.guard != g || !h.opts.Allowed() {
		return ErrFenced
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	commitCtx, cancel := context.WithCancel(ctx)
	stopLease := context.AfterFunc(j.leaseCtx, cancel)
	stopJob := context.AfterFunc(j.ctx, cancel)
	defer cancel()
	defer stopLease()
	defer stopJob()
	return fn(commitCtx)
}
func (h *Host) captured(j *job) (frame, ExecutionGuard, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.jobs[j.key] != j || !j.active || j.ctx.Err() != nil || j.leaseCtx.Err() != nil || !h.opts.Allowed() {
		return frame{}, ExecutionGuard{}, ErrFenced
	}
	return j.frame, ExecutionGuard{Ledger: j.guard, BundleID: j.frame.input.BundleID}, nil
}
func (h *Host) fence(ctx context.Context, j *job, g ExecutionGuard) error {
	_, current, err := h.captured(j)
	if err != nil {
		return err
	}
	if current != g {
		return ErrFenced
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.opts.Fence(ctx, g); err != nil {
		return ErrFenced
	}
	return nil
}
func (h *Host) setLocal(j *job, phase, reason string, result Result, leader bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.jobs[j.key] != j {
		return
	}
	old := h.statuses[j.key]
	s := InstanceStatus{Owner: j.frame.instance.owner, Phase: phase, Reason: reason, Leader: leader, Result: result, LastSync: old.LastSync, CredentialFingerprints: j.frame.instance.CredentialFingerprints()}
	if phase == "Ready" {
		s.LastSync = h.opts.Now()
	}
	h.statuses[j.key] = s
}
func (h *Host) leaseID(j *job) string {
	return fmt.Sprintf("%s-%s-%d", h.incarnation, kinds.Digest([]byte(j.key))[:16], h.sequence.Add(1))
}
