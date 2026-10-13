---
title: Highly available DNS for a network
linkTitle: Network admin
audience: Network admin
description: A virtual IP held by keepalived, dnsdist on every host, real client addresses through authenticated PROXYv2, and zone transfers to your existing secondaries.
weight: 30
---

The dnsdist and keepalived supervisors need a license key that includes the
edge; a free personal key covers it ([Licensing](/project/licensing/)).

kudurru keeps the GPL programs it relies on at arm's length: dnsdist (load
balancing, packet cache, encrypted transports) and keepalived (VRRP) run as
separate programs in their own images, each with a kudurru supervisor as
PID 1 that renders their configuration, checks it with the real program and
runs it (`kudurru edge run`, `kudurru vip run`).

## The shape

```
clients ──▶ VIP (keepalived, VRRPv3 unicast, auth_hmac, nopreempt)
               │
     dnsdist :53 / :853 / :443 on every host ──PROXYv2 + TLV secret──▶ kudurru :5353 on every host
```

- keepalived decides only who holds the VIP. Its track script probes the
  local dnsdist, so a host whose kudurru is down keeps serving through the
  other hosts' kudurru, and the VIP moves only when dnsdist or the host fails.
- dnsdist forwards with PROXYv2, accepted only from the exact peers in
  [`listeners.proxyProtocolFrom`](/reference/configuration/#listeners.proxyProtocolFrom)
  and only with the shared secret of
  [`listeners.proxyProtocolSecretFile`](/reference/configuration/#listeners.proxyProtocolSecretFile).
  Every kudurru therefore logs and groups the real client address.
- `kudurru lameduck` drains one node from every dnsdist pool without moving
  the VIP; `kudurru vip drain` moves the VIP away.

Guides: [Compose HA on three hosts](/deploy/compose-ha/) (`ha-static`,
identical file-managed configuration on every host) and, in Kubernetes,
[the edge and `vip.provider=keepalived`](/deploy/helm/#edge-dnsdist) in the
Helm chart. The supervisor flags are in the CLI reference under
[`kudurru edge`](/reference/cli/#kudurru-edge) and
[`kudurru vip`](/reference/cli/#kudurru-vip).

## Authoritative zones and transfers

Primary zones have real SOA serials (`max(previous + 1, unix time)`),
serve AXFR to the secondaries you allow and send NOTIFY when the serial
changes. The zone's `spec.policy` decides who may query and transfer
(`allow`, `allowOnlyPrivate`, `useNetworkACL` with `transferACL`, ...),
and `transferTSIGKeyRefs` names TSIG keys from the
[`tsigKeys`](/reference/configuration/#tsigKeys) setting. BIND 9 as a
secondary is part of the end-to-end tests. Zones you have today import
with `kudurru import bind <zone> -f file.zone` and export again with
`kudurru zones export`.

In `ha-static` every host computes its own serial, so the serials of the
twins are close but not identical: let external secondaries transfer from
one named host.

## Resolver

- **Upstream groups** with the strategies `parallel_best`, `strict`,
  `random` and `rtt_weighted`, over UDP, TCP, DoT, DoH and DoQ
  ([`upstreams.groups`](/reference/configuration/#upstreams.groups)).
- **Conditional forwarding** by longest suffix
  ([`upstreams.conditional`](/reference/configuration/#upstreams.conditional)),
  or a `DNSZone` of type `forward` with its own upstream group.
- **EDNS Client Subnet** stripped by default
  ([`upstreams.ecs.mode`](/reference/configuration/#upstreams.ecs.mode)).
- **Rate limiting** per prefix for UDP with TC=1 slip
  ([`ratelimit.*`](/reference/configuration/#ratelimit.enabled)).
- **Cache** with prefetch and serve-stale (RFC 8767)
  ([`cache.*`](/reference/configuration/#cache.maxEntries)).

## Observability

Prometheus metrics on `:9153/metrics` (`listeners.metrics.address`), a
Grafana dashboard and alert rules in `deploy/grafana` and `deploy/prometheus`,
unauthenticated health on `:8081/healthz/*`, and `kudurru health --vrrp` for
the VRRP state of a node. The per-node query log keeps raw rows for
[`querylog.retention.raw`](/reference/configuration/#querylog.retention.raw)
and rollups for long-term views; the privacy mode is applied at ingest.

## Ports

| Port | Protocol | Purpose |
|---|---|---|
| 53 | udp, tcp | DNS (5353 PROXYv2-only behind the dnsdist edge) |
| 853 | tcp, udp | DNS over TLS, DNS over QUIC |
| 443 | tcp, udp | DNS over HTTPS, HTTP/3 |
| 8080 | tcp | API and dashboard |
| 8081 | tcp | health, no authentication |
| 8082 | tcp | node-to-node, always TLS |
| 8084 | tcp | edge supervisor readiness |
| 9153 | tcp | Prometheus metrics |

The complete list, with the external-dns webhook port, is in
[Deploying kudurru](/deploy/#ports).
