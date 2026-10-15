# Enterprise

kudurru Enterprise is for organizations that run DNS on their own
infrastructure and need commercial production rights with enterprise
features. Deploy with Kubernetes and CRDs, or with Docker and Compose.

Commercial Core is also available for organizations that need production
rights for the core DNS server and dashboard without enterprise modules.
For licensing and deployment discussions, contact
[hello@kudurru.io](mailto:hello@kudurru.io?subject=kudurru%20Enterprise).
The [website enterprise page](https://kudurru.io/enterprise/) gives the
short overview.

## Features

The following capabilities are available today. The accepted Order Form
identifies the Enterprise plan or the named modules included in a purchase.
The public Enterprise subscription includes all currently shipped `ee/`
modules together; named-module agreements remain available for exceptional
deployments.

| Capability | What it provides | Details |
|---|---|---|
| Core DNS and dashboard | Authoritative zones, forwarding, encrypted DNS transports, blocking, the query log and short-term statistics | [Architecture overview](architecture/README.md) |
| Multiple nodes and policies | The agreed node allowance, plus policies beyond the default for client groups and schedules | [Groups, clients and policies](guide/admin/05-groups-clients-policies.md) |
| Identity and access | OIDC single sign-on, role bindings, zone-scoped roles and Kubernetes RBAC | [People, roles and tokens](guide/admin/09-people-and-tokens.md) |
| Audit trail | Stored events, review of changes and audit export | [Audit](architecture/observability.md#audit) |
| Long-term statistics | Query statistics for periods older than 24 hours, using rollups without retaining every query | [Long-term statistics](guide/admin/03-watch-the-network.md#read-the-long-term-statistics) |
| Unblock requests | A request-access form on the block page and an administrator review workflow | [Access requests](guide/admin/06-access-requests.md) |
| Edge and virtual IP | kudurru supervisors for dnsdist load balancing and keepalived VIP failover, with backend health and VIP visibility | [Compose HA](deploy/compose-ha.md) |
| Integrations | The external-dns webhook provider for DNSRecordSet objects | [Configuration and stores](architecture/configuration-and-store.md) |

Cloudflare split-horizon and UniFi inventory integrations are planned;
they are not current capabilities.

## Pricing

Both commercial editions are billed **annually in USD**. Each subscription
has one platform fee per legal organization, plus a fee for the aggregate
licensed production nodes across its covered clusters and environments.

| Edition | Annual platform fee | Annual per-node fee | Scope |
|---|---|---|---|
| Commercial Core | $99 | $99 | Commercial production rights for core DNS and its dashboard |
| Enterprise | $499 | $299 | Commercial production rights plus all currently shipped enterprise modules |

Neither commercial edition has DNS query, zone, CPU core or user limits.
Support contracts are separate. Eligible Community/Homelab use remains free.

The annual calculation is:

```text
Total = platform fee + (per-node fee × licensed nodes)
```

For 25–49 licensed nodes, the entire per-node component receives a 10%
discount; the platform fee is unchanged. Deployments of 50 or more nodes
are quoted individually.

| Licensed production nodes | Commercial Core/year | Enterprise/year |
|---|---|---|
| 1 | $198 | $798 |
| 3 | $396 | $1,396 |
| 5 | $594 | $1,994 |
| 10 | $1,089 | $3,489 |
| 25 | $2,326.50 | $7,226.50 |
| 50+ | Contact us | Contact us |

For example, 5 Enterprise nodes cost $499 + (5 × $299) = **$1,994/year**.
That is equivalent to $166.17/month, billed annually. Monthly equivalents
are illustrative; subscriptions are collected annually.

Use the [website pricing calculator](https://kudurru.io/enterprise/) to
compare editions and node counts. The accepted Order Form and
[commercial agreement](../COMMERCIAL_LICENSE_AGREEMENT.md) set the actual
fees, payment terms, node allowance, subscription term and included features.

## What counts as a node

A node is one independently running kudurru DNS server instance or replica,
whether on a physical host, VM, container or Kubernetes workload. A replica
that can actively serve or manage DNS is a node. DNS clients are not nodes.

- Three running Kubernetes replicas count as 3 nodes, even on one host.
- Concurrent production nodes count across the environments and clusters
  covered by the agreement.
- Stored images, stopped standbys and backups do not count as running nodes.
- Separate dnsdist and keepalived processes are not nodes merely by existing.
- Testing solely to evaluate a prospective license is permitted by the
  Community License (§2.3). This covers isolated, non-production tests and
  demonstrations; it does not authorize an operational DNS service.
  Continuously running staging, disaster recovery and failover nodes must be
  explicitly covered by the Order Form. Extra running nodes are not included
  by default.

The commercial agreement also permits up to 24 hours of overlap solely for
replacement of a failed node or an ordinary rolling upgrade, without
increasing steady-state capacity. See
[counting nodes](../COMMERCIAL_LICENSE_AGREEMENT.md#4-licensing-plans-keys-and-node-limits)
for the full terms.

## Commercial Core and Enterprise

| Plan | Scope |
|---|---|
| Commercial Core | Business or government production rights for the core DNS server and dashboard, within the agreed node count and organizational scope |
| Enterprise | Commercial production rights plus all currently shipped enterprise modules and advanced entitlements, recorded in the Order Form |

A Commercial Core purchase does not automatically include enterprise
modules. MSP, service-provider, affiliate and government scope must be
expressly covered by the Order Form. Pooled client deployments need separate
service-provider authorization, using either commercial edition.

Optional business-hours assistance starts at **$1,200/year**, subject to
availability and a separate agreement. Software license prices include no
contractual support, 24/7 coverage or guaranteed response times. Support
scope, response times and any SLA depend on the Order Form or an
incorporated support schedule.

Eligible personal, homelab and qualifying nonprofit use remains free under
the Community License. A free personal/nonprofit key covers up to 3 nodes
and additional features for that eligible use. It does not grant business
or government production rights. The Kudurru Community License specifically
excludes government production use. See [Licensing](../LICENSING.md) for the
eligibility rules and the full feature comparison.

## Deployment and license keys

Nodes verify license keys locally with a signature check. They do not
contact a license server. DNS queries continue to be answered regardless
of key state; a missing or expired key can limit management features.
Technical grace periods do not extend the commercial subscription term.

Use the [Helm guide](deploy/helm.md) for Kubernetes,
[Docker guide](deploy/docker.md) for a single container, or
[Compose HA guide](deploy/compose-ha.md) for the edge and VIP deployment.
The [license administration guide](guide/admin/13-license.md) explains key
installation and renewal. [Licensing architecture](architecture/licensing.md)
describes verification and expiry behavior.

## Discuss your deployment

Email [hello@kudurru.io](mailto:hello@kudurru.io?subject=kudurru%20Enterprise)
with your organization, expected node count, deployment model, required
features and timeline. Include staging, failover, support and any
service-provider needs so the Order Form can cover the intended scope.
