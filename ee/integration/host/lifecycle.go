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
	"github.com/robertherbaugh/kudurru/internal/store"
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
		s.Result = valueResult(&s.Result)
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
		var next disposition
		if exists {
			next = h.disposition(i)
		}
		// A job is its instance in one mode. A delete request, a release and
		// the end of either replace the job like any other change of authority.
		if !exists || !sameInstance(j.frame.instance, i) || !next.run || next.mode != j.mode {
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
			nextDigest := sourceDigest
			if digest, ok := input.ReconcileDigests[i.owner]; ok {
				nextDigest = digest
			}
			changed := j.sourceDigest != nextDigest
			if !j.active && (changed || j.frame.input.BundleID != i.BundleID()) {
				// A setup refusal releases its callback, not the elector's Run.
				// Changed authority must join that old election before a new
				// lifecycle can start; unchanged refused work is never replayed.
				j.cancel()
				continue
			}
			j.sourceDigest = nextDigest
			// An identical capture is the same immutable cut. Keep its original
			// frame so periodic polls cannot fence a longer source observation.
			// Any real bundle/source/RV replacement still revokes that frame.
			if changed || j.frame.input.BundleID != i.BundleID() || j.frame.instance.definition.ResourceVersion != i.definition.ResourceVersion {
				nextInput := input
				nextInput.BundleID = i.BundleID()
				j.frame = frame{i, nextInput}
				// A terminating job reports why its instance still exists and
				// may wait long between runs. Its own status write is a new
				// resource version of the same instance in the same mode, so
				// the report follows the frame instead of going stale.
				if s, ok := h.statuses[key]; ok && j.mode != GuardNormal && s.Owner == i.owner {
					s.Revision = i.definition.ResourceVersion
					h.statuses[key] = s
				}
			} else {
				// Equal authority may refresh FUTURE request resources. Current
				// requests retain their original immutable input in requestOrigin.
				nextInput := input
				nextInput.BundleID = i.BundleID()
				j.frame.input = nextInput
			}
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
		next := h.disposition(i)
		if !next.run {
			h.statuses[key] = InstanceStatus{Owner: i.owner, Revision: i.definition.ResourceVersion, Phase: next.phase, Reason: next.reason, CredentialFingerprints: i.CredentialFingerprints()}
			continue
		}
		jobCtx, cancel := context.WithCancel(ctx)
		digest := sourceDigest
		if selected, ok := input.ReconcileDigests[i.owner]; ok {
			digest = selected
		}
		nextInput := input
		nextInput.BundleID = i.BundleID()
		j := &job{key: key, mode: next.mode, frame: frame{i, nextInput}, sourceDigest: digest, ctx: jobCtx, cancel: cancel, done: make(chan struct{}), commands: make(chan command, 8), reconcile: make(chan struct{}, 1)}
		h.jobs[key] = j
		phase := "Waiting"
		if next.mode != GuardNormal {
			phase = PhaseTerminating
		}
		h.statuses[key] = InstanceStatus{Owner: i.owner, Revision: i.definition.ResourceVersion, Phase: phase, Reason: "Election", CredentialFingerprints: i.CredentialFingerprints()}
		go h.elect(j)
	}
}

// disposition is what the host does with one captured instance: run a job in
// a mode, or only report why it does not.
type disposition struct {
	mode          GuardMode
	run           bool
	phase, reason string
}

// disposition decides from the captured object alone. A terminating instance
// is the host's to end only when root mounted the terminating callbacks and
// the object carries the reserved cleanup finalizer (D367); otherwise it is
// paused as before. A release runs without a license, a credential or regard
// to suspension, because it makes no provider call. Cleanup pauses, with no
// provider call, while the license does not allow integrations or the
// instance is suspended; a missing credential is decided inside the job,
// because only a publisher's cleanup needs one.
func (h *Host) disposition(i *Instance) disposition {
	d := i.definition
	allowed := h.opts.Allowed()
	terminating := !d.DeletionTimestamp.IsZero()
	if terminating && h.opts.Finalize != nil {
		mode, ok := InstanceMode(d)
		switch {
		case !ok:
			return disposition{phase: "Paused", reason: "Deleting"}
		case mode == GuardRelease:
			return disposition{mode: GuardRelease, run: true}
		case !allowed:
			return disposition{mode: mode, phase: PhaseTerminating, reason: "LicenseUnavailable"}
		case d.Spec.Suspend:
			return disposition{mode: mode, phase: PhaseTerminating, reason: "Suspended"}
		}
		return disposition{mode: GuardCleanup, run: true}
	}
	reason := ""
	switch {
	case terminating:
		reason = "Deleting"
	case i.credentialError:
		reason = "CredentialsUnavailable"
	case !allowed:
		reason = "LicenseUnavailable"
	case d.Spec.Suspend:
		reason = "Suspended"
	case h.opts.Finalize != nil && store.ReservedFinalizerUnarmed(d):
		// The policy promises cleanup, but the object does not carry the
		// finalizer that keeps the promise through a delete. Root refuses
		// every effect of such an instance; say why instead of failing setup.
		reason = ReasonFinalizerMissing
	}
	if reason != "" {
		return disposition{phase: "Paused", reason: reason}
	}
	return disposition{run: true}
}

// allowed is the license gate of one job. Only a release runs without it,
// and still only while root's serving lifetime lasts.
func (h *Host) allowed(j *job) bool {
	if j.mode == GuardRelease {
		return h.opts.Alive == nil || h.opts.Alive()
	}
	return h.opts.Allowed()
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
		if j.mode != GuardNormal {
			// A terminating job takes a slot for each bounded run, not for
			// the lease: a blocked cleanup waits without starving others.
			h.runTerminating(leaseCtx, j)
			return
		}
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
	if j == nil || !j.active || j.ctx.Err() != nil || j.leaseCtx.Err() != nil || j.guard != g || !h.allowed(j) {
		return ErrFenced
	}
	if request, ok := ctx.Value(scopeKey{}).(requestScope); ok && request.original != nil {
		if request.j != j || request.g.Ledger != g || j.frame.instance != request.original.instance || j.leaseCtx != request.original.lease || j.frame.input.BundleID != request.g.BundleID {
			return ErrFenced
		}
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
	if h.jobs[j.key] != j || !j.active || j.ctx.Err() != nil || j.leaseCtx.Err() != nil || !h.allowed(j) {
		return frame{}, ExecutionGuard{}, ErrFenced
	}
	return j.frame, ExecutionGuard{Ledger: j.guard, BundleID: j.frame.input.BundleID, Mode: j.mode}, nil
}
func (h *Host) fence(ctx context.Context, j *job, g ExecutionGuard) error {
	if original, ok := ctx.Value(commandCaptureKey{}).(*CommandCapture); ok {
		if original == nil || original.host != h || original.job != j || original.origin == nil {
			return ErrFenced
		}
		if err := original.origin.Err(); err != nil {
			return err
		}
	}
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
	s := InstanceStatus{Owner: j.frame.instance.owner, Revision: j.frame.instance.definition.ResourceVersion, Phase: phase, Reason: reason, Leader: leader, Result: valueResult(&result), LastSync: old.LastSync, CredentialFingerprints: j.frame.instance.CredentialFingerprints()}
	if phase == "Ready" {
		s.LastSync = h.opts.Now()
	}
	h.statuses[j.key] = s
}
func (h *Host) leaseID(j *job) string {
	return fmt.Sprintf("%s-%s-%d", h.incarnation, kinds.Digest([]byte(j.key))[:16], h.sequence.Add(1))
}
