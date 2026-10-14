package providerresources

import (
	"context"
	"math"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"k8s.io/apimachinery/pkg/util/validation"
)

// New validates budgets without starting workers or performing I/O.
func New(opts Options) (*Owner, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxConcurrent == 0 {
		opts.MaxConcurrent = 4
	}
	if opts.OperationTimeout == 0 {
		opts.OperationTimeout = 15 * time.Second
	}
	if opts.RefreshInterval == 0 {
		opts.RefreshInterval = 5 * time.Minute
	}
	if opts.MaxFreshness == 0 {
		opts.MaxFreshness = 15 * time.Minute
	}
	if opts.RetryInterval == 0 {
		opts.RetryInterval = 10 * time.Second
	}
	if opts.MaxConcurrent < 1 || opts.MaxConcurrent > 4 || opts.OperationTimeout <= 0 || opts.OperationTimeout > 15*time.Second || opts.RefreshInterval <= 0 || opts.MaxFreshness <= 0 || opts.MaxFreshness > 15*time.Minute || opts.RefreshInterval > opts.MaxFreshness || opts.RetryInterval <= 0 || opts.RetryInterval > 5*time.Minute {
		return nil, ErrBounds
	}
	return &Owner{opts: opts, candidates: map[string]*AuthorityCandidate{}, wake: make(chan struct{}, 1), stop: make(chan struct{}), changed: make(chan struct{}, 1), candidateChanges: make(chan struct{}, 1), done: make(chan struct{})}, nil
}
func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'f') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
func desiredIdentity(d Desired) (string, error) {
	if d.Host == nil || d.Host.network == nil || !validHash(d.RegistryDigest) || d.Sources == nil || d.Authorities == nil {
		return "", ErrInvalid
	}
	if len(d.Sources) > servingauth.MaxProviders || len(d.Authorities) > providerregistry.MaxAuthorities {
		return "", ErrBounds
	}
	budget := 0
	ids := map[string]bool{}
	issuers := map[string]bool{}
	for _, s := range d.Sources {
		if len(validation.IsDNS1123Label(s.Ref.ID)) != 0 || slices.Contains([]string{"local", "kudurru", "unix", "internal", "kubernetes"}, s.Ref.ID) || !safe(s.Ref.UID, 512, false) || (s.Ref.Origin != servingauth.ProviderConfig && s.Ref.Origin != servingauth.ProviderObject) || s.CreatedAt.IsZero() || s.CreatedAt.Year() < 1970 || s.CreatedAt.Year() > 9999 || !validHash(s.SpecIdentity) || !validHash(s.PolicyIdentity) || s.Issuer != s.Spec.Issuer || ids[s.Ref.ID] || issuers[s.Issuer] {
			return "", ErrInvalid
		}
		if err := checkBudget(reflect.ValueOf(s.Spec), &budget); err != nil {
			return "", err
		}
		ids[s.Ref.ID] = true
		issuers[s.Issuer] = true
		p, err := oidccore.Compile(s.Spec, s.Ref.ID)
		if err != nil || p.Identity() != s.PolicyIdentity {
			return "", ErrInvalid
		}
	}
	keys := map[string]bool{}
	activeIDs := map[string]bool{}
	active := 0
	for _, a := range d.Authorities {
		if a.FormatVersion != providerregistry.FormatVersion || a.Revision == 0 || !validHash(a.SpecIdentity) || !safe(a.Provider.UID, 512, false) || len(validation.IsDNS1123Label(a.Provider.ID)) != 0 || (a.Provider.Origin != servingauth.ProviderConfig && a.Provider.Origin != servingauth.ProviderObject) {
			return "", ErrInvalid
		}
		k := providerregistry.AuthorityKey(a.Provider)
		if keys[k] {
			return "", ErrInvalid
		}
		keys[k] = true
		for _, s := range []string{a.Binding.Profile, a.Binding.FingerprintKeyID, a.Binding.CredentialFingerprint, a.Binding.CredentialPolicy, a.Binding.NetworkPolicy, a.Binding.MetadataBinding, a.Binding.EntitlementIdentity} {
			if !safe(s, 512, false) {
				return "", ErrInvalid
			}
		}
		if a.Active {
			active++
			if activeIDs[a.Provider.ID] {
				return "", ErrInvalid
			}
			activeIDs[a.Provider.ID] = true
		}
	}
	if active > providerregistry.MaxActiveAuthorities {
		return "", ErrBounds
	}
	sources := slices.Clone(d.Sources)
	slices.SortFunc(sources, func(a, b servingauth.ProviderDescriptor) int {
		if a.Ref.ID < b.Ref.ID {
			return -1
		}
		if a.Ref.ID > b.Ref.ID {
			return 1
		}
		return 0
	})
	authorities := slices.Clone(d.Authorities)
	slices.SortFunc(authorities, func(a, b providerregistry.AuthorityRecord) int {
		ka, kb := providerregistry.AuthorityKey(a.Provider), providerregistry.AuthorityKey(b.Provider)
		if ka < kb {
			return -1
		}
		if ka > kb {
			return 1
		}
		return 0
	})
	return hash(struct {
		Profile, Registry, Host, Network, Credential, Entitlement, Key string
		Serial                                                         uint64
		Allowed                                                        bool
		Sources                                                        []servingauth.ProviderDescriptor
		Authorities                                                    []providerregistry.AuthorityRecord
	}{"provider-desired/v1", d.RegistryDigest, d.Host.identity, d.Host.network.identity, d.Host.credentialPolicy, d.Host.entitlement, d.Host.fingerprintKey, d.Host.serial, d.Host.allowed, sources, authorities}), nil
}
func detachDesired(d Desired) Desired {
	d.Sources = slices.Clone(d.Sources)
	for i := range d.Sources {
		d.Sources[i] = cloneDescriptor(d.Sources[i])
	}
	d.Authorities = slices.Clone(d.Authorities)
	return d
}
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Changes is a coalesced dependency wakeup. Subscribe before acquiring a cut.
func (o *Owner) Changes() <-chan struct{} { return o.changed }

// AuthorityCandidates wakes the root's explicit durable admission coordinator.
func (o *Owner) AuthorityCandidates() <-chan struct{} { return o.candidateChanges }

// TakeCandidates detaches bounded candidates; each must Match before admission.
func (o *Owner) TakeCandidates() []*AuthorityCandidate {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]*AuthorityCandidate, 0, len(o.candidates))
	for _, c := range o.candidates {
		next := *c
		next.receipt = c.Receipt()
		out = append(out, &next)
	}
	clear(o.candidates)
	return out
}
func lock(ctx context.Context, try func() bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if try() {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if try() {
				return nil
			}
		}
	}
}
func (o *Owner) readLock(ctx context.Context) error  { return lock(ctx, o.mu.TryRLock) }
func (o *Owner) writeLock(ctx context.Context) error { return lock(ctx, o.mu.TryLock) }

// WithReplacement provides root the SAME exclusion as worker replacements. Root
// holds this guard through host/registry/floor Command work. Do not acquire a
// read guard or reenter owner methods inside fn; use the provided capability.
func (o *Owner) WithReplacement(ctx context.Context, fn func(*Replacement) error) error {
	if fn == nil {
		return ErrInvalid
	}
	if err := o.writeLock(ctx); err != nil {
		return err
	}
	defer o.mu.Unlock()
	if o.closed.Load() {
		return ErrClosed
	}
	r := &Replacement{owner: o}
	r.active.Store(true)
	defer r.active.Store(false)
	return fn(r)
}

// SetDesired denies changed authorities immediately, before background I/O.
func (o *Owner) SetDesired(ctx context.Context, d Desired) error {
	return o.WithReplacement(ctx, func(r *Replacement) error { return r.SetDesired(d) })
}

// SetDesired installs an exact detached cut under the exclusive guard.
func (r *Replacement) SetDesired(d Desired) error {
	if r == nil || !r.active.Load() {
		return ErrClosed
	}
	id, err := desiredIdentity(d)
	if err != nil {
		return err
	}
	o := r.owner
	if id == o.desiredID {
		return nil
	}
	if o.revision == math.MaxUint64 || o.desiredRevision == math.MaxUint64 {
		return ErrBounds
	}
	d = detachDesired(d)
	entries := make([]entry, 0, len(d.Sources))
	for _, s := range d.Sources {
		next := entry{descriptor: s, resource: unavailable(s, d, "host-policy")}

		entries = append(entries, next)
	}
	// Any desired change conservatively denies first; accepted matching material
	// can be reacquired without extending a stale resource or guessing host grants.
	o.desiredRevision++
	o.desired = d
	o.desiredID = id
	clear(o.candidates)
	if o.cancel != nil {
		o.cancel()
	}
	o.install(entries)
	signal(o.wake)
	return nil
}
func unavailable(s servingauth.ProviderDescriptor, d Desired, reason string) servingauth.ProviderResource {
	return servingauth.ProviderResource{Ref: s.Ref, SpecIdentity: s.SpecIdentity, PolicyIdentity: s.PolicyIdentity, Availability: servingauth.ProviderUnavailable, UnavailableReason: reason, TrustIdentity: hash(struct{ Host, Network, Registry string }{d.Host.identity, d.Host.network.identity, d.RegistryDigest})}
}
func (o *Owner) install(entries []entry) {
	o.revision++
	s := &snapshot{desiredID: o.desiredID, desiredRevision: o.desiredRevision, revision: o.revision, entries: entries, observed: o.opts.Now()}
	ids := []string{}
	for _, e := range entries {
		identity, _ := servingauth.ProviderResourceIdentity(e.descriptor, e.resource)
		ids = append(ids, identity)
		if e.resource.Availability == servingauth.ProviderReady && (s.until.IsZero() || e.until.Before(s.until)) {
			s.until = e.until
		}
	}
	s.identity = hash(struct {
		Desired   string
		Revision  uint64
		Resources []string
	}{s.desiredID, s.revision, ids})
	o.current.Store(s)
	signal(o.changed)
}
func validAt(s *snapshot, now time.Time) error {
	if s == nil {
		return ErrUnavailable
	}
	if now.Before(s.observed) {
		return ErrStale
	}
	if !s.until.IsZero() && !now.Before(s.until) {
		return ErrUnavailable
	}
	return nil
}
func resources(s *snapshot) []servingauth.ProviderResource {
	if s == nil {
		return nil
	}
	out := make([]servingauth.ProviderResource, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.resource)
	}
	return out
}

// Acquire is exact memory-only capture. It never starts acquisition or network.
func (o *Owner) Acquire(ctx context.Context, d Desired) (*Lease, error) {
	id, err := desiredIdentity(d)
	if err != nil {
		return nil, err
	}
	if err = o.readLock(ctx); err != nil {
		return nil, err
	}
	defer o.mu.RUnlock()
	s := o.current.Load()
	if o.closed.Load() {
		return nil, ErrClosed
	}
	if s == nil || s.desiredID != id {
		return nil, ErrStale
	}
	if err = validAt(s, o.opts.Now()); err != nil {
		return nil, err
	}
	return &Lease{owner: o, snapshot: s}, nil
}

// Resources returns detached offline verifier capabilities.
func (l *Lease) Resources() []servingauth.ProviderResource {
	if l == nil || l.released.Load() {
		return nil
	}
	return resources(l.snapshot)
}

// Identity names this monotonic runtime cut.
func (l *Lease) Identity() string {
	if l == nil || l.released.Load() {
		return ""
	}
	return l.snapshot.identity
}

// Current checks memory identity and hard freshness without I/O.
func (l *Lease) Current() bool {
	return l != nil && !l.released.Load() && !l.owner.closed.Load() && l.owner.current.Load() == l.snapshot && validAt(l.snapshot, l.owner.opts.Now()) == nil
}

// Release retires this handle; immutable public-key inputs need no I/O teardown.
func (l *Lease) Release() {
	if l != nil {
		l.released.Store(true)
	}
}

// Endpoints returns only ready leased metadata. No transport/secret escapes.
func (l *Lease) Endpoints() []EndpointInfo {
	if l == nil || l.released.Load() {
		return nil
	}
	out := []EndpointInfo{}
	for _, e := range l.snapshot.entries {
		if e.resource.Availability == servingauth.ProviderReady {
			id, _ := servingauth.ProviderResourceIdentity(e.descriptor, e.resource)
			out = append(out, EndpointInfo{e.descriptor.Ref, id, e.metadata.authorization, e.metadata.token, e.metadata.jwksURL})
		}
	}
	return out
}

// Run owns and joins every bounded acquisition job. Desired updates cancel jobs;
// late results are ignored under the exact private captured desired identity.
func (o *Owner) Run(ctx context.Context) error {
	o.mu.Lock()
	if o.closed.Load() {
		o.mu.Unlock()
		return ErrClosed
	}
	started := o.running.CompareAndSwap(false, true)
	o.mu.Unlock()
	if !started {
		return ErrInvalid
	}
	defer close(o.done)
	defer func() {
		o.mu.Lock()
		o.closed.Store(true)
		o.current.Store(nil)
		clear(o.candidates)
		signal(o.changed)
		o.mu.Unlock()
	}()
	runctx, cancel := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	defer func() { cancel(); <-watchDone }()
	go func() {
		defer close(watchDone)
		select {
		case <-o.stop:
			cancel()
		case <-runctx.Done():
		}
	}()
	due := map[string]time.Time{}
	failures := map[string]int{}
	lastDesired := uint64(0)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := runctx.Err(); err != nil {
			return err
		}
		o.mu.Lock()
		if o.closed.Load() {
			o.mu.Unlock()
			return ErrClosed
		}
		d, id, desiredRevision := o.desired, o.desiredID, o.desiredRevision
		s := o.current.Load()
		if desiredRevision != lastDesired {
			clear(due)
			clear(failures)
			lastDesired = desiredRevision
		}
		if s == nil {
			o.mu.Unlock()
			select {
			case <-runctx.Done():
				return runctx.Err()
			case <-o.wake:
			}
			continue
		}
		now := o.opts.Now()
		entries := slices.Clone(s.entries)
		expired := false
		for i, e := range entries {
			if e.resource.Availability == servingauth.ProviderReady && !now.Before(e.until) {
				entries[i].resource = unavailable(e.descriptor, d, "keys")
				entries[i].until = time.Time{}
				expired = true
			}
		}
		if expired {
			if o.revision == math.MaxUint64 {
				o.mu.Unlock()
				return ErrBounds
			}
			o.install(entries)
		}
		selected := []entry{}
		for _, e := range entries {
			if len(selected) >= o.opts.MaxConcurrent {
				break
			}
			if !now.Before(due[e.descriptor.Ref.ID]) {
				selected = append(selected, e)
			}
		}
		if len(selected) == 0 {
			o.mu.Unlock()
			select {
			case <-runctx.Done():
				return runctx.Err()
			case <-o.wake:
			case <-ticker.C:
			}
			continue
		}
		jobsctx, jobsCancel := context.WithTimeout(runctx, o.opts.OperationTimeout)
		o.cancel = jobsCancel
		o.mu.Unlock()
		results := make([]acquisition, len(selected))
		var wg sync.WaitGroup
		for i, e := range selected {
			wg.Add(1)
			go func(i int, e entry) { defer wg.Done(); results[i] = o.acquire(jobsctx, d, e.descriptor, e) }(i, e)
		}
		wg.Wait()
		jobsCancel()
		if err := o.writeLock(runctx); err != nil {
			return err
		}
		o.cancel = nil
		if o.closed.Load() {
			o.mu.Unlock()
			return ErrClosed
		}
		if id != o.desiredID || desiredRevision != o.desiredRevision {
			o.mu.Unlock()
			continue
		}
		entries = slices.Clone(o.current.Load().entries)
		changed := false
		for _, a := range results {
			for i, e := range entries {
				if e.descriptor.Ref != a.descriptor.Ref {
					continue
				}
				interval := o.opts.RetryInterval
				if a.reason == "" && o.opts.Now().Before(a.until) {
					failures[a.descriptor.Ref.ID] = 0
					interval = o.opts.RefreshInterval / 2
					if remaining := a.until.Sub(o.opts.Now()) / 2; remaining < interval {
						interval = remaining
					}
					if interval < time.Millisecond {
						interval = time.Millisecond
					}
					if sameBinding(d, a.descriptor, a.binding) {
						res := servingauth.ProviderResource{Ref: a.descriptor.Ref, SpecIdentity: a.descriptor.SpecIdentity, PolicyIdentity: a.descriptor.PolicyIdentity, Availability: servingauth.ProviderReady, Verifier: a.verifier, VerifierIdentity: a.verifier.Identity(), TrustIdentity: hash(a.binding)}
						entries[i] = entry{a.descriptor, res, a.metadata, a.until}
						changed = true
					} else {
						receipt := providerregistry.DesiredAuthority{Source: cloneDescriptor(a.descriptor), Binding: a.binding}
						o.candidates[a.descriptor.Ref.ID] = &AuthorityCandidate{owner: o, desiredRevision: desiredRevision, desiredID: id, registryDigest: d.RegistryDigest, hostID: d.Host.identity, receipt: receipt}
						signal(o.candidateChanges)
						entries[i] = entry{descriptor: a.descriptor, resource: unavailable(a.descriptor, d, "host-policy"), metadata: a.metadata}
						changed = true
					}
				} else {
					if failures[a.descriptor.Ref.ID] < 32 {
						failures[a.descriptor.Ref.ID]++
					}
					for j := 1; j < failures[a.descriptor.Ref.ID] && interval < 5*time.Minute; j++ {
						interval *= 2
					}
					if interval > 5*time.Minute {
						interval = 5 * time.Minute
					}
					interval = retryJitter(interval, hash(struct {
						Desired string
						Failure int
						At      int64
					}{id, failures[a.descriptor.Ref.ID], o.opts.Now().UnixNano()}))
					if !a.transient || e.resource.Availability != servingauth.ProviderReady || !o.opts.Now().Before(e.until) {
						reason := a.reason
						if reason == "" {
							reason = "keys"
						}
						entries[i] = entry{descriptor: a.descriptor, resource: unavailable(a.descriptor, d, reason)}
						changed = true
					}
					delete(o.candidates, a.descriptor.Ref.ID)
				}
				due[a.descriptor.Ref.ID] = o.opts.Now().Add(interval)
				break
			}
		}
		if changed {
			if o.revision == math.MaxUint64 {
				o.mu.Unlock()
				return ErrBounds
			}
			o.install(entries)
		}
		o.mu.Unlock()
	}
}

// Close cancels acquisition and joins Run outside replacement/publication guards.
func (o *Owner) Close() error {
	o.mu.Lock()
	if !o.closed.Swap(true) {
		if o.cancel != nil {
			o.cancel()
		}
		o.stopOnce.Do(func() { close(o.stop) })
		o.current.Store(nil)
		clear(o.candidates)
		signal(o.changed)
	}
	running := o.running.Load()
	o.mu.Unlock()
	if running {
		<-o.done
	}
	return nil
}

func retryJitter(base time.Duration, identity string) time.Duration {
	factor := 90 + int(identity[0])%21
	value := base * time.Duration(factor) / 100
	if value > 5*time.Minute {
		return 5 * time.Minute
	}
	return value
}

// Retire denies all current resource capabilities under an exclusive guard. Root
// uses it after a failed authority/host mutation or replacement failure; a fresh
// exact SetDesired is required before any complete provider cut can be captured.
// It cannot fail or revive old leases, including at counter exhaustion.
func (r *Replacement) Retire() {
	if r == nil || !r.active.Load() {
		return
	}
	o := r.owner
	if o.cancel != nil {
		o.cancel()
	}
	o.desired = Desired{}
	o.desiredID = ""
	clear(o.candidates)
	o.current.Store(nil)
	signal(o.changed)
	signal(o.wake)
}
