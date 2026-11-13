# Stored TSIG keys

Complete SQLite and Kubernetes nodes can manage encrypted TSIG credentials
through the authenticated API. Only the `admin` role has `tsigkeys.read`,
`tsigkeys.write` and `tsigkeys.delete` by default. File-configured keys are
listed as read-only; a stored key cannot shadow a file key.

Use HTTPS and the existing bearer-token or cookie/CSRF authentication flow.
Keep secret input out of command arguments and logs. Requests are limited to
4 KiB, reject unknown or duplicate fields, and never return secret material.
All paths below are relative to `/api/v1`.

| Method and path | Request body | Permission |
| --- | --- | --- |
| `GET /tsigkeys` | None | `tsigkeys.read` |
| `POST /tsigkeys` | `keyName`, `algorithm`, `secret` | `tsigkeys.write` |
| `POST /tsigkeys/{keyName}/replace` | `uid`, `resourceVersion`, `secret`, optional `algorithm` | `tsigkeys.write` |
| `POST /tsigkeys/{keyName}/revoke` | `uid`, `resourceVersion` | `tsigkeys.delete` |
| `POST /tsigkeys/{keyName}/reseal` | `uid`, `resourceVersion` | `tsigkeys.write` |

`kudurru tsigkeys` drives these routes. It reads the secret only from
`--secret-file` (a private file, or `-` for stdin), never from an argument,
and no command prints a secret: the server never returns one, so keep your own
copy for the peers that share the key. The file holds the standard base64
secret; whitespace and line breaks are ignored, so wrapped `openssl` output
works.

```sh
umask 077
openssl rand -base64 32 > transfer-key.secret
kudurru tsigkeys create transfer-key --algorithm hmac-sha256 --secret-file transfer-key.secret
kudurru tsigkeys list                       # names, algorithms, UID and resourceVersion
kudurru tsigkeys replace transfer-key. --uid <uid> --resource-version <rv> --secret-file new.secret
kudurru tsigkeys reseal transfer-key. --uid <uid> --resource-version <rv>
kudurru tsigkeys revoke transfer-key. --uid <uid> --resource-version <rv>
```

Each command prints its receipt (`-o json` or `-o yaml` for the full
document). The CLI sends every command once and never follows a redirect,
refreshes a binding or retries. A refusal keeps the server's status and key.
On an enrolled SQLite follower the commands answer `503 primary_required`
before they run; nothing changes, and the command must be sent to the primary
(for example with `--server`). A `202` receipt exits with status 0 and a note
on standard error that publication was not confirmed. A missing or unusable
response reports an unknown outcome: run `kudurru tsigkeys list` and review
the fresh metadata before any further action. `kudurru api` still reaches the
same routes for scripts that build their own requests.

A zone names the key in `spec.policy.transferTSIGKeyRefs`,
`spec.policy.notifyTSIGKeyRef` or, for [dynamic updates](dynamic-updates.md),
`spec.policy.updatePolicies`.

Names are canonical DNS names; a name without the final dot is stored with it.
Algorithms are HMAC-SHA256 or HMAC-SHA512.
`secret` is canonical standard base64 encoding of at most 512 decoded bytes.
Creation requires an absent stored name. Other commands require the exact UID
and resourceVersion from reviewed metadata; stale bindings return a conflict.
Metadata contains the name, algorithm, source and, for stored keys, UID,
resourceVersion and creation time. It contains no secret, ciphertext, secret
file path or private credential epoch.

Each command is attempted once under the original credential, writer and
source bounds. A receipt has `committed: true`. HTTP 201 for create or 200 for
other commands also has `published: true`: the node observed the exact complete
publication while the original operation remained authorized. HTTP 202 has
`published: false`: the transaction committed but that bounded acknowledgment
was unavailable. It does not mean the key was rolled back, and it does not
establish replication or continuing availability. A revoke receipt contains
historical deleted metadata and `removed: true`.

After an error or missing response, inspect fresh metadata before deciding on
another operation. The transaction outcome may be uncertain. Do not
automatically replay a command, follow a mutation redirect, replace its reviewed
binding with a newly read version, or interpret publication failure as rollback.
Followers can expose complete metadata but cannot acquire writer authority from
an already captured request during promotion.

## Encryption-key rotation and backup

Replacing a TSIG key changes its credential epoch. Resealing encrypts the same
credential under the current encryption owner and preserves that epoch. Start
with both the current and previous encryption keys available, review metadata,
then reseal each stored key with its exact UID/resourceVersion. Confirm the
resulting metadata and signed DNS behavior before independently restarting with
only the current encryption key. No live encryption-owner replacement is
implied by this procedure.

Safe backups exclude stored credentials. Full backups preserve ciphertext and
need the matching encryption keys and cluster identity. Protect those keys
separately; ciphertext alone cannot restore signing capability.

## Kubernetes

Kubernetes nodes load the stored encrypted TSIG records at startup, alongside
file keys, and read them again every ten seconds while they run. At startup the
actual cluster identity, complete private inventory and every encrypted owner
must validate within `store.kube.startupTimeout`. Duplicate file/store names,
malformed records, unavailable keys or API access failure leave signed DNS,
signed transfers and signed NOTIFY unavailable. A file
key does not bypass an unknown stored source. Unsigned DNS from a valid saved
snapshot remains available independently.

A failed startup preparation still requires a restart. After a successful
startup, each node rereads the complete stored source every ten seconds and after
its own commands. A changed source that prepares replaces the signing and
verification ring in place; a changed source that does not prepare withdraws
stored and file signatures, as at startup, until a later source prepares. A failed
read keeps the current ring. The startup warning reports unavailable preparation
without exposing private source data.

On Kubernetes the same `/tsigkeys` commands apply. Each command first requires the
node's published complete source to still be current, then writes one object
with its exact resourceVersion. Because Kubernetes assigns object UIDs, create is
two-phase: a provisional carrier is created and immediately re-sealed bound to
its assigned UID. If that second step fails, the node deletes exactly that
carrier once and reports the failure. If a carrier that cannot be opened remains,
for example after a crash between the two steps, signed DNS is withdrawn until it
is removed with `revoke` using its exact `uid` and `resourceVersion`. Other nodes
adopt every change within ten seconds; a receipt's `published` reflects only the
node that ran the command.
