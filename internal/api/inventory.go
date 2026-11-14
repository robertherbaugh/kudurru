package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dhcpdns"
	"github.com/robertherbaugh/kudurru/internal/inventory"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// InventoryReadSource returns the inventory snapshot that serves DNS for this
// request: the original serving cut on the complete SQLite runtime, the shared
// producer holder on Kubernetes. A nil snapshot is an empty inventory (D397).
type InventoryReadSource func(context.Context) (*inventory.Snapshot, error)

// InventoryLeaseSource lists this node's bound DHCP leases from the committed
// in-memory lease snapshots (dhcpdns.Resolver).
type InventoryLeaseSource interface {
	Leases() []dhcpdns.Lease
}

// Inventory device kinds.
const (
	InventoryKindSource = "inventory"
	InventoryKindDHCP   = "dhcp"
)

// InventoryAddress is one reported address of a device and what it means for
// DNS identity now. Only "assigned" identifies queries from the address.
type InventoryAddress struct {
	IP          string `json:"ip"`
	State       string `json:"state" enum:"assigned,expired,ambiguous,superseded,withdrawn" doc:"assigned: queries from this address get this device's identity. expired: the observation passed expiresAt. ambiguous: an equally new claim of another device collides (or, for DHCP, another scope leases the address to a different MAC), so neither identifies. superseded: ownerSource/ownerId holds a newer claim; for a DHCP lease, an inventory device with MACs owns the address and its MACs win. withdrawn: the device's own source later removed the claim."`
	OwnerSource string `json:"ownerSource,omitempty" doc:"Source of the claim that owns a superseded address."`
	OwnerID     string `json:"ownerId,omitempty" doc:"Device ID of the claim that owns a superseded address."`
}

// InventoryDevice is one device known to the inventory, whether or not it
// has sent a DNS query: an inventory source observation (UniFi, webhook) or
// a bound DHCP lease of this node. GET /clients lists query-log clients.
type InventoryDevice struct {
	Source     string             `json:"source" doc:"Inventory source ID (integration-<digest> for an integration), or dhcp/<scope> for a lease of this node."`
	Kind       string             `json:"kind" enum:"inventory,dhcp"`
	ID         string             `json:"id" doc:"Device ID within the source; the leased address for DHCP."`
	Name       string             `json:"name,omitempty"`
	Hostname   string             `json:"hostname,omitempty"`
	MACs       []string           `json:"macs" nullable:"false"`
	Tags       []string           `json:"tags" nullable:"false"`
	Addresses  []InventoryAddress `json:"addresses" nullable:"false"`
	ObservedAt *time.Time         `json:"observedAt,omitempty" doc:"When the source actually saw the device; absent for DHCP leases."`
	ExpiresAt  time.Time          `json:"expiresAt" doc:"Absolute end of the observation or lease; failed polls never extend it."`
	Stale      bool               `json:"stale" doc:"The observation or lease has expired; the device no longer identifies queries."`
}

// InventoryBody is GET /inventory.
type InventoryBody struct {
	List[InventoryDevice]
	Node       string    `json:"node" doc:"The node that answered; DHCP leases are this node's."`
	Generation uint64    `json:"generation" doc:"Generation of the inventory snapshot read (0: empty)."`
	ReadAt     time.Time `json:"readAt" doc:"Time used to decide staleness and address states."`
	// NodeLocalLeases lets a session without cluster.read (operator,
	// auditor) learn what GET /cluster would tell it: this node is one member
	// of a SQLite cluster, so its DHCP leases are its own (D397).
	NodeLocalLeases bool `json:"nodeLocalLeases" doc:"True when the answering node is one member of a SQLite cluster: lease devices and lease MACs are this node's own, and a lease MAC selects mac matchers only for queries this node answers (D397)."`
}

type inventoryOutput struct{ Body *InventoryBody }

type inventoryInput struct {
	Source   string `query:"source" maxLength:"300" doc:"Only devices of this exact source."`
	Kind     string `query:"kind" enum:"inventory,dhcp," doc:"Only inventory-source devices or only DHCP leases."`
	Stale    string `query:"stale" enum:"true,false," doc:"Only stale (true) or only live (false) devices."`
	Q        string `query:"q" maxLength:"253" doc:"Case-insensitive substring of the ID, name, hostname, a MAC or an address."`
	Limit    int    `query:"limit" minimum:"0" maximum:"5000" doc:"Page size (default 500)."`
	Continue string `query:"continue" maxLength:"1024" doc:"Token from the previous page."`
}

func (s *Server) registerInventory() {
	op := operation(http.MethodGet, "/inventory", "listInventory", "Devices known to the inventory, including ones that never queried", "clients")
	op.Description = "Inventory devices from integration sources and this node's DHCP leases, with MACs, names, tags, staleness and the ownership state of every address (D196, D397). " +
		"Unlike GET /clients it does not depend on DNS traffic. Pages are cut by (source, id); each page reads the current snapshot."
	register(s, op, perm{resource: "inventory", verb: "read"}, s.listInventory)
}

func (s *Server) listInventory(ctx context.Context, in *inventoryInput) (*inventoryOutput, error) {
	if s.opts.InventoryReads == nil {
		return nil, newError(http.StatusServiceUnavailable, KeyUnavailable, "the client inventory is not mounted on this server",
			"a complete kudurru serve node mounts it; send the request to such a node, or read clients seen in DNS traffic with GET /api/v1/clients")
	}
	snap, err := s.opts.InventoryReads(ctx)
	if err != nil || ctx.Err() != nil {
		return nil, errUnavailable("the serving inventory is unavailable")
	}
	after, err := decodeCursor(in.Continue)
	if err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 500
	}
	at := s.opts.Now()
	var leases []dhcpdns.Lease
	if s.opts.InventoryLeases != nil {
		leases = s.opts.InventoryLeases.Leases()
	}
	devices := inventoryDevices(snap, leases, at)
	q := strings.ToLower(strings.TrimSpace(in.Q))
	out := make([]InventoryDevice, 0, min(limit, len(devices)))
	cont := ""
	afterSource, afterID, _ := strings.Cut(after, "\x00")
	for _, d := range devices {
		if after != "" && cmp.Or(strings.Compare(d.Source, afterSource), strings.Compare(d.ID, afterID)) <= 0 || !inventoryMatch(d, in, q) {
			continue
		}
		if len(out) == limit {
			cont = encodeCursor(out[len(out)-1].Source + "\x00" + out[len(out)-1].ID)
			break
		}
		out = append(out, d)
	}
	return &inventoryOutput{Body: &InventoryBody{List: *newList(out, cont), Node: s.opts.NodeName, Generation: snap.Generation(), ReadAt: at, NodeLocalLeases: s.sqliteClusterMember(ctx)}}, nil
}

// sqliteClusterMember reports whether this node is one member of a SQLite
// cluster, as GET /cluster shows it: the SQLite driver with more than one
// peer, or a primary or follower replication role.
func (s *Server) sqliteClusterMember(ctx context.Context) bool {
	info := s.opts.Store.Info()
	if info.Driver != sqlite.DriverName {
		return false
	}
	if info.Role == "primary" || info.Role == "follower" || s.clusterRole(ctx).Follower {
		return true
	}
	peers, err := s.opts.Membership.Peers(ctx)
	return err == nil && len(peers) > 1
}

func inventoryMatch(d InventoryDevice, in *inventoryInput, q string) bool {
	if in.Source != "" && d.Source != in.Source || in.Kind != "" && d.Kind != in.Kind {
		return false
	}
	if in.Stale == "true" && !d.Stale || in.Stale == "false" && d.Stale {
		return false
	}
	if q == "" {
		return true
	}
	fields := append([]string{d.ID, d.Name, d.Hostname}, d.MACs...)
	for _, a := range d.Addresses {
		fields = append(fields, a.IP)
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// inventoryDevices merges the snapshot with the leases under the identify
// stage's rule: inventory MACs win, and a live lease fills the MAC only for an
// address without them (D397). The result is sorted by (source, id).
func inventoryDevices(snap *inventory.Snapshot, leases []dhcpdns.Lease, at time.Time) []InventoryDevice {
	devices := snap.Devices(at)
	out := make([]InventoryDevice, 0, len(devices)+len(leases))
	for _, d := range devices {
		observed := d.ObservedAt
		item := InventoryDevice{Source: d.Source, Kind: InventoryKindSource, ID: d.ID, Name: d.Name, Hostname: d.Hostname,
			MACs: nonNil(d.MACs), Tags: nonNil(d.Tags), Addresses: make([]InventoryAddress, 0, len(d.Addresses)), ObservedAt: &observed, ExpiresAt: d.ExpiresAt, Stale: d.Stale}
		for _, a := range d.Addresses {
			item.Addresses = append(item.Addresses, InventoryAddress{IP: a.IP.String(), State: string(a.State), OwnerSource: a.OwnerSource, OwnerID: a.OwnerID})
		}
		out = append(out, item)
	}
	live := map[string]string{} // address -> MAC of the live leases; "" when they disagree
	for _, l := range leases {
		if !at.Before(l.ExpiresAt) {
			continue
		}
		key := l.Address.String()
		if mac, seen := live[key]; seen && mac != l.MAC {
			live[key] = ""
		} else if !seen {
			live[key] = l.MAC
		}
	}
	for _, l := range leases {
		addr := InventoryAddress{IP: l.Address.String(), State: string(inventory.AddressAssigned)}
		stale := !at.Before(l.ExpiresAt)
		switch {
		case stale:
			addr.State = string(inventory.AddressExpired)
		case live[addr.IP] == "":
			addr.State = string(inventory.AddressAmbiguous)
		default:
			if c := snap.Lookup(l.Address, at); c != nil && len(c.MACs) > 0 {
				addr.State, addr.OwnerSource, addr.OwnerID = string(inventory.AddressSuperseded), c.Source, c.ID
			}
		}
		out = append(out, InventoryDevice{Source: "dhcp/" + l.Scope, Kind: InventoryKindDHCP, ID: addr.IP, Hostname: l.Hostname,
			MACs: []string{l.MAC}, Tags: []string{}, Addresses: []InventoryAddress{addr}, ExpiresAt: l.ExpiresAt, Stale: stale})
	}
	slices.SortFunc(out, func(a, b InventoryDevice) int {
		return cmp.Or(strings.Compare(a.Source, b.Source), strings.Compare(a.ID, b.ID))
	})
	return out
}
