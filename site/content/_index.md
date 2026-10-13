---
title: kudurru
description: "A Kubernetes-native DNS server that also runs as one container: authoritative zones with real serials and transfers, a forwarding resolver with DoT, DoH and DoQ, blocking with client groups, a dashboard that explains every answer, and OIDC with RBAC."
actions:
  - {label: Get started, page: /start, primary: true}
  - {label: Run one container, page: /deploy/docker}
  - {label: Install with Helm, page: /deploy/helm}
links:
  - {page: /reference/configuration, text: "Every key of `kudurru.yaml`: type, default, scope, environment variable."}
  - {page: /reference/crds, text: "`DNSZone`, `DNSRecordSet`, `BlockList`, `ClientGroup` and `Policy`, field by field."}
  - {page: /reference/api, label: REST API reference, text: "The OpenAPI document, the endpoint index with permissions, and an API explorer."}
  - {page: /reference/cli, text: "Every `kudurru` command and flag."}
  - {page: /migrate, label: Migration guides, text: "How Pi-hole and AdGuard Home concepts map onto kudurru objects."}
  - {page: /architecture, label: Architecture, text: "How kudurru works inside, with a diagram for every flow: queries, policies, upstreams, the store, deployment shapes."}
---

## Status

kudurru v0.1 ("stone") is in release preparation. **No version has been
tagged yet.** The guides use the image and chart names that
the v0.1.0 release publishes (`ghcr.io/robertherbaugh/kudurru:0.1.0`,
`oci://ghcr.io/robertherbaugh/charts/kudurru`); until that tag exists, build
from source: `make web-build build` writes `bin/kudurru` with the dashboard
embedded, and `make docker` builds the image ([Contributing](/project/contributing/)).

## Source-available, free at home

kudurru is source-available under the PolyForm Noncommercial License: personal
use is free and a business needs an agreement. A license key (free for personal
use, up to 3 nodes) unlocks more than one node, more than one policy and some
features; DNS answering never depends on a key. See [Licensing](/project/licensing/).

## One object model, every shape

The same five kinds describe a home network in one container and a
multi-replica deployment in Kubernetes. In a cluster they are CRDs stored by
the apiserver; in Docker they live in an embedded SQLite store and are
written with the same YAML (`kudurru apply -f zone.yaml`). Every node answers
from memory and keeps answering from its last good snapshot when the store
is unreachable.
