# 13. License and keys

This chapter explains the kudurru license, what a license key unlocks and
how to put a key on a node.

**Before you start:** You have the role Admin to read the page **License**.
You need access to the host or the deployment files to put a key on a node.

## What the license says

The core DNS server and dashboard use the Kudurru Community License.
Personal, household, homelab and qualifying nonprofit use is free under its
terms. Businesses and governments may develop and evaluate it in isolated
nonproduction environments; production DNS service needs an accepted
commercial agreement and Order Form, including for no-key features.
A school or research organization must meet the nonprofit eligibility rules.

The enterprise code and documentation have their own notices in
[ee/LICENSE.md](../../../ee/LICENSE.md) and [site/LICENSE.md](../../../site/LICENSE.md).
Anyone may read the docs. The website and license-server source have separate
source-viewing licenses and are excluded from the community and commercial grants.

NOTE: A key records an entitlement, not a license. Free personal/nonprofit
keys are for eligible community use only. Commercial enterprise modules must
be authorized in the Order Form. Technical grace periods do not extend an
expired commercial agreement.

## What a key changes

DNS never depends on a key. A node answers queries in the same way with a
key, with no key and with an expired key. A key changes the number of nodes,
the number of policies and the features below.

| | No key | Personal/nonprofit key (free) | Business key |
|---|---|---|---|
| DNS, blocking, zones, the block page, the dashboard | Yes | Yes | Yes |
| Nodes in one cluster | 1 | Up to 3 | As agreed |
| Policies | 1, the policy `default` | No limit | No limit |
| Roles other than the built-in administrator | No | Yes | If included in the Order Form |
| Single sign-on | No | Yes | If included in the Order Form |
| The audit log and its export | No | Yes | If included in the Order Form |
| Statistics for more than 24 hours | No | Yes | If included in the Order Form |
| Access requests from the block page | No | Yes | If included in the Order Form |
| The dnsdist and keepalived edge | No | Yes | If included in the Order Form |
| Integrations, for example the external-dns webhook | No | Yes | If included in the Order Form |
| Business or government production use | No | No | As granted by the agreement |

A node without a key has one administrator: the first-boot administrator.
The bootstrap token and the admin socket also have full access. Role
bindings stay in the configuration, but they give no access.

A node without a key also has one policy: the policy `default` that kudurru
makes at the first start. Change that policy to change the rules for your
clients. kudurru does not make a second policy through the dashboard or the
API without a key. If you delete the policy `default`, you can make it again
with the same name. To make other policies, you must have a key. Policies
that exist stay and keep working.

## Read the state of a node

Open **System** > **License**. The page shows the state of the key, the
plan, the number of nodes and the features that the node has.

| State | Meaning |
|---|---|
| No key | The node has no key. It runs with the free limits. |
| Active | The key is valid. |
| Expired, in grace period | The key expired less than 30 days ago. It still works. |
| Expired | The key expired more than 30 days ago. The node runs with the free limits. |
| Invalid key | kudurru cannot verify the key. The page shows the reason. |

A banner at the top of each page tells you about a key that is invalid,
expired or close to its end. The banner also tells you if the cluster has
more nodes than the key allows.

NOTE: When the cluster has too many nodes, kudurru refuses changes to the
configuration through the API. Reading, signing in and DNS keep working.

## Put a key on a node

kudurru reads the key from the first of these places that has one:

1. The option `--license-key` of `kudurru serve`.
2. The environment variable `KUDURRU_LICENSE_KEY`.
3. The file that the variable `KUDURRU_LICENSE_KEY_FILE` names.
4. The file `license.key` in the data directory of the node.

To put a key on a node:

1. Copy the key to one of the places above.
2. Wait up to 1 minute.
3. Open **System** > **License**.

The node reads the key again each minute. You do not restart it. In Helm,
set the value `license.existingSecret`. In Compose, set the variable
`KUDURRU_LICENSE_KEY`.

The dnsdist and keepalived containers get the key in the same way. They do not
start for the first time without a key that covers the edge. After that, an
expired key never stops them.

To check a key without a running node, use the command line:

```sh
kudurru license verify <key>
kudurru license status
```

## Get a key

Write to <hello@kudurru.io>. For a free personal key, give the name for the
key and the use. For a business key, give the company, the number of nodes
and the features that you need.
