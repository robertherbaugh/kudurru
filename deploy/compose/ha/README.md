# Compose HA: dnsdist + keepalived on 2 or 3 nodes

One directory per node, identical except `NODE_NAME`, `NODE_IP` and the priority
index. This is the `ha-static` profile of docs/architecture.md §6.5:

- every node runs kudurru, a dnsdist that lists **all** kudurru nodes as backends,
  and keepalived holding the VIP with VRRPv3 unicast, `state BACKUP`, `nopreempt`
  and authenticated adverts;
- configuration is file-managed and distributed by your own tooling (git, ansible,
  rsync); `cluster.role: static` makes the UI read-only so twins cannot diverge;
- follower mode (v0.2) and raft (v1.0) reuse this compose file unchanged: only
  environment variables on the kudurru service change.

Nodes: node-a 10.0.0.11 (priority 200), node-b 10.0.0.12 (190), node-c 10.0.0.13 (180).
VIP: 10.0.0.53/24 on eth0, VRID 53.

The `kudurru-dnsdist` and `kudurru-keepalived` images and the `edge run` /
`vip run` supervisors are milestone M4 deliverables. Until then this directory
documents the target shape.
