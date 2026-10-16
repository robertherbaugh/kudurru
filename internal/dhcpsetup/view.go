package dhcpsetup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

// Read returns desired pending scope values and current committed API options
// for other scopes. Revisions are always current, including after a failed
// apply, so explicitly re-saving pending values can refresh their guard.
func (m *Manager) Read(ctx context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return State{}, os.ErrClosed
	}
	state, _, err := m.readLocked(ctx)
	return state, err
}

// Startup returns a detached, fixed-at-Open plan registry. Save never changes
// the instructions chosen for the current process's startup.
func (m *Manager) Startup() []ScopePlan {
	m.mu.Lock()
	defer m.mu.Unlock()
	return orderedPlans(m.startup)
}

func orderedPlans(plans map[string]ScopePlan) []ScopePlan {
	result := make([]ScopePlan, 0, len(plans))
	for _, plan := range plans {
		result = append(result, plan)
	}
	slices.SortFunc(result, func(a, b ScopePlan) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	return result
}

func (m *Manager) leasePath(name string) string {
	filename := "dhcp-" + name + ".db"
	if name == "default" {
		filename = "dhcp.db"
	}
	return filepath.Join(filepath.Dir(m.path), filename)
}

func (m *Manager) readLocked(ctx context.Context) (State, map[string]dhcp.LeaseSettings, error) {
	view := cloneState(m.state)
	view.ScopeRevisions = map[string]uint64{}
	view.PendingScopes = []string{}
	metadata := map[string]dhcp.LeaseSettings{}
	// Include removed scopes so a failed rename/retirement can be repaired by
	// re-adding the original scope with its actual revision, rather than zero.
	entries, err := os.ReadDir(filepath.Dir(m.path))
	if err != nil {
		return State{}, nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		scope := ""
		if name == "dhcp.db" {
			scope = "default"
		} else if strings.HasPrefix(name, "dhcp-") && strings.HasSuffix(name, ".db") {
			scope = strings.TrimSuffix(strings.TrimPrefix(name, "dhcp-"), ".db")
			if scope == "default" {
				continue
			}
		}
		if !safeScopeName(scope) {
			continue
		}
		meta, _, err := dhcp.ReadLeaseSettings(ctx, m.leasePath(scope))
		if err != nil {
			return State{}, nil, err
		}
		metadata[scope] = meta
		view.ScopeRevisions[scope] = meta.Revision
	}
	for i, scope := range view.Scopes {
		if err := ctx.Err(); err != nil {
			return State{}, nil, err
		}
		// Host drafts may have invalid names while disabled; do not derive any
		// filesystem path from an unchecked operator-provided scope name.
		if !safeScopeName(scope.Name) {
			return State{}, nil, fmt.Errorf("%w: unsafe scope name %q", ErrInvalidConfiguration, scope.Name)
		}
		meta, found := metadata[scope.Name]
		if !found {
			meta, _, err = dhcp.ReadLeaseSettings(ctx, m.leasePath(scope.Name))
			if err != nil {
				return State{}, nil, err
			}
		}
		metadata[scope.Name] = meta
		view.ScopeRevisions[scope.Name] = meta.Revision
		plan, planned := m.plans[scope.Name]
		pending := planned && plan.Token != meta.AppliedSetupToken
		if pending {
			view.PendingScopes = append(view.PendingScopes, scope.Name)
		}
		if !pending && scope.Management == "api" && meta.Revision > 0 {
			scope = overlaySettings(scope, meta)
			normalized, err := validateScopes(ctx, false, []config.DHCPScope{scope}, false)
			if err != nil {
				return State{}, nil, err
			}
			view.Scopes[i] = normalized[0]
		}
	}
	return view, metadata, nil
}

func safeScopeName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func overlaySettings(scope config.DHCPScope, meta dhcp.LeaseSettings) config.DHCPScope {
	opts := meta.Options
	scope.RangeStart, scope.RangeEnd, scope.Router = opts.RangeStart.String(), opts.RangeEnd.String(), opts.Router.String()
	scope.DNSServers = []string{}
	for _, ip := range opts.DNSServers {
		scope.DNSServers = append(scope.DNSServers, ip.String())
	}
	scope.DomainName, scope.LeaseDuration = opts.DomainName, config.Duration(opts.LeaseDuration)
	scope.RegisterDNS, scope.DNSTTL = opts.RegisterDNS, config.Duration(opts.DNSTTL)
	scope.Reservations = []config.DHCPReservation{}
	for _, r := range meta.Reservations {
		scope.Reservations = append(scope.Reservations, config.DHCPReservation{MAC: r.MAC, IP: r.IP.String(), Hostname: r.Hostname})
	}
	return scope
}

func (m *Manager) needsRestart(candidate State, plans map[string]ScopePlan) bool {
	if candidate.Enabled != m.baseline.Enabled || candidate.ManagedBy != m.baseline.ManagedBy || len(plans) != len(m.startup) {
		return true
	}
	for name, plan := range plans {
		if baseline, ok := m.startup[name]; !ok || plan.Token != baseline.Token {
			return true
		}
	}
	return false
}

// Restoring the running scope's topology and current API options cancels a
// pending edit. Reuse only an already-applied startup token, so a canceled edit
// cannot replay stale seed options or bypass the persisted revision guard.
func (m *Manager) restoredStartupPlan(ctx context.Context, scope config.DHCPScope, meta dhcp.LeaseSettings) (ScopePlan, bool) {
	plan, ok := m.startup[scope.Name]
	if !ok || plan.Token != meta.AppliedSetupToken {
		return ScopePlan{}, false
	}
	for _, baseline := range m.baseline.Scopes {
		if baseline.Name != scope.Name {
			continue
		}
		active := overlaySettings(baseline, meta)
		normalized, err := validateScopes(ctx, false, []config.DHCPScope{active}, false)
		if err == nil && reflect.DeepEqual(scope, normalized[0]) {
			return plan, true
		}
		break
	}
	return ScopePlan{}, false
}
