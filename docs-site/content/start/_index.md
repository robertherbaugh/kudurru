---
title: Get started
description: Pick the page for the way you run DNS. Each one names the guides and references that matter for that job and skips the rest.
weight: 10
---

For a guided tour of the app, begin with [First steps](/guide/first-steps/).
If kudurru already runs, go to [Sign in](/guide/first-steps/sign-in/).

## Choose a deployment

| Shape | Source of truth | Guide |
|---|---|---|
| One container or a Linux package | SQLite on a persistent volume | [Docker and packages](/deploy/docker/) |
| Three hosts with a virtual IP | Identical file-managed configuration on each host | [Compose HA](/deploy/compose-ha/) |
| Kubernetes, several replicas | CRDs in the apiserver | [Helm](/deploy/helm/), [kustomize](/deploy/kustomize/) |
| Kubernetes, one standalone node | SQLite on a PersistentVolumeClaim | [Helm standalone mode](/deploy/helm/#modestandalone) |

After installation, follow [Connect a client](/guide/first-steps/connect/) to
verify DNS before changing your network. For address assignment, follow
[Set up DHCP](/guide/dhcp/).

Some features and multi-node deployments need a [license key](/project/licensing/).
DNS answering never depends on a key.
