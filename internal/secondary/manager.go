// Package secondary maintains independent, durable copies of external zones.
// It never owns DNSZone serials or writes transferred records to the object store.
package secondary

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
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// Receiver is the authenticated transport. Implementations bound each exchange.
type Receiver interface {
	SOA(context.Context, *v1alpha1.DNSZone, string) (*dns.SOA, error)
	AXFR(context.Context, *v1alpha1.DNSZone, string) ([]dns.RR, error)
}

// incrementalReceiver returns a complete candidate after applying a bounded
// IXFR stream, including protocol-level AXFR fallback. The manager only supplies
// an unexpired base received under the same credentials.
type incrementalReceiver interface {
	IXFR(context.Context, *v1alpha1.DNSZone, string, []dns.RR) ([]dns.RR, error)
}

// Options supplies node-local persistence and configuration snapshots.
type Options struct {
	Cache *zonecache.Cache
	Blobs localdb.BlobStore
	// ClusterID is an independent stable cluster identity, available even
	// when the apiserver is down (serve derives it from the encryption key).
	ClusterID string
	Keyring   dnsserver.TSIGKeyring
	Receiver  Receiver
	Now       func() time.Time
	Logger    *slog.Logger
	// PollInterval bounds scheduler latency; DNS checks expiry itself.
	PollInterval  time.Duration
	MaxConcurrent int
}

// Status describes this node only, never the shared DNSZone status.
type Status struct {
	successes, failures uint64
	Identity            string    `json:"-"`
	Zone                string    `json:"zone"`
	Name                string    `json:"name"`
	Serial              uint32    `json:"serial"`
	RecordCount         int       `json:"recordCount"`
	Primary             string    `json:"primary,omitempty"`
	ConfirmedAt         time.Time `json:"confirmedAt,omitzero"`
	ExpiresAt           time.Time `json:"expiresAt,omitzero"`
	NextRefresh         time.Time `json:"nextRefresh,omitzero"`
	Available           bool      `json:"available"`
	ConfigurationError  string    `json:"configurationError,omitempty"`
	Error               string    `json:"error,omitempty"`
}

// Snapshot is immutable. Load it once for each DNS query.
type Snapshot struct {
	zones       map[string]*zone.Zone
	status      []Status
	credentials map[string]string
}

// Lookup returns the transferred version matching a configured placeholder.
func (s *Snapshot) Lookup(identity string) *zone.Zone {
	if s == nil {
		return nil
	}
	return s.zones[identity]
}

// Manager runs one scheduler per replica, with bounded concurrent transfers.
type Manager struct {
	opts      Options
	current   atomic.Pointer[Snapshot]
	wake      chan struct{}
	triggerMu sync.Mutex
	triggers  map[string]bool
}

// New constructs a manager; invalid global dependencies are startup errors.
func New(opts Options) (*Manager, error) {
	if opts.Cache == nil || opts.Blobs == nil || opts.ClusterID == "" || opts.Receiver == nil {
		return nil, errors.New("secondary: cache, persistence, cluster identity and receiver are required")
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
	m := &Manager{opts: opts, wake: make(chan struct{}, 1), triggers: make(map[string]bool)}
	m.current.Store(&Snapshot{zones: map[string]*zone.Zone{}})
	opts.Cache.OnPublish(func(*zonecache.Snapshot) { m.Wake() })
	return m, nil
}

// Wake requests reconciliation without blocking the config publisher.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Trigger schedules an authenticated NOTIFY hint for the exact configuration.
// Hints coalesce in a bounded queue; they never replace SOA polling or extend
// expiry. False means the configuration is unavailable, changed, or overloaded.
func (m *Manager) Trigger(identity string) bool {
	snap := m.opts.Cache.Snapshot()
	if snap == nil || !snap.Authoritative() {
		return false
	}
	found := false
	zones := snap.Objects.Zones()
	conflicts := zone.ConflictingApices(zones)
	for _, dz := range zones {
		if dz.Spec.Type == v1alpha1.ZoneTypeSecondary && zone.SecondaryIdentity(dz) == identity {
			found = !conflicts[dns.CanonicalName(dz.Spec.ZoneName)]
			break
		}
	}
	if !found {
		return false
	}
	m.triggerMu.Lock()
	defer m.triggerMu.Unlock()
	if !m.triggers[identity] && len(m.triggers) >= 128 {
		return false
	}
	m.triggers[identity] = true
	m.Wake()
	return true
}

func (m *Manager) takeTriggers() map[string]bool {
	m.triggerMu.Lock()
	defer m.triggerMu.Unlock()
	out := m.triggers
	m.triggers = make(map[string]bool)
	return out
}

// Snapshot returns the current immutable transferred data.
func (m *Manager) Snapshot() *Snapshot { return m.current.Load() }

// Statuses reports availability at the current clock, including expiry
// between scheduler ticks. Returned data can be safely modified by callers.
func (m *Manager) Statuses() []Status {
	current := m.opts.Cache.Snapshot()
	if current == nil {
		return nil
	}
	wanted := make(map[string]bool)
	zones := current.Objects.Zones()
	conflicts := zone.ConflictingApices(zones)
	for _, dz := range zones {
		if dz.Spec.Type == v1alpha1.ZoneTypeSecondary {
			wanted[zone.SecondaryIdentity(dz)] = true
		}
	}
	out := slices.DeleteFunc(slices.Clone(m.Snapshot().status), func(st Status) bool { return !wanted[st.Identity] })
	now := m.opts.Now()
	for i := range out {
		out[i] = statusAt(out[i], current, conflicts, now)
	}
	return out
}

// StatusFor reads only a copy matching this exact configuration and UID.
func (m *Manager) StatusFor(dz *v1alpha1.DNSZone) (Status, bool) {
	if dz == nil || dz.Spec.Type != v1alpha1.ZoneTypeSecondary {
		return Status{}, false
	}
	id := zone.SecondaryIdentity(dz)
	current := m.opts.Cache.Snapshot()
	if current == nil {
		return Status{}, false
	}
	configured, ok := current.Objects.Zone(dz.Name)
	if !ok || zone.SecondaryIdentity(configured) != id {
		return Status{}, false
	}
	for _, st := range m.Snapshot().status {
		if st.Identity == id {
			return statusAt(st, current, zone.ConflictingApices(current.Objects.Zones()), m.opts.Now()), true
		}
	}
	return Status{}, false
}

func statusAt(st Status, config *zonecache.Snapshot, conflicts map[string]bool, now time.Time) Status {
	st.Available = !st.ConfirmedAt.IsZero() && now.Before(st.ExpiresAt)
	if config == nil || !config.Authoritative() {
		st.Available = false
		st.ConfigurationError = "Zone configuration is unavailable or stale; this node answers SERVFAIL."
	}
	if apex := dns.CanonicalName(st.Name); conflicts[apex] {
		st.Available = false
		st.ConfigurationError = fmt.Sprintf("Multiple DNSZone objects configure %s; this node answers SERVFAIL at the conflicting apex.", apex)
	}
	return st
}

type entry struct {
	dz             *v1alpha1.DNSZone
	id, credential string
	z              *zone.Zone
	data           *envelope
	status         Status
	next           time.Time
	busy           bool
	cancel         context.CancelFunc
	notified       bool
	lastAttempt    time.Time
}

type result struct {
	entry   *entry
	data    *envelope
	z       *zone.Zone
	primary string
	err     error
}

// Run receives configuration changes and refreshes every zone independently.
// Shutdown cancels and waits for every transfer before persistence is closed.
func (m *Manager) Run(ctx context.Context) error {
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
			// Reconcile before committing a result: canceled or superseded
			// transfers must never recreate a deleted durable copy.
			m.reconcile(ctx, entries)
			if entries[r.entry.id] == r.entry {
				e := r.entry
				e.busy = false
				e.cancel()
				e.cancel = nil
				now := m.opts.Now()
				if r.err == nil {
					r.err = m.persist(ctx, e.id, r.data)
				}
				if r.err != nil {
					e.status.failures++
					e.status.Error = r.err.Error()
					delay := 30 * time.Second
					if e.z != nil && e.z.SOA() != nil {
						delay = timer(e.z.SOA().Retry)
					}
					e.next = now.Add(delay)
					m.opts.Logger.Warn("secondary refresh failed", "zone", e.dz.Name, "error", r.err)
				} else {
					e.data, e.z = r.data, r.z
					e.credential = r.data.Credential
					e.next = r.data.ConfirmedAt.Add(timer(r.z.SOA().Refresh))
					e.status = Status{successes: e.status.successes + 1, failures: e.status.failures, Identity: e.id, Zone: e.dz.Name, Name: e.dz.Spec.ZoneName, Serial: r.z.Serial(), RecordCount: r.z.RecordCount(), Primary: r.primary,
						ConfirmedAt: r.data.ConfirmedAt, ExpiresAt: r.data.ExpiresAt}
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		m.reconcile(ctx, entries)
		now := m.opts.Now()
		for id := range m.takeTriggers() {
			if e := entries[id]; e != nil {
				e.notified = true
			}
		}
		// Stable ordering makes initial scheduling deterministic. Running
		// and retry-delayed zones cannot monopolize the worker budget.
		ids := slices.Sorted(maps.Keys(entries))
		for _, id := range ids {
			e := entries[id]
			if active >= m.opts.MaxConcurrent {
				break
			}
			if e.notified && !e.busy {
				// At most one hint-driven SOA check per second, even under a
				// NOTIFY flood. A hint during a transfer survives its completion.
				due := e.lastAttempt.Add(time.Second)
				if due.Before(e.next) {
					e.next = due
				}
			}
			if e.busy || now.Before(e.next) {
				continue
			}
			jobCtx, jobCancel := context.WithCancel(ctx)
			e.busy, e.cancel = true, jobCancel
			e.notified, e.lastAttempt = false, now
			active++
			workers.Add(1)
			go func(e *entry) {
				defer workers.Done()
				r := m.refresh(jobCtx, e)
				select {
				case results <- r:
				case <-ctx.Done():
				}
			}(e)
		}
		m.publish(entries)
	}
}

func (m *Manager) reconcile(ctx context.Context, entries map[string]*entry) {
	snap := m.opts.Cache.Snapshot()
	if snap == nil || snap.State == zonecache.Unsynced {
		return
	}
	want := map[string]bool{}
	for _, dz := range snap.Objects.Zones() {
		if dz.Spec.Type != v1alpha1.ZoneTypeSecondary {
			continue
		}
		id := zone.SecondaryIdentity(dz)
		want[id] = true
		if entries[id] != nil {
			continue
		}
		e := &entry{dz: dz.DeepCopy(), id: id, status: Status{Identity: id, Zone: dz.Name, Name: dz.Spec.ZoneName}}
		entries[id] = e
		var err error
		e.credential, err = m.credential(dz)
		if err == nil {
			e.data, e.z, err = m.restore(ctx, e)
		}
		if err != nil && !errors.Is(err, localdb.ErrNoBlob) {
			e.status.Error = err.Error()
		}
		if e.data != nil {
			e.status.Serial, e.status.ConfirmedAt, e.status.ExpiresAt = e.z.Serial(), e.data.ConfirmedAt, e.data.ExpiresAt
			e.status.RecordCount = e.z.RecordCount()
			e.status.Primary = e.data.Primary
		}
		// Always confirm after restart; loading data never resets expiry.
	}
	for id, e := range entries {
		if !want[id] {
			if e.cancel != nil {
				e.cancel()
			}
			delete(entries, id)
			if blobs, ok := m.opts.Blobs.(interface {
				DeleteBlob(context.Context, string) error
			}); ok {
				if err := blobs.DeleteBlob(ctx, "secondary/"+id); err != nil {
					m.opts.Logger.Warn("secondary cleanup failed", "zone", e.dz.Name, "error", err)
				}
			}
		}
	}
}

func (m *Manager) publish(entries map[string]*entry) {
	snap := &Snapshot{zones: make(map[string]*zone.Zone, len(entries)), credentials: make(map[string]string, len(entries))}
	for id, e := range entries {
		if e.z != nil {
			snap.zones[id] = e.z
			if e.data != nil && e.data.Identity == id {
				// Provenance belongs to the acquired/persisted copy, never a
				// later expected credential from the mutable scheduler entry.
				snap.credentials[id] = e.data.Credential
			}
		}
		st := e.status
		st.NextRefresh = e.next
		snap.status = append(snap.status, st)
	}
	slices.SortFunc(snap.status, func(a, b Status) int {
		if a.Zone < b.Zone {
			return -1
		}
		if a.Zone > b.Zone {
			return 1
		}
		return 0
	})
	m.current.Store(snap)
}

func timer(seconds uint32) time.Duration {
	if seconds == 0 {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}

func (m *Manager) credential(dz *v1alpha1.DNSZone) (string, error) {
	if dz.Spec.Secondary == nil || len(dz.Spec.Secondary.Primaries) == 0 {
		return "", errors.New("secondary requires at least one primary")
	}
	if dz.Spec.Secondary.TSIGKeyRef == "" {
		return "unsigned", nil
	}
	if m.opts.Keyring == nil {
		return "", errors.New("secondary TSIG key is unavailable")
	}
	secret, alg, ok := m.opts.Keyring.Lookup(dnsserver.TSIGKeyName(dz.Spec.Secondary.TSIGKeyRef))
	if !ok {
		return "", errors.New("secondary TSIG key is unavailable")
	}
	h := sha256.New()
	_, _ = h.Write([]byte(alg))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(secret)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (m *Manager) refresh(ctx context.Context, e *entry) result {
	r := result{entry: e}
	if _, err := zone.BuildSecondary(e.dz, nil, time.Time{}); err != nil {
		r.err = fmt.Errorf("invalid secondary configuration: %w", err)
		return r
	}
	credential, err := m.credential(e.dz)
	if err != nil {
		r.err = err
		return r
	}
	var errs []error
	for _, primary := range e.dz.Spec.Secondary.Primaries {
		if ctx.Err() != nil {
			r.err = ctx.Err()
			return r
		}
		soa, err := m.opts.Receiver.SOA(ctx, e.dz, primary)
		var records []dns.RR
		if err == nil && soa == nil {
			err = errors.New("primary returned no SOA")
		}
		// RFC 1034 4.3.5: expired data is obsolete. Keep its serial for
		// diagnostics, but acquire a fresh copy rather than letting that
		// obsolete serial pin a restored primary out of service forever.
		baseline := e.z != nil && e.z.AvailableAt(m.opts.Now())
		if err == nil && baseline && soa.Serial == e.z.Serial() && credential == e.credential {
			records, err = e.data.records()
		} else if err == nil {
			if baseline && soa.Serial != e.z.Serial() && soa.Serial-e.z.Serial() >= 1<<31 {
				err = errors.New("primary SOA serial is older or ambiguous")
			} else if receiver, ok := m.opts.Receiver.(incrementalReceiver); ok && baseline && credential == e.credential {
				var base []dns.RR
				base, err = e.data.records()
				if err == nil {
					records, err = receiver.IXFR(ctx, e.dz, primary, base)
				}
			} else {
				records, err = m.opts.Receiver.AXFR(ctx, e.dz, primary)
			}
		}
		if err == nil {
			if len(records) == 0 {
				err = errors.New("empty zone transfer")
			} else if first, ok := records[0].(*dns.SOA); !ok || first == nil {
				err = errors.New("zone transfer does not begin with SOA")
			} else {
				// A primary may advance between the SOA poll and transfer,
				// but must not roll back a version already accepted here.
				if (baseline && first.Serial != e.z.Serial() && first.Serial-e.z.Serial() >= 1<<31) || (first.Serial != soa.Serial && first.Serial-soa.Serial >= 1<<31) {
					err = errors.New("transferred serial is older or ambiguous")
				} else {
					confirmed := m.opts.Now().UTC()
					data := &envelope{Version: 1, ClusterID: m.opts.ClusterID, Identity: e.id, Credential: credential,
						ConfirmedAt: confirmed, ExpiresAt: confirmed.Add(time.Duration(first.Expire) * time.Second), Primary: primary}
					var built *zone.Zone
					built, err = zone.BuildSecondary(e.dz, records, data.ExpiresAt)
					if err == nil {
						for _, rr := range records {
							data.Records = append(data.Records, rr.String())
						}
						r.data, r.z, r.primary = data, built, primary
						return r
					}
				}
			}
		}
		errs = append(errs, fmt.Errorf("%s: %w", primary, err))
	}
	r.err = errors.Join(errs...)
	return r
}

type envelope struct {
	Version     int       `json:"version"`
	ClusterID   string    `json:"clusterID"`
	Identity    string    `json:"identity"`
	Credential  string    `json:"credential"`
	ConfirmedAt time.Time `json:"confirmedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Primary     string    `json:"primary"`
	Records     []string  `json:"records"`
	Checksum    string    `json:"checksum"`
}

func (e *envelope) records() ([]dns.RR, error) {
	out := make([]dns.RR, 0, len(e.Records))
	for _, line := range e.Records {
		rr, err := dns.NewRR(line)
		if err != nil {
			return nil, errors.New("invalid persisted secondary record")
		}
		out = append(out, rr)
	}
	return out, nil
}

func (e *envelope) digest() string {
	candidate := *e
	candidate.Checksum = ""
	b, _ := json.Marshal(candidate)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (m *Manager) persist(ctx context.Context, id string, data *envelope) error {
	data.Checksum = data.digest()
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(b) > 256<<20 {
		return errors.New("persisted secondary exceeds size limit")
	}
	if err := m.opts.Blobs.PutBlob(ctx, "secondary/"+id, b); err != nil {
		return fmt.Errorf("persisting secondary: %w", err)
	}
	return nil
}

func (m *Manager) restore(ctx context.Context, e *entry) (*envelope, *zone.Zone, error) {
	b, err := m.opts.Blobs.GetBlob(ctx, "secondary/"+e.id)
	if err != nil {
		return nil, nil, err
	}
	if len(b.Data) > 256<<20 {
		return nil, nil, errors.New("persisted secondary exceeds size limit")
	}
	var data envelope
	if err := json.Unmarshal(b.Data, &data); err != nil {
		return nil, nil, errors.New("invalid secondary snapshot")
	}
	if data.Version != 1 || data.ClusterID != m.opts.ClusterID || data.Identity != e.id || data.Credential != e.credential || data.Checksum != data.digest() {
		return nil, nil, errors.New("secondary snapshot identity, version, credentials or checksum mismatch")
	}
	if data.ConfirmedAt.IsZero() || data.ConfirmedAt.After(m.opts.Now()) {
		return nil, nil, errors.New("invalid secondary confirmation time")
	}
	rrs, err := data.records()
	if err != nil {
		return nil, nil, err
	}
	if len(rrs) == 0 {
		return nil, nil, errors.New("empty secondary snapshot")
	}
	soa, ok := rrs[0].(*dns.SOA)
	if !ok || !data.ExpiresAt.Equal(data.ConfirmedAt.Add(time.Duration(soa.Expire)*time.Second)) {
		return nil, nil, errors.New("invalid secondary expiry")
	}
	z, err := zone.BuildSecondary(e.dz, rrs, data.ExpiresAt)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid secondary snapshot: %w", err)
	}
	return &data, z, nil
}
