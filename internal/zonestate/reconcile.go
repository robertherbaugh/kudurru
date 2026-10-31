package zonestate

import (
	"context"
	"errors"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zonekinds "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// DesiredInputs is a complete detached actual source read, never a watch payload.
type DesiredInputs struct {
	Zone       *public.DNSZone
	RecordSets []*public.DNSRecordSet
	Policy     *Policy
	Targets    []DesiredTarget
	Complete   bool
}

// DesiredReader reads actual inputs after each head read; errors never imply prune.
type DesiredReader interface {
	ReadDesired(context.Context, zonekinds.ZoneID) (*DesiredInputs, error)
}

// DesiredCompiler produces one whole-zone candidate, preserving dynamic data.
type DesiredCompiler interface {
	Project(context.Context, *View, *DesiredInputs, time.Time) (*Compilation, error)
}

// SourceDiagnostic reports a desired projection's effective visibility.
type SourceDiagnostic struct{ SourceID, Outcome, Reason string }

// Compilation contains a complete replacement aggregate and bounded diagnostics.
type Compilation struct {
	Aggregate   *Aggregate
	Diagnostics []SourceDiagnostic
}

// CoordinatorOptions supplies bounded reconcile adapters. Guard runs inside the repository CAS.
type CoordinatorOptions struct {
	Repository   *Repository
	Reader       DesiredReader
	Compiler     DesiredCompiler
	Materializer *Materializer
	Guard        Guard
	Now          func() time.Time
	Limits       Limits
	MaxAttempts  int
	Timeout      time.Duration
}

// Coordinator owns no goroutines; every conflict repeats head, actual reads and prebuild.
type Coordinator struct{ options CoordinatorOptions }

// ReconcileResult distinguishes durable commitment from local publication failure.
type ReconcileResult struct {
	View               *View
	Changed, Published bool
	Diagnostics        []SourceDiagnostic
}

// NewCoordinator validates adapters and applies finite attempt/time bounds.
func NewCoordinator(o CoordinatorOptions) (*Coordinator, error) {
	if o.Repository == nil || o.Reader == nil || o.Compiler == nil || o.Materializer == nil {
		return nil, ErrInvalid
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 8
	}
	if o.MaxAttempts > 64 {
		return nil, ErrCapacity
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.Limits = o.Limits.Defaults()
	return &Coordinator{options: o}, nil
}

// Reconcile prebuilds before a single CAS, and publishes only after durable commit.
func (c *Coordinator) Reconcile(ctx context.Context, id zonekinds.ZoneID) (ReconcileResult, error) {
	o := c.options
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	for attempt := 0; attempt < o.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return ReconcileResult{}, err
		}
		base, err := o.Repository.Read(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return ReconcileResult{}, err
		}
		if base == nil || (base.Head != nil && base.Head.Spec.Lifecycle != zonekinds.LifecycleActive) {
			return ReconcileResult{}, ErrInvalid
		}
		desired, err := o.Reader.ReadDesired(ctx, id)
		if err != nil {
			return ReconcileResult{}, err
		}
		if desired == nil || !desired.Complete {
			return ReconcileResult{}, ErrInvalid
		}
		compiled, err := o.Compiler.Project(ctx, base, desired, o.Now())
		if err != nil {
			return ReconcileResult{}, err
		}
		if compiled == nil || compiled.Aggregate == nil || compiled.Aggregate.Zone != id {
			return ReconcileResult{}, ErrInvalid
		}
		p, err := Prepare(compiled.Aggregate, base.Head, o.Limits)
		if err != nil {
			return ReconcileResult{}, err
		}
		if base.Head != nil && p.Head.Spec.RootDigest == base.Head.Spec.RootDigest {
			err = o.Materializer.Load(ctx, base)
			return ReconcileResult{View: base, Published: err == nil && o.Materializer.installed(base), Diagnostics: compiled.Diagnostics}, err
		}
		a, err := VerifyPrepared(p, o.Limits)
		if err != nil {
			return ReconcileResult{}, err
		}
		candidate := &View{Head: p.Head, Aggregate: a}
		built, err := o.Materializer.Prepare(ctx, candidate)
		if err != nil {
			return ReconcileResult{}, err
		}
		committed, err := o.Repository.Commit(ctx, base.Expected, p, o.Guard)
		if errors.Is(err, ErrConflict) || errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return ReconcileResult{}, err
		}
		err = o.Materializer.Install(ctx, committed, built)
		return ReconcileResult{View: committed, Changed: true, Published: err == nil && o.Materializer.installed(committed), Diagnostics: compiled.Diagnostics}, err
	}
	return ReconcileResult{}, ErrConflict
}

// Expire uses the same fresh-source/CAS path; Capture already fails closed at the deadline.
func (c *Coordinator) Expire(ctx context.Context, id zonekinds.ZoneID) (ReconcileResult, error) {
	return c.Reconcile(ctx, id)
}
