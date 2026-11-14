# Compose HA: dnsdist + keepalived on three nodes

One directory per node, identical except `KUDURRU_NODE__NAME`, `--peers` and
`--priority-index` in `docker-compose.yml`. This is the `ha-static` shape of
dev-docs/design-record.md §6.5:

- every node runs kudurru on `:5353`, a dnsdist that lists **all** kudurru
  nodes as backends (PROXYv2 with the shared TLV secret), and keepalived
  holding the VIP with VRRPv3 unicast, `state BACKUP`, `nopreempt` and
  `auth_hmac`;
- `kudurru.yaml` and `objects.d/` are identical on every node and distributed
  by your own tooling (git, ansible, rsync); the deploy tests fail if the
  three copies here differ;
- the secrets (`encryption_key`, `proxy_secret`, `vrrp_hmac`) must be the same
  files on every node: the encryption key derives the node-to-node TLS and
  token (D47, D92), the proxy secret authenticates PROXYv2.

Nodes: node-a 10.0.0.11 (priority 200), node-b 10.0.0.12 (190), node-c
10.0.0.13 (180). VIP: 10.0.0.53/24 on eth0, VRID 53. Replace the addresses
and the interface in all three directories.

v0.1.0 includes the `kudurru-dnsdist` and `kudurru-keepalived` images and the
`edge run` / `vip run` supervisors. They need a license key with the edge
entitlement; eligible personal users can request a free key. Each node is
an independent SQLite instance. `cluster.role: static` mounts the configured
`cluster.peers` for authenticated peer status and refuses API object and
cluster-setting writes, including API-owned objects. Distribute the same files
to every node; blocking toggles and cache controls remain local. These files
do not set up replication: SQLite nodes that replicate from one primary are
the separate enrolled cluster shape
([docs/deploy/sqlite-cluster.md](../../../docs/deploy/sqlite-cluster.md)),
which `cluster.role: static` excludes. The guide for this directory is
[docs/deploy/compose-ha.md](../../../docs/deploy/compose-ha.md).
