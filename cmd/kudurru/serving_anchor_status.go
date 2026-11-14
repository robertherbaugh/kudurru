package main

import (
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/api"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
)

// Anchor worker states (api.RootAnchorWorker.State). The set is fixed: it is
// also the label set of kudurru_root_anchor_worker_state.
const (
	anchorWorkerNotRunning   = "not_running"
	anchorWorkerRunning      = "running"
	anchorWorkerClockWait    = "clock_wait"
	anchorWorkerRetry        = "retry"
	anchorWorkerRaced        = "raced"
	anchorWorkerWaitingImage = "waiting_image"
	anchorWorkerRetired      = "retired"
	anchorWorkerStopped      = "stopped"
)

var anchorWorkerStates = []string{anchorWorkerNotRunning, anchorWorkerRunning, anchorWorkerClockWait, anchorWorkerRetry, anchorWorkerRaced, anchorWorkerWaitingImage, anchorWorkerRetired, anchorWorkerStopped}

// Outcomes of one elected updater acquisition, the label set of
// kudurru_root_anchor_updater_acquisitions_total: applied (the reply was
// committed or found unchanged), raced (an ordinary commit moved the
// canonical generation while the reply was in flight, so the reply was
// discarded and a new acquisition follows), failed (transport; a new
// acquisition is scheduled), canceled (the writer term or process ended
// while it was in flight; nothing is retried in that term) and refused (any
// other error, which stops the updater for the term: a trust or admission
// refusal, or a store failure).
const (
	anchorAcquisitionApplied  = "applied"
	anchorAcquisitionRaced    = "raced"
	anchorAcquisitionFailed   = "failed"
	anchorAcquisitionCanceled = "canceled"
	anchorAcquisitionRefused  = "refused"
)

var anchorAcquisitionOutcomes = []string{anchorAcquisitionApplied, anchorAcquisitionRaced, anchorAcquisitionFailed, anchorAcquisitionCanceled, anchorAcquisitionRefused}

// Pull results (api.RootAnchorPull.Result).
const (
	anchorPullInstalled   = "installed"
	anchorPullUnreachable = "unreachable"
	anchorPullRejected    = "rejected"
	anchorPullRefused     = "refused"
)

var anchorRefusalReasons = []sqlite.AnchorRefusalReason{sqlite.AnchorRefusedHistory, sqlite.AnchorRefusedPolicy, sqlite.AnchorRefusedDormantOwner,
	sqlite.AnchorRefusedRecoveryHistory, sqlite.AnchorRefusedCarriedRevocation, sqlite.AnchorRefusedNotSuccessor}

type anchorWorkerView struct {
	state     string
	since     time.Time
	next      time.Time
	err       string
	lastError string
}

type anchorPullView struct {
	result       string
	at           time.Time
	refusal      *sqlite.AnchorRefusal
	refusedSince time.Time
}

// anchorStatusBoard records what the anchor workers and the member puller
// last did, for GET /cluster/anchors and the kudurru_root_anchor_* metrics. It
// is memory only and written only by the workers it describes; nothing reads
// it to decide anything. The zero value is ready to use.
type anchorStatusBoard struct {
	mu         sync.Mutex
	now        func() time.Time
	updater    anchorWorkerView
	supervisor anchorWorkerView
	pull       anchorPullView
	// acquisitions counts this process's elected updater acquisitions (root
	// queries), by outcome, across every writer term.
	acquisitions map[string]uint64
}

func (b *anchorStatusBoard) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *anchorStatusBoard) set(view *anchorWorkerView, state string, next time.Time, err error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if view.state != state {
		view.since = b.clock()
	}
	view.state, view.next, view.err = state, next, ""
	if err != nil {
		view.err = err.Error()
		view.lastError = view.err
	}
}

func (b *anchorStatusBoard) setUpdater(state string, next time.Time, err error) {
	if b != nil {
		b.set(&b.updater, state, next, err)
	}
}

func (b *anchorStatusBoard) setSupervisor(state string, next time.Time, err error) {
	if b != nil {
		b.set(&b.supervisor, state, next, err)
	}
}

// countAcquisition records the outcome of one updater acquisition.
func (b *anchorStatusBoard) countAcquisition(outcome string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.acquisitions == nil {
		b.acquisitions = make(map[string]uint64, len(anchorAcquisitionOutcomes))
	}
	b.acquisitions[outcome]++
}

// observePull records one coordinator pull result. A refusal of the primary's
// root-anchor owner is logged once when it begins and when its class changes,
// and once when it ends with an install.
func (b *anchorStatusBoard) observePull(err error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	var refusal *sqlite.AnchorRefusal
	switch {
	case err == nil:
		if b.pull.refusal != nil {
			slog.Info("member installed the primary's root anchor state; refusal ended", "refused", string(b.pull.refusal.Reason), "since", b.pull.refusedSince)
		}
		b.pull = anchorPullView{result: anchorPullInstalled, at: now}
	case errors.As(err, &refusal):
		if b.pull.refusal == nil || b.pull.refusal.Reason != refusal.Reason || !slices.Equal(b.pull.refusal.Keys, refusal.Keys) {
			slog.Warn("member refuses the primary's root anchor state and stays on its own last-good data", "reason", string(refusal.Reason), "keys", len(refusal.Keys))
		}
		since := b.pull.refusedSince
		if b.pull.refusal == nil {
			since = now
		}
		b.pull = anchorPullView{result: anchorPullRefused, at: now, refusal: &sqlite.AnchorRefusal{Reason: refusal.Reason, Keys: slices.Clone(refusal.Keys)}, refusedSince: since}
	case errors.Is(err, clusterreplica.ErrUnavailable):
		// An unreachable primary says nothing about the refusal: keep it.
		b.pull.result, b.pull.at = anchorPullUnreachable, now
	default:
		b.pull = anchorPullView{result: anchorPullRejected, at: now}
	}
}

type anchorStatusSnapshot struct {
	updater, supervisor anchorWorkerView
	pull                anchorPullView
	acquisitions        map[string]uint64
}

func (b *anchorStatusBoard) snapshot() anchorStatusSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := anchorStatusSnapshot{updater: b.updater, supervisor: b.supervisor, pull: b.pull, acquisitions: make(map[string]uint64, len(b.acquisitions))}
	for outcome, n := range b.acquisitions {
		out.acquisitions[outcome] = n
	}
	if out.pull.refusal != nil {
		copied := *out.pull.refusal
		copied.Keys = slices.Clone(copied.Keys)
		out.pull.refusal = &copied
	}
	return out
}

func timePointer(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func (v anchorWorkerView) api() api.RootAnchorWorker {
	state := v.state
	if state == "" {
		state = anchorWorkerNotRunning
	}
	return api.RootAnchorWorker{State: state, Since: timePointer(v.since), Next: timePointer(v.next), Error: v.err, LastError: v.lastError}
}

// anchorMetrics exports the board as kudurru_root_anchor_* gauges and the
// updater's acquisition counter. Collect reads memory only: no store read,
// lock wait or network I/O.
type anchorMetrics struct {
	runtime      *servingRuntime
	worker       *prometheus.Desc
	refused      *prometheus.Desc
	admission    *prometheus.Desc
	acquisitions *prometheus.Desc
}

func newAnchorMetrics(s *servingRuntime) *anchorMetrics {
	return &anchorMetrics{runtime: s,
		worker:       prometheus.NewDesc("kudurru_root_anchor_worker_state", "Managed root-anchor worker state (1 for the current state): the elected updater or the deny-only supervisor.", []string{"worker", "state"}, nil),
		refused:      prometheus.NewDesc("kudurru_root_anchor_adoption_refused", "Whether this member refuses the primary's root-anchor state at its last pull, by reason (1 refusing).", []string{"reason"}, nil),
		admission:    prometheus.NewDesc("kudurru_root_anchor_admission_current", "Whether this process holds managed root-anchor serving admission (0 on a managed node means a restart is needed to publish again).", nil, nil),
		acquisitions: prometheus.NewDesc("kudurru_root_anchor_updater_acquisitions_total", "Root-anchor acquisitions the elected updater of this process made, by outcome: applied, raced (an ordinary commit landed while the reply was in flight; the reply was discarded and a new acquisition follows), failed (transport), refused (terminal for the writer term) or canceled (the writer term or process ended during the query).", []string{"outcome"}, nil),
	}
}

func (m *anchorMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.worker
	ch <- m.refused
	ch <- m.admission
	ch <- m.acquisitions
}

func (m *anchorMetrics) Collect(ch chan<- prometheus.Metric) {
	snap := m.runtime.anchorStatus.snapshot()
	for worker, view := range map[string]anchorWorkerView{"updater": snap.updater, "supervisor": snap.supervisor} {
		current := view.state
		if current == "" {
			current = anchorWorkerNotRunning
		}
		for _, state := range anchorWorkerStates {
			value := 0.0
			if state == current {
				value = 1
			}
			ch <- prometheus.MustNewConstMetric(m.worker, prometheus.GaugeValue, value, worker, state)
		}
	}
	// A primary does not pull, so a refusal recorded before a promotion (or
	// before this member's branch was kept) is no longer current: export it
	// only while this member is not the primary, as GET /cluster/anchors does.
	var mode replication.Mode
	if m.runtime != nil && m.runtime.opts.Store != nil {
		mode = m.runtime.opts.Store.Authority().Mode
	}
	refusal := anchorCurrentRefusal(snap.pull, mode)
	for _, reason := range anchorRefusalReasons {
		value := 0.0
		if refusal != nil && refusal.Reason == reason {
			value = 1
		}
		ch <- prometheus.MustNewConstMetric(m.refused, prometheus.GaugeValue, value, string(reason))
	}
	admitted := 0.0
	if m.runtime.anchorAdmissionCurrent() {
		admitted = 1
	}
	ch <- prometheus.MustNewConstMetric(m.admission, prometheus.GaugeValue, admitted)
	for _, outcome := range anchorAcquisitionOutcomes {
		ch <- prometheus.MustNewConstMetric(m.acquisitions, prometheus.CounterValue, float64(snap.acquisitions[outcome]), outcome)
	}
}

// anchorCurrentRefusal is the recorded refusal of the primary's owner while
// this member is in mode. A primary does not pull, so nothing it recorded as a
// follower is current.
func anchorCurrentRefusal(pull anchorPullView, mode replication.Mode) *sqlite.AnchorRefusal {
	if mode == replication.Primary {
		return nil
	}
	return pull.refusal
}

func (s *servingRuntime) anchorAdmissionCurrent() bool {
	return s != nil && s.anchors != nil && s.anchors.admission != nil && s.anchors.admission.current()
}

// rootAnchorProjection is the read-only projection of one installed image's
// root-anchor owner and this member's carrier, shared by GET /cluster/anchors
// and `kudurru anchors status --offline`. Remaining hold-down is computed
// from the retained credit.
func rootAnchorProjection(image *replication.Image, local *sqlite.AnchorLocalView, a replication.Authority) (api.RootAnchorOwner, *api.RootAnchorLocal, api.RootAnchorAuthority, error) {
	authority := api.RootAnchorAuthority{Mode: string(a.Mode), MemberID: a.MemberID, TermEpoch: a.Term.Epoch, InstalledTermEpoch: a.Installed.Term.Epoch,
		Conflict: a.Conflict != nil || a.Divergence != nil}
	authority.FencedBehindTerm = a.Mode == replication.Fenced && !authority.Conflict && a.Installed.Term.ClusterID == a.Term.ClusterID && a.Installed.Term.Epoch < a.Term.Epoch
	owner := api.RootAnchorOwner{Phase: "awaiting", Keys: []api.RootAnchorKey{}, RecoveryGenerations: []uint64{}}
	tags := map[string]uint16{}
	revoked := map[string]bool{}
	if image != nil && !accepted.IsAwaitingOwner(image.Payload.Runtime) {
		snapshot, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
		if err != nil {
			return owner, nil, authority, err
		}
		root, version := snapshot.Inspect(), snapshot.Version()
		owner.Phase, owner.Digest, owner.Generation, owner.PureGeneration = string(root.Phase), version.Digest, version.Generation, version.Pure.Generation
		owner.PolicyID, owner.SeedDigest = root.PolicyID, root.SeedDigest
		if root.Phase == accepted.Managed {
			state, err := snapshot.State()
			if err != nil {
				return owner, nil, authority, err
			}
			view := state.Inspect()
			owner.Quarantined, owner.ClockUncertain = view.Quarantined, view.ClockUncertain
			owner.CheckpointAt, owner.NextRefresh = timePointer(view.Checkpoint.Wall), timePointer(view.Schedule.NextRefresh)
			owner.RetryIntervalSeconds = view.Schedule.RetryInterval.Seconds()
			for _, r := range view.Recoveries {
				owner.RecoveryGenerations = append(owner.RecoveryGenerations, r.Generation)
			}
			for _, key := range view.Keys {
				out := api.RootAnchorKey{ID: key.ID, Status: string(key.Status), FirstSeen: key.FirstSeen.UTC(),
					AcceptedAt: timePointer(key.AcceptedAt), MissingSince: timePointer(key.MissingSince), RevokedAt: timePointer(key.RevokedAt)}
				if tag, algorithm, err := key.KeyTag(); err == nil {
					out.KeyTag, out.Algorithm = tag, algorithm
					tags[key.ID] = tag
				}
				if key.Status == trustanchors.AddPending {
					hold := max(trustanchors.AddHoldDown, time.Duration(key.OriginalTTL)*time.Second)
					out.HoldDownRemainingSeconds = max(0, (hold - key.Elapsed).Seconds())
				}
				revoked[key.ID] = key.Status == trustanchors.Revoked
				owner.Keys = append(owner.Keys, out)
			}
		}
	}
	if local == nil {
		return owner, nil, authority, nil
	}
	carrier := &api.RootAnchorLocal{Phase: local.Phase, OwnerGeneration: local.Owner.Generation, RetiredThrough: local.RetiredThrough, Terminal: local.Terminal, Denied: []api.RootAnchorDenial{}}
	carrier.Retired = local.Phase == "managed" && (local.Terminal || local.RetiredThrough >= local.Owner.Generation)
	for _, d := range local.Denied {
		carrier.Denied = append(carrier.Denied, api.RootAnchorDenial{KeyTag: tags[d.ID], ID: d.ID, FirstOwnerGeneration: d.FirstOwner.Generation, RevokedByOwner: revoked[d.ID]})
	}
	return owner, carrier, authority, nil
}

// rootAnchorWarnings are the fixed sentences of api.RootAnchorStatus.Warnings.
func rootAnchorWarnings(status api.RootAnchorStatus) []string {
	warnings := []string{}
	if status.Owner.Quarantined {
		warnings = append(warnings, "every trusted root key is lost; recover with `kudurru anchors recover --offline` on the stopped primary")
	}
	if status.Local != nil {
		if status.Local.Terminal {
			warnings = append(warnings, "this member's root-anchor output is terminal; replace the member")
		} else if status.Local.Retired {
			warnings = append(warnings, "this member retired its output after a signed root key revocation; it answers SERVFAIL until it installs state that revokes the key")
		}
		for _, d := range status.Local.Denied {
			if !d.RevokedByOwner {
				warnings = append(warnings, "this member denied a root key that its installed state still trusts; as the primary it would answer SERVFAIL")
				break
			}
		}
	}
	if status.Authority.FencedBehindTerm {
		warnings = append(warnings, "this member is fenced by a newer term whose data it never installed; it serves its own last-good data DNS-only")
	}
	if status.Pull != nil && status.Pull.Refusal != "" {
		warnings = append(warnings, "this member refuses the primary's root-anchor state ("+status.Pull.Refusal+"); see \"A member returns with different trust-anchor state\"")
	}
	if status.AnchorMode == string(config.RootAnchorsManaged) && status.ManagedTopology != "" && status.ManagedTopology != "disabled" &&
		status.Owner.Phase == string(accepted.Managed) && !status.AdmissionCurrent {
		warnings = append(warnings, "managed root-anchor admission is closed for this process; restart it to publish again")
	}
	for name, worker := range map[string]api.RootAnchorWorker{"updater": status.Updater, "supervisor": status.Supervisor} {
		if worker.State == anchorWorkerStopped {
			warnings = append(warnings, "the root-anchor "+name+" stopped: "+worker.Error)
		}
	}
	slices.Sort(warnings)
	return warnings
}
