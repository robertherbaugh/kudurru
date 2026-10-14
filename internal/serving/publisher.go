package serving

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Source supplies coherent installed candidates and a serialized publication
// barrier. *sqlite.Store implements this interface. ServingAuthority is local
// memory only; DNS/protected reads never invoke a database reader.
type Source interface {
	ReadServingCandidate(context.Context) (*replication.Image, sqlite.GenerationToken, error)
	PublishIfCurrent(context.Context, sqlite.GenerationToken, sqlite.PublicationOptions, func() error) error
	GenerationChanges() <-chan struct{}
	AuthorityChanges() <-chan struct{}
	ServingAuthority() (replication.Authority, bool)
}

// Candidate carries one installed cut. Prepare must return all owner components
// for Token.Generation(), not a mixture of independent watcher snapshots.
type Candidate struct {
	Image *replication.Image
	Token sqlite.GenerationToken
}

// PrepareFunc is the single required all-owner preparation callback.
type PrepareFunc func(context.Context, Candidate) (Prepared, error)

// Options supplies owner preparation, a complete required manifest, and optional
// coalesced dependency notifications. DependencyChanges must subscribe BEFORE
// Prepare captures dependencies; Prepared.Current is the final cursor check.
type Options struct {
	Source            Source
	Prepare           PrepareFunc
	Required          []string
	DependencyChanges func() <-chan struct{}
	RetryInterval     time.Duration
}

// Publisher owns one complete serving pointer and a real promotion barrier.
type Publisher struct {
	opts        Options
	current     atomic.Pointer[Bundle]
	publication chan struct{} // context-aware mutex; order: publication -> store
	started     atomic.Bool
	closed      atomic.Bool
	stop        chan struct{}
	stopOnce    sync.Once
	initialized chan struct{}
	initOnce    sync.Once
	wake        chan struct{}
	mu          sync.Mutex
	changed     chan struct{}
	installed   sqlite.GenerationToken
	preparing   bool
	reason      string
	sequence    uint64 // under publication lock
}

// New validates the complete owner manifest. No partial/zone-only default is
// provided. It performs no I/O and never mounts public or internal handlers.
func New(opts Options) (*Publisher, error) {
	if nilValue(opts.Source) || opts.Prepare == nil || len(opts.Required) == 0 {
		return nil, fmt.Errorf("serving: source, prepare and complete manifest required")
	}
	opts.Required = slices.Clone(opts.Required)
	slices.Sort(opts.Required)
	for i, name := range opts.Required {
		if !validName(name) || (i > 0 && opts.Required[i-1] == name) {
			return nil, ErrComponents
		}
	}
	for _, name := range CompleteManifest() {
		if !slices.Contains(opts.Required, name) {
			return nil, fmt.Errorf("%w: manifest omits %q", ErrComponents, name)
		}
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = time.Second
	}
	p := &Publisher{opts: opts, publication: make(chan struct{}, 1), stop: make(chan struct{}), initialized: make(chan struct{}), wake: make(chan struct{}, 1), changed: make(chan struct{}), reason: "no_bundle"}
	p.publication <- struct{}{}
	return p, nil
}

// Initialized closes only after the first complete normal publication. It is a
// historical startup signal; current readiness must use Status.ProtectedReady.
func (p *Publisher) Initialized() <-chan struct{} { return p.initialized }

// Wake asks the preparation loop to recapture node-local dependencies. It is
// coalesced, nonblocking, and safe before Run starts.
func (p *Publisher) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Publisher) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.publication:
		return nil
	}
}

func (p *Publisher) unlock() { p.publication <- struct{}{} }

func (p *Publisher) update(token sqlite.GenerationToken, preparing bool, reason string) {
	p.mu.Lock()
	if token.Generation().Term.ClusterID != "" {
		p.installed = token
	}
	p.preparing, p.reason = preparing, reason
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

type attemptResult struct {
	token  sqlite.GenerationToken
	bundle *Bundle
	err    error
}

func callPrepare(ctx context.Context, prepare PrepareFunc, candidate Candidate) (result Prepared, err error) {
	defer func() {
		if recover() != nil {
			err = ErrPrepare // never expose private object/panic text in status
		}
	}()
	return prepare(ctx, candidate)
}

func (p *Publisher) prepare(ctx context.Context) attemptResult {
	image, token, err := p.opts.Source.ReadServingCandidate(ctx)
	if err != nil {
		return attemptResult{err: err}
	}
	if image == nil || !image.Generation().Same(token.Generation()) {
		return attemptResult{err: ErrComponents}
	}
	p.update(token, true, "preparing")
	prepared, err := callPrepare(ctx, p.opts.Prepare, Candidate{Image: image, Token: token})
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		releasePrepared(prepared)
		return attemptResult{token: token, err: err}
	}
	bundle, err := newBundle(token, prepared, p.opts.Required)
	if err != nil {
		releasePrepared(prepared)
	}
	return attemptResult{token: token, bundle: bundle, err: err}
}

func (p *Publisher) publish(ctx context.Context, next *Bundle) error {
	if err := p.lock(ctx); err != nil {
		next.release()
		return err
	}
	var previous *Bundle
	err := p.opts.Source.PublishIfCurrent(ctx, next.token, sqlite.PublicationOptions{DNSOnly: next.dnsOnly}, func() error {
		if p.closed.Load() {
			return ErrClosed
		}
		if !dependenciesCurrent(next.current) {
			return ErrDependency
		}
		if p.sequence == math.MaxUint64 {
			return ErrClosed
		}
		p.sequence++
		next.identity.Publication = p.sequence
		previous = p.current.Swap(next)
		return nil
	})
	p.unlock()
	if err != nil {
		next.release()
		return err
	}
	if previous != nil {
		previous.release()
	}
	if !next.dnsOnly {
		p.initOnce.Do(func() { close(p.initialized) })
	}
	return nil
}

// Run subscribes before reading, cancels obsolete preparation and publishes only
// a current complete cut. At most one preparer runs; it must honor cancellation.
// Owner errors retain the whole last-good bundle and retry exact dependencies.
func (p *Publisher) Run(ctx context.Context) error {
	if !p.started.CompareAndSwap(false, true) {
		return fmt.Errorf("serving: Run called twice")
	}
	defer p.Close()
	for {
		if p.closed.Load() || ctx.Err() != nil {
			return nil
		}
		generation := p.opts.Source.GenerationChanges()
		authority := p.opts.Source.AuthorityChanges()
		var dependencies <-chan struct{}
		if p.opts.DependencyChanges != nil {
			dependencies = p.opts.DependencyChanges()
		}
		work, cancel := context.WithCancel(ctx)
		result := make(chan attemptResult, 1)
		go func() { result <- p.prepare(work) }()
		var r attemptResult
		obsolete := false
		select {
		case r = <-result:
		case <-generation:
			obsolete = true
		case <-authority:
			obsolete = true
		case <-dependencies:
			obsolete = true
		case <-p.wake:
			obsolete = true
		case <-ctx.Done():
			obsolete = true
		case <-p.stop:
			obsolete = true
		}
		if obsolete {
			cancel()
			r = <-result // join; do not leak old callbacks/resources
			if r.bundle != nil {
				r.bundle.release()
			}
			if errors.Is(r.err, store.ErrClosed) {
				return r.err
			}
			continue
		}
		// Keep work alive through the token CAS; cancellation before publication
		// would reject every successfully prepared candidate.
		if r.err == nil {
			r.err = p.publish(work, r.bundle)
		}
		cancel()
		if errors.Is(r.err, store.ErrClosed) {
			return r.err
		}
		reason := "current"
		if r.err != nil {
			reason = "prepare_failed"
			if errors.Is(r.err, replication.ErrCAS) || errors.Is(r.err, ErrDependency) {
				reason = "candidate_changed"
			} else if errors.Is(r.err, replication.ErrReadOnly) {
				reason = "no_installed_image"
			}
		} else if r.bundle.dnsOnly {
			reason = "dns_only"
		}
		p.update(r.token, false, reason)
		if reason == "candidate_changed" && !errors.Is(r.err, ErrDependency) {
			continue
		}
		var retry <-chan time.Time
		var timer *time.Timer
		if r.err != nil {
			timer = time.NewTimer(p.opts.RetryInterval)
			retry = timer.C
		}
		select {
		case <-generation:
		case <-authority:
		case <-dependencies:
		case <-p.wake:
		case <-retry:
		case <-ctx.Done():
		case <-p.stop:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// Close stops new captures/publications and releases the publisher's reference.
// Outstanding leases retain their resources. It waits for a held Pin to release.
func (p *Publisher) Close() {
	p.stopOnce.Do(func() { p.closed.Store(true); close(p.stop) })
	// Do not hold a publication lock while invoking an owner hook.
	<-p.publication
	previous := p.current.Swap(nil)
	p.unlock()
	if previous != nil {
		previous.release()
	}
	p.update(sqlite.GenerationToken{}, false, "closed")
}

// Capture acquires one whole immutable cut without reading SQLite. It may serve
// last-good DNS while newer data prepares or the node is fenced.
func (p *Publisher) Capture() (*Lease, error) {
	for {
		if p.closed.Load() {
			return nil, ErrClosed
		}
		bundle := p.current.Load()
		if bundle == nil {
			return nil, ErrPending
		}
		if bundle.retain() {
			if p.closed.Load() {
				bundle.release()
				return nil, ErrClosed
			}
			return &Lease{bundle: bundle}, nil
		}
	}
}

func eligible(bundle *Bundle, a replication.Authority, blocked bool) bool {
	return bundle != nil && !bundle.dnsOnly && !blocked && !bundle.token.DNSOnly() && bundle.token.Matches(a) && dependenciesCurrent(bundle.current)
}

// CaptureHTTP fails closed when installed/authority/dependency cursors are ahead
// of serving. Both authority samples are cheap atomics, never database reads.
// Canonical mutations must still authorize against their current CommandTx.
func (p *Publisher) CaptureHTTP(ctx context.Context) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, blockedBefore := p.opts.Source.ServingAuthority()
	lease, err := p.Capture()
	if err != nil {
		return nil, err
	}
	after, blockedAfter := p.opts.Source.ServingAuthority()
	if !eligible(lease.bundle, before, blockedBefore) || !eligible(lease.bundle, after, blockedAfter) {
		lease.Release()
		return nil, ErrPending
	}
	return lease, nil
}

// Pin is a real publication barrier compatible with replica.ServingPin. It
// holds the serving pointer stable through eligibility/SQLite promotion CAS.
// Release before waiting for new-term publication. DNS never takes this lock.
func (p *Publisher) Pin(ctx context.Context) (replication.Generation, func(), error) {
	if err := p.lock(ctx); err != nil {
		return replication.Generation{}, nil, err
	}
	bundle := p.current.Load()
	a, blocked := p.opts.Source.ServingAuthority()
	if p.closed.Load() || !eligible(bundle, a, blocked) {
		p.unlock()
		if bundle != nil {
			// Failed pins report the actual last-good served identity for
			// diagnostics, but confer no barrier or promotion eligibility.
			return bundle.identity.Base, nil, ErrPending
		}
		return replication.Generation{}, nil, ErrPending
	}
	var once sync.Once
	return bundle.identity.Base, func() { once.Do(p.unlock) }, nil
}
