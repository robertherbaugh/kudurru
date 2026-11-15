// Package inventory publishes immutable client identity snapshots. Producers
// poll off the query path; readers perform bounded memory lookups only.
package inventory

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

const (
	maxSources   = 128
	maxClients   = 100000
	maxAddresses = 16
	maxAge       = 24 * time.Hour
)

// Client is an observation from one inventory source. ObservedAt is when the
// device/address was actually seen, not the time a stale poll was repeated.
// ExpiresAt is absolute and cannot be renewed after a failed poll.
type Client struct {
	Source     string       `json:"source"`
	ID         string       `json:"id"`
	Name       string       `json:"name,omitempty"`
	Hostname   string       `json:"hostname,omitempty"`
	IPs        []netip.Addr `json:"ips"`
	MACs       []string     `json:"macs,omitempty"`
	Tags       []string     `json:"tags,omitempty"`
	ObservedAt time.Time    `json:"observedAt"`
	ExpiresAt  time.Time    `json:"expiresAt"`
	names      []string
}

// Names returns the shared, read-only normalized names captured at publication.
func (c *Client) Names() []string { return c.names }

type claim struct {
	source, id        string
	observed, expires time.Time
	client            *Client // nil is a removal or ambiguous ownership claim
}

// Snapshot is immutable. Lookup results and their slices are shared read-only.
type Snapshot struct {
	generation uint64
	clients    []*Client
	byIP       map[netip.Addr]claim
}

// Generation identifies one complete published snapshot on this process.
func (s *Snapshot) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.generation
}

// Lookup returns only the newest unambiguous live claim. An expired newest
// claim never falls back to an older client's identity for the same address.
func (s *Snapshot) Lookup(ip netip.Addr, at time.Time) *Client {
	if s == nil {
		return nil
	}
	c := s.byIP[ip.Unmap().WithZone("")]
	if c.client == nil || !at.Before(c.expires) {
		return nil
	}
	return c.client
}

// Clients returns private copies, including stale devices, for diagnostics.
func (s *Snapshot) Clients() []Client {
	if s == nil {
		return nil
	}
	out := make([]Client, 0, len(s.clients))
	for _, c := range s.clients {
		out = append(out, clone(*c))
	}
	return out
}

type batch struct {
	revision uint64
	clients  []*Client
}

// Holder serializes complete producer replacements and atomically publishes
// snapshots. A producer uses a stable source ID and increasing revisions.
type Holder struct {
	mu      sync.Mutex
	sources map[string]batch
	current atomic.Pointer[Snapshot]
	now     func() time.Time
}

// New returns an empty holder. now is only called by producers, never readers.
func New(now func() time.Time) *Holder {
	if now == nil {
		now = time.Now
	}
	return &Holder{sources: map[string]batch{}, now: now}
}

// Snapshot returns a captured immutable cut; nil is an empty inventory.
func (h *Holder) Snapshot() *Snapshot {
	if h == nil {
		return nil
	}
	return h.current.Load()
}

// Replace validates a whole source batch before changing anything. Empty input
// removes that source's devices while remembering removed address ownership.
func (h *Holder) Replace(source string, revision uint64, records []Client) error {
	if source == "" || len(source) > 256 || revision == 0 || len(records) > maxClients {
		return errors.New("inventory: invalid source, revision or batch size")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.sources[source]
	if revision <= previous.revision {
		return errors.New("inventory: stale source revision")
	}
	if previous.revision == 0 && len(h.sources) >= maxSources {
		return errors.New("inventory: source limit reached")
	}
	total := len(records)
	for id, b := range h.sources {
		if id != source {
			total += len(b.clients)
		}
	}
	if total > maxClients {
		return errors.New("inventory: total client limit reached")
	}
	now := h.now()
	clients := make([]*Client, 0, len(records))
	seen := map[string]bool{}
	for _, record := range records {
		c, err := normalize(source, record, now)
		if err != nil {
			return err
		}
		if seen[c.ID] {
			return errors.New("inventory: duplicate client ID")
		}
		seen[c.ID] = true
		clients = append(clients, c)
	}
	snap := &Snapshot{byIP: map[netip.Addr]claim{}}
	old := h.current.Load()
	if old != nil {
		snap.generation = old.generation
		// Keep ownership watermarks for the longest accepted observation age.
		// Deleted/expired clients cannot resurrect behind a newer assignment.
		for ip, c := range old.byIP {
			if now.Sub(c.observed) <= maxAge {
				c.client = nil
				snap.byIP[ip] = c
			}
		}
	}
	newClaims := map[netip.Addr]bool{}
	for _, c := range clients {
		for _, ip := range c.IPs {
			newClaims[ip] = true
		}
	}
	for _, c := range previous.clients {
		for _, ip := range c.IPs {
			if !newClaims[ip] {
				p := snap.byIP[ip]
				if p.source == source && p.id == c.ID {
					snap.byIP[ip] = claim{source: source, id: c.ID, observed: now, expires: now}
				}
			}
		}
	}
	appendBatch := func(items []*Client) {
		for _, c := range items {
			snap.clients = append(snap.clients, c)
			for _, ip := range c.IPs {
				p, exists := snap.byIP[ip]
				if !exists || c.ObservedAt.After(p.observed) {
					snap.byIP[ip] = claim{source: c.Source, id: c.ID, observed: c.ObservedAt, expires: c.ExpiresAt, client: c}
				} else if c.ObservedAt.Equal(p.observed) {
					if p.source == c.Source && p.id == c.ID {
						p.client, p.expires = c, c.ExpiresAt
					} else {
						p.source, p.id, p.client = "", "", nil
					}
					snap.byIP[ip] = p
				}
			}
		}
	}
	for id, b := range h.sources {
		if id != source {
			appendBatch(b.clients)
		}
	}
	appendBatch(clients)
	if len(snap.clients) > maxClients || len(snap.byIP) > maxClients*maxAddresses {
		return errors.New("inventory: total inventory limit reached")
	}
	slices.SortFunc(snap.clients, func(a, b *Client) int {
		if n := strings.Compare(a.Source, b.Source); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	snap.generation++
	h.sources[source] = batch{revision: revision, clients: clients}
	h.current.Store(snap)
	return nil
}

func clone(c Client) Client {
	c.IPs, c.MACs, c.Tags, c.names = slices.Clone(c.IPs), slices.Clone(c.MACs), slices.Clone(c.Tags), slices.Clone(c.names)
	return c
}

func normalize(source string, c Client, now time.Time) (*Client, error) {
	c = clone(c)
	if c.ID == "" || len(c.ID) > 256 || (c.Source != "" && c.Source != source) || len(c.IPs) > maxAddresses || len(c.MACs) > maxAddresses || len(c.Tags) > 64 || len(c.Name) > 253 || len(c.Hostname) > 253 {
		return nil, errors.New("inventory: invalid client identity or limits")
	}
	if c.ObservedAt.IsZero() || c.ObservedAt.After(now) || now.Sub(c.ObservedAt) > maxAge || !c.ExpiresAt.After(c.ObservedAt) || c.ExpiresAt.Sub(c.ObservedAt) > maxAge {
		return nil, errors.New("inventory: invalid observation lifetime")
	}
	c.Source = source
	for i, ip := range c.IPs {
		if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ip.Zone() != "" {
			return nil, errors.New("inventory: invalid IP")
		}
		c.IPs[i] = ip.Unmap()
	}
	for i, mac := range c.MACs {
		m, err := net.ParseMAC(mac)
		if err != nil || len(m) != 6 || m[0]&1 != 0 {
			return nil, errors.New("inventory: invalid MAC")
		}
		c.MACs[i] = m.String()
	}
	slices.Sort(c.MACs)
	c.MACs = slices.Compact(c.MACs)
	for i, tag := range c.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || len(tag) > 128 || strings.ContainsAny(tag, "\r\n\x00") {
			return nil, errors.New("inventory: invalid tag")
		}
		c.Tags[i] = tag
	}
	slices.Sort(c.Tags)
	c.Tags = slices.Compact(c.Tags)
	c.names = nil
	for _, name := range []string{c.Name, c.Hostname} {
		name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
		if name == "" {
			continue
		}
		// A display name can be prose. Only DNS names participate in matching.
		if _, ok := dns.IsDomainName(name); ok && !strings.ContainsAny(name, " \\*\t\r\n\x00") {
			c.names = append(c.names, name)
		} else if name == strings.TrimSuffix(strings.ToLower(strings.TrimSpace(c.Hostname)), ".") {
			return nil, errors.New("inventory: invalid hostname")
		}
	}
	slices.Sort(c.names)
	c.names = slices.Compact(c.names)
	return &c, nil
}
