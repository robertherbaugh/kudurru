# Stored TSIG keys

Complete SQLite nodes can manage encrypted TSIG credentials through the
authenticated API. Only the `admin` role has `tsigkeys.read`, `tsigkeys.write`
and `tsigkeys.delete` by default. File-configured keys are listed as read-only;
a stored key cannot shadow a file key. Kubernetes management currently returns
an unavailable-capability response and does not fall back to Secret writes.

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

Names are canonical DNS names. Algorithms are HMAC-SHA256 or HMAC-SHA512.
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
separately; ciphertext alone cannot restore signing capability. Kubernetes
stored-key activation and full supported-driver rotation qualification remain
separate from this SQLite management surface.
