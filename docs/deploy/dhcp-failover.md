# DHCP automatic failover

[DHCP guides](dhcp.md) · [Step-by-step setup](../guide/dhcp/README.md)

## Three-node automatic failover

Configure exactly three Linux/SQLite nodes. Two voters must be reachable to
allocate or renew; losing one voter permits an election, while losing two
stops successful DHCP replies until quorum returns. Every node needs its own
persistent data directory and fixed server-facing interface addresses, including
each directly served client link. A relay-only scope can use an off-subnet
local server address. Relay
forwarding must reach all three servers. A replacement leader uses its own
server identifier: a client whose original server is down reaches it at T2
rebinding. Existing leases remain usable until their expiry. This does not
require a shared VIP and does not replicate DNS configuration objects.

1. Provision interfaces, UDP 67 access and the [required capabilities](dhcp-host.md#network-and-privileges) on
   all nodes. Permit a chosen TCP peer port between the three nodes, such as
   7447. Keep host clocks synchronized and monitor clock health; forward clock
   jumps can prematurely expire leases even when replicas agree.
2. Create the same scope names, subnets, relay trust, conflict-detection settings
   and initial options on all three dashboards. Each scope's interface and
   server IP are local to that node. Keep DHCP disabled until setup is complete.
3. Open **Automatic failover → Configure failover**. Set the same cluster ID
   and three voter IDs/advertised peer addresses everywhere, and select this
   node's distinct ID and listen address. For every scope, enter all three
   server addresses, with exactly the same mapping on every node. Those
   addresses are excluded from allocations. An optional dashboard URL is only
   a navigation reference; replication and forwarded management use peer TCP.
4. Choose credentials: the same existing `KUDURRU_ENCRYPTION_KEY` on every host
   derives authenticated peer certificates, or provide CA, certificate and
   private-key file paths on each host. Explicit certificates need both client
   and server authentication EKUs, CN equal to the voter ID, and a SAN matching
   the advertised peer host. Provision secrets outside the browser; the UI
   stores paths only. All three TLS paths are required together.
5. Select **Bootstrap this new cluster** on the alphabetically first voter
   only. That is the only node allowed to adopt existing standalone leases.
   Other voters must have empty lease state or their own existing state from
   this same cluster. Never copy another node's directory or bootstrap an
   independent cluster to recover from loss of quorum.
6. Save failover and enabled state for restart on each node, then restart all
   three. The dashboard reports voter role, leader, quorum readiness, applied
   and committed indexes. No DHCP reply succeeds before the initial seed is
   committed by a majority. Verify acquisition and renewals, then stop one
   voter and verify the remaining majority elects/retains a serving leader.

For file-managed deployment, the same fields live under `dhcp.failover`.
This example names the first node; the other two change `nodeID`,
`listenAddress`, their local scope interface/server IP, and set `bootstrap: false`:

```yaml
dhcp:
  failover:
    enabled: true
    clusterID: home-dhcp
    nodeID: stone-1
    listenAddress: 0.0.0.0:7447
    peers:
      - {id: stone-1, address: '192.0.2.2:7447'}
      - {id: stone-2, address: '192.0.2.3:7447'}
      - {id: stone-3, address: '192.0.2.4:7447'}
    bootstrap: true
```

Repeat the mapping on all replicas of a scope (or the legacy flat `dhcp`
scope), using that scope's server addresses. Different scopes can use different
mappings:

```yaml
failoverServers:
  - {nodeID: stone-1, address: 192.0.2.2}
  - {nodeID: stone-2, address: 192.0.2.3}
  - {nodeID: stone-3, address: 192.0.2.4}
```

These are fragments of the complete setup above, not standalone configurations.
Use `management: api` for replicated live network-option and reservation edits.
Adopt file-managed scopes before initializing failover if you want that live
UI ownership; an established file-managed cluster cannot change management mode
in place.
Mutations made through any healthy node's API forward to the current leader and
wait for the connected node's committed SQLite state before reporting success.
A timed-out write may still commit; reload current state and its revision before
retrying. Pool ownership is never delegated to independent local callbacks. DNS answers
on followers use their last committed snapshots, so a disconnected follower
may serve a previous lease name until expiry or catch-up.

Once a cluster identity exists, the UI freezes shared scope registry/topology,
voter IDs/addresses, cluster ID and per-scope server mappings. Change network
options and reservations through the live scope editors; imports also replicate.
Local interface, peer listen/TLS references and node enable/disable may be saved
for restart. Stop a voter by disabling DHCP while retaining failover settings.
Deleting failover settings does not convert an established replica into a
standalone allocator. Enabling failover on the initial bootstrap can apply its
staged seed once; other node-local setup tokens cannot overwrite cluster state.

Preserve the entire `dhcp-raft` directory, including `identity.json`, Bolt log
and snapshots, with the lease databases and setup registry. Restart an intact
failed node with the same identity and reachable peer address; it replays logs
or receives a snapshot. If its SQLite apply failed, correct the storage problem
before restarting it. The failed replica stops voting; the remaining two can
continue. A node with lost Raft state and a persisted initialized identity
refuses automatic bootstrap. Do not erase that guard.

A successful peer connection or heartbeat does not prove state has caught up.
After a sustained outage, the current Raft implementation can wait up to 10.24
seconds between log-replication attempts; snapshot transfer and durable apply
take additional time. The individual command timeout is not a voter-recovery
deadline. Verify applied/committed indexes and retained lease state before
declaring the restarted voter synchronized.

Changing shared topology or voter identity requires a coordinated migration:
stop all old allocators, retain complete backups and export current leases,
prepare compatible scopes on fresh directories with a new cluster ID, and
start the sole bootstrap and two empty voters with DHCP client ingress blocked.
Wait for quorum, import ownership through the live API, and verify committed
state on all replicas before exposing DHCP traffic. Imports require quorum. Preserve every live allocation/quarantine, reservation and network
option; use a nonoverlapping pool or drain leases when topology is incompatible.
Never start old and migrated clusters against the same clients. There is no
online membership-reconfiguration or force-new-cluster recovery endpoint.
