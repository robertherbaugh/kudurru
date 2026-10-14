package stub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Options supplies node-local dependencies; none are used by query handlers.
type Options struct {
	Cache         *zonecache.Cache
	Blobs         localdb.BlobStore
	ClusterID     string
	Discoverer    Discoverer
	Now           func() time.Time
	PollInterval  time.Duration
	MaxConcurrent int
	Logger        *slog.Logger
}

// Status describes one node's discovered routing state, never shared status.
type Status struct {
	Identity           string            `json:"-"`
	Zone               string            `json:"zone"`
	Name               string            `json:"name"`
	Serial             uint32            `json:"serial"`
	Servers            []zone.StubServer `json:"servers,omitempty"`
	ConfirmedAt        time.Time         `json:"confirmedAt,omitzero"`
	ExpiresAt          time.Time         `json:"expiresAt,omitzero"`
	NextRefresh        time.Time         `json:"nextRefresh,omitzero"`
	Available          bool              `json:"available"`
	ConfigurationError string            `json:"configurationError,omitempty"`
	Error              string            `json:"error,omitempty"`
}

// Snapshot holds immutable discovered routes; capture it once per query.
type Snapshot struct {
	zones  map[string]*zone.Zone
	status []Status
}

// Lookup cannot revive configuration absent from the captured zone set.
func (s *Snapshot) Lookup(identity string) *zone.Zone {
	if s == nil {
		return nil
	}
	return s.zones[identity]
}

// Manager runs bounded discovery workers and persists before publication.
type Manager struct {
	opts    Options
	current atomic.Pointer[Snapshot]
	wake    chan struct{}
	running atomic.Bool
}

// New validates startup dependencies and subscribes to configuration changes.
func New(opts Options) (*Manager, error) {
	if opts.Cache == nil || opts.Blobs == nil || opts.ClusterID == "" || opts.Discoverer == nil {
		return nil, errors.New("stub: cache, blobs, cluster identity and discoverer are required")
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
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 4
	}
	if opts.MaxConcurrent > 32 {
		return nil, errors.New("stub concurrency exceeds 32 workers")
	}
	m := &Manager{opts: opts, wake: make(chan struct{}, 1)}
	m.current.Store(&Snapshot{zones: map[string]*zone.Zone{}})
	opts.Cache.OnPublish(func(*zonecache.Snapshot) { m.Wake() })
	return m, nil
}

// Wake coalesces scheduler hints without blocking the config publisher.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Snapshot returns the current immutable routes.
func (m *Manager) Snapshot() *Snapshot { return m.current.Load() }

// Statuses returns private copies for currently configured objects only.
func (m *Manager) Statuses() []Status {
	config := m.opts.Cache.Snapshot()
	if config == nil {
		return nil
	}
	wanted := map[string]bool{}
	for _, dz := range config.Objects.Zones() {
		if dz.Spec.Type == v1alpha1.ZoneTypeStub {
			wanted[zone.StubIdentity(dz)] = true
		}
	}
	conflicts := zone.ConflictingApices(config.Objects.Zones())
	out := []Status{}
	for _, st := range m.Snapshot().status {
		if !wanted[st.Identity] {
			continue
		}
		st.Servers = slices.Clone(st.Servers)
		st.Available = st.ConfigurationError == "" && !st.ConfirmedAt.IsZero() && !m.opts.Now().Before(st.ConfirmedAt) && m.opts.Now().Before(st.ExpiresAt)
		if !config.Authoritative() {
			st.Available = false
			st.ConfigurationError = "Zone configuration is unavailable or stale; this node answers SERVFAIL."
		}
		if conflicts[dns.CanonicalName(st.Name)] {
			st.Available = false
			st.ConfigurationError = "Multiple objects configure this apex; this node answers SERVFAIL."
		}
		out = append(out, st)
	}
	return out
}

// StatusFor binds diagnostic reads to the current UID and complete spec.
func (m *Manager) StatusFor(dz *v1alpha1.DNSZone) (Status, bool) {
	if dz == nil || dz.Spec.Type != v1alpha1.ZoneTypeStub {
		return Status{}, false
	}
	for _, st := range m.Statuses() {
		if st.Identity == zone.StubIdentity(dz) {
			return st, true
		}
	}
	return Status{}, false
}

type entry struct {
	dz     *v1alpha1.DNSZone
	id     string
	z      *zone.Zone
	status Status
	next   time.Time
	cancel context.CancelFunc
}
type result struct {
	entry *entry
	data  *zone.StubData
	z     *zone.Zone
	err   error
}

// Run cancels superseded jobs and joins all workers before returning.
func (m *Manager) Run(ctx context.Context) error {
	if !m.running.CompareAndSwap(false, true) {
		return errors.New("stub manager is already running")
	}
	defer m.running.Store(false)
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	entries := map[string]*entry{}
	results := make(chan result, m.opts.MaxConcurrent)
	ticker := time.NewTicker(m.opts.PollInterval)
	defer ticker.Stop()
	active := 0
	m.Wake()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-m.wake:
		case <-ticker.C:
		case r := <-results:
			active--
			m.reconcile(ctx, entries)
			if entries[r.entry.id] == r.entry {
				e := r.entry
				e.cancel()
				e.cancel = nil
				config := m.opts.Cache.Snapshot()
				if config == nil || !config.Authoritative() || zone.ConflictingApices(config.Objects.Zones())[dns.CanonicalName(e.dz.Spec.ZoneName)] {
					r.err = errors.New("stub configuration became unavailable during discovery")
				}
				if r.err == nil {
					r.err = m.persist(ctx, e.id, r.data)
				}
				if r.err != nil {
					e.status.Error = r.err.Error()
					delay := 30 * time.Second
					if e.z != nil {
						delay = seconds(e.z.SOA().Retry)
					}
					e.next = m.opts.Now().Add(delay)
					m.opts.Logger.Warn("stub discovery failed", "zone", e.dz.Name, "error", r.err)
				} else {
					e.z = r.z
					interval := min(seconds(r.z.SOA().Refresh), r.data.ExpiresAt.Sub(r.data.ConfirmedAt)/2)
					e.next = r.data.ConfirmedAt.Add(interval)
					e.status = Status{Identity: e.id, Zone: e.dz.Name, Name: e.dz.Spec.ZoneName, Serial: e.z.Serial(), Servers: e.z.StubServers(), ConfirmedAt: r.data.ConfirmedAt, ExpiresAt: r.data.ExpiresAt}
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		m.reconcile(ctx, entries)
		config := m.opts.Cache.Snapshot()
		if config != nil && config.Authoritative() {
			conflicts := zone.ConflictingApices(config.Objects.Zones())
			for _, id := range slices.Sorted(maps.Keys(entries)) {
				e := entries[id]
				if active >= m.opts.MaxConcurrent {
					break
				}
				if e.cancel != nil || m.opts.Now().Before(e.next) || e.status.ConfigurationError != "" || conflicts[dns.CanonicalName(e.dz.Spec.ZoneName)] {
					continue
				}
				// A worker owns only the captured configuration; scheduler fields are
				// never read by it, avoiding races while configuration changes.
				jobCtx, jobCancel := bounded(ctx, 5*time.Second, 5*time.Second)
				e.cancel = jobCancel
				active++
				workers.Add(1)
				go func(e *entry) {
					defer workers.Done()
					data, err := m.opts.Discoverer.Discover(jobCtx, e.dz)
					var z *zone.Zone
					if err == nil {
						if data == nil || data.ConfirmedAt.After(m.opts.Now()) || !m.opts.Now().Before(data.ExpiresAt) {
							err = errors.New("stub discovery has invalid confirmation/expiry")
						} else {
							z, err = zone.BuildStub(e.dz, data)
						}
					}
					select {
					case results <- result{entry: e, data: data, z: z, err: err}:
					case <-ctx.Done():
					}
				}(e)
			}
		}
		m.publish(entries)
	}
}

func seconds(n uint32) time.Duration {
	if n == 0 {
		return time.Second
	}
	return time.Duration(n) * time.Second
}

func (m *Manager) reconcile(ctx context.Context, entries map[string]*entry) {
	config := m.opts.Cache.Snapshot()
	if config == nil || config.State == zonecache.Unsynced {
		return
	}
	wanted := map[string]bool{}
	conflicts := zone.ConflictingApices(config.Objects.Zones())
	for _, dz := range config.Objects.Zones() {
		if dz.Spec.Type != v1alpha1.ZoneTypeStub {
			continue
		}
		id := zone.StubIdentity(dz)
		wanted[id] = true
		if e := entries[id]; e != nil {
			if (!config.Authoritative() || conflicts[dns.CanonicalName(dz.Spec.ZoneName)]) && e.cancel != nil {
				e.cancel()
			}
			continue
		}
		e := &entry{dz: dz.DeepCopy(), id: id, status: Status{Identity: id, Zone: dz.Name, Name: dz.Spec.ZoneName}}
		entries[id] = e
		if err := ValidateConfig(dz); err != nil {
			e.status.ConfigurationError = err.Error()
			continue
		}
		data, z, err := m.restore(ctx, e)
		if err != nil {
			if !errors.Is(err, localdb.ErrNoBlob) {
				e.status.Error = err.Error()
			}
			continue
		}
		e.z = z
		e.status.Serial, e.status.Servers, e.status.ConfirmedAt, e.status.ExpiresAt = z.Serial(), z.StubServers(), data.ConfirmedAt, data.ExpiresAt
		// Always attempt confirmation after restart without extending expiry.
	}
	for id, e := range entries {
		if wanted[id] {
			continue
		}
		if e.cancel != nil {
			e.cancel()
		}
		delete(entries, id)
		if blobs, ok := m.opts.Blobs.(interface {
			DeleteBlob(context.Context, string) error
		}); ok {
			if err := blobs.DeleteBlob(ctx, "stub/"+id); err != nil {
				m.opts.Logger.Warn("stub cleanup failed", "zone", e.dz.Name, "error", err)
			}
		}
	}
}

func (m *Manager) publish(entries map[string]*entry) {
	s := &Snapshot{zones: map[string]*zone.Zone{}}
	for id, e := range entries {
		if e.z != nil {
			s.zones[id] = e.z
		}
		st := e.status
		st.NextRefresh = e.next
		s.status = append(s.status, st)
	}
	slices.SortFunc(s.status, func(a, b Status) int {
		if a.Zone < b.Zone {
			return -1
		}
		if a.Zone > b.Zone {
			return 1
		}
		return 0
	})
	m.current.Store(s)
}

type envelope struct {
	Version     int               `json:"version"`
	ClusterID   string            `json:"clusterID"`
	Identity    string            `json:"identity"`
	Records     []string          `json:"records"`
	Servers     []zone.StubServer `json:"servers"`
	ConfirmedAt time.Time         `json:"confirmedAt"`
	ExpiresAt   time.Time         `json:"expiresAt"`
	Checksum    string            `json:"checksum"`
}

func (e *envelope) digest() string {
	c := *e
	c.Checksum = ""
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (m *Manager) persist(ctx context.Context, id string, data *zone.StubData) error {
	e := envelope{Version: 1, ClusterID: m.opts.ClusterID, Identity: id, Servers: data.Servers, ConfirmedAt: data.ConfirmedAt, ExpiresAt: data.ExpiresAt}
	for _, rr := range data.Records {
		e.Records = append(e.Records, rr.String())
	}
	e.Checksum = e.digest()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > 1<<20 {
		return errors.New("persisted stub exceeds byte bound")
	}
	if err := m.opts.Blobs.PutBlob(ctx, "stub/"+id, b); err != nil {
		return fmt.Errorf("persisting stub: %w", err)
	}
	return nil
}

func (m *Manager) restore(ctx context.Context, e *entry) (*zone.StubData, *zone.Zone, error) {
	b, err := m.opts.Blobs.GetBlob(ctx, "stub/"+e.id)
	if err != nil {
		return nil, nil, err
	}
	if len(b.Data) > 1<<20 {
		return nil, nil, errors.New("persisted stub exceeds byte bound")
	}
	var disk envelope
	if err := json.Unmarshal(b.Data, &disk); err != nil {
		return nil, nil, errors.New("invalid stub snapshot")
	}
	if disk.Version != 1 || disk.ClusterID != m.opts.ClusterID || disk.Identity != e.id || disk.Checksum != disk.digest() {
		return nil, nil, errors.New("stub snapshot identity/version/checksum mismatch")
	}
	if disk.ConfirmedAt.IsZero() || disk.ConfirmedAt.After(m.opts.Now()) || len(disk.Records) > zone.MaxStubRecords {
		return nil, nil, errors.New("invalid persisted stub confirmation/record bound")
	}
	data := &zone.StubData{Servers: disk.Servers, ConfirmedAt: disk.ConfirmedAt, ExpiresAt: disk.ExpiresAt}
	for _, line := range disk.Records {
		rr, err := dns.NewRR(line)
		if err != nil {
			return nil, nil, errors.New("invalid persisted stub RR")
		}
		data.Records = append(data.Records, rr)
	}
	z, err := zone.BuildStub(e.dz, data)
	if err != nil {
		return nil, nil, err
	}
	return data, z, nil
}
