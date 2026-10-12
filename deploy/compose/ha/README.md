# Compose HA: dnsdist + keepalived on three nodes

One directory per node, identical except `KUDURRU_NODE__NAME`, `--peers` and
`--priority-index` in `docker-compose.yml`. This is the `ha-static` shape of
docs/architecture.md §6.5:

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

The `kudurru-dnsdist` and `kudurru-keepalived` images and the `edge run` /
`vip run` supervisors are milestone M4 deliverables, and the read-only API of
`cluster.role: static` lands with the API package; until then this directory
documents the target shape. The guide is
[docs/deploy/compose-ha.md](../../../docs/deploy/compose-ha.md).
