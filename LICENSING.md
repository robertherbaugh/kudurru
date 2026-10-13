# Licensing

kudurru is **source-available**, not open source. You can read, build and run
all of it, and personal use is free. A business that wants to run it needs an
agreement.

| You are | You may | You need |
|---|---|---|
| A person running kudurru at home, in a homelab or for study | Use, modify and share it, for free | Nothing, except a free personal key for more than one node or for the `ee/` features |
| A charity, school or public research body | The same, under the noncommercial terms | A free personal key on request |
| A business, or anyone using it for a commercial purpose | Evaluate it | A commercial agreement and a business key. Write to the contact below. |

The license is the [PolyForm Noncommercial License 1.0.0](LICENSE)
([SPDX](https://spdx.org/licenses/PolyForm-Noncommercial-1.0.0.html)). The text
is also on [kudurru.io](https://kudurru.io/license/text/) and in the
[docs](https://docs.kudurru.io/project/license/), all from the one `LICENSE` file. It
allows any noncommercial purpose; personal use, research, hobby projects and
use by charities, schools and public bodies count as noncommercial. Anything
else is commercial and is not licensed by this repository.

## Keys

The same rules, in short form, are on [kudurru.io/license](https://kudurru.io/license).

A **license key** unlocks what a single free node does not have:

| | No key (community) | Personal key (free) | Business key |
|---|---|---|---|
| DNS answering, blocking, zones, the block page, the dashboard, short-term statistics, API tokens, the built-in administrator | Yes | Yes | Yes |
| Nodes in one cluster | 1 | up to 3 | as agreed |
| Policies | 1 (the seeded default) | unlimited | unlimited |
| Roles beyond the one built-in administrator: role bindings, zone-scoped roles, Kubernetes RBAC (`ee/rbac`) | No | Yes | Yes |
| OIDC single sign-on (`ee/oidc`) | No | Yes | Yes |
| The audit trail: stored events, `GET /audit`, export (`ee/auditstore`); the audit log lines are always written | No | Yes | Yes |
| Long-term statistics: any window older than 24 hours (`ee/longterm`) | No | Yes | Yes |
| Unblock requests: the block page's request form and the review of requests (the block page itself is free) | No | Yes | Yes |
| dnsdist and keepalived edge, VIP (`ee/edge`) | No | Yes | Yes |
| Integrations, such as the external-dns webhook (`ee/integration`) | No | Yes | Yes |
| Use by a business | No | No | Yes |

A personal key is only for noncommercial use; the license says so, and the key
does not change it. A key is not a license by itself: it records the license
you were granted.

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

Everything outside `ee/` is under the PolyForm Noncommercial License. The
directory [`ee/`](ee/) holds the features that need a key; it is under the same
license plus the extra terms in [`ee/LICENSE.md`](ee/LICENSE.md). The
blocked-services and safe-search catalog in `internal/listdata/catalogue` is
data written for kudurru and is licensed under Apache-2.0, so it can be
published and reused on its own. Third-party software keeps its own licenses
([NOTICE](NOTICE)).

## Contributing

Contributions need the contributor agreement in [CONTRIBUTING.md](CONTRIBUTING.md)
so that the project can keep offering commercial licenses.

## Contact

Commercial licenses, free personal keys and licensing questions:
hello@kudurru.io
