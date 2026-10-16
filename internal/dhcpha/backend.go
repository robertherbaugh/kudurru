package dhcpha

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/hashicorp/raft"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

// Backend sends DHCP work only to the local leader, while typed management
// writes can route to the current leader over authenticated peer transport.
type Backend struct {
	node  *Node
	scope string
}

// Configuration returns this node's committed effective scope settings.
func (b *Backend) Configuration() dhcp.ManagementState {
	if s := b.node.fsm.stores[b.scope]; s != nil {
		return s.Configuration()
	}
	return dhcp.ManagementState{}
}

// CheckServing fences nonmutating DHCP responses with current quorum authority.
func (b *Backend) CheckServing(ctx context.Context) error { return b.node.Ready(ctx) }
func (b *Backend) execute(ctx context.Context, c dhcp.ReplicaCommand, management bool) (dhcp.ReplicaResult, error) {
	if b.node.fsm.stores[b.scope] == nil {
		return dhcp.ReplicaResult{}, errors.New("dhcp HA: unknown scope")
	}
	cmd := command{Scope: b.scope, Lease: c, Fingerprint: b.node.fsm.fingerprint}
	if b.node.raft.State() == raft.Leader || !management {
		return b.node.propose(ctx, cmd, false)
	}
	if b.node.closed.Load() || b.node.fsm.failed.Load() != nil {
		return dhcp.ReplicaResult{}, ErrUnavailable
	}
	leader, _ := b.node.raft.LeaderWithID()
	if leader == "" {
		return dhcp.ReplicaResult{}, ErrNotLeader
	}
	return b.node.remote(ctx, string(leader), cmd, false)
}

// Offer commits an address hold before returning it to the protocol handler.
func (b *Backend) Offer(ctx context.Context, c dhcp.Client, ip netip.Addr) (dhcp.Lease, error) {
	r, e := b.execute(ctx, dhcp.ReplicaCommand{Operation: "offer", Client: c, Address: ip}, false)
	return r.Lease, e
}

// Acknowledge commits a binding before permitting its DHCP ACK.
func (b *Backend) Acknowledge(ctx context.Context, c dhcp.Client, ip netip.Addr, mode dhcp.RequestMode) (dhcp.Lease, error) {
	r, e := b.execute(ctx, dhcp.ReplicaCommand{Operation: "acknowledge", Client: c, Address: ip, Mode: mode}, false)
	return r.Lease, e
}

// Release removes matching client ownership through the quorum.
func (b *Backend) Release(ctx context.Context, c dhcp.Client, ip netip.Addr) error {
	_, e := b.execute(ctx, dhcp.ReplicaCommand{Operation: "release", Client: c, Address: ip}, false)
	return e
}

// Decline durably quarantines the client's address on every replica.
func (b *Backend) Decline(ctx context.Context, c dhcp.Client, ip netip.Addr) error {
	_, e := b.execute(ctx, dhcp.ReplicaCommand{Operation: "decline", Client: c, Address: ip}, false)
	return e
}

// QuarantineOffer commits a detected conflict only for the exact held offer.
func (b *Backend) QuarantineOffer(ctx context.Context, c dhcp.Client, ip netip.Addr) error {
	_, e := b.execute(ctx, dhcp.ReplicaCommand{Operation: "quarantine", Client: c, Address: ip}, false)
	return e
}

// UpdateOptions routes a revision-checked settings mutation to the leader.
func (b *Backend) UpdateOptions(ctx context.Context, revision uint64, p dhcp.PoolOptions) error {
	_, e := b.execute(ctx, dhcp.ReplicaCommand{Operation: "options", Revision: revision, Options: p}, true)
	return e
}

// PutReservation routes a revision-checked reservation to the leader.
func (b *Backend) PutReservation(ctx context.Context, revision uint64, r dhcp.Reservation) error {
	_, e := b.execute(ctx, dhcp.ReplicaCommand{Operation: "put-reservation", Revision: revision, Reservation: r}, true)
	return e
}

// DeleteReservation routes removal to the leader, preserving live ownership.
func (b *Backend) DeleteReservation(ctx context.Context, revision uint64, mac string) error {
	_, e := b.execute(ctx, dhcp.ReplicaCommand{Operation: "delete-reservation", Revision: revision, MAC: mac}, true)
	return e
}

// Import merges leases through consensus without replacing existing owners.
func (b *Backend) Import(ctx context.Context, format string, data []byte) (dhcp.ImportResult, error) {
	r, e := b.execute(ctx, dhcp.ReplicaCommand{Operation: "import", Format: format, Data: data}, true)
	return r.Import, e
}

type managementRequest struct {
	Command command
	Seed    bool
}
type managementResponse struct {
	Result      dhcp.ReplicaResult
	Code, Error string
	Index       uint64
}

func (n *Node) remote(ctx context.Context, address string, cmd command, seed bool) (dhcp.ReplicaResult, error) {
	ctx, cancel := context.WithTimeout(ctx, n.cfg.RequestTimeout)
	defer cancel()
	conn, err := n.stream.dial(ctx, address, "management", n.cfg.RequestTimeout)
	if err != nil {
		return dhcp.ReplicaResult{}, errors.Join(ErrUnavailable, err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if err = writeFrame(conn, managementRequest{cmd, seed}); err != nil {
		return dhcp.ReplicaResult{}, errors.Join(ErrUnavailable, err, ctx.Err())
	}
	var response managementResponse
	if err = readFrame(conn, &response, maxCommandBytes); err != nil {
		return dhcp.ReplicaResult{}, errors.Join(ErrUnavailable, err, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return dhcp.ReplicaResult{}, errors.Join(ErrUnavailable, err)
	}
	if !seed && response.Index > 0 {
		if err := n.waitApplied(ctx, cmd.Scope, response.Index); err != nil {
			return dhcp.ReplicaResult{}, err
		}
	}
	if response.Error != "" {
		return response.Result, decodeError(response.Code, response.Error)
	}
	return response.Result, nil
}

// waitApplied fences forwarded management results without waiting on SQLite's
// transaction mutex. Its budget is the same one used for forwarding the call.
func (n *Node) waitApplied(ctx context.Context, scope string, index uint64) error {
	store := n.fsm.stores[scope]
	if store == nil {
		return ErrUnavailable
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrUnavailable, err)
		}
		if n.closed.Load() || n.fsm.failed.Load() != nil {
			return ErrUnavailable
		}
		if store.ReplicaAppliedIndex() >= index {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrUnavailable, ctx.Err())
		case <-n.done:
			return ErrUnavailable
		case <-ticker.C:
		}
	}
}

func (n *Node) serveManagement(conn net.Conn, peer string) {
	select {
	case n.management <- struct{}{}:
		defer func() { <-n.management }()
	default:
		return
	}
	limit := uint32(maxCommandBytes)
	if peer == n.cfg.Peers[0].ID {
		limit = maxSnapshotBytes
	}
	var request managementRequest
	if readFrame(conn, &request, limit) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), n.cfg.RequestTimeout)
	defer cancel()
	var result dhcp.ReplicaResult
	var err error
	cmd := request.Command
	if cmd.Fingerprint != n.fsm.fingerprint {
		err = ErrUnavailable
	} else if request.Seed {
		if peer != n.cfg.Peers[0].ID || cmd.Seed == nil {
			err = ErrUnavailable
		} else {
			result, err = n.propose(ctx, cmd, true)
		}
	} else if cmd.Seed != nil {
		err = ErrUnavailable
	} else {
		switch cmd.Lease.Operation {
		case "options", "put-reservation", "delete-reservation", "import":
			if n.fsm.stores[cmd.Scope] == nil {
				err = errors.New("dhcp HA: unknown scope")
			} else {
				result, err = n.propose(ctx, cmd, false)
			}
		default:
			err = errors.New("dhcp HA: remote protocol allocation is forbidden")
		}
	}
	r := managementResponse{Result: result}
	if !request.Seed {
		if store := n.fsm.stores[cmd.Scope]; store != nil {
			r.Index = store.ReplicaAppliedIndex()
		}
	}
	if err != nil {
		r.Code = encodeError(err)
		r.Error = err.Error()
	}
	_ = writeFrame(conn, r)
}

var errorCodes = map[string]error{
	"not-leader": ErrNotLeader, "no-quorum": ErrNoQuorum, "unavailable": ErrUnavailable,
	"no-address": dhcp.ErrNoAddress, "unknown-lease": dhcp.ErrUnknownLease, "address-unavailable": dhcp.ErrUnavailable,
	"revision-conflict": dhcp.ErrRevisionConflict, "managed-by-file": dhcp.ErrManagedByFile,
	"invalid-import": dhcp.ErrInvalidImport, "import-too-large": dhcp.ErrImportTooLarge,
}

func encodeError(err error) string {
	for code, target := range errorCodes {
		if errors.Is(err, target) {
			return code
		}
	}
	return "rejected"
}
func decodeError(code, message string) error {
	if target := errorCodes[code]; target != nil {
		return errors.Join(target, errors.New(message))
	}
	return errors.New(message)
}
