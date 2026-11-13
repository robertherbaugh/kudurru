package serving

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"weak"

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
	// PreparationTimeout bounds the original candidate read, build and swap.
	// Zero preserves the existing caller lifetime. Managed-root composition
	// must choose a positive bound no greater than 30 seconds before Run.
	PreparationTimeout time.Duration
	// Report, when set, observes the outcome of every completed attempt of
	// Run: the Status reason and the preparation or publication error, nil
	// after a publication. Status carries only the reason; without Report
	// nothing says which object or owner a candidate failed on. Run calls it
	// from its own goroutine, holding no lock, and waits for it to return.
	// The error may name stored objects: it is for the operator's log, never
	// for a client.
	Report func(reason string, err error)
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
	// awaiting counts HTTP callers that wait for an eligible publication
	// before they own a lease (AwaitHTTP).
	awaiting atomic.Int64
	// wakes counts Wake calls. A preparation notes it before it reads its
	// candidate; a Wake that arrives later is answered by one more
	// preparation after this one publishes (D405), never by canceling it.
	wakes atomic.Uint64
	// retired names the last publications that swaps replaced, newest
	// first, each with the moment it stopped being served, for ServedWithin
	// (D395). A DNS-only (fenced) publication and Close clear it. The
	// pointers are weak: they never keep a released publication's memory
	// alive. Only publish and Close write it, under the publication lock.
	retired [retiredDepth]atomic.Pointer[retiredBundle]
}

// retiredDepth bounds how many replaced publications ServedWithin still
// recognizes. Several swaps can follow one another within a few milliseconds
// (a zone write is several commits), so a query in flight can outlive more
// than its own publication's immediate successor.
const retiredDepth = 8

type retiredBundle struct {
	bundle weak.Pointer[Bundle]
	at     time.Time
}

// clearRetired forgets every replaced publication. Caller holds the
// publication lock.
func (p *Publisher) clearRetired() {
	for i := range p.retired {
		p.retired[i].Store(nil)
	}
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
	if opts.PreparationTimeout < 0 || opts.PreparationTimeout > 30*time.Second {
		return nil, fmt.Errorf("serving: invalid preparation lifetime")
	}
	p := &Publisher{opts: opts, publication: make(chan struct{}, 1), stop: make(chan struct{}), initialized: make(chan struct{}), wake: make(chan struct{}, 1), changed: make(chan struct{}), reason: "no_bundle"}
	p.publication <- struct{}{}
	return p, nil
}

// Initialized closes only after the first complete normal publication. It is a
// historical startup signal; current readiness must use Status.ProtectedReady.
func (p *Publisher) Initialized() <-chan struct{} { return p.initialized }

// Wake asks the preparation loop to recapture node-local dependencies. It is
// coalesced, nonblocking, and safe before Run starts. It does not cancel a
// preparation in progress (D405): when that preparation read its candidate
// before the Wake, one more preparation follows its publication, however many
// Wakes arrived meanwhile; a preparation that had not read its candidate yet
// already includes what the Wake announced.
func (p *Publisher) Wake() {
	p.wakes.Add(1)
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
	// wakes is the Wake count noted before the candidate was read.
	wakes uint64
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
	wakes := p.wakes.Load()
	image, token, err := p.opts.Source.ReadServingCandidate(ctx)
	if err != nil {
		return attemptResult{err: err, wakes: wakes}
	}
	if image == nil || !image.Generation().Same(token.Generation()) {
		return attemptResult{err: ErrComponents, wakes: wakes}
	}
	p.update(token, true, "preparing")
	prepared, err := callPrepare(ctx, p.opts.Prepare, Candidate{Image: image, Token: token})
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		releasePrepared(prepared)
		return attemptResult{token: token, err: err, wakes: wakes}
	}
	bundle, err := newBundle(token, prepared, p.opts.Required)
	if err != nil {
		releasePrepared(prepared)
	}
	return attemptResult{token: token, bundle: bundle, err: err, wakes: wakes}
}

// settleWake decides what the Wakes that arrived while a preparation ran ask
// for. Wakes counted before it read its candidate are included in it, so their
// buffered signal is dropped; a later one leaves the signal set, and the loop
// prepares once more. The signal is drained before the count is compared, so
// a Wake racing with this call is never lost.
func (p *Publisher) settleWake(noted uint64) {
	select {
	case <-p.wake:
	default:
	}
	if p.wakes.Load() != noted {
		p.Wake()
	}
}

func (p *Publisher) publish(ctx context.Context, next *Bundle) error {
	if err := p.lock(ctx); err != nil {
		next.release()
		return err
	}
	var previous *Bundle
	swapped := false
	err := p.opts.Source.PublishIfCurrent(ctx, next.token, sqlite.PublicationOptions{DNSOnly: next.dnsOnly, Anchor: next.anchor}, func() error {
		if swapped {
			return ErrComponents
		}
		if p.closed.Load() {
			return ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !dependenciesCurrent(next.current) {
			return ErrDependency
		}
		if p.sequence == math.MaxUint64 {
			return ErrClosed
		}
		sequence := p.sequence + 1
		if next.anchor != nil {
			if err := next.anchor.Apply(); err != nil {
				return err
			}
		}
		// No fallible work or external callback may separate registration from
		// its matching complete pointer swap. Store resolves under the same R.
		p.sequence = sequence
		next.identity.Publication = sequence
		// Record the publication being replaced BEFORE the swap (D395). A
		// reader in ServedWithin loads current first, so it then finds the
		// replaced publication either still current or already retired; with
		// the record written after the swap, a query in flight that looked in
		// between found it in neither place and its answer was withheld. Only
		// publish and Close write current, both under the publication lock, so
		// this load is what the swap replaces. A fenced successor clears the
		// record before it is served, so nothing qualifies across the fence.
		if replaced := p.current.Load(); replaced != nil && !next.dnsOnly {
			for i := retiredDepth - 1; i > 0; i-- {
				p.retired[i].Store(p.retired[i-1].Load())
			}
			p.retired[0].Store(&retiredBundle{bundle: weak.Make(replaced), at: time.Now()})
		} else {
			p.clearRetired()
		}
		previous = p.current.Swap(next)
		swapped = true
		return nil
	})
	p.unlock()
	if !swapped {
		next.release()
		if err == nil {
			err = ErrComponents
		}
		return err
	}
	if previous != nil {
		previous.release()
	}
	if err == nil && !next.dnsOnly {
		p.initOnce.Do(func() { close(p.initialized) })
	}
	// An outer source wrapper may report an error AFTER an actual swap. Keep
	// the publisher's live reference and actual disposition in that case.
	return err
}

// Run subscribes before reading, cancels obsolete preparation and publishes only
// a current complete cut. At most one preparer runs; it must honor cancellation.
// Owner errors retain the whole last-good bundle and retry exact dependencies.
// A new generation, authority or dependency cursor makes the preparation in
// progress obsolete (its publication would be refused); a Wake does not
// (D405): it is coalesced into at most one more preparation after this one.
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
		var work context.Context
		var cancel context.CancelFunc
		if p.opts.PreparationTimeout > 0 {
			work, cancel = context.WithTimeout(ctx, p.opts.PreparationTimeout)
		} else {
			work, cancel = context.WithCancel(ctx)
		}
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
		p.settleWake(r.wakes)
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
		if p.opts.Report != nil {
			p.opts.Report(reason, r.err)
		}
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
	p.clearRetired()
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

// CheckReadLease refuses a superseded original protected read. It does not
// capture a successor or confer mutation authority. No I/O or owner work runs.
func (p *Publisher) CheckReadLease(ctx context.Context, lease *Lease) error {
	if ctx == nil || p == nil || lease == nil || lease.Bundle() == nil {
		return ErrReleased
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a, blocked := p.opts.Source.ServingAuthority()
	if p.closed.Load() || p.current.Load() != lease.bundle || !eligible(lease.bundle, a, blocked) {
		return ErrPending
	}
	return nil
}

// AwaitHTTP marks one caller that is waiting for an eligible publication
// before it owns a lease, until done is called. A request of several items
// lets the node publish for such callers between its items (D378) instead of
// committing on while they run out of their capture wait. The mark grants and
// excludes nothing; done is idempotent.
func (p *Publisher) AwaitHTTP() (done func()) {
	if p == nil {
		return func() {}
	}
	p.awaiting.Add(1)
	var once sync.Once
	return func() { once.Do(func() { p.awaiting.Add(-1) }) }
}

// AwaitingHTTP reports whether a caller is marked by AwaitHTTP now.
func (p *Publisher) AwaitingHTTP() bool { return p != nil && p.awaiting.Load() > 0 }

// Serves reports whether value is, by pointer identity, the named component of
// the publication that is served now. It answers false once a successor has
// been published or the publisher closed, and for a value that is not a
// pointer. It reads two atomics and one immutable map, takes no lock and
// never reads the Store, so the DNS output path may call it. It says nothing
// about canonical currency: a commit that has not been published yet leaves
// the served publication served (D378).
func (p *Publisher) Serves(name string, value any) bool {
	if p == nil || p.closed.Load() || value == nil || reflect.ValueOf(value).Kind() != reflect.Pointer {
		return false
	}
	bundle := p.current.Load()
	if bundle == nil {
		return false
	}
	c, ok := bundle.components[name]
	return ok && c.Value == value
}

// ServedWithin reports whether value is, by pointer identity, the named
// component of the publication that is served now, or of a publication that
// stopped being served at most grace ago (D395). A lease of such a
// publication was captured while it was served, so an answer computed from
// it was computed from committed data that was current when the query was
// received; emitting it a moment after the swap is what the client would
// have seen had the query been answered a moment earlier. It counts from the
// swap that replaced that publication, however many swaps followed it, and
// covers the last retiredDepth replaced publications. Nothing qualifies
// across a DNS-only (fenced) publication or after Close, and nothing past
// grace. Like Serves it reads atomics and immutable maps, takes no lock,
// allocates nothing and never reads the Store.
func (p *Publisher) ServedWithin(name string, value any, grace time.Duration) bool {
	if p.Serves(name, value) {
		return true
	}
	if p == nil || grace <= 0 || p.closed.Load() || value == nil || reflect.ValueOf(value).Kind() != reflect.Pointer {
		return false
	}
	for i := range p.retired {
		r := p.retired[i].Load()
		if r == nil || time.Since(r.at) > grace {
			// Entries are newest first: every later one retired earlier.
			return false
		}
		bundle := r.bundle.Value()
		if bundle == nil {
			continue
		}
		if c, ok := bundle.components[name]; ok && c.Value == value {
			return true
		}
	}
	return false
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
