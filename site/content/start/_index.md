---
title: Get started
description: Pick the page for the way you run DNS. Each one names the guides and references that matter for that job and skips the rest.
weight: 10
---

kudurru is one binary with several roles: the DNS server (`kudurru serve`),
the client (`kudurru get`, `apply`, `zones`, `query`, ...), and the
supervisors that run dnsdist (`kudurru edge run`) and keepalived
(`kudurru vip run`) in their own containers. Which of them you need depends on
the deployment shape:

| Shape | Source of truth | Guide |
|---|---|---|
| One container (Docker, Podman) or a Linux package | `state.db` on a volume | [One container](/deploy/docker/) |
| Three hosts with a virtual IP (dnsdist + keepalived) | identical file-managed configuration on every host | [Compose HA](/deploy/compose-ha/) |
| Kubernetes, several replicas | CRDs in the apiserver | [Helm](/deploy/helm/), [kustomize](/deploy/kustomize/) |
| Kubernetes, one node ("Pi-hole on k3s") | `state.db` on a PersistentVolumeClaim | [Helm, `mode=standalone`](/deploy/helm/#modestandalone) |
