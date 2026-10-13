# Licensing

kudurru is **source-available**. The core DNS server and dashboard use the
[Kudurru Community License](LICENSE); other areas have the folder-specific
licenses listed below.

| Use | Permission |
|---|---|
| Personal, household or homelab | Free under the Community License; a free key unlocks up to 3 nodes and additional features |
| A qualifying nonprofit | The same free entitlements, subject to Section 2.2 of the Community License |
| Isolated development, evaluation or security research | Permitted under Section 2.3, including for businesses and governments; this does not permit production DNS service |
| Business, government, MSP or contractor production | An accepted [Commercial Software License Agreement](COMMERCIAL_LICENSE_AGREEMENT.md) and Order Form are required, including for no-key features |

A nonprofit must be legally established, operate without distributing net
earnings for private benefit, be independent of government control, and not
provide managed DNS or similar services to businesses or governments. A
school or research organization's activity alone does not make it eligible.

You may modify and share Covered Software for the permitted community uses.
Keep the notices, include the license, identify changes, and provide the
source for changes in distributed executables. Those rights do not cover
`www/` or `license-server/`.

The Community License text is also on
[kudurru.io](https://kudurru.io/license/text/) and in the
[docs](https://docs.kudurru.io/project/license/), all from the one `LICENSE` file.

## Keys

The same rules, in short form, are on [kudurru.io/license](https://kudurru.io/license).

A **license key** unlocks what a single free node does not have:

| | No key (community) | Personal/nonprofit key (free) | Business key |
|---|---|---|---|
| DNS answering, blocking, zones, the block page, the dashboard, short-term statistics, API tokens, the built-in administrator | Yes | Yes | Yes |
| Nodes in one cluster | 1 | up to 3 | as agreed |
| Policies | 1 (the seeded default) | unlimited | unlimited |
| Roles beyond the one built-in administrator: role bindings, zone-scoped roles, Kubernetes RBAC (`ee/rbac`) | No | Yes | Yes* |
| OIDC single sign-on (`ee/oidc`) | No | Yes | Yes* |
| The audit trail: stored events, `GET /audit`, export (`ee/auditstore`); the audit log lines are always written | No | Yes | Yes* |
| Long-term statistics: any window older than 24 hours (`ee/longterm`) | No | Yes | Yes* |
| Unblock requests: the block page's request form and the review of requests (the block page itself is free) | No | Yes | Yes* |
| dnsdist and keepalived edge, VIP (`ee/edge`) | No | Yes | Yes* |
| Integrations, such as the external-dns webhook (`ee/integration`) | No | Yes | Yes* |
| Business or government production use | No | No | Only as granted by the agreement |

\* If included in commercial license agreement.

Free personal/nonprofit keys are only for eligible community use. A key
records an entitlement; it does not replace the license or agreement. A
Commercial Core agreement does not automatically authorize enterprise
features: the Order Form must include the Enterprise plan or named modules.
MSP, affiliate and government scopes must be authorized in the Order Form.
The 30-day technical grace period does not extend a commercial subscription.

Give kudurru the key in one of these ways, first match wins:

1. `kudurru serve --license-key <key>`
2. the environment variable `KUDURRU_LICENSE_KEY`
3. the file named by `KUDURRU_LICENSE_KEY_FILE` (use this with a Secret or a Docker secret)
4. `<node.dataDir>/license.key`

`kudurru license status` shows what a node would run with and
`kudurru license verify <key>` checks a key without starting anything.
`GET /api/v1/license` reports the state of a running node.

## What a missing or expired key does

Licensing never touches DNS. Queries are answered the same with any key state,
with no key, with an expired key and with a key the node cannot verify. Keys
are verified on the node with a signature check; no node contacts a license
server.

Only the management plane changes:

- **No key, or an invalid key**: one node, one policy (the seeded default)
  and no licensed features. Role bindings grant nothing (the first-boot
  administrator, the bootstrap token and the admin socket still have full
  access), OIDC sign-in is switched off, the audit trail, long-term statistics
  and unblock requests answer `402 license_required`, the external-dns webhook
  is not started, and the dnsdist and keepalived supervisors refuse a first
  start that has no key (see below). DNS, blocking, zones, the block page and
  the dashboard are unchanged.
- **A second policy** cannot be created through the API while the limit is
  one. A new node starts with the policy `default`, so without a key that is
  the policy you have: edit it to change what clients get. The API counts every
  policy, whoever made it, and without a key it creates no policy but
  `default`: if the default is deleted, it can be created again under that
  name, and any other policy needs a key. Existing policies keep working and
  stay editable, and a key lifts the limit. This limit is best effort: it guards the API, not the data path, so
  policies made another way (`kubectl` or GitOps in Kubernetes, object files,
  another replica at the same moment) are not stopped by it, and the terms of
  the license apply either way.
- **More nodes than the key allows**: configuration writes through the API
  answer `402 license_required` until the cluster is back under the limit.
  Reads, sign-in, pausing blocking and flushing the cache still work.
- **An expired key** keeps working for a 30-day grace period and logs a warning.
  After that the node runs as if it had no key.
- **The dnsdist and keepalived supervisors** sit in the DNS data path, so a
  lapse never stops them. They refuse only a start that has no key at all (or
  one that cannot be verified, or one that never included the edge): nothing
  was running, so nothing is lost. A key in its grace period, and a key that
  has expired since, still start them, with a warning in the log. They check the
  key once, at the start, and a running supervisor is never stopped by an
  expiry. Renew the key; the warning is the reminder.

The metrics `kudurru_license_info`, `kudurru_license_expires_timestamp_seconds`
and `kudurru_license_node_limit` let you alert before a key runs out.

## Layout

| Area | Terms |
|---|---|
| Core DNS server, binaries and dashboard | [Kudurru Community License](LICENSE), or an accepted commercial agreement for its authorized scope |
| `ee/` enterprise materials | [Enterprise notice](ee/LICENSE.md): eligible community entitlements, or Enterprise/named modules in the commercial Order Form |
| `site/` documentation source and build system | [Documentation notice](site/LICENSE.md): community previews and contributions; commercial private mirrors under Section 3.3 of the agreement |
| `www/` marketing website | [Website Source-Viewing License](www/LICENSE.md), copyright Robert Herbaugh; inspection only unless separately authorized |
| `license-server/` issuance service | [License Server Source-Viewing License](license-server/LICENSE.md), copyright Robert Herbaugh; inspection only unless separately authorized |

Anyone may read and link to the published documentation without buying a
license. A commercial agreement does not grant rights to build, modify,
redistribute or host the marketing website or license server.

The blocked-services and safe-search catalog in `internal/listdata/catalogue`
is separately licensed data under Apache-2.0; that license applies only to
the catalog. Third-party software keeps its own licenses ([NOTICE](NOTICE)).
Existing grants for historical releases remain in effect.

## Contributing

Contributions need the contributor agreement in [CONTRIBUTING.md](CONTRIBUTING.md)
so that the project can keep offering commercial licenses.

## Contact

Commercial licenses, free personal keys and licensing questions:
hello@kudurru.io
