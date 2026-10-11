package cluster

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// ErrNoLeader is returned by Membership.Leader while no leader is known
// (no Lease holder yet, or a static list without a leader).
var ErrNoLeader = errors.New("cluster: no leader known")

// Peer is one node as seen by the membership source.
type Peer struct {
	// Name is the node identity (node.name; the pod name in Kubernetes,
	// which is also the Lease holder identity).
	Name string `json:"name"`
	// Address is the host:port of the node's internal listener (:8082).
	// It is empty for this node when the node never dials itself.
	Address string `json:"address,omitempty"`
	// Self is true for the local node.
	Self bool `json:"self,omitempty"`
}

// Membership answers "who are the nodes and who leads" for the internal
// plane (docs/architecture.md §3.4: static | endpointslice | follower |
// raft). Replicas use Leader to find the node that serves list bodies by
// digest (§7.3); the Nodes page scatter-gathers Peers.
type Membership interface {
	// Leader returns the current leader. Self is set when this node leads,
	// in which case callers read local state instead of dialling.
	Leader(ctx context.Context) (Peer, error)
	// Peers returns every known node including this one.
	Peers(ctx context.Context) ([]Peer, error)
}

// Self is the single-node membership: this node is the only member and
// always the leader. It fits Docker single-instance and the ha-static
// shape, where every node is its own leader with its own state.db.
type Self struct {
	// Name is node.name.
	Name string
}

// Leader implements Membership.
func (s Self) Leader(context.Context) (Peer, error) {
	return Peer{Name: s.Name, Self: true}, nil
}

// Peers implements Membership.
func (s Self) Peers(context.Context) ([]Peer, error) {
	return []Peer{{Name: s.Name, Self: true}}, nil
}

// Static is a fixed member list from configuration. LeaderName picks the
// leader among Peers; the peer whose Name equals SelfName is this node.
type Static struct {
	SelfName   string
	LeaderName string
	Members    []Peer
}

// Leader implements Membership.
func (s *Static) Leader(context.Context) (Peer, error) {
	if s.LeaderName == "" {
		return Peer{}, ErrNoLeader
	}
	if s.LeaderName == s.SelfName {
		return Peer{Name: s.SelfName, Self: true}, nil
	}
	for _, p := range s.Members {
		if p.Name == s.LeaderName {
			p.Self = false
			return p, nil
		}
	}
	return Peer{}, fmt.Errorf("%w: leader %q is not in the static member list", ErrNoLeader, s.LeaderName)
}

// Peers implements Membership.
func (s *Static) Peers(context.Context) ([]Peer, error) {
	out := make([]Peer, 0, len(s.Members)+1)
	self := false
	for _, p := range s.Members {
		p.Self = p.Name == s.SelfName
		self = self || p.Self
		out = append(out, p)
	}
	if !self && s.SelfName != "" {
		out = append(out, Peer{Name: s.SelfName, Self: true})
	}
	return out, nil
}

// ResolveFunc maps a node identity (the Lease holder) to the address of
// its internal listener, for example "<pod>.<headless-service>:8082" or an
// EndpointSlice lookup.
type ResolveFunc func(ctx context.Context, identity string) (string, error)

// ElectorMembership derives the leader from a store.Elector (the Lease
// holder in Kubernetes) and resolves its identity through Resolve. Peers
// lists only the leader and this node; the EndpointSlice source that
// enumerates every replica lands with the Nodes page.
type ElectorMembership struct {
	SelfName string
	Elector  store.Elector
	Resolve  ResolveFunc
	// CacheTTL bounds how long a resolved leader address is reused; zero
	// keeps it until the leader changes (fine for DNS names, too long for
	// pod IPs: a leader pod that restarts under the same name gets a new
	// address).
	CacheTTL time.Duration
	// Now is the clock of CacheTTL; default time.Now.
	Now func() time.Time

	mu       sync.Mutex
	cache    map[string]string
	cachedAt time.Time
}

// Leader implements Membership.
func (m *ElectorMembership) Leader(ctx context.Context) (Peer, error) {
	if m.Elector != nil && m.Elector.IsLeader() {
		return Peer{Name: m.SelfName, Self: true}, nil
	}
	var id string
	if m.Elector != nil {
		id = m.Elector.Leader()
	}
	switch {
	case id == "":
		return Peer{}, ErrNoLeader
	case id == m.SelfName:
		// The elector reports this node's identity without an active term
		// (the term is ending or starting): treat it as unknown rather than
		// dialling ourselves.
		return Peer{}, ErrNoLeader
	case m.Resolve == nil:
		return Peer{}, fmt.Errorf("%w: no resolver for leader %q", ErrNoLeader, id)
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	m.mu.Lock()
	addr, ok := m.cache[id]
	if ok && m.CacheTTL > 0 && now().Sub(m.cachedAt) >= m.CacheTTL {
		ok = false
	}
	m.mu.Unlock()
	if !ok {
		var err error
		addr, err = m.Resolve(ctx, id)
		if err != nil {
			return Peer{}, fmt.Errorf("cluster: resolve leader %q: %w", id, err)
		}
		m.mu.Lock()
		// One entry: a new leader replaces the old one.
		m.cache = map[string]string{id: addr}
		m.cachedAt = now()
		m.mu.Unlock()
	}
	return Peer{Name: id, Address: addr}, nil
}

// Peers implements Membership.
func (m *ElectorMembership) Peers(ctx context.Context) ([]Peer, error) {
	out := []Peer{{Name: m.SelfName, Self: true}}
	if l, err := m.Leader(ctx); err == nil && !l.Self {
		out = append(out, l)
	}
	return out, nil
}
