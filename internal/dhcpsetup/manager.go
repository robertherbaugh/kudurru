// Package dhcpsetup owns durable, node-local DHCP setup. Changes are desired
// configuration for the next restart; this package never opens lease stores,
// binds sockets, or changes the configuration selected at process startup.
package dhcpsetup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

var (
	// ErrRevisionConflict means another setup save changed the desired state.
	ErrRevisionConflict = errors.New("dhcpsetup: setup revision conflict")
	// ErrManagedByFile requires explicit adoption before replacing host setup.
	ErrManagedByFile = errors.New("dhcpsetup: setup is managed by the host file")
	// ErrEnvironmentLocked prevents taking over any environment-owned DHCP key.
	ErrEnvironmentLocked = errors.New("dhcpsetup: DHCP setup is locked by the environment")
	// ErrInvalidConfiguration identifies malformed or unsafe desired setup.
	ErrInvalidConfiguration = errors.New("dhcpsetup: invalid DHCP configuration")
)

const (
	settingsFilename = "dhcp-settings.json"
	lockFilename     = "dhcp-settings.lock"
	settingsVersion  = 1
	maxSettingsBytes = 4 << 20
	maxScopes        = 256
)

// State is a detached desired setup, not the currently running service state.
type State struct {
	Enabled           bool
	Scopes            []config.DHCPScope
	Revision          uint64
	ManagedBy         string
	RestartRequired   bool
	EnvironmentLocked bool
	ScopeRevisions    map[string]uint64
	PendingScopes     []string
}

// ScopePlan is one immutable startup apply instruction.
type ScopePlan struct {
	Name             string `json:"name"`
	Token            string `json:"token"`
	ExpectedRevision uint64 `json:"expectedRevision"`
}

type document struct {
	Version  int                `json:"version"`
	Revision uint64             `json:"revision"`
	Enabled  bool               `json:"enabled"`
	Scopes   []config.DHCPScope `json:"scopes"`
	Plans    []ScopePlan        `json:"plans"`
}

// Manager holds an exclusive lifetime lock even while DHCP is disabled.
// Effective is fixed at Open; Save only publishes fully persisted desired state.
type Manager struct {
	mu        sync.Mutex
	path      string
	lock      *os.File
	state     State
	baseline  State
	effective config.DHCP
	plans     map[string]ScopePlan
	startup   map[string]ScopePlan
	closed    bool
	closeErr  error
	uncertain error
	syncFile  func(*os.File) error
	rename    func(string, string) error
}

// Open chooses persisted UI setup, or the original host configuration when no
// UI file exists. Invalid saved data and environment conflicts fail closed.
func Open(dir string, host config.DHCP, envLocked []string) (*Manager, error) {
	if dir == "" {
		return nil, fmt.Errorf("dhcpsetup: data directory is required")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("dhcpsetup: create data directory: %w", err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	lock, err := acquireLock(filepath.Join(dir, lockFilename))
	if err != nil {
		return nil, err
	}
	m := &Manager{path: filepath.Join(dir, settingsFilename), lock: lock, plans: map[string]ScopePlan{}, syncFile: (*os.File).Sync, rename: os.Rename}
	fail := func(err error) (*Manager, error) { _ = m.Close(); return nil, err }
	var conflicts []string
	for _, path := range envLocked {
		if path == "dhcp" || strings.HasPrefix(path, "dhcp.") {
			conflicts = append(conflicts, path)
		}
	}
	m.state = State{Enabled: host.Enabled, Revision: 1, ManagedBy: "api", EnvironmentLocked: len(conflicts) > 0, Scopes: []config.DHCPScope{}}
	if hostConfigured(host) || len(conflicts) > 0 {
		m.state.ManagedBy = "file"
	}
	m.effective = cloneDHCP(host)
	data, err := readSettings(m.path)
	savedUI := false
	switch {
	case errors.Is(err, os.ErrNotExist):
		if hostConfigured(host) {
			scopes, err := hostScopes(host)
			if err != nil {
				return fail(err)
			}
			m.state.Scopes = scopes
		}
	case err != nil:
		return fail(err)
	default:
		savedUI = true
		if len(conflicts) > 0 {
			return fail(fmt.Errorf("%w: %s conflict with saved UI setup; remove the environment variables or %s", ErrEnvironmentLocked, strings.Join(conflicts, ", "), m.path))
		}
		saved, err := decodeDocument(data)
		if err != nil {
			return fail(err)
		}
		scopes, err := validateScopes(context.Background(), saved.Enabled, saved.Scopes, true)
		if err != nil {
			return fail(err)
		}
		m.state = State{Enabled: saved.Enabled, Scopes: scopes, Revision: saved.Revision, ManagedBy: "api"}
		if len(saved.Plans) != len(scopes) {
			return fail(fmt.Errorf("%w: scope plans do not match saved scopes", ErrInvalidConfiguration))
		}
		for _, plan := range saved.Plans {
			if plan.Token == "" || len(plan.Token) > 128 || strings.ContainsAny(plan.Token, "\x00\r\n") {
				return fail(fmt.Errorf("%w: invalid scope setup token", ErrInvalidConfiguration))
			}
			if _, duplicate := m.plans[plan.Name]; duplicate {
				return fail(fmt.Errorf("%w: duplicate scope setup plan", ErrInvalidConfiguration))
			}
			m.plans[plan.Name] = plan
		}
		for _, scope := range scopes {
			if _, found := m.plans[scope.Name]; !found {
				return fail(fmt.Errorf("%w: missing scope setup plan", ErrInvalidConfiguration))
			}
		}
	}
	view, _, err := m.readLocked(context.Background())
	if err != nil {
		return fail(err)
	}
	m.state.ScopeRevisions = maps.Clone(view.ScopeRevisions)
	m.state.PendingScopes = append([]string{}, view.PendingScopes...)
	if savedUI {
		m.effective = config.DHCP{Enabled: view.Enabled, Scopes: cloneScopes(view.Scopes)}
	}
	m.baseline = cloneState(view)
	m.startup = maps.Clone(m.plans)
	return m, nil
}

// State returns the desired configuration with all nested slices copied.
func (m *Manager) State() State { m.mu.Lock(); defer m.mu.Unlock(); return cloneState(m.state) }

// Effective returns a detached copy of the configuration selected at Open.
// Saving desired setup never changes this baseline during the process lifetime.
func (m *Manager) Effective() config.DHCP {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneDHCP(m.effective)
}

// Save stages desired configuration and its observed DB revisions. It only
// reads lease databases; startup repeats the revision/live-state checks while
// atomically committing settings and the scope's new token.
func (m *Manager) Save(ctx context.Context, revision uint64, enabled bool, scopes []config.DHCPScope, adopt bool, expected map[string]uint64) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return State{}, os.ErrClosed
	}
	if m.uncertain != nil {
		return State{}, m.uncertain
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if m.state.EnvironmentLocked {
		return State{}, fmt.Errorf("%w: remove DHCP environment overrides before using UI setup", ErrEnvironmentLocked)
	}
	if revision != m.state.Revision {
		return State{}, ErrRevisionConflict
	}
	if m.state.ManagedBy == "file" && !adopt {
		return State{}, ErrManagedByFile
	}
	normalized, err := validateScopes(ctx, enabled, scopes, true)
	if err != nil {
		return State{}, err
	}
	current, metadata, err := m.readLocked(ctx)
	if err != nil {
		return State{}, err
	}
	// Removed scopes are also covered by the GET snapshot. Their databases
	// remain untouched and retirement is checked at the next startup.
	for name, actual := range current.ScopeRevisions {
		if supplied, found := expected[name]; supplied != actual || !found && actual != 0 {
			return State{}, fmt.Errorf("%w: scope %q configuration changed", ErrRevisionConflict, name)
		}
	}
	byName := map[string]config.DHCPScope{}
	for _, scope := range current.Scopes {
		byName[scope.Name] = scope
	}
	plans := map[string]ScopePlan{}
	refreshed := false
	for _, scope := range normalized {
		meta, found := metadata[scope.Name]
		if !found {
			var err error
			meta, _, err = dhcp.ReadLeaseSettings(ctx, m.leasePath(scope.Name))
			if err != nil {
				return State{}, err
			}
			metadata[scope.Name] = meta
		}
		if supplied, exists := expected[scope.Name]; supplied != meta.Revision || !exists && meta.Revision != 0 {
			return State{}, fmt.Errorf("%w: scope %q configuration changed", ErrRevisionConflict, scope.Name)
		}
		plan, planned := m.plans[scope.Name]
		changed := !reflect.DeepEqual(scope, byName[scope.Name])
		retry := planned && plan.Token != meta.AppliedSetupToken && plan.ExpectedRevision != meta.Revision
		if !planned || changed || retry {
			if original, ok := m.restoredStartupPlan(ctx, scope, meta); ok {
				plan = original
			} else {
				plan = ScopePlan{Name: scope.Name, Token: rand.Text(), ExpectedRevision: meta.Revision}
			}
			refreshed = true
		}
		plans[scope.Name] = plan
		// Validate changed/pending settings without acquiring the live store's
		// exclusive lock. No cleanup, schema migration, or settings writes occur.
		if plan.Token != meta.AppliedSetupToken {
			configs, err := (config.DHCP{Scopes: []config.DHCPScope{scope}}).Configs()
			if err != nil {
				return State{}, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
			}
			if err := dhcp.CheckLeaseSetup(ctx, m.leasePath(scope.Name), configs[0], meta.Revision); err != nil {
				if errors.Is(err, dhcp.ErrRevisionConflict) {
					return State{}, fmt.Errorf("%w: scope %q configuration changed", ErrRevisionConflict, scope.Name)
				}
				return State{}, fmt.Errorf("%w: scope %q cannot apply: %v", ErrInvalidConfiguration, scope.Name, err)
			}
		}
	}
	candidate := State{Enabled: enabled, Scopes: normalized, ManagedBy: "api", Revision: m.state.Revision, ScopeRevisions: maps.Clone(current.ScopeRevisions), PendingScopes: []string{}}
	for _, scope := range normalized {
		meta := metadata[scope.Name]
		candidate.ScopeRevisions[scope.Name] = meta.Revision
		if plans[scope.Name].Token != meta.AppliedSetupToken {
			candidate.PendingScopes = append(candidate.PendingScopes, scope.Name)
		}
	}
	if sameDesired(candidate, current) && !refreshed {
		return current, nil
	}
	if candidate.Revision == math.MaxUint64 {
		return State{}, fmt.Errorf("dhcpsetup: setup revision exhausted")
	}
	candidate.Revision++
	candidate.RestartRequired = m.needsRestart(candidate, plans)
	data, err := json.Marshal(document{Version: settingsVersion, Revision: candidate.Revision, Enabled: enabled, Scopes: normalized, Plans: orderedPlans(plans)})
	if err != nil {
		return State{}, err
	}
	if len(data)+1 > maxSettingsBytes {
		return State{}, fmt.Errorf("%w: setup exceeds %d bytes", ErrInvalidConfiguration, maxSettingsBytes)
	}
	replaced, err := m.writeSettings(ctx, append(data, '\n'))
	if err != nil {
		if replaced {
			m.uncertain = fmt.Errorf("dhcpsetup: setup replacement could not be confirmed durable; restart before saving again: %w", err)
			return State{}, m.uncertain
		}
		return State{}, err
	}
	m.state, m.plans = candidate, plans
	return cloneState(candidate), nil
}

// Close releases the setup lock. It is idempotent and never changes saved data.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	if m.lock != nil {
		m.closeErr = m.lock.Close()
	}
	return m.closeErr
}

func sameDesired(a, b State) bool {
	return a.Enabled == b.Enabled && a.ManagedBy == b.ManagedBy && reflect.DeepEqual(a.Scopes, b.Scopes)
}

func decodeDocument(data []byte) (document, error) {
	var saved document
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return saved, fmt.Errorf("%w: decode saved setup: %v", ErrInvalidConfiguration, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return saved, fmt.Errorf("%w: expected exactly one saved document", ErrInvalidConfiguration)
	}
	if saved.Version != settingsVersion || saved.Revision == 0 {
		return saved, fmt.Errorf("%w: unsupported saved setup version or revision", ErrInvalidConfiguration)
	}
	return saved, nil
}

func cloneState(s State) State {
	s.Scopes = cloneScopes(s.Scopes)
	s.ScopeRevisions = maps.Clone(s.ScopeRevisions)
	s.PendingScopes = append([]string{}, s.PendingScopes...)
	return s
}
func cloneScopes(scopes []config.DHCPScope) []config.DHCPScope {
	result := make([]config.DHCPScope, len(scopes))
	copy(result, scopes)
	for i := range result {
		result[i].DNSServers = append([]string{}, result[i].DNSServers...)
		result[i].Reservations = append([]config.DHCPReservation{}, result[i].Reservations...)
		result[i].TrustedRelays = append([]config.DHCPTrustedRelay{}, result[i].TrustedRelays...)
	}
	return result
}
func cloneDHCP(d config.DHCP) config.DHCP {
	d.DNSServers = append([]string{}, d.DNSServers...)
	d.Reservations = append([]config.DHCPReservation{}, d.Reservations...)
	d.TrustedRelays = append([]config.DHCPTrustedRelay{}, d.TrustedRelays...)
	d.Scopes = cloneScopes(d.Scopes)
	return d
}
