# Compose HA: three hosts, one VIP

The `ha-static` shape of dev-docs/design-record.md §4.4 and §6.5: every host runs
kudurru, dnsdist and keepalived from `deploy/compose/ha/node-{a,b,c}`.

```
          clients ──▶ VIP 10.0.0.53 (keepalived, VRRPv3 unicast, auth_hmac)
                         │
     ┌───────────────────┼────────────────────┐
  node-a .11          node-b .12           node-c .13
  dnsdist :53 ──PROXYv2──▶ kudurru :5353 on every node (each dnsdist lists all three)
```

- keepalived decides only who holds the VIP: `state BACKUP`, `nopreempt`,
  unicast peers, `auth_hmac`. Its track script probes the local dnsdist on
  :53 over UDP and TCP and requires at least one healthy backend through the
  dnsdist API, so a node whose kudurru is down but whose dnsdist is healthy
  keeps serving through the other nodes' kudurru.
- dnsdist forwards with PROXYv2 and the shared TLV secret, so every kudurru
  logs the real client address and applies client groups correctly.
- kudurru runs the same file-managed configuration on every node.
  `kudurru.yaml` and `objects.d/` are distributed by your own tooling (git,
  ansible, rsync); `cluster.role: static` makes the API and dashboard read-only for object
  changes, including objects previously managed by the API. Every member
  remains its own leader and has an independent database identity.

The `kudurru-dnsdist` and `kudurru-keepalived` images (supervisors
`kudurru edge run` and `kudurru vip run`) are milestone M4 deliverables.

Static twins do not replicate: you distribute the files, and the API accepts
no object changes. For SQLite nodes that take changes through the API and the
dashboard and copy them to the other nodes, see the
[enrolled SQLite cluster](sqlite-cluster.md). It is a different shape: a
database that is enrolled does not start with `cluster.role: static`. The
compose files of this page are not set up for it, and its acceptance behind
a keepalived VIP on separate machines has not been run.

## Prepare every host

The dnsdist and keepalived containers need a license key that includes the
edge: set `KUDURRU_LICENSE_KEY` in the environment of every host (the compose
files pass it to all three containers). Without a key a first start is refused;
an expired key never stops a restart.
A free personal key covers up to 3 nodes for eligible community use
([LICENSING.md](../../LICENSING.md)).

1. Replace the example addresses (10.0.0.11-13, VIP 10.0.0.53/24, `eth0`) in
   the three `docker-compose.yml` files: `--backends`, `--peers`, `--vip` and
   `KUDURRU_LISTENERS__PROXYPROTOCOLFROM` (exact `/32`s of the three hosts,
   never the LAN prefix). Also replace the internal endpoints in the identical
   `cluster.peers` list in every `kudurru.yaml`. Each unique name must match
   that host's `node.name`; endpoints include the TLS port (normally 8082).
2. Create the secrets once and copy the same files to every host:

   ```sh
   mkdir -p secrets
   openssl rand -base64 32 > secrets/encryption_key   # shared: sessions, internal TLS and token
   openssl rand -base64 18 > secrets/admin_password
   openssl rand -hex 32 > secrets/proxy_secret        # PROXYv2 TLV 0xE0, at least 32 bytes
   openssl rand -hex 16 > secrets/vrrp_hmac           # keepalived auth_hmac
   chmod 0600 secrets/*
   ```

3. Put the TLS certificate for DoT/DoH (the same pair on every node) into
   `certs/tls.crt` and `certs/tls.key`.
4. Copy `node-a` to the first host, `node-b` to the second, `node-c` to the
   third, and start them: `docker compose up -d`.

## Check

```sh
dig @10.0.0.53 example.com                 # through the VIP
dig @10.0.0.12 -p 5353 example.com         # kudurru on node-b directly (no PROXYv2 header: attributed to the peer)
docker compose exec keepalived kudurru health --vrrp
curl -s http://10.0.0.11:8081/healthz | jq
```

(`kudurru lameduck [5s]` drains the local node: run it with
`docker compose exec kudurru kudurru lameduck 5s` before stopping the
container; `kudurru lameduck --cancel` undoes it.) Stop kudurru on the VIP holder: the VIP stays, its dnsdist sends the
queries to the other two nodes. Stop dnsdist (or the host): the VIP moves
within `fall 3` seconds. `kudurru lameduck` drains a node from every
dnsdist's pool without moving the VIP; `kudurru vip drain` moves it (D51).

## Changing configuration

Static membership is a host-only setting that requires a restart. Its shared
TLS and bearer credentials permit internal reads; the peer list grants no
replication, enrollment or promotion authority. A static process refuses a
database already enrolled as a primary or follower. An unavailable peer remains
listed with unknown readiness; local DNS keeps using its admitted local state.

Object, cluster-setting, IAM credential and provider mutation requests return
`503 read_only` after ordinary authentication and permission checks. The
external-dns mutation webhook cannot be enabled in this mode. Supply the
bootstrap password file shown above or provision accounts before enabling
static mode. Public block-page unblock requests are disabled as well.
A generated password requiring a first-login change cannot be
changed through this read-only API. Host-local administration remains a
separate privileged operation. Sign-in, sign-out and inventory reads remain
available. Blocking toggles and cache flushes affect only the addressed node;
blocking responses explicitly report `scope: node`.

Edit `kudurru.yaml` or `objects.d/` in your source of truth and distribute it
to all three hosts. Objects re-apply on file change; keys flagged `R` in
`kudurru config schema` need `docker compose restart kudurru` (one host at a
time). Cluster settings are the exception: each twin copies them from
`kudurru.yaml` into its database on its first 0.2 start and keeps them, so a
later edit of a cluster key in the file is not applied (the start logs a
warning naming the keys whose file value differs), and the API refuses
settings writes. Set the cluster settings you need
before a host's first 0.2 start
([Upgrading from 0.1.x](runbooks.md#upgrading-from-01x)); changing them
afterwards is not supported in 0.2.0. Zone serials are
`max(prev+1, unix_now)` on each twin, close but not identical: external
secondaries should transfer from one named node.

## Audit log, query log and statistics of all nodes

Each node keeps its own audit log and query log, and its dashboard shows
those. To see all three nodes on every dashboard, give them a central
PostgreSQL database ([postgres.md](postgres.md)): put the connection string
in `secrets/database_dsn` on every host and enable
`KUDURRU_DATABASE__DSN_FILE` in the three `docker-compose.yml` files. The
database is optional for DNS: when it is away, the nodes answer as before
and keep their data until it is back.

## Upgrading

One host at a time: `docker compose pull && docker compose up -d`, wait for
`kudurru health --ready`, then the next host. See
[runbooks.md](runbooks.md#upgrading).

From 0.1.x, take the new compose files first (`command: [serve]`,
`cluster.role: static` with `cluster.peers`) and follow
[Upgrading from 0.1.x](runbooks.md#upgrading-from-01x). While one host still
runs 0.1.0, the upgraded hosts list it on **Nodes** with its version and
readiness and report its health detail as `peer health: unsupported`;
each host keeps answering from its own files.
