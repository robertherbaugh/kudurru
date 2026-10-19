package providercoordinator

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// New starts no work. Nil admission is allowed for read-only nodes but can
// NEVER authorize RunWriter. Supplied store/schema/host/resources are immutable.
func New(o Options) (*Coordinator, error) {
	if o.Store == nil || o.Resources == nil || o.Publisher == nil || o.Host == nil || o.HostChanges == nil || o.PinHost == nil || o.Schema.Scheme == nil || o.Schema.SchemaVersion < 1 || len(o.Schema.Kinds) == 0 || len(o.Schema.Kinds) > 1024 {
		return nil, ErrInvalid
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.RetryInterval == 0 {
		o.RetryInterval = time.Second
	}
	if o.OperationTimeout == 0 {
		o.OperationTimeout = 15 * time.Second
	}
	if o.RetryInterval <= 0 || o.RetryInterval > time.Minute || o.OperationTimeout <= 0 || o.OperationTimeout > 15*time.Second {
		return nil, ErrInvalid
	}
	o.Schema.Kinds = slices.Clone(o.Schema.Kinds)
	life, cancel := context.WithCancel(context.Background())
	return &Coordinator{opts: o, life: life, cancel: cancel, changed: make(chan struct{})}, nil
}
func (c *Coordinator) context(ctx context.Context) (context.Context, func(), error) {
	if c == nil || ctx == nil {
		return nil, nil, ErrInvalid
	}
	if c.closed.Load() {
		return nil, nil, ErrClosed
	}
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.life, cancel)
	return child, func() { stop(); cancel() }, nil
}

// Changes broadcasts each invalidation. Subscribe BEFORE Capture. Only Run
// consumes Owner.Changes; multiple readers must never compete for its buffer.
func (c *Coordinator) Changes() <-chan struct{} { c.mu.Lock(); defer c.mu.Unlock(); return c.changed }
func (c *Coordinator) signal() {
	c.mu.Lock()
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
	c.opts.Publisher.Wake()
}

// reconcile uses ONE actual validated image/token and pure host capture. The
// writer barrier checks the token with memory-only callback; SetDesired follows
// outside that writer callback but inside the SAME exclusive resource guard.
// This path never calls Command and works on followers without writer tokens.
func (c *Coordinator) reconcile(ctx context.Context) (time.Time, error) {
	image, token, err := c.opts.Store.ReadServingCandidate(ctx)
	if err != nil {
		return time.Time{}, err
	}
	r, sources, err := c.image(ctx, image)
	if err != nil {
		return time.Time{}, err
	}
	var h HostCapture
	defer func() { releaseHost(h) }()
	err = c.opts.Resources.WithReplacement(ctx, func(replacement *providerresources.Replacement) error {
		return c.opts.PinHost(ctx, func() error {
			var e error
			h, e = c.host(ctx, r)
			if e != nil {
				replacement.Retire()
				return e
			}
			e = c.opts.Store.PublishIfCurrent(ctx, token, sqlite.PublicationOptions{DNSOnly: token.DNSOnly()}, func() error {
				if !c.hostCurrent(h) {
					return ErrStale
				}
				return ctx.Err()
			})
			if e != nil {
				return e
			}
			d := providerresources.Desired{RegistryDigest: r.Digest(), Host: h.Providers, Sources: sources, Authorities: r.Authorities()}
			if e = replacement.SetDesired(d); e != nil {
				replacement.Retire()
			}
			return e
		})
	})
	return h.ValidUntil, err
}

// Run owns acquisition/reconcile on every node. It has NO durable writer work,
// never samples WriterToken, and joins its bounded resource worker on every exit.
func (c *Coordinator) Run(ctx context.Context) (runErr error) {
	ctx, stop, err := c.context(ctx)
	if err != nil {
		return err
	}
	defer stop()
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.runDone != nil {
		c.mu.Unlock()
		return ErrRunning
	}
	done := make(chan struct{})
	c.runDone = done
	c.mu.Unlock()
	defer close(done)
	worker := make(chan error, 1)
	workerJoined := false
	workctx, cancel := context.WithCancel(ctx)
	go func() { worker <- c.opts.Resources.Run(workctx) }()
	defer func() {
		cancel()
		if !workerJoined {
			if err := <-worker; err != nil && !errors.Is(err, context.Canceled) {
				runErr = errors.Join(runErr, err)
			}
		}
		c.closed.Store(true)
		c.cancel()
		c.signal()
	}()
	resourceChanges := c.opts.Resources.Changes()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		generation := c.opts.Store.GenerationChanges()
		authority := c.opts.Store.AuthorityChanges()
		host := c.opts.HostChanges()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e := <-worker:
			workerJoined = true
			return e
		case <-generation:
		case <-authority:
		case <-host:
		case <-resourceChanges:
			c.signal()
		case <-timer.C:
		}
		opctx, finish := context.WithTimeout(ctx, c.opts.OperationTimeout)
		deadline, e := c.reconcile(opctx)
		finish()
		if errors.Is(e, store.ErrClosed) || errors.Is(e, providerresources.ErrClosed) {
			return e
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(deadlineDelay(c.opts.Now(), deadline, c.opts.RetryInterval))
	}
}

// Close cancels both lifetimes and joins before returning. Never invoke it from
// a resource/publication/Command callback. Store and publisher remain root-owned.
func (c *Coordinator) Close(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrInvalid
	}
	c.mu.Lock()
	c.closed.Store(true)
	c.cancel()
	run, writer := c.runDone, c.writerDone
	c.mu.Unlock()
	for _, done := range []chan struct{}{writer, run} {
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := c.opts.Resources.Close(); err != nil {
		return err
	}
	return nil
}
