package servingbuild

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PrimaryStatusOptions mounts only on the committed SQLite controller. Pin must
// be the complete dependency pin used by primary commands and publication.
type PrimaryStatusOptions struct {
	Store     *sqlite.Store
	Publisher *serving.Publisher
	Pin       currentguard.PinFunc
	Now       func() time.Time
}

// PrimaryStatus projects exact committed data into public diagnostic status.
// It never compiles desired records or stamps a serial. All writes are fenced
// by one original whole lease and the same concrete source/lifecycle/head cut.
type PrimaryStatus struct{ opts PrimaryStatusOptions }

// NewPrimaryStatus starts no workers and borrows all supplied owners.
func NewPrimaryStatus(o PrimaryStatusOptions) (*PrimaryStatus, error) {
	if o.Store == nil || o.Publisher == nil || o.Pin == nil {
		return nil, serving.ErrComponents
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &PrimaryStatus{opts: o}, nil
}

// Subscribe is a close-and-replace broadcast; controllers resubscribe before
// scanning. Deadline rechecks use Reconcile's bounded retryAfter separately.
func (s *PrimaryStatus) Subscribe() <-chan struct{} { return s.opts.Publisher.Subscribe() }

const primaryStatusRetry = time.Second

// Reconcile uses the ORIGINAL carried elector token; it never asks the store
// for a new writer. Pending protected publication defers WITHOUT writing false
// readiness, preventing a status-RV/source-alignment feedback loop.
func (s *PrimaryStatus) Reconcile(ctx context.Context, cached *public.DNSZone) (bool, time.Duration, error) {
	if s == nil || ctx == nil || cached == nil || cached.Namespace != "" || cached.Spec.Type != public.ZoneTypePrimary {
		return false, 0, pr.ErrInvalid
	}
	writer, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok || writer.Term().Validate() != nil || writer.Incarnation() == 0 {
		return false, 0, replication.ErrFenced
	}
	target := serving.TermActivation{Term: writer.Term(), Incarnation: writer.Incarnation()}
	lease, err := s.opts.Publisher.CaptureHTTP(ctx)
	if err != nil {
		if errors.Is(err, serving.ErrPending) {
			return false, primaryStatusRetry, nil
		}
		return false, 0, err
	}
	defer lease.Release()
	bundle := lease.Bundle()
	value, _ := bundle.Component("runtime")
	owner, ok := value.(*RuntimeOwner)
	if !ok || owner == nil || owner.Primary() == nil || owner.Zones() == nil || !owner.Primary().Base().Same(bundle.Identity().Base) || !owner.Zones().Base().Same(bundle.Identity().Base) {
		return false, 0, serving.ErrComponents
	}
	value, _ = bundle.Component("zones")
	if value != owner.Zones() {
		return false, 0, serving.ErrComponents
	}
	value, _ = bundle.Component("objects")
	objects, ok := value.(*zonecache.Snapshot)
	if !ok || objects == nil {
		return false, 0, serving.ErrComponents
	}
	object, found := objects.Objects.Get(store.KeyOf(cached))
	desired, ok := object.(*public.DNSZone)
	if !found || !ok || !equality.Semantic.DeepEqual(cached, desired) {
		return false, primaryStatusRetry, nil
	}
	now := s.opts.Now()
	if now.IsZero() {
		return false, 0, pr.ErrInvalid
	}
	observation, hasLifecycle := owner.Primary().CommittedPrimaryStatus(desired.Name)
	status, ready := primaryStatusProjection(desired, owner, observation, hasLifecycle, now)
	apex, _ := dynamicupdate.CanonicalName(desired.Spec.ZoneName)
	route := owner.Zones().Zones(now).Match(apex)
	delay := time.Duration(0)
	if hasLifecycle && observation.ExpiresAt.After(now) {
		delay = observation.ExpiresAt.Sub(now)
	}
	if !ready && (delay == 0 || delay > primaryStatusRetry) {
		delay = primaryStatusRetry
	}
	fence := pr.PublicationFence{Term: target.Term, Incarnation: target.Incarnation}
	wrote := false
	err = s.opts.Pin(ctx, fence, func(p currentguard.Pinned) error {
		if p.Current == nil || !p.Current() {
			return serving.ErrDependency
		}
		check := func(ctx context.Context) error {
			if !p.Current() {
				return serving.ErrDependency
			}
			if err := s.opts.Publisher.CheckWriterLease(ctx, lease, target); err != nil {
				return err
			}
			actual := s.opts.Now()
			if actual.Before(now) {
				return serving.ErrDependency
			}
			// These are the original immutable model and precomputed ancestor
			// deadline. No preparation, copying, parsing or locking in this hook.
			if ready && (route == nil || !route.RoutingAvailableAt(actual) || !owner.Primary().Admitted(apex, actual)) {
				return serving.ErrPending
			}
			return nil
		}
		if err := check(ctx); err != nil {
			return err
		}
		return s.opts.Store.Command(ctx, target.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if !tx.Authority().Same(target.Term) || tx.Incarnation() != target.Incarnation {
				return replication.ErrFenced
			}
			if err := check(ctx); err != nil {
				return err
			}
			var current public.DNSZone
			if err := tx.Get(ctx, store.KeyOf(desired), &current); err != nil {
				return err
			}
			if !equality.Semantic.DeepEqual(&current, desired) {
				return serving.ErrSuperseded
			}
			if err := primaryStatusCAS(ctx, tx, desired, observation, hasLifecycle); err != nil {
				return err
			}
			if err := tx.RequireCommitCheck(check); err != nil {
				return err
			}
			if equality.Semantic.DeepEqual(current.Status, status) {
				return nil
			}
			current.Status = status
			if err := tx.UpdateStatus(ctx, &current); err != nil {
				return err
			}
			wrote = true
			return nil
		})
	})
	if err != nil {
		if errors.Is(err, serving.ErrPending) || errors.Is(err, serving.ErrSuperseded) || errors.Is(err, serving.ErrDependency) || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return false, primaryStatusRetry, nil
		}
		return false, 0, err
	}
	return wrote, delay, nil
}

func primaryStatusCAS(ctx context.Context, tx *sqlite.CommandTx, desired *public.DNSZone, o pr.PrimaryStatusObservation, exists bool) error {
	var lifecycle zk.ZoneLifecycle
	err := tx.Get(ctx, store.Key{Kind: zk.KindZoneLifecycle, Name: zk.LifecycleName(tx.Authority().ClusterID, desired.Name)}, &lifecycle)
	if !exists {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return serving.ErrSuperseded
	}
	if err != nil {
		return err
	}
	l, m := lifecycle.Spec, o.Member
	if lifecycle.ResourceVersion != o.LifecycleResourceVersion || l.Zone != m.Zone || l.Type != m.Type || l.Incarnation != m.Incarnation || l.Sequence != m.Sequence || l.Phase != m.Phase || l.Source != o.Source || l.HeadZone() != o.HeadZone || !slices.Equal(l.Parents, m.Parents) {
		return serving.ErrSuperseded
	}
	var head zk.ZoneState
	err = tx.Get(ctx, store.Key{Kind: zk.KindZoneState, Name: zk.HeadName(o.HeadZone)}, &head)
	if o.HeadResourceVersion == "" {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return serving.ErrSuperseded
	}
	if err != nil {
		return err
	}
	if head.ResourceVersion != o.HeadResourceVersion || head.Spec.Zone != o.HeadZone || head.Spec.CommitSequence != m.Version.CommitSequence || head.Spec.RootDigest != m.Version.RootDigest {
		return serving.ErrSuperseded
	}
	return nil
}

func primaryStatusProjection(desired *public.DNSZone, owner *RuntimeOwner, o pr.PrimaryStatusObservation, exists bool, now time.Time) (public.DNSZoneStatus, bool) {
	status := desired.DeepCopy().Status
	status.ObservedGeneration = desired.Generation
	status.Serial, status.ContentHash, status.RecordCount = 0, "", 0
	apex, err := dynamicupdate.CanonicalName(desired.Spec.ZoneName)
	token, tokenErr := primaryzone.SourceToken(desired)
	aligned := exists && err == nil && tokenErr == nil && o.Member.Zone.ObjectName == desired.Name && o.Member.Zone.UID == string(desired.UID) && o.Member.Zone.Apex == apex && o.Member.Type == public.ZoneTypePrimary && o.ConfigMatchesDesired && o.HeadResourceVersion != "" && o.CommittedSource.UID == token.UID && o.CommittedSource.IntentDigest == token.IntentDigest
	ready := false
	reason, message := "AwaitingCommittedPrimary", "Waiting for the current primary configuration to be committed and published."
	if aligned {
		status.Serial, status.ContentHash, status.RecordCount = o.Serial, o.ContentHash, o.RecordCount
		reason, message = "CommittedPrimaryUnavailable", "Committed primary data is currently unavailable for serving."
		route := owner.Zones().Zones(now).Match(apex)
		ready = owner.Primary().Admitted(apex, now) && route != nil && route.Name() == apex && route.Type() == public.ZoneTypePrimary && route.RoutingAvailableAt(now)
		if ready {
			reason, message = public.ReasonServing, fmt.Sprintf("%d committed records", o.RecordCount)
		}
	}
	conditionStatus := metav1.ConditionFalse
	if ready {
		conditionStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: public.ConditionReady, Status: conditionStatus, Reason: reason, Message: message, ObservedGeneration: desired.Generation, LastTransitionTime: metav1.NewTime(now.Truncate(time.Second))})
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: public.ConditionDegraded, Status: metav1.ConditionFalse, Reason: public.ReasonAsExpected, ObservedGeneration: desired.Generation, LastTransitionTime: metav1.NewTime(now.Truncate(time.Second))})
	return status, ready
}
