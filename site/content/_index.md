---
title: kudurru
description: "A Kubernetes-native DNS server that also runs as one container: authoritative zones with real serials and transfers, a resolver that forwards over DoT, DoH and DoQ or resolves from the root with DNSSEC validation, blocking with client groups, a dashboard that explains every answer, and OIDC with RBAC."
actions:
  - {label: Get started, page: /start, primary: true}
  - {label: Run one container, page: /deploy/docker}
  - {label: Install with Helm, page: /deploy/helm}
links:
  - {page: /reference/configuration, text: "Every key of `kudurru.yaml`: type, default, scope, environment variable."}
  - {page: /reference/crds, text: "`DNSZone`, `DNSRecordSet`, `BlockList`, `ClientGroup` and `Policy`, field by field."}
  - {page: /reference/api, label: REST API reference, text: "The OpenAPI document, the endpoint index with permissions, and an API explorer."}
  - {page: /reference/cli, text: "Every `kudurru` command and flag."}
  - {page: /migrate, label: Migration guides, text: "Compare and migrate from Pi-hole, AdGuard Home or Technitium."}
  - {page: /architecture, label: Architecture, text: "How kudurru works inside, with a diagram for every flow: queries, policies, upstreams, the store, deployment shapes."}
---

## Status

**v0.1.0 ("stone") · 16 October 2026.** The first release includes the
resolver (forwarding, or recursive with DNSSEC validation), primary and
forward zones, outbound zone transfers, blocking, the dashboard and CLI,
Kubernetes CRDs and Helm charts, optional edge supervisors, and
[opt-in DHCPv4 on Linux](/deploy/dhcp/) with either store, with dashboard
setup and optional three-node quorum failover on SQLite or an elected serving
replica on [Kubernetes](/deploy/dhcp/#kubernetes). Use `ghcr.io/robertherbaugh/kudurru:0.1.0` for Docker and
`--version 0.1.0` with the Helm charts. See the
[changelog](/project/changelog/) for the release scope and known limitations,
or the [release assets](https://github.com/robertherbaugh/kudurru/releases/tag/v0.1.0)
for binaries and packages. The [architecture tour](/architecture/#built-and-not-built)
describes what is implemented and what is planned.

## Source-available, free at home

kudurru's core uses the Kudurru Community License for personal, homelab,
qualifying nonprofit and isolated community-development uses. Business and
government production use needs an accepted commercial agreement and Order
Form. Other folders have their own notices. A license key (free for eligible
community use, up to 3 nodes) unlocks more than one node, more than one
policy and some features; DNS answering never depends on a key. See
[Licensing](/project/licensing/).

## One object model, every shape

The same five kinds describe a home network in one container and a
multi-replica deployment in Kubernetes. In a cluster they are CRDs stored by
the apiserver; in Docker they live in an embedded SQLite store and are
written with the same YAML (`kudurru apply -f zone.yaml`). Every node answers
from memory and keeps answering from its last good snapshot when the store
is unreachable.
