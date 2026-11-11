package host

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// PhaseTerminating is the phase of an Integration that is being deleted and
// still carries the reserved cleanup finalizer (D367). Its reason says what
// the elected job is doing or what keeps it from finishing.
const PhaseTerminating = "Terminating"

// Reasons of the terminating job. A blocked reason never finalizes: the
// object stays until the cause is gone or the operator releases it by
// changing its deletion policy to Retain.
const (
	// ReasonFinalizerMissing pauses a live instance whose policy is Delete
	// but which is stored without the reserved finalizer.
	ReasonFinalizerMissing = "CleanupFinalizerMissing"
	// ReasonCleanup: the job deleted or forgot records and has more to do, or
	// is about to purge and finalize.
	ReasonCleanup = "CleanupInProgress"
	// ReasonReleaseFailed: the release could not remove the finalizer yet.
	ReasonReleaseFailed = "ReleaseFailed"
	// ReasonAuthorityChanged: the captured frame was replaced or root refused
	// the guard; the next run captures a fresh frame.
	ReasonAuthorityChanged = "AuthorityChanged"
	// ReasonCredentials: a publisher's cleanup needs its credential.
	ReasonCredentials = "CredentialsUnavailable"
	// ReasonProviderUnavailable: no provider of this type can be built.
	ReasonProviderUnavailable = "ProviderUnavailable"
	// ReasonProvisionFailed: the provider refused its configuration or
	// credential, or could not be reached.
	ReasonProvisionFailed = "ProvisionFailed"
	// ReasonSnapshotIncomplete: a destination could not be read completely.
	ReasonSnapshotIncomplete = "SnapshotIncomplete"
	// ReasonPendingRecovery: an earlier provider write has an uncertain
	// outcome that a complete reread did not resolve.
	ReasonPendingRecovery = "PendingRecovery"
	// ReasonDestinationRemoved: the ledger holds records of a destination the
	// spec no longer names, so they can be neither read nor deleted.
	ReasonDestinationRemoved = "DestinationRemoved"
	// ReasonRecordsConflict: records of the instance share a name or a record
	// set with rows of another owner and are left in place.
	ReasonRecordsConflict = "RecordsConflict"
	// ReasonOwnershipUnproven: a row carries the instance's marker without
	// ledger evidence, or the ledger lists provider IDs that still exist
	// without the marker.
	ReasonOwnershipUnproven = "OwnershipUnproven"
	// ReasonBudgetExhausted: the shared request budget has no room.
	ReasonBudgetExhausted = "BudgetExhausted"
	// ReasonRateLimited: the provider, or the budget behind an adapter that
	// reports it that way, asked the host to wait.
	ReasonRateLimited = "RateLimited"
	// ReasonProviderFailed: a provider request failed.
	ReasonProviderFailed = "ProviderFailed"
	// ReasonLocalStateRemains: root still finds local or private state.
	ReasonLocalStateRemains = "LocalStateRemains"
	// ReasonCleanupFailed: a local read or write failed.
	ReasonCleanupFailed = "CleanupFailed"
	// ReasonCapacity: the instance or the store holds more than one of the
	// bounded reads of a cleanup admits. Nothing was decided from a partial
	// read; the run is repeated at the resync interval.
	ReasonCapacity = "CapacityExceeded"
)

// PurgeResult counts what one Purge removed. All zero means nothing was left.
type PurgeResult struct {
	// Objects are source-owned public objects (record sets and zones).
	Objects int
	// SourceRows are the source head and its chunks.
	SourceRows  int
	Allocations int
	Deliveries  int
	// Clients are inventory clients withdrawn with the source.
	Clients int
}

// CleanupSummary is what the finalizing lease did itself (D367). Work an
// earlier lease or process finished is not in it; each of those steps wrote
// its own audit rows when it committed. Root records the summary with the
// finalizer removal.
type CleanupSummary struct {
	// Released: the finalizer goes without cleanup because the policy is no
	// longer Delete. Everything the instance published stays.
	Released bool
	// RecordSetsDeleted are provider record sets confirmed deleted.
	RecordSetsDeleted int
	// RecordsForgotten are ledger rows removed, OperationsForgotten the
	// operations removed with them or as leftovers.
	RecordsForgotten    int
	OperationsForgotten int
	Purged              PurgeResult
	// Runs is the number of bounded runs the lease needed.
	Runs int
}

// cleanupSource is the source digest of every cleanup plan: a terminating
// instance selects nothing, so there is no source selection to bind.
var cleanupSource = kinds.Digest([]byte("integration-cleanup"))

// terminatingLease is the state of one terminating lease.
type terminatingLease struct {
	inspected bool
	publisher bool
	provider  integration.Integration
	summary   CleanupSummary
	// progress: the current run confirmed a delete or forgot a record.
	progress bool
	// forgetLimit bounds the operations of one Forget commit of this lease;
	// zero is ledger.MaxForgetOperations. A run that reached its bound inside
	// a Forget without any progress sets it to half of what that Forget
	// tried and it never grows again in the lease, so a history whose commits
	// cannot finish in one run's bound still goes in smaller ones instead of
	// being cut at the same place every run.
	forgetLimit int
	// forgetCut: the current run's last Forget, of forgetTried rows, ended
	// with the run's bound.
	forgetCut   bool
	forgetTried int
	// localCut: the run's bound ended one of its local reads or commits
	// (a ledger or operation read, an intent, receipt or confirmation, a
	// Forget, a fence, the purge or the finalization), so a run cut without
	// progress names a local failure, not a provider one.
	localCut bool
	// providerCut: a provider request of the run returned only after the
	// run's bound. The provider took the bound, so the local commits that
	// follow it on the expired context (such as recording the partial
	// receipt it returned) do not make the run a local failure.
	providerCut bool
	// forgetTook is how long the last Forget that committed took.
	forgetTook time.Duration
}

// forgetChunk is how many operations the next Forget of this lease carries.
func (st *terminatingLease) forgetChunk() int {
	if st.forgetLimit <= 0 || st.forgetLimit > ledger.MaxForgetOperations {
		return ledger.MaxForgetOperations
	}
	return st.forgetLimit
}

// local notes whether the run's bound ended a local step that returned err,
// and returns err. A nil lease (an ordinary sync) notes nothing.
func (st *terminatingLease) local(ctx context.Context, err error) error {
	if st != nil && err != nil && ctx.Err() != nil {
		st.localCut = true
	}
	return err
}

// provided notes whether a provider request returned only after the run's
// bound. A nil lease (an ordinary sync) notes nothing.
func (st *terminatingLease) provided(ctx context.Context) {
	if st != nil && ctx.Err() != nil {
		st.providerCut = true
	}
}

// errYield ends a run that made progress before its bound would cut the next
// commit; the work left continues in the next run at once.
var errYield = errors.New("integration cleanup: run yields")

// yields reports whether a run that made progress should end before its
// next local commit: what is left of its bound is less than one and a half
// times the last commit took, or less than an eighth of the bound, so the
// commit would likely be cut and its work thrown away. A run without
// progress always goes on, so a slow store still moves.
func (h *Host) yields(ctx context.Context, st *terminatingLease) bool {
	deadline, ok := ctx.Deadline()
	return ok && st.progress && time.Until(deadline) < max(st.forgetTook*3/2, h.opts.OperationTimeout/8)
}

// forget runs one Forget of the lease and notes whether the run's bound
// ended it.
func (h *Host) forget(ctx context.Context, g ExecutionGuard, st *terminatingLease, l *kinds.IntegrationLedger, ops []*kinds.IntegrationOperation, proof ledger.Observation) error {
	started := time.Now()
	err := st.local(ctx, h.opts.Ledger.Forget(ctx, g.Ledger, l, ops, proof))
	st.forgetCut = err != nil && ctx.Err() != nil
	st.forgetTried = len(ops)
	if err == nil {
		st.forgetTook = time.Since(started)
	}
	return err
}

type terminatingOutcome struct {
	done bool
	// stop ends the lease without finalizing: nothing can change until the
	// instance itself does, and a changed instance replaces the job.
	stop bool
	// again: the run made progress and more work is known to remain.
	again bool
	// uncounted: the run was fenced before it counted what is left.
	uncounted bool
	reason    string
	result    Result
	err       error
}

// runTerminating is the lease of a terminating instance (D367). It repeats
// bounded runs until the finalizer is gone: each run takes one concurrency
// slot and one operation timeout, captures a fresh frame, and leaves all
// durable state where the next run, lease or process resumes from it.
func (h *Host) runTerminating(leaseCtx context.Context, j *job) {
	ctx, cancel := context.WithCancel(leaseCtx)
	stopJob := context.AfterFunc(j.ctx, cancel)
	defer stopJob()
	defer cancel()
	h.mu.Lock()
	if h.jobs[j.key] != j || j.active || j.ctx.Err() != nil || leaseCtx.Err() != nil {
		h.mu.Unlock()
		return
	}
	definition := j.frame.instance.definition
	g, err := j.frame.instance.TerminatingGuard(h.leaseID(j))
	if err != nil {
		h.mu.Unlock()
		return
	}
	j.active = true
	j.guard = g
	j.leaseCtx = ctx
	h.mu.Unlock()
	st := &terminatingLease{}
	defer func() { h.mu.Lock(); j.active = false; j.provider = nil; h.mu.Unlock() }()
	defer func() {
		cancel()
		if st.provider == nil {
			return
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), h.opts.StopTimeout)
		defer stopCancel()
		if err := st.provider.Stop(stopCtx); err != nil {
			h.opts.Logger.Warn("integration provider stop failed", "integration", j.key)
		}
	}()
	// A blocked cleanup is re-evaluated at the instance's resync interval,
	// at least every ten minutes: nothing local changes when an operator
	// resolves a conflict at the provider. A change of the object itself
	// replaces the job and runs at once.
	interval := definition.Spec.Resync.Duration
	if interval <= 0 || interval > 10*time.Minute {
		interval = 10 * time.Minute
	}
	interval = max(interval, time.Second)
	var failures, fenced uint32
	last := ""
	var counted Result
	for {
		select {
		case h.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		runCtx, runCancel := context.WithTimeout(ctx, h.opts.OperationTimeout)
		st.summary.Runs++
		st.progress, st.forgetCut, st.localCut, st.providerCut = false, false, false, false
		out, f, g := h.terminate(runCtx, j, st)
		bounded := runCtx.Err() != nil && ctx.Err() == nil
		runCancel()
		<-h.slots
		if bounded && !out.done {
			// The run reached its bound. With progress that is the ordinary
			// way a long cleanup continues. Without, either a local Forget
			// commit is too large for the bound, which the next run retries
			// with half of it, or another local step (a read, a fence, a
			// commit) was too slow for it, a failure of the store that backs
			// off like any other, or a provider that hangs. A provider
			// request that returned after the bound wins over the local
			// commits it left to run on the expired context.
			switch {
			case st.progress:
				out.again, out.reason = true, ReasonCleanup
			case st.forgetCut && st.forgetTried > 1:
				st.forgetLimit = st.forgetTried / 2
				out.again, out.reason, out.err = true, ReasonCleanup, nil
			case st.localCut && !st.providerCut:
				out.err = &Failure{Kind: FailureTemporary}
				out.reason = ReasonCleanupFailed
			default:
				out.err = &Failure{Kind: FailureTemporary}
				if st.publisher {
					out.reason = ReasonProviderFailed
				}
			}
		}
		if out.done {
			h.opts.Logger.Info("integration finalized", "integration", j.key, "released", st.summary.Released, "recordSetsDeleted", st.summary.RecordSetsDeleted, "recordsForgotten", st.summary.RecordsForgotten, "runs", st.summary.Runs)
			h.Wake()
			return
		}
		if ctx.Err() != nil {
			return
		}
		if out.uncounted {
			// A run that was fenced before it could count did not look: it
			// keeps what the last run reported instead of claiming that
			// nothing is left.
			out.result = counted
		} else {
			counted = out.result
		}
		h.setLocal(j, PhaseTerminating, out.reason, out.result, true)
		if out.reason != last {
			h.opts.Logger.Info("integration terminating", "integration", j.key, "reason", out.reason, "remaining", out.result.Remaining, "blocked", out.result.Conflicts, "pending", out.result.Pending)
			last = out.reason
		}
		if f.instance != nil && !errors.Is(out.err, ErrFenced) {
			statusCtx, statusCancel := context.WithTimeout(ctx, h.opts.OperationTimeout)
			if scoped, err := h.scopedOriginal(statusCtx, j, f, g); err == nil {
				h.publishStatus(scoped, j, g)
			}
			statusCancel()
			// A changed status is a new resource version of the object; the
			// next run needs the frame that carries it.
			h.Wake()
		}
		if out.stop {
			return
		}
		delay := interval
		switch {
		case out.again:
			failures, fenced = 0, 0
			delay = h.opts.PollInterval
		case errors.Is(out.err, ErrFenced), errors.Is(out.err, ErrStalePreview), errors.Is(out.err, store.ErrConflict):
			// The frame or a ledger version moved under this run. The next
			// run reads both again; back off so a lasting refusal stays cheap.
			if fenced < 16 {
				fenced++
			}
			delay = min(interval, h.opts.PollInterval<<min(fenced-1, 10))
		case errors.Is(out.err, ledger.ErrBudget):
			if failures < 32 {
				failures++
			}
			delay = min(5*time.Minute, time.Second<<min(failures, 9))
		default:
			if failures < 32 {
				failures++
			}
			if retry, ok := h.retryDelay(out.err, failures); ok {
				delay = retry
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-j.reconcile:
			timer.Stop()
			failures, fenced = 0, 0
		case <-timer.C:
		}
	}
}

// terminate is one bounded run. It returns the frame and guard it captured
// so the caller can publish the outcome under the same authority.
func (h *Host) terminate(ctx context.Context, j *job, st *terminatingLease) (terminatingOutcome, frame, ExecutionGuard) {
	f, g, err := h.captured(j)
	if err != nil {
		return terminatingOutcome{reason: ReasonAuthorityChanged, uncounted: true, err: ErrFenced}, frame{}, ExecutionGuard{}
	}
	ctx, err = h.scopedOriginal(ctx, j, f, g)
	if err != nil {
		return terminatingOutcome{reason: ReasonAuthorityChanged, uncounted: true, err: ErrFenced}, frame{}, ExecutionGuard{}
	}
	if err := st.local(ctx, h.fence(ctx, j, g)); err != nil {
		return terminatingOutcome{reason: ReasonAuthorityChanged, uncounted: true, err: ErrFenced}, f, g
	}
	if g.Mode == GuardRelease {
		st.summary.Released = true
		if err := st.local(ctx, h.opts.Finalize(ctx, g, st.summary)); err != nil {
			return failed(ctx, ReasonReleaseFailed, Result{}, err), f, g
		}
		return terminatingOutcome{done: true}, f, g
	}
	// What is left is counted before anything can stop the run, so every
	// outcome reports it: the ledger rows of the instance not yet forgotten.
	result := Result{}
	total, _, err := h.ledgerCounts(ctx, f.instance.owner, nil)
	if st.local(ctx, err) != nil {
		// Nothing was counted: the run keeps what the last run reported
		// instead of claiming that nothing is left.
		out := failed(ctx, ReasonCleanupFailed, result, err)
		out.uncounted = true
		return out, f, g
	}
	result.Remaining = total
	if !st.inspected {
		provider, err := h.opts.Factory(f.instance.definition.Spec.Type)
		if err != nil || provider == nil {
			return terminatingOutcome{reason: ReasonProviderUnavailable, result: result, err: ErrUnsupported}, f, g
		}
		_, st.publisher = provider.(Publisher)
		st.inspected = true
	}
	if st.publisher {
		// Only a publisher can own provider-side records, and only its
		// cleanup needs a credential and a reachable provider.
		if f.instance.credentialError {
			return terminatingOutcome{reason: ReasonCredentials, stop: true, result: result, err: ErrCredentials}, f, g
		}
		if st.provider == nil {
			if err := h.provisionCleanup(ctx, j, f, st); err != nil {
				return failed(ctx, ReasonProvisionFailed, result, err), f, g
			}
		}
		clean, out := h.cleanupRecords(ctx, j, f, g, st, total)
		if !clean {
			return out, f, g
		}
		result = out.result
	} else if total > 0 {
		// No publisher ever wrote for this instance, so a ledger row of it has
		// no destination this job could read.
		return terminatingOutcome{reason: ReasonDestinationRemoved, result: result}, f, g
	}
	if out, ok := h.forgetLeftovers(ctx, f, g, st, result); !ok {
		return out, f, g
	}
	if err := st.local(ctx, h.fence(ctx, j, g)); err != nil {
		return terminatingOutcome{reason: ReasonAuthorityChanged, result: result, err: ErrFenced}, f, g
	}
	purged, err := h.opts.Purge(ctx, g)
	err = st.local(ctx, err)
	st.summary.Purged.Objects += purged.Objects
	st.summary.Purged.SourceRows += purged.SourceRows
	st.summary.Purged.Allocations += purged.Allocations
	st.summary.Purged.Deliveries += purged.Deliveries
	st.summary.Purged.Clients += purged.Clients
	if err != nil {
		return failed(ctx, ReasonCleanupFailed, result, err), f, g
	}
	if err := st.local(ctx, h.fence(ctx, j, g)); err != nil {
		return terminatingOutcome{reason: ReasonAuthorityChanged, result: result, err: ErrFenced}, f, g
	}
	if err := st.local(ctx, h.opts.Finalize(ctx, g, st.summary)); err != nil {
		return failed(ctx, ReasonCleanupFailed, result, err), f, g
	}
	return terminatingOutcome{done: true}, f, g
}

// failed classifies an error of a terminating run. The reason given is used
// only when the error itself does not say more.
func failed(ctx context.Context, reason string, result Result, err error) terminatingOutcome {
	out := terminatingOutcome{reason: reason, result: result, err: err}
	switch {
	case errors.Is(err, ErrFenced):
		out.reason, out.err = ReasonAuthorityChanged, ErrFenced
	case errors.Is(err, ErrRemains):
		out.reason = ReasonLocalStateRemains
	case errors.Is(err, ledger.ErrBudget):
		out.reason, out.err = ReasonBudgetExhausted, ledger.ErrBudget
	case errors.Is(err, ledger.ErrPending):
		out.reason = ReasonPendingRecovery
	case errors.Is(err, plan.ErrIncomplete):
		out.reason = ReasonSnapshotIncomplete
	case errors.Is(err, ErrCapacity), errors.Is(err, plan.ErrCapacity):
		out.reason = ReasonCapacity
	case ctx.Err() != nil:
		// The run ended at its bound or with its lease; the caller decides.
	default:
		if f := retryFailure(err); f != nil {
			out.err = f
			if f.Kind == FailureRateLimited {
				out.reason = ReasonRateLimited
			} else if reason != ReasonProvisionFailed {
				out.reason = ReasonProviderFailed
			}
		} else if errors.Is(err, ErrProvider) && reason != ReasonProvisionFailed {
			out.reason = ReasonProviderFailed
		}
	}
	return out
}

// provisionCleanup builds and checks the publisher of a cleanup lease with
// the captured named credentials, like an ordinary lease, but never starts
// it: a terminating instance observes and delivers nothing.
func (h *Host) provisionCleanup(ctx context.Context, j *job, f frame, st *terminatingLease) error {
	provider, err := h.opts.Factory(f.instance.definition.Spec.Type)
	if err != nil || provider == nil {
		return ErrUnsupported
	}
	err = provider.Provision(ctx, providerHost{h: h, j: j}, f.instance.ProviderSpec(), f.instance.Secrets())
	if err == nil {
		err = provider.Validate(ctx)
	}
	if err == nil {
		var checked integration.Status
		checked, err = provider.Check(ctx)
		if err == nil && (!checked.Authenticated || !checked.Reachable) {
			err = ErrProvider
		}
	}
	if err != nil {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), h.opts.StopTimeout)
		defer stopCancel()
		if stopErr := provider.Stop(stopCtx); stopErr != nil {
			h.opts.Logger.Warn("integration provider stop failed", "integration", j.key)
		}
		return terminatingError(ctx, err)
	}
	st.provider = provider
	h.mu.Lock()
	j.provider = provider
	h.mu.Unlock()
	return nil
}

// terminatingError keeps the two sentinels the terminating loop schedules
// on and sanitizes everything else like an ordinary provider error.
func terminatingError(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ledger.ErrBudget):
		return ledger.ErrBudget
	case errors.Is(err, ErrFenced):
		return ErrFenced
	}
	return safeError(ctx, err)
}

// cleanupPublications reads every destination the terminating instance's
// configuration still names: its complete remote snapshot and the instance's
// ledger rows there, bound into a cleanup plan input with nothing desired.
func (h *Host) cleanupPublications(ctx context.Context, j *job, f frame, g ExecutionGuard, st *terminatingLease) ([]publicationInput, error) {
	publisher, ok := j.provider.(Publisher)
	if !ok {
		return nil, ErrUnsupported
	}
	pubs, err := publisher.Destinations(ctx)
	if err != nil {
		return nil, terminatingError(ctx, err)
	}
	if len(pubs) > 256 {
		return nil, ErrCapacity
	}
	seen := map[kinds.Destination]bool{}
	out := make([]publicationInput, 0, len(pubs))
	for _, pub := range pubs {
		if pub.Destination.Validate() != nil || seen[pub.Destination] {
			return nil, ErrInvalid
		}
		seen[pub.Destination] = true
		remote, err := publisher.Records(ctx, pub.Destination)
		if err != nil {
			return nil, terminatingError(ctx, err)
		}
		if !remote.Complete {
			return nil, plan.ErrIncomplete
		}
		ledgers, err := h.ledgers(ctx, f.instance.owner, pub.Destination)
		if st.local(ctx, err) != nil {
			return nil, err
		}
		in := publicationInput{pub, remote, plan.Input{Owner: f.instance.owner, Destination: pub.Destination, ConfigDigest: g.Ledger.ConfigDigest, CredentialDigest: g.Ledger.CredentialDigest, SourceDigest: cleanupSource, Policy: pub.Policy, Current: remote.Records, Ledger: ledgers, DesiredComplete: true, CurrentComplete: true, AdoptUnowned: pub.AdoptUnowned, AdoptExternalDNSOwner: pub.AdoptExternalDNSOwner, Cleanup: true}}
		if _, err := validatePendingObservation(in.planInput); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	slices.SortFunc(out, func(a, b publicationInput) int {
		x, _ := json.Marshal(a.publication.Destination)
		y, _ := json.Marshal(b.publication.Destination)
		return strings.Compare(string(x), string(y))
	})
	if err := st.local(ctx, h.fence(ctx, j, g)); err != nil {
		return nil, err
	}
	return out, nil
}

// cleanupRecords removes what the instance provably owns at its provider and
// then its ledger: pending intents are resolved from a complete reread
// first, proven record sets are deleted through the ordinary intent, fence,
// provider, receipt and confirm sequence, and a ledger row is forgotten once
// a fresh complete reread shows its record absent. It reports clean only
// when no record, ledger row or uncertain operation of the instance is left.
// Anything it cannot remove is counted and named, never skipped. remaining is
// the number of ledger rows the run started with; every outcome that stops
// before the forgetting carries it.
//
// The ledger is read one configured destination at a time, as an ordinary
// sync reads it, so cleanup admits every instance a sync admits. Rows of a
// destination the configuration no longer names are only counted.
func (h *Host) cleanupRecords(ctx context.Context, j *job, f frame, g ExecutionGuard, st *terminatingLease, remaining int) (bool, terminatingOutcome) {
	result := Result{Remaining: remaining}
	owner := f.instance.owner
	inputs, err := h.cleanupPublications(ctx, j, f, g, st)
	if err != nil {
		return false, failed(ctx, ReasonCleanupFailed, result, err)
	}
	recovered := false
	for _, in := range inputs {
		for _, l := range in.planInput.Ledger {
			if l.Spec.Pending == nil {
				continue
			}
			recovered = true
			if err := h.recover(ctx, j, g, in, l, st); err != nil {
				result.Pending++
				return false, failed(ctx, ReasonCleanupFailed, result, err)
			}
		}
	}
	if recovered {
		// Recovery changed ledger versions; rebuild complete observations.
		if inputs, err = h.cleanupPublications(ctx, j, f, g, st); err != nil {
			return false, failed(ctx, ReasonCleanupFailed, result, err)
		}
	}
	applied := Result{}
	var applyErr error
	for _, in := range inputs {
		var p *plan.Plan
		if p, applyErr = plan.Build(in.planInput); applyErr != nil {
			break
		}
		if applyErr = h.applyPlan(ctx, j, g, inputs, p, &applied, st); applyErr != nil {
			break
		}
	}
	result.Applied, result.Pending = applied.Applied, applied.Pending
	st.summary.RecordSetsDeleted += applied.Applied
	st.progress = st.progress || applied.Applied > 0
	if applyErr != nil {
		return false, failed(ctx, ReasonCleanupFailed, result, applyErr)
	}
	if result.Applied > 0 {
		// Judge the destinations from what they hold now, not from receipts.
		if inputs, err = h.cleanupPublications(ctx, j, f, g, st); err != nil {
			return false, failed(ctx, ReasonCleanupFailed, result, err)
		}
	}
	observed := h.opts.Now()
	published := make([]kinds.Destination, 0, len(inputs))
	planned, conflicts, unproven := 0, 0, 0
	// Rows that carry the owner's marker, and every provider ID a destination
	// holds: a ledger row is forgotten only when neither still shows it.
	marked := map[kinds.RecordIdentity]bool{}
	present := map[kinds.Destination]map[string]bool{}
	for _, in := range inputs {
		published = append(published, in.publication.Destination)
		p, err := plan.Build(in.planInput)
		if err != nil {
			return false, failed(ctx, ReasonCleanupFailed, result, err)
		}
		planned += len(p.Operations)
		for _, kept := range p.Retained {
			if kept.Reason == plan.RetainedUnproven {
				unproven++
			} else {
				conflicts++
			}
		}
		ids := map[string]bool{}
		for _, row := range in.remote.Records {
			for _, id := range row.IDs {
				ids[id] = true
			}
			if row.Owner.ClusterID == owner.ClusterID && row.Owner.IntegrationUID == owner.UID {
				if n, err := plan.Normalize(row.Endpoint); err == nil {
					marked[kinds.RecordIdentity{Owner: owner, Destination: in.publication.Destination, Key: kinds.RecordKey{Name: n.DNSName, Type: n.RecordType, SetIdentifier: n.SetIdentifier}}] = true
				}
			}
		}
		present[in.publication.Destination] = ids
	}
	// Every ledger row of the instance, on any destination. The rows of the
	// destinations read above are in their inputs; the others are stranded.
	left, stranded, err := h.ledgerCounts(ctx, owner, published)
	if st.local(ctx, err) != nil {
		return false, failed(ctx, ReasonCleanupFailed, result, err)
	}
	result.Remaining = left
	// A row leaves with the operations of its record that one bounded batch
	// holds, read when the first row is due. The rest of a longer history is
	// forgotten as leftovers once no ledger row is left.
	var history map[kinds.RecordIdentity][]*kinds.IntegrationOperation
	for _, in := range inputs {
		ids := present[in.publication.Destination]
		for _, l := range in.planInput.Ledger {
			if l.Spec.Pending != nil {
				result.Pending++
				continue
			}
			// Nothing is forgotten at a key that still shows the owner's marker:
			// the plan above deletes that record set or names why it stays.
			if marked[l.Spec.Record] {
				continue
			}
			proof := ledger.Observation{}
			if l.Spec.ContentHash != "" || len(l.Spec.ProviderIDs) != 0 {
				// The baseline still lists provider content, so no delete of this
				// job confirmed its absence. It may go only when this fresh
				// complete snapshot holds none of its provider IDs any more.
				survives := false
				for _, id := range l.Spec.ProviderIDs {
					survives = survives || ids[id]
				}
				if survives {
					unproven++
					continue
				}
				proof = ledger.Observation{ObservedAt: observed}
			}
			if history == nil {
				operations, err := h.operationBatch(ctx, owner)
				if st.local(ctx, err) != nil {
					result.Conflicts = conflicts + unproven
					return false, failed(ctx, ReasonCleanupFailed, result, err)
				}
				history = map[kinds.RecordIdentity][]*kinds.IntegrationOperation{}
				for _, op := range operations {
					history[op.Spec.Record] = append(history[op.Spec.Record], op)
				}
			}
			if h.yields(ctx, st) {
				result.Conflicts = conflicts + unproven
				return false, terminatingOutcome{reason: ReasonCleanup, result: result, again: true}
			}
			ops := history[l.Spec.Record]
			first := ops[:min(len(ops), st.forgetChunk())]
			if err := h.forget(ctx, g, st, l, first, proof); err != nil {
				result.Conflicts = conflicts + unproven
				if errors.Is(err, ledger.ErrBaseline) {
					// The observation aged past its bound; the next run rereads.
					return false, terminatingOutcome{reason: ReasonCleanup, result: result, again: st.progress}
				}
				return false, failed(ctx, ReasonCleanupFailed, result, err)
			}
			left--
			result.Remaining = left
			st.progress = true
			st.summary.RecordsForgotten++
			st.summary.OperationsForgotten += len(first)
		}
	}
	result.Conflicts = conflicts + unproven
	switch {
	case result.Pending > 0:
		return false, terminatingOutcome{reason: ReasonPendingRecovery, result: result}
	case stranded > 0:
		return false, terminatingOutcome{reason: ReasonDestinationRemoved, result: result}
	case conflicts > 0:
		return false, terminatingOutcome{reason: ReasonRecordsConflict, result: result}
	case unproven > 0:
		return false, terminatingOutcome{reason: ReasonOwnershipUnproven, result: result}
	case left > 0 || planned > 0:
		// Record sets appeared or changed between the plan and the reread.
		return false, terminatingOutcome{reason: ReasonCleanup, result: result, again: st.progress}
	}
	return true, terminatingOutcome{result: result}
}

// forgetLeftovers removes operations of the instance whose record has no
// ledger row: intents whose reservation failed, and history beyond what left
// with its ledger row. It runs only once no ledger row of the instance is
// left, and forgets one stored page at a time, so it never holds more than a
// page of a history, whose length only the store's own limits bound.
// It reports done only after a complete read found no operation: a read that
// forgot something is repeated, because deleting under it can hide rows. Each
// commit carries at most the lease's forget chunk, and a run that made
// progress ends before its bound would cut the next commit.
func (h *Host) forgetLeftovers(ctx context.Context, f frame, g ExecutionGuard, st *terminatingLease, result Result) (terminatingOutcome, bool) {
	for {
		forgotten := false
		err := st.local(ctx, h.operationPages(ctx, f.instance.owner, func(page []*kinds.IntegrationOperation) (bool, error) {
			history := map[kinds.RecordIdentity][]*kinds.IntegrationOperation{}
			var records []kinds.RecordIdentity
			for _, op := range page {
				if _, seen := history[op.Spec.Record]; !seen {
					records = append(records, op.Spec.Record)
				}
				history[op.Spec.Record] = append(history[op.Spec.Record], op)
			}
			result.Remaining = len(records)
			for _, record := range records {
				for ops := history[record]; len(ops) > 0; {
					if h.yields(ctx, st) {
						return false, errYield
					}
					chunk := ops[:min(len(ops), st.forgetChunk())]
					if err := h.forget(ctx, g, st, nil, chunk, ledger.Observation{}); err != nil {
						return false, err
					}
					forgotten, st.progress = true, true
					st.summary.OperationsForgotten += len(chunk)
					ops = ops[len(chunk):]
				}
				result.Remaining--
			}
			return true, nil
		}))
		if errors.Is(err, errYield) {
			return terminatingOutcome{reason: ReasonCleanup, result: result, again: true}, false
		}
		if err != nil {
			return failed(ctx, ReasonCleanupFailed, result, err), false
		}
		if !forgotten {
			return terminatingOutcome{}, true
		}
	}
}
