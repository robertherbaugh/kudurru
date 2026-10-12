---
title: Kubernetes with Helm
linkTitle: Kubernetes
audience: Kubernetes operator
description: Two charts, CRDs as the source of truth, replicas that answer from memory through an apiserver outage, and aggregated RBAC roles.
weight: 20
---

In a cluster the `kudurru.io` CRDs are the configuration: zones, record
sets, lists, client groups and policies are objects you `kubectl apply`,
review in Git and sync with your GitOps tool. Every replica watches them,
serves from memory and keeps the last good snapshot in `local.db`, so DNS
keeps answering while the apiserver is down. One replica, elected with a
Lease, runs the controller (serials, list fetching, NOTIFY).

## Install

Two charts, so that `helm uninstall` never deletes the CRDs and with them
every object: `kudurru-crds` and `kudurru`. The [Helm guide](/deploy/helm/)
covers the install, the first login, every deployment shape and the values;
[kustomize](/deploy/kustomize/) has the same manifests as overlays.

```sh
kubectl create namespace kudurru-system
kubectl label namespace kudurru-system pod-security.kubernetes.io/enforce=restricted
helm install kudurru-crds oci://ghcr.io/robertherbaugh/charts/kudurru-crds --version 0.1.0
helm install kudurru oci://ghcr.io/robertherbaugh/charts/kudurru --version 0.1.0 \
  --namespace kudurru-system
kubectl create namespace dns && kubectl create namespace apps   # the samples' namespaces
kubectl apply -k config/samples/
kubectl get zone,rrset,blocklist,cg,pol -A
```

Two replicas need a license key: without one kudurru runs as one node, so
changes through the API and the dashboard answer `402 license_required` until
you add a key (a free personal key covers 3 nodes) or set `replicaCount=1`
([license key](/deploy/helm/#license-key)). DNS answering is not affected.

The defaults follow the Restricted Pod Security Standard: two replicas, a
PodDisruptionBudget, topology spread and one mixed-protocol `LoadBalancer`
Service with `externalTrafficPolicy: Local`, because client addresses are
what client groups and the query log are about.

## Choose a shape

| Goal | Where |
|---|---|
| Pin the Service address with MetalLB or kube-vip | [Shapes](/deploy/helm/#shapes): `vip.provider` |
| A load balancer without mixed-protocol support | `service.dns.splitProtocols=true` |
| One pod per node on the host network | `hostNetwork.enabled=true` |
| One node and no CRDs | [`mode=standalone`](/deploy/helm/#modestandalone) |
| dnsdist in front (DoT/DoH/DoQ, packet cache) | [Edge](/deploy/helm/#edge-dnsdist): `edge.enabled=true` |
| DNS that starts before the control plane | `priorityClassName`, `persistence.local.enabled` |

## Objects and settings

- The objects: [CRD reference](/reference/crds/), with the validation the
  apiserver enforces. `kubectl explain dnszone.spec` works as well.
- The server settings (`config.node` and `config.settings` in the values):
  [settings reference](/reference/configuration/). Keys flagged *restart*
  roll the pods through the ConfigMap checksum.
- external-dns writes records through kudurru's webhook provider
  (`externalDNS.webhook.*`; it needs a license key that includes
  integrations); the records land in `DNSRecordSet` objects managed by
  `integration:external-dns`.

## Access

People sign in with OIDC (`auth.oidc.providers`) or the local admin; OIDC and
roles other than the built-in administrator need a license key
([what a key unlocks](/project/licensing/)). Their roles come from role bindings (viewer, auditor, operator, zone-admin,
admin, scoped per zone). ServiceAccount tokens are authorized with a
SubjectAccessReview against the aggregated ClusterRoles the chart renders
from `internal/rbac/permissions.yaml`; with `rbac.aggregateToDefaultRoles`
the built-in `view`, `edit` and `admin` roles carry the matching kudurru
permissions. See [RBAC](/deploy/helm/#rbac).

## Day two

- [Runbooks](/deploy/runbooks/): cold start without the apiserver,
  rotating the encryption key, upgrades.
- [Monitoring](/deploy/helm/#monitoring): ServiceMonitor or PodMonitor,
  PrometheusRule and the Grafana dashboard from the chart.
- `kudurru nodes` lists the replicas with their readiness and the leader;
  `kudurru doctor` diagnoses one node's configuration and environment.
