package rpz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Receiver is implemented by secondary.TransferClient. IXFR is optional for
// test/adaptor receivers; its result must be a complete replacement RR set.
type Receiver interface {
	SOA(context.Context, *v1alpha1.DNSZone, string) (*dns.SOA, error)
	AXFR(context.Context, *v1alpha1.DNSZone, string) ([]dns.RR, error)
}

type incrementalReceiver interface {
	IXFR(context.Context, *v1alpha1.DNSZone, string, []dns.RR) ([]dns.RR, error)
}

// BlobStore includes deletion to invalidate an acceptance marker if its
// configuration changes while that marker is being committed.
type BlobStore interface {
	localdb.BlobStore
	DeleteBlob(context.Context, string) error
}

// Options supplies node-local dependencies. Query readers only use snapshots.
type Options struct {
	Cache         *zonecache.Cache
	Blobs         BlobStore
	ClusterID     string
	Keyring       dnsserver.TSIGKeyring
	Receiver      Receiver
	Now           func() time.Time
	Logger        *slog.Logger
	PollInterval  time.Duration
	MaxConcurrent int
	// Timeout bounds the whole primary failover/transfer operation.
	Timeout time.Duration
}

// Status describes this node's feed, never shared authoritative zone status.
type Status struct {
	Identity           string    `json:"-"`
	List               string    `json:"list"`
	Source             string    `json:"source"`
	Origin             string    `json:"origin"`
	Serial             uint32    `json:"serial"`
	Stats              Stats     `json:"stats"`
	Primary            string    `json:"primary,omitempty"`
	ConfirmedAt        time.Time `json:"confirmedAt,omitzero"`
	ExpiresAt          time.Time `json:"expiresAt,omitzero"`
	NextRefresh        time.Time `json:"nextRefresh,omitzero"`
	Available          bool      `json:"available"`
	ConfigurationError string    `json:"configurationError,omitempty"`
	Error              string    `json:"error,omitempty"`
}

// Snapshot is immutable. Capture it once together with the policy/configuration
// generation, then select feeds in the same ordered list/source sequence.
type Snapshot struct {
	zones       map[string]*Zone
	status      map[string]Status
	credentials map[string]string
}

// Lookup returns only the exact configuration identity's data.
func (s *Snapshot) Lookup(identity string) *Zone {
	if s == nil {
		return nil
	}
	return s.zones[identity]
}

// Select uses ordered policy-selected BlockLists from a pinned configuration.
// A missing/invalid/stale configured source remains a required sentinel.
// Ordinary/disabled lists and sources are omitted. No secrets/store are read.
func (s *Snapshot) Select(lists []*v1alpha1.BlockList, configurationAvailable bool) *Selection {
	var zones []*Zone
	for _, list := range lists {
		if list == nil {
			continue
		}
		for i := range list.Spec.Sources {
			if !configured(list, i) {
				continue
			}
			var z *Zone
			if configurationAvailable {
				z = s.Lookup(SourceIdentity(list, i))
			}
			if z == nil {
				spec, _, _ := sourceConfig(list, i)
				z, _ = Compile(spec, nil)
				if z == nil {
					// Invalid origins/exclusions still retain selected feed coverage
					// and their diagnostic identity, never an empty selection.
					z = &Zone{spec: spec}
				}
			}
			zones = append(zones, z)
		}
	}
	return NewSelection(zones...)
}

// Manager schedules bounded independent transfers and commits before publish.
type Manager struct {
	opts    Options
	current snapshotowner.Owner[Snapshot]
	wake    chan struct{}
	running atomic.Bool
}

// New constructs a manager; startup dependency errors are explicit.
func New(opts Options) (*Manager, error) {
	if opts.Cache == nil || opts.Blobs == nil || opts.ClusterID == "" || opts.Receiver == nil {
		return nil, errors.New("rpz: cache, durable blobs, cluster identity and receiver are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 4
	}
	if opts.MaxConcurrent > 32 {
		return nil, errors.New("rpz: concurrency exceeds 32 workers")
	}
	m := &Manager{opts: opts, wake: make(chan struct{}, 1)}
	if err := m.current.Publish(context.Background(), &Snapshot{zones: map[string]*Zone{}, status: map[string]Status{}}, sameSnapshot); err != nil {
		return nil, err
	}
	opts.Cache.OnPublish(func(*zonecache.Snapshot) { m.Wake() })
	return m, nil
}

// Snapshot returns the current immutable node-local feed set.
func (m *Manager) Snapshot() *Snapshot { return m.Capture().Snapshot() }

// Wake coalesces scheduler hints without blocking configuration publishers.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Statuses reflects only currently configured enabled feeds, checking exact
// identity, credential fingerprint, expiry and the existing stale-config gate.
func (m *Manager) Statuses() []Status {
	config := m.opts.Cache.Snapshot()
	if config == nil {
		return nil
	}
	var out []Status
	runtime := m.Snapshot()
	now := m.opts.Now()
	for _, list := range config.Objects.BlockLists() {
		for i := range list.Spec.Sources {
			if !configured(list, i) {
				continue
			}
			spec, dz, err := sourceConfig(list, i)
			st := Status{Identity: spec.Identity, List: spec.List, Source: spec.Source, Origin: spec.Origin}
			if saved, ok := runtime.status[spec.Identity]; ok && err == nil {
				st = saved
			}
			if err != nil {
				st.ConfigurationError = err.Error()
			}
			if !config.Authoritative() {
				st.ConfigurationError = "RPZ configuration is unavailable or stale; selected policy fails closed."
			}
			if err == nil {
				if credential, err := m.credential(dz); err != nil {
					st.ConfigurationError = err.Error()
				} else if saved := runtime.credentials[spec.Identity]; saved != "" && saved != credential {
					st.ConfigurationError = "RPZ credentials changed; awaiting fresh data."
				}
			}
			st.Available = st.ConfigurationError == "" && runtime.Lookup(spec.Identity).AvailableAt(now)
			out = append(out, st)
		}
	}
	slices.SortFunc(out, func(a, b Status) int {
		if a.List != b.List {
			return stringsCompare(a.List, b.List)
		}
		return stringsCompare(a.Source, b.Source)
	})
	return out
}

// StatusFor binds API diagnostics to the current object/source identity.
func (m *Manager) StatusFor(list *v1alpha1.BlockList, index int) (Status, bool) {
	id := SourceIdentity(list, index)
	if id == "" {
		return Status{}, false
	}
	for _, st := range m.Statuses() {
		if st.Identity == id {
			return st, true
		}
	}
	return Status{}, false
}

func stringsCompare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

type entry struct {
	spec       Spec
	dz         *v1alpha1.DNSZone
	credential string
	z          *Zone
	data       *envelope
	status     Status
	next       time.Time
	busy       bool
	cancel     context.CancelFunc
	generation uint64
}

type transferJob struct {
	entry      *entry
	config     *zonecache.Snapshot
	spec       Spec
	dz         *v1alpha1.DNSZone
	credential string
	z          *Zone
	data       *envelope
}

type transferResult struct {
	job  transferJob
	z    *Zone
	data *envelope
	err  error
}

// Run owns all mutable scheduler state. Configuration-generation changes
// cancel busy workers, including A-B-A changes with the same final identity.
func (m *Manager) Run(ctx context.Context) error {
	if !m.running.CompareAndSwap(false, true) {
		return errors.New("rpz: manager is already running")
	}
	defer m.running.Store(false)
	// nosemgrep: trailofbits.go.iterate-over-empty-map.iterate-over-empty-map -- reconcile populates entries in the loop; deferred cleanup visits those entries at shutdown
	entries := map[string]*entry{}
	results := make(chan transferResult, m.opts.MaxConcurrent)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer func() {
		for _, e := range entries {
			if e.cancel != nil {
				e.cancel()
			}
		}
	}()
	ticker := time.NewTicker(m.opts.PollInterval)
	defer ticker.Stop()
	active := 0
	var pending *transferResult
	for {
		if ctx.Err() != nil {
			return nil
		}
		config := m.opts.Cache.Snapshot()
		m.reconcile(ctx, entries, config)
		if pending != nil {
			active--
			m.complete(ctx, entries, *pending)
			pending = nil
		}
		if config != nil && config.Authoritative() {
			keys := make([]string, 0, len(entries))
			for id := range entries {
				keys = append(keys, id)
			}
			slices.Sort(keys)
			for _, id := range keys {
				e := entries[id]
				if active >= m.opts.MaxConcurrent || e.busy || e.status.ConfigurationError != "" || m.opts.Now().Before(e.next) {
					continue
				}
				job := transferJob{e, config, e.spec, e.dz, e.credential, e.z, e.data}
				jobCtx, cancel := context.WithCancel(ctx)
				e.busy, e.cancel = true, cancel
				active++
				workers.Go(func() {
					defer cancel()
					r := m.refresh(jobCtx, job)
					select {
					case results <- r:
					case <-ctx.Done():
					}
				})
			}
		}
		if err := m.publish(ctx, entries); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-m.wake:
		case r := <-results:
			pending = &r
		}
	}
}

func (m *Manager) reconcile(ctx context.Context, entries map[string]*entry, config *zonecache.Snapshot) {
	if config == nil || config.State == zonecache.Unsynced {
		return
	}
	wanted := map[string]bool{}
	for _, list := range config.Objects.BlockLists() {
		for i := range list.Spec.Sources {
			if !configured(list, i) {
				continue
			}
			spec, dz, err := sourceConfig(list, i)
			id := spec.Identity
			if id == "" {
				continue // invalid format-only sources remain unavailable via Select
			}
			wanted[id] = true
			credential := ""
			if err == nil {
				credential, err = m.credential(dz)
			}
			old := entries[id]
			if old != nil && old.credential == credential && old.status.ConfigurationError == errorText(err) {
				if !old.busy || old.generation == config.Generation {
					old.generation = config.Generation
					continue
				}
			}
			e := &entry{spec: spec, dz: dz, credential: credential, generation: config.Generation,
				status: Status{Identity: id, List: spec.List, Source: spec.Source, Origin: spec.Origin, ConfigurationError: errorText(err)}}
			if old != nil && old.cancel != nil {
				old.cancel()
			}
			if err == nil {
				if old != nil && old.credential == credential {
					e.z, e.data, e.status = old.z, old.data, old.status
					e.status.ConfigurationError = ""
				} else {
					e.data, e.z, err = m.restore(ctx, e)
					if err != nil && !errors.Is(err, localdb.ErrNoBlob) {
						e.status.Error = err.Error()
					}
					if e.data != nil {
						e.setDataStatus()
					}
				}
			}
			entries[id] = e
		}
	}
	for id, e := range entries {
		if !wanted[id] {
			if e.cancel != nil {
				e.cancel()
			}
			delete(entries, id)
		}
	}
}

func (m *Manager) complete(ctx context.Context, entries map[string]*entry, r transferResult) {
	e := entries[r.job.spec.Identity]
	if e != r.job.entry {
		return
	}
	e.busy, e.cancel = false, nil
	if m.opts.Cache.Snapshot() != r.job.config {
		e.next = time.Time{}
		return
	}
	if fingerprint, err := m.credential(e.dz); err != nil || fingerprint != r.job.credential {
		e.z, e.data = nil, nil
		e.status.ConfigurationError = "RPZ credentials changed; awaiting fresh data."
		return
	}
	if r.err == nil {
		persistCtx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
		defer cancel()
		r.err = m.persist(persistCtx, e.spec.Identity, r.data)
		if m.opts.Cache.Snapshot() != r.job.config {
			// Candidates cannot restore until a separate checksum-bound marker
			// accepts them. No best-effort cleanup is needed at this boundary.
			e.next = time.Time{}
			return
		}
		if fingerprint, err := m.credential(e.dz); err != nil || fingerprint != r.job.credential {
			e.z, e.data = nil, nil
			e.status.ConfigurationError = "RPZ credentials changed during persistence."
			return
		}
		if r.err == nil {
			r.err = m.accept(persistCtx, e.spec.Identity, r.data)
			if m.opts.Cache.Snapshot() != r.job.config || !m.credentialsMatch(e.dz, r.job.credential) {
				if err := m.invalidateAcceptance(ctx, e.spec.Identity); err != nil {
					e.status.Error = "RPZ configuration changed during acceptance; durable invalidation failed."
				}
				e.z, e.data = nil, nil
				e.next = time.Time{}
				return
			}
		}
	}
	if r.err != nil {
		e.status.Error = r.err.Error()
		retry := time.Second * 5
		if e.z != nil {
			retry = timer(e.z.soa.Retry)
		}
		e.next = m.opts.Now().Add(retry)
		return
	}
	e.data, e.z, e.status.Error = r.data, r.z, ""
	e.setDataStatus()
	e.next = r.data.ConfirmedAt.Add(timer(r.z.soa.Refresh))
}

func (m *Manager) credentialsMatch(dz *v1alpha1.DNSZone, expected string) bool {
	fingerprint, err := m.credential(dz)
	return err == nil && fingerprint == expected
}

func (e *entry) setDataStatus() {
	e.status.Serial, e.status.Stats = e.z.soa.Serial, e.z.stats
	e.status.Primary, e.status.ConfirmedAt, e.status.ExpiresAt = e.data.Primary, e.data.ConfirmedAt, e.data.ExpiresAt
}

func (m *Manager) publish(ctx context.Context, entries map[string]*entry) error {
	snap := &Snapshot{zones: map[string]*Zone{}, status: map[string]Status{}, credentials: map[string]string{}}
	for id, e := range entries {
		if e.z != nil && e.status.ConfigurationError == "" {
			snap.zones[id] = e.z
		}
		st := e.status
		st.NextRefresh = e.next
		snap.status[id] = st
		if e.data != nil && e.data.Identity == id {
			// Keep the acquired copy's provenance when current credentials
			// change; binding must never relabel old data with the new key.
			snap.credentials[id] = e.data.Credential
		}
	}
	return m.current.Publish(ctx, snap, sameSnapshot)
}

func (m *Manager) credential(dz *v1alpha1.DNSZone) (string, error) {
	if dz == nil || dz.Spec.Secondary == nil {
		return "", fmt.Errorf("rpz: invalid transfer configuration")
	}
	if dz.Spec.Secondary.TSIGKeyRef == "" {
		return "unsigned", nil
	}
	if m.opts.Keyring == nil {
		return "", fmt.Errorf("rpz: TSIG key unavailable")
	}
	secret, alg, ok := m.opts.Keyring.Lookup(dnsserver.TSIGKeyName(dz.Spec.Secondary.TSIGKeyRef))
	if !ok {
		return "", fmt.Errorf("rpz: TSIG key unavailable")
	}
	if _, ok := dnsserver.TSIGAlgorithm(alg); !ok || len(secret) == 0 {
		return "", fmt.Errorf("rpz: invalid TSIG key or algorithm")
	}
	h := sha256.New()
	_, _ = h.Write([]byte(alg))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(secret)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func timer(seconds uint32) time.Duration { return max(time.Second, time.Duration(seconds)*time.Second) }
func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
