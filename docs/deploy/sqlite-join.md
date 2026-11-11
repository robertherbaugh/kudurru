# Join a new SQLite receiver

This page is the detail of one step of the
[enrolled SQLite cluster](sqlite-cluster.md) guide, which starts with the
activation of the first primary and also covers promotion.

This path requires an activated SQLite primary running the enrolled internal
listener and a private invitation issued for the receiver's exact name and
internal address. It is separate from static twins and Kubernetes storage.
Separate-process acceptance covers temporary partition with the same primary,
automatic addition/deletion catch-up, forwarding and physical restart, promoted
primary rejoin, and three-member conflicting promotions with explicit offline
branch recovery. An explicit incomplete-join recovery path is described below.

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
rollback; inspect state before deciding on another mutation.

Canonical object writes are forwarded the same way for DNS zones, record sets,
block lists, client groups and policies: `POST`, `PUT` and `DELETE` under
`/api/v1/objects/`, `POST /api/v1/apply` (`kudurru apply`), the `POST`, `PUT`
and `DELETE` routes of `/api/v1/blocklists`, `/api/v1/clientgroups` and
`/api/v1/policies` that the dashboard object editors use, and
`POST /api/v1/{kind}:batchDelete`. The
follower normalizes the document, sends it once and returns the primary's exact
public result: the committed object, or the primary's own refusal such as `412`
for a stale `If-Match` or `409` for an existing object. The primary repeats
normalization and validation, re-authorizes the original credential against
the object it actually reads, and commits the write, its audit event and the
command's deduplication record together. The follower serves the result only
after its next pull, so an immediate read on the follower can still show the
previous state. A write whose exchange with the primary fails for any reason,
including an unreachable primary, answers `503 completion_unknown`: the
follower cannot tell whether the primary received it, so inspect the object
before a new decision. A primary that answers but cannot run the command
refuses it with `503 unavailable`, and the write was not applied.

A follower request has one 30-second lifetime, which starts before the body is
read, and stays bound to the data the follower had installed when the request
began. `POST /api/v1/apply` and `:batchDelete` send one command for each
document or name, in order, inside that single request, and answer `200` with
one result for each item as they do on the primary. The primary answers each
command before any of its other writers can run, so its own follow-up work on
an earlier item, such as writing the status of an object that the request just
created, does not disturb the answer for the next item.

An item carries its own error and is not applied (`errorKey` `unavailable`)
when the lifetime has ended ("the request's 30-second forwarding lifetime
ended") or when the follower installed newer data in the meantime ("the
follower's source changed"). The last case is the one to expect for a long
apply: the follower's next pull, five seconds by default, installs the earlier
documents of the same request, and every later document is then refused. A
single-item route answers `503 unavailable` with the same messages. An item
whose exchange with the primary fails, including an unreachable primary,
reports `completion_unknown`, as a single write does. Earlier items stay
committed and nothing is replayed. Check the per-item results. Running the
apply again through the follower does not finish a file that was cut: the
follower forwards the committed documents again, the primary records every
forwarded command with its deduplication record, also one whose result is
`unchanged`, and the follower's next pull therefore changes its source again.
Send an apply of many documents, and every restore, to the primary.

On the primary, and on a standalone node, `POST /api/v1/apply` and
`:batchDelete` run one command for each document or name inside the one
request, and one request applies every item. Until an item of the request
has completed, each item is admitted on the publication that the request
began on, exactly as a single write is. The request's first commit retires
this publication, so each later item is admitted by a different comparison:
the last command that changed the node's data must still be the one it was
when the request's previous item ended, which is the request's own commit
whenever that item changed something. The node's own
follow-up writes, such as object status and the published state of a zone,
are not commands and do not count. A command of anyone else in between does:
another request that changes data under any credential, a sign-in, a write
forwarded from a follower, a migration step. Every later item then carries
`errorKey` `unavailable` ("another change was committed on this node while
the request ran; this item was not applied; review current state before a new
decision"). It is not applied and never tried again, and the earlier items
stay committed. Run the apply again: it is a new request on current state,
and committed documents report `unchanged`.

Every item verifies the original credential and its permissions again in its
own transaction, so a token that is revoked or loses its grant between two
items stops the later ones with the credential's own error. The node must
stay the writer in the same term for the whole request. Its keys, license and
provider data may be replaced while the request runs: each item uses the ones
that are current when it runs. The request's one lifetime of five minutes
bounds all items: an item that is reached after it ended reports
`unavailable` ("the request's lifetime ended before this item was reached; it
was not applied").

The zone convenience commands the dashboard's
record editor uses (`POST /zones/{zone}/records`, `DELETE
/zones/{zone}/records/{name}/{type}`, `POST /zones/{zone}/import` and `DELETE
/zones/{zone}`) are forwarded the same way: the primary runs the route's own
plan, including all-or-nothing imports, and returns its exact output or refusal.
Local password login works on a follower too: the follower prepares a new
session identifier and CSRF secret, forwards the username, password, factor code
and client address once to the primary, and keeps the CSRF secret itself. The
primary applies the shared lockout counters, password, factor and local-login
policy and enrolls the session in one command; refused attempts still count
there. The follower answers after its own pull installs the session, so the
cookie works on every member. A login that cannot reach the primary returns `503`
and is not attempted. Settings, other identity administration, integrations and
remaining mutation domains still require the primary.


## Reconnecting an old primary

A retained primary periodically exchanges terms with current enrolled peers over
mutual TLS. When it observes an authenticated successor, it durably fences writes;
a later independent follower pull validates, installs and publishes that image.
This does not automatically promote a node or select a conflicting branch.

The two-process acceptance test now covers public promotion after primary loss
and automatic old-primary rejoin after restart, preserving private identities.
Independent-VM VIP acceptance is owner-deferred follow-up.


## Recover from conflicting promotions

Manual promotion is epoch fencing, not consensus. If two followers are promoted
while isolated from each other, both accept writes at the same epoch. When they
reconnect, their scheduled peer exchange records a durable conflict on both:
writes are refused and the HTTP API closes (it reports `generation_pending`),
while each member keeps answering DNS from its last valid local data, including
after a restart. Nothing chooses a branch automatically.

Decide which member's data should survive, then on that member only:

```sh
# Stop kudurru on the member first; the database lock refuses a running server.
kudurru cluster resolution --offline --data-dir /var/lib/kudurru > resolution.json
# Inspect resolution.json: mode, term, conflict and "resolvable": true.
kudurru cluster resolve --offline --data-dir /var/lib/kudurru --review resolution.json \
  --confirm-node node-b --request-id resolve-2026-11-09 --reason "keep node-b changes"
```

`cluster resolve` rechecks the exact saved review, allocates a new epoch above
every observed term, keeps this member's rows (its prior image is also retained as
private quarantine) and writes a `cluster.resolve` audit event attributed to
`host:offline-cli` in the same transaction. Start the member again. The other
members adopt the new term only through their next scheduled authenticated peer
exchange, quarantine their own branch when they install this one and serve it.
Writes that only exist on a discarded branch are not replayed; re-apply any you
still need through the new primary. A returning older primary is fenced and
follows the same way.

Do not resolve on more than one member, and never repeat the command after an
unknown result: run `cluster resolution` again and decide from the current state.
The command needs the member's data directory and its retained
`member-identity.json`, and makes no network connection. Running it on a member
without a recorded conflict or divergence refuses without changes.


## Managed root anchors on fenced and conflicted members

With `upstreams.recursion.anchorMode: managed` (RFC 5011), a member that is
fenced or holds an unresolved conflict keeps answering DNS from its last valid
local data and its last accepted root trust anchors, and it restarts in that
state. It does not update trust anchors: only the current primary does. It still
checks the root DNSKEY set on its own, about once an hour. If it sees a signed
revocation of a trusted key, it retires its current trust-anchor state
immediately and answers `SERVFAIL` until it installs a newer state from the
primary. These members therefore still need outbound TCP 53 to the configured
root servers.

Keep member clocks synchronized. A member whose clock is up to five minutes
behind the primary waits for its own clock and logs
`check time synchronization`. A member that is further behind stops its root
anchor checks (a follower, fenced or conflicted member) or its trust-anchor
updates (a newly promoted primary) with `invalid clock continuity` until the
clock is fixed and kudurru is restarted, or until the primary changes again.
