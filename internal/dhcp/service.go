package dhcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"sort"
	"sync"
)

// Service shares one exclusive transport per interface across named scopes.
// The caller owns stores; topology is immutable for the service's lifetime.
type Service struct {
	scopes                     map[string]*Server
	stores                     map[string]*LeaseStore
	backends                   map[string]ManagedBackend
	replicationStatus          func() ReplicationStatus
	topology                   map[string]Config
	names                      []string
	listeners                  []*Server
	mu                         sync.Mutex
	running, closed, listening bool
	closeErr                   error
}

// NewService uses committed effective pool options, including API overrides.
// It does not bind sockets or take ownership of the supplied lease stores.
func NewService(configs []Config, stores map[string]*LeaseStore, logger *slog.Logger) (*Service, error) {
	if len(configs) == 0 {
		return nil, errors.New("dhcp: at least one scope is required")
	}
	if err := ValidateTopology(configs); err != nil {
		return nil, err
	}
	s := &Service{scopes: map[string]*Server{}, stores: map[string]*LeaseStore{}, topology: map[string]Config{}}
	groups := map[string][]string{}
	effectiveConfigs := make([]Config, 0, len(configs))
	for _, original := range configs {
		cfg := normalizedConfig(original)
		store := stores[cfg.Name]
		if store == nil {
			return nil, fmt.Errorf("dhcp: missing lease store for scope %q", cfg.Name)
		}
		effective := normalizedConfig(store.Configuration().Config)
		if !sameTopology(cfg, effective) {
			return nil, fmt.Errorf("dhcp: lease store topology differs for scope %q", cfg.Name)
		}
		effectiveConfigs = append(effectiveConfigs, effective)
		server, err := NewServer(effective, store, logger)
		if err != nil {
			return nil, err
		}
		s.scopes[cfg.Name], s.stores[cfg.Name], s.topology[cfg.Name] = server, store, cfg
		s.names = append(s.names, cfg.Name)
		groups[cfg.Interface] = append(groups[cfg.Interface], cfg.Name)
	}
	if err := ValidateTopology(effectiveConfigs); err != nil {
		return nil, err
	}
	sort.Strings(s.names)
	interfaces := make([]string, 0, len(groups))
	for iface := range groups {
		interfaces = append(interfaces, iface)
	}
	sort.Strings(interfaces)
	for _, iface := range interfaces {
		cfg := s.topology[groups[iface][0]]
		cfg.RelayOnly = true
		for _, name := range groups[iface] {
			if !s.topology[name].RelayOnly {
				cfg.RelayOnly = false
			}
		}
		transport := &Server{cfg: cfg, logger: logger, listen: listenDHCP}
		transport.dispatch = func(ctx context.Context, wire []byte, peer *net.UDPAddr) ([]byte, *net.UDPAddr, error) {
			return s.dispatch(ctx, cfg.Interface, wire, peer)
		}
		s.listeners = append(s.listeners, transport)
	}
	return s, nil
}

func sameTopology(a, b Config) bool {
	return a.Name == b.Name && a.Interface == b.Interface && a.ServerIP == b.ServerIP && a.Subnet == b.Subnet &&
		a.Management == b.Management && a.RelayOnly == b.RelayOnly && a.ConflictDetection == b.ConflictDetection && a.ProbeTimeout == b.ProbeTimeout &&
		reflect.DeepEqual(a.FailoverServers, b.FailoverServers) && reflect.DeepEqual(a.TrustedRelays, b.TrustedRelays)
}

// ScopeNames returns a sorted copy of the stable scope names.
func (s *Service) ScopeNames() []string { return append([]string(nil), s.names...) }

// Scope returns the caller-owned store for a named scope.
func (s *Service) Scope(name string) (*LeaseStore, error) {
	store := s.stores[name]
	if store == nil {
		return nil, fmt.Errorf("dhcp: unknown scope %q", name)
	}
	return store, nil
}

// MutateScope serializes all managed/import writes with protocol handling.
// Even if fn returns an error after a commit, copy committed configuration before
// unlocking so the allocator and wire options always agree for the next packet.
func (s *Service) MutateScope(ctx context.Context, name string, fn func(context.Context, *LeaseStore) error) error {
	if s.backends[name] != nil {
		return ErrReplicatedMutation
	}
	server := s.scopes[name]
	if server == nil {
		return fmt.Errorf("dhcp: unknown scope %q", name)
	}
	if fn == nil {
		return errors.New("dhcp: mutation callback is required")
	}
	server.processMu.Lock()
	defer server.processMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	err := fn(ctx, s.stores[name])
	server.cfg = normalizedConfig(s.stores[name].Configuration().Config)
	return err
}

func (s *Service) dispatch(ctx context.Context, iface string, wire []byte, peer *net.UDPAddr) ([]byte, *net.UDPAddr, error) {
	request := decodeClientPacket(wire, peer)
	if request == nil {
		return nil, nil, nil
	}
	gateway, current := packetAddress(request.GatewayIPAddr), packetAddress(request.ClientIPAddr)
	for _, name := range s.names {
		cfg := s.topology[name]
		if cfg.Interface != iface {
			continue
		}
		selected := false
		if !gateway.IsUnspecified() {
			server := s.scopes[name]
			server.processMu.Lock()
			selected = server.acceptsPacket(request, peer)
			server.processMu.Unlock()
		} else if !current.IsUnspecified() {
			// Never infer the scope from option 50 or server identifier. A configured
			// remote client may legitimately bypass its relay during T1 renewal.
			selected = cfg.Subnet.Contains(current)
		} else {
			selected = !cfg.RelayOnly
		}
		if selected {
			return s.scopes[name].handlePacket(ctx, wire, peer)
		}
	}
	// A known client on a direct link may renew an invalid old address. Let
	// its allocator decide NAK vs silence, after checking all routed scopes so
	// the default LAN can never steal a legitimate remote client's renewal.
	if gateway.IsUnspecified() && !current.IsUnspecified() {
		for _, name := range s.names {
			cfg := s.topology[name]
			if cfg.Interface == iface && !cfg.RelayOnly {
				return s.scopes[name].handlePacket(ctx, wire, peer)
			}
		}
	}
	return nil, nil, nil
}

// Listen binds every interface before accepting packets. Any failure closes all
// acquired sockets; retry requires a new service, preventing partial startup.
func (s *Service) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if s.listening {
		return nil
	}
	for _, listener := range s.listeners {
		if err := listener.Listen(); err != nil {
			s.closed = true
			for _, owned := range s.listeners {
				s.closeErr = errors.Join(s.closeErr, owned.Close())
			}
			return err
		}
	}
	s.listening = true
	return nil
}

// Run supervises all bounded serial listeners. Failure of one cancels the rest.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		return net.ErrClosed
	}
	if s.running {
		s.mu.Unlock()
		return errors.New("dhcp: service is already running")
	}
	if !s.listening {
		s.mu.Unlock()
		return errors.New("dhcp: Listen must succeed before Run")
	}
	s.running = true
	s.mu.Unlock()
	defer func() { _ = s.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(s.listeners))
	for _, listener := range s.listeners {
		go func() { results <- listener.Run(ctx) }()
	}
	var result error
	for range s.listeners {
		err := <-results
		if err != nil && result == nil {
			result = err
		}
		cancel()
	}
	return result
}

// Close interrupts listeners without closing stores; it is idempotent.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		for _, listener := range s.listeners {
			s.closeErr = errors.Join(s.closeErr, listener.Close())
		}
	}
	return s.closeErr
}
