# Compose HA: three hosts, one VIP

The `ha-static` shape of docs/architecture.md §4.4 and §6.5: every host runs
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
  ansible, rsync); the UI is read-only for `kudurru.io` objects in this shape.

The `kudurru-dnsdist` and `kudurru-keepalived` images (supervisors
`kudurru edge run` and `kudurru vip run`) are milestone M4 deliverables.

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
   never the LAN prefix).
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

Edit `kudurru.yaml` or `objects.d/` in your source of truth and distribute it
to all three hosts. Objects re-apply on file change; keys flagged `R` in
`kudurru config schema` need `docker compose restart kudurru` (one host at a
time). Zone serials are `max(prev+1, unix_now)` on each twin, close but not
identical: external secondaries should transfer from one named node.

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
