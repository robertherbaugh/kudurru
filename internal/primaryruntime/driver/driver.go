// Package driver recovers and reconciles committed-primary membership only for
// a root-issued SQLite writer. Queries use the complete serving publisher.
package driver

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// Writer is the exact admitted term/local incarnation. Run never adopts another.
type Writer = pr.PublicationFence

// Publication acknowledges an exact member in one protected complete eligible
// bundle. Wait must not retain Store/provider/runtime locks. Subscribe first.
type Publication interface {
	Wake()
	Subscribe() <-chan struct{}
	Wait(context.Context, Writer, pr.Member) error
}

// Options supplies actual SQLite, the root-assembled runtime and local registry.
type Options struct {
	Store                 *sqlite.Store
	Runtime               *pr.Runtime
	Image                 pr.ImageOptions
	Publication           Publication
	MaxZones, MaxWork     int
	PollInterval, Timeout time.Duration
}

// ZoneStatus contains bounded local diagnostics, never credentials/raw errors.
type ZoneStatus struct {
	ObjectName, Apex, Phase, Pending string
	Incarnation, Sequence            uint64
	Version                          zonestate.Version
	Installed, Acknowledged          bool
	Published                        bool
	RetryAt                          time.Time
}

// Status is a detached snapshot. Ready means complete restore, not DNS readiness.
type Status struct {
	Running, Ready bool
	Writer         Writer
	Pending        string
	Zones          []ZoneStatus
}

type retry struct {
	cursor   string
	failures uint
	at       time.Time
	status   ZoneStatus
}

// Driver runs one bounded fair scheduler and one serialized NOTIFY worker.
type Driver struct {
	o            Options
	wake         chan struct{}
	mu           sync.Mutex
	status       Status
	running      bool
	retries      map[string]retry
	acknowledged map[string]pr.Member
	after        string
}

// New validates finite bounds; a missing guard still fails closed in the adapter.
func New(o Options) (*Driver, error) {
	if o.Store == nil || o.Runtime == nil || o.Publication == nil || o.Image.Scheme == nil || len(o.Image.Validation.Kinds) == 0 || o.Image.ClusterID == "" {
		return nil, pr.ErrInvalid
	}
	if o.MaxZones <= 0 {
		o.MaxZones = 10000
	}
	if o.MaxWork <= 0 {
		o.MaxWork = 32
	}
	if o.MaxZones > 100000 || o.MaxWork > 128 {
		return nil, pr.ErrCapacity
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Timeout > time.Minute || o.PollInterval > time.Minute {
		return nil, pr.ErrCapacity
	}
	o.Image.MaxZones = o.MaxZones
	return &Driver{o: o, wake: make(chan struct{}, 1), retries: map[string]retry{}, acknowledged: map[string]pr.Member{}}, nil
}

// Wake coalesces a hint. It never supplies source data or writer authority.
func (d *Driver) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Status returns detached, bounded, sanitized diagnostics.
func (d *Driver) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.status
	s.Zones = slices.Clone(s.Zones)
	return s
}

func (d *Driver) current(w Writer) bool {
	a, blocked := d.o.Store.ServingAuthority()
	return !blocked && a.Writable() && a.Term.Same(w.Term) && a.Incarnation == w.Incarnation
}

// Run first restores ONE complete actual image under its exact candidate barrier.
// Root must join prior writer work and invoke Run before mounting UPDATE ingress.
func (d *Driver) Run(ctx context.Context, w Writer) error {
	if w.Term.Validate() != nil || w.Incarnation == 0 {
		return pr.ErrInvalid
	}
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return pr.ErrConflict
	}
	d.running = true
	d.status = Status{Running: true, Writer: w}
	d.retries = map[string]retry{}
	d.acknowledged = map[string]pr.Member{}
	d.after = ""
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.running = false; d.status.Running = false; d.mu.Unlock() }()
	ctx, cancel := context.WithCancel(pr.WithPublicationFence(ctx, w))
	defer cancel()
	// Subscribe before any reads. Closed channels are generation hints; renew them.
	generation := d.o.Store.GenerationChanges()
	authority := d.o.Store.AuthorityChanges()
	publication := d.o.Publication.Subscribe()
	if !d.current(w) {
		return pr.ErrUnavailable
	}
	var notifyDone chan struct{}
	defer func() {
		cancel()
		if notifyDone != nil {
			<-notifyDone
		}
	}()
	restored := false
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-authority:
			authority = d.o.Store.AuthorityChanges()
		case <-generation:
			generation = d.o.Store.GenerationChanges()
		case <-publication:
			publication = d.o.Publication.Subscribe()
		case <-d.wake:
		case <-timer.C:
		}
		if !d.current(w) {
			return pr.ErrUnavailable
		}
		opctx, stop := context.WithTimeout(ctx, d.o.Timeout)
		index, p, token, err := d.read(opctx)
		if err == nil && !restored {
			var restore *pr.Restore
			restore, err = d.o.Runtime.PrepareRestore(p)
			if err == nil {
				err = d.o.Store.PublishIfCurrent(opctx, token, sqlite.PublicationOptions{}, func() error {
					if !d.current(w) {
						return pr.ErrUnavailable
					}
					return d.o.Runtime.InstallRestore(restore)
				})
			}
			if err == nil {
				restored = true
				d.o.Publication.Wake()
				notifyDone = make(chan struct{})
				go func() { defer close(notifyDone); _ = d.o.Runtime.RunNotify(ctx) }()
			}
		}
		stop()
		if err != nil {
			d.recordGlobal(restored, reason(err))
		} else {
			err = d.work(ctx, w, index)
			d.recordGlobal(true, reason(err))
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d.o.PollInterval)
	}
}

func reason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, pr.ErrCapacity):
		return "capacity"
	case errors.Is(err, pr.ErrInvalid):
		return "invalid-input"
	case errors.Is(err, pr.ErrConflict), errors.Is(err, replication.ErrCAS):
		return "conflict"
	case errors.Is(err, pr.ErrUnavailable):
		return "unavailable"
	default:
		return "retry"
	}
}
func (d *Driver) recordGlobal(ready bool, pending string) {
	d.mu.Lock()
	d.status.Ready = ready
	d.status.Pending = pending
	d.mu.Unlock()
}
