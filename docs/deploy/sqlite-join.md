# Join a new SQLite receiver

This path requires an activated SQLite primary running the enrolled internal
listener and a private invitation issued for the receiver's exact name and
internal address. It is separate from static twins and Kubernetes storage.
Full partition/rejoin qualification remains open. An explicit incomplete-join
recovery path is described below.

1. On the primary, review `cluster enrollment` and issue `cluster join-token`
   using that exact review, target, absolute expiry and audit reason. Transfer
   the returned invitation or JSON receipt through a private trusted channel.
   The invitation contains a bearer secret and the original primary CA; never
   log it or obtain replacement trust from an unverified server.
2. On the receiver, stop kudurru. Configure the same original
   `KUDURRU_ENCRYPTION_KEY` or `KUDURRU_ENCRYPTION_KEY_FILE` as the primary.
   The invitation does not contain that key, and joining does not create one.
   Use a new data directory that does not already exist. Confirm the target
   name/address independently against the primary's reviewed invitation.
3. Run the offline command as the OS user that will own the receiver data:

   ```sh
   kudurru cluster join --offline \
     --data-dir /var/lib/kudurru/new-receiver \
     --invitation-file /run/private/receiver-invitation.json \
     --confirm-name receiver \
     --confirm-address 10.0.0.12:8082
   ```

4. Require both `primary.committed: true` and a nonempty `installed` generation
   in the successful JSON result. Start the normal server with `node.dataDir`
   set to this directory, the same node name/internal address and encryption
   configuration. Do not configure `cluster.role: static`. Keep the private
   member identity file with this database across restarts.

The command has one original 30-second lifetime, also bounded by invitation
expiry. It sends one proposal and performs one member-authenticated image fetch.
Existing directories and databases are refused. It never starts a daemon or
claims that the newly installed image is published or that all peers converged.

On failure, preserve the directory, database, private identity and diagnostic
result. The primary may have accepted membership even if the receiver could not
confirm the response or install its first image. Do not replay the proposal,
replace the local identity, delete the directory or issue a successor invitation
as an automatic recovery step. Inspect the retained primary and local state
before a separate operator decision. The initial command never retries a failed
or uncertain proposal automatically.

## Recover an incomplete join

Keep the receiver stopped. With the original invitation file, encryption
configuration, database and private member identity, run:

```sh
kudurru cluster join-reconcile --offline \
  --data-dir /var/lib/kudurru/new-receiver \
  --invitation-file /run/private/receiver-invitation.json \
  --confirm-name receiver \
  --confirm-address 10.0.0.12:8082
```

This is a new, explicitly requested 30-second observation. It sends no join
proposal and creates or repairs no local identity or database. It requires the
immutable local intent committed before the original proposal was dispatched.
That record binds the exact invitation, original member CA and never-installed
local state; the invitation secret itself is not stored there.

The primary must already admit the retained member over mutual TLS. Recovery
fetches one image from the original primary CA, address and term, and requires
the exact consumed invitation and member identity in that image. The invitation
may have expired: it is historical evidence here, not renewed permission to join.
Both original CAs must still be valid. Missing or pruned invitation history,
replacement identity, changed primary term, missing intent, already installed
data, or a primary that never accepted the original member all refuse recovery.
Older incomplete receivers without this durable intent cannot use this path.

Require a successful command with `observed: true` and a nonempty `installed`
generation before normal startup. `observed` reports existing membership, not a
new primary commit. Neither field establishes serving publication or convergence.
On refusal, retain all evidence for investigation; do not delete intent, replace
identity or automatically submit another proposal.


## Commands through a follower

After retained enrolled services start, `POST /api/v1/dns/blocking` can send one
bounded command to the original enrolled primary. The primary rechecks the
original raw credential and settings version and commits the status, audit and
dedup result atomically. A relative timer is resolved before the network exchange.

HTTP 202 reports `committed: true` with `publication: pending`; it does not claim
that the receiving follower is already serving the new state. Read
`GET /api/v1/dns/blocking` on the node to observe its subsequent publication.
The dashboard and CLI distinguish the receipt from that observation. There are
no automatic retries or redirects. A lost or expired response does not prove
rollback; inspect state before deciding on another mutation. Other public
mutation domains and full partition/rejoin acceptance remain unfinished.
