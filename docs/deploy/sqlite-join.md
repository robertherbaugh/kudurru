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

## Versions of the members

Every member of an enrolled cluster must run the same release. Members
exchange data only as complete images, and a member accepts an image only
from a member with exactly its own store schema (9 in 0.2.0) and the same
required features. There is no compatibility mode:

- A member at another schema or feature set refuses the images it receives,
  keeps answering from its last good image and cannot be promoted. It
  catches up on its next pull once it runs the primary's release again.
- 0.1.0 has no enrolled clusters and can never be a member. Started on the
  data directory of a member, it refuses the directory (`state.db was
  written by a newer kudurru`) and changes no file.
- Joining always uses the primary's release: `cluster join --offline` and
  the receiver's first start must be the same release as the primary.

To upgrade an enrolled cluster to a release with another store schema, plan
a short window without object changes. Back up every member's data directory.
Upgrade the followers one at a time (each keeps serving its last good image
and waits), then the primary. After the primary's restart the followers
install its next image and follow again; check `kudurru nodes` and `/cluster`
on each. Do not promote a member while the members run different releases.


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

When the cluster settings changed on the primary after the follower's last
pull, the primary refuses the switch and the follower relays that answer:
`409 conflict`, and the blocking state is not changed. Read the state again and
repeat the switch if it is still wanted. A switch that the follower could not
send answers `503 unavailable` and says that nothing was applied; one whose
exchange failed answers `503 completion_unknown`, because the primary may have
committed it.

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
previous state. A write whose exchange with the primary fails after the
follower connected to it answers `503 completion_unknown`: the follower cannot
tell whether the primary received it, so inspect the object before a new
decision. When the primary's host refuses the connection or is reported
unreachable, the follower never connected, nothing was sent, and the answer is
`503 unavailable` saying that the write was not applied. A connection attempt
that is still waiting when the request's deadline ends (a host that silently
drops packets) answers `503 completion_unknown` after that deadline, because
the follower does not tell it apart from an exchange that stopped later. A
primary that answers but cannot run the command refuses it with `503
unavailable`, and the write was not applied.

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
ended"), when the follower installed newer data in the meantime ("the
follower's source changed"), or when the primary refused the command because
the follower's view of the cluster's authority no longer held ("cluster
authority changed"). The second case is the one to expect for a long
apply: the follower's next pull, five seconds by default, installs the earlier
documents of the same request, and every later document is then refused. A
single-item route answers `503 unavailable` with the same messages. An item
whose exchange with the primary fails after a connection was made, or whose
connection attempt runs into the deadline, reports `completion_unknown`, and
one whose connection was refused or whose host was reported unreachable
reports `unavailable`, as a single write does. Earlier items stay
committed and nothing is replayed. Check the per-item results. Running the
apply again through the follower does not reliably finish a file that was
cut: the
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
whenever that item changed something. Writes that the node makes without an
API command are not detected and do not count: its follow-up writes, such as
object status and the published state of a zone, and also RFC 2136 updates,
commands of the local admin socket, the declarative objects source and
provider and integration reconciliation, although some of them act for
someone else. A command through the API in between does count: another
request that changes data under any credential, a sign-in, a write forwarded
from a follower, a migration step or an operator-started integration
operation. Every later item then carries
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
was not applied"). `kudurru apply` and `kudurru restore` wait the same five
minutes for the answer unless `--request-timeout` is set.

Each item commits on its own. DNS keeps answering throughout, for the node's
own zones too, from what the node published last. Other requests answer `503
generation_pending` after two seconds until the node has published again, so
the request lets the node publish before its next item, inside its lifetime:
at once when another caller's read is waiting, and after two seconds of work
in any case. It waits until the node has published the items so far (for a
bounded time), then commits nothing for 200 milliseconds. Other requests are
served in that pause; a change committed by another request in the pause
stops the rest of the request as described
above. [sqlite-cluster.md](sqlite-cluster.md#4-changes-through-a-follower)
gives the sizes and the answers of a request that is refused as a whole.

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
and is not attempted.

Sign-out (`POST /auth/logout` and `POST /auth/logout/browser`) and revoking one
of your own sessions (`DELETE /auth/sessions/{id}`) are forwarded too. The
primary checks the credential again and ends exactly the session that the
cookie belongs to, or the session that you named if it is one of your own
(otherwise `404`). The follower then clears the browser's cookies and refuses
the session as soon as its pull installs the primary's state, usually at once.
A follower offers no provider sign-out continuation: OIDC sign-in and its
logout run on the primary. If the primary cannot be reached, sign-out still
clears the browser's cookies but answers `503 unavailable` (nothing was sent)
or `503 completion_unknown` (the exchange failed), and the message says that
the session may stay valid until it expires. Sign in again when the primary is
back and revoke the session under Sessions.

Cluster settings changes (`PATCH /settings`, the dashboard's Settings page)
are forwarded as well. The primary repeats every check of its own route and
merges the change into the settings that the follower showed: if they changed
on the primary since the follower installed them, the answer is `409
conflict` and nothing is applied; reload and review. After a change commits,
the follower pulls once and answers when it serves the new values (it waits at
most five seconds), so your next change through the same follower, such as the
next key on the Settings page, is made against your own change and is not
refused. A `409` therefore means that someone else changed the settings, or that
the follower could not install the primary's state in time; a reload shows the
current values once the follower has pulled.

Changing your own password (`POST /auth/password`) and creating or revoking
personal API tokens (`POST /auth/tokens`, `DELETE /auth/tokens/{id}`) are
forwarded as well, so an account that must change its password can do so on
the follower it signed in to. The follower checks the new password's length
rules and hashes it; only the hash, the current password, the factor code and
the client address travel to the primary, once. The primary checks your
credential, the current password and the factor against the shared lockout
counters and commits the change; refused attempts count there as for a login.
A browser session gets a new cookie from the follower, and the old one stops
working on every member. For a token, the follower generates the secret and
sends only its identifier and hash: the primary applies the route's own
checks (scopes no wider than yours, `auth.tokens.maxLifetime`, the expiry of
the token you call with) and stores the token, and the secret is never sent
to the primary. Revoking a token keeps the route's rule: your own token, or
someone else's with `tokens.delete`; otherwise `404`. In each case the
follower pulls the primary's state once and answers when it serves the result
itself (it waits at most five seconds), so a new token or cookie works on that
follower at once. A password change that committed is never reported as
unavailable: with an API token the follower answers it after those five
seconds even if it does not serve the change yet, and for a browser session
that it cannot give a new cookie in time it answers `503
committed_output_unavailable`, which means that the password was changed and
you sign in with the new one. If the primary cannot be reached the answer is
`503 unavailable` (nothing was sent) or `503 completion_unknown` (the exchange
failed): sign in or list your tokens to see what happened before you repeat
it. `kudurru login --username` therefore works
against a follower too.

Every other change, including node settings (`PATCH /node/config`), is
refused on a follower, as the next section describes.

## What a follower forwards, runs itself and refuses

A follower treats every changing request (`POST`, `PUT`, `PATCH`, `DELETE`) in
one of three ways. The server decides by route before it reads the body, after
it has checked the credential and the route's permission. A route that is in
neither of the first two groups is refused, so a route that a later version
adds is refused on a follower until that version says otherwise. The
[endpoint index](../../docs-site/content/reference/api/endpoints.md) shows the
group of every route in its "On a follower" column, and the OpenAPI document
carries it as `x-kudurru-follower`.

| Group | Routes | What the follower does |
|---|---|---|
| Forwarded | `POST`, `PUT`, `DELETE` under `/objects/` for DNS zones, record sets, block lists, client groups and policies; `POST /apply`; `POST`, `PUT`, `DELETE` on `/blocklists`, `/clientgroups`, `/policies`; `POST /{kind}:batchDelete` for those five kinds; `POST /zones/{zone}/records`, `DELETE /zones/{zone}/records/{name}/{type}`, `POST /zones/{zone}/import`, `DELETE /zones/{zone}`; `POST /dns/blocking`; `POST /auth/login`; `POST /auth/logout`, `POST /auth/logout/browser`, `DELETE /auth/sessions/{id}`; `PATCH /settings`; `POST /auth/password`; `POST /auth/tokens`, `DELETE /auth/tokens/{id}` | Sends one typed command to the primary and returns the primary's answer, as described above. |
| Local | `POST /cache/flush`; `DELETE /querylog`; `POST /querylog/rollup-evidence/verify`; `POST /dns/query`; the DHCP routes under `/node/dhcp/v4/`; `POST /cluster/promote`, `POST /cluster/activate`, `POST /cluster/join-token`; the two legacy routes under `/settings/sections/` | Runs the request on this node. The first five act on the node's own cache, query log, resolver and DHCP server. The cluster commands decide from this node's own role: promotion runs on a follower, activation and invitations answer `409 conflict` there. The legacy settings routes answer `503 unavailable` on every SQLite node. |
| Primary only | Everything else: `PATCH /node/config`; the allow and deny lists (`/allowlist`, `/denylist`, `/domains/{action}/{kind}`, `/queries/{id}/actions`, a block list's source entries); `POST /blocklists/{namespace}/{name}/refresh`; `POST /zones/{zone}/notify`; authenticators (`/auth/totp/`); command-line OIDC login; `/iam/` users, external accounts and role bindings; `/tsigkeys`; `/integrations`; `/imports`, including the two previews, which write nothing but plan an apply that only the primary runs; unblock request decisions; creating and revoking operator edge blocks (`/edge/blocks`); DHCPScope objects, including `POST /dhcpscopes:batchDelete` | Answers `503 primary_required`. Nothing is sent to the primary and nothing is changed. |

The refusal names the primary and carries a hint, and it has no `Retry-After`
header, because waiting does not change it:

```json
{
  "error": {
    "key": "primary_required",
    "message": "this request runs only on the primary; this node is an enrolled follower of node-a and does not forward it",
    "hint": "send it to the API of the primary node-a; GET /api/v1/cluster on this node names the operations a follower forwards or runs itself"
  },
  "took_ms": 0.4
}
```

The answer is the same while the primary is unreachable. Send the request to
the primary, or promote the follower if the primary is lost. The CLI prints the
server's message and hint and adds its own hint that names `kudurru cluster
status`. Four cases need a different next step:

- `PATCH /node/config` changes the log level and format of the node that
  answers. A follower cannot store that preference, and the primary would
  change only its own. Set `log.level` and `log.format` in the follower's
  configuration file or environment instead; the hint says so.
- OIDC sign-in, in the browser (`GET /auth/oidc/{idp}/start`) and from the
  command line, runs on the primary. A follower's `GET /auth/status` lists no
  provider and carries `"role": "follower"` and the primary's name, so a login
  page can say where to sign in with a provider. Local password login works on
  the follower, and so does `kudurru login --username`.
- The allow and deny list commands and unblock request decisions run on the
  primary. Through a follower, change an allow or deny list by writing its
  BlockList object (`PUT /blocklists/{namespace}/{name}`, the list editor, or
  `kudurru apply`), which is forwarded.
- A DHCPScope object written through `/objects/dhcpscopes` or as a document of
  `POST /apply` is refused with `primary_required` for that object or document.

`GET /api/v1/cluster` (`kudurru cluster status`) reports the role of the node
that answers. On a follower `role` is `follower`, `primary` and `leader` name
the primary, `primaryRequired` is `true`, and `forwardedOperations` and
`localOperations` list the operation IDs of the first two groups, so a client
can disable every other change before it sends it. On the primary and on a
node that is not enrolled, `role` is `primary` or `standalone` and
`primaryRequired` is `false`. On a follower, `GET /api/v1/settings/_schema`
marks the node-scope settings `readOnly`, because `PATCH /node/config` is not
forwarded; cluster settings stay writable there, because `PATCH /settings` is
forwarded to the primary.

The dashboard reads those lists from the node it is served by. A session
without `cluster.read` (every built-in role except admin) cannot read
`GET /cluster`; the dashboard then takes the role and the primary from the
public `GET /auth/status` and the lists from the release it is built with. On
a follower it shows a banner above every page, for every signed-in role, that names the primary and says which
changes are sent there, and the Nodes page shows the node's role, the primary
and how many kinds of change are forwarded or run locally. A control whose
change needs the primary is disabled, and its tooltip names the primary: the
allow and deny lists and a block list's source entries, list refresh, Send
NOTIFY, imports, authenticator setup, external account and authenticator
recovery commands, and unblock decisions (on the Requests list and on a
request's page). Everything a follower forwards stays available, and about six
seconds after any change sent from the dashboard (from a dialog, a wizard or
a row action) it reads its pages again, because the follower serves the
primary's commit only after its next pull. The dashboard rereads the node's
role every 30 seconds and at once when a change answers `primary_required`,
so an open tab follows a promotion. A refusal that still reaches the dashboard (for example
from an older page) is shown as "Needs the primary" with the primary's name,
and `503 completion_unknown` as "Outcome unknown", asking you to reload and
check before you try again. When the server answers a sign-out but cannot
confirm it (the follower could not reach the primary, or its answer was lost),
the server clears the session cookie, the dashboard signs out of the browser
and the login page says that the session may stay valid until it expires.
When the sign-out request does not reach the server at all, nothing cleared
the cookie, so the dashboard stays signed in and says that the sign-out
failed. The login page of a follower says that single
sign-on runs on the primary.

On a follower the block page keeps explaining a block, but it shows "This
server cannot take a request for access right now" in place of the request
form, because only the primary stores unblock requests. A request that the
follower has received still shows its state: an approval as on the primary, a
pending or declined request with its status and that message, without an
invitation to add details or ask again.


## Reconnecting an old primary

A retained primary periodically exchanges terms with current enrolled peers over
mutual TLS. When it observes an authenticated successor, it durably fences writes;
a later independent follower pull validates, installs and publishes that image.
This does not automatically promote a node or select a conflicting branch.
Between the observation and that pull the old primary's API is closed: every
request, including the provider-login routes, answers `503 cluster_fenced`,
which names the newer primary when its term carries a name, with no
`Retry-After` header. The member answers again as
a follower after the pull. If the answer persists, check that the member
reaches the new primary's internal listener.

The two-process acceptance test now covers public promotion after primary loss
and automatic old-primary rejoin after restart, preserving private identities.
Independent-VM VIP acceptance is owner-deferred follow-up.


## Recover from conflicting promotions

Manual promotion is epoch fencing, not consensus. If two followers are promoted
while isolated from each other, both accept writes at the same epoch. When they
reconnect, their scheduled peer exchange records a durable conflict on both:
writes are refused and the HTTP API closes, while each member keeps answering
DNS from its last valid local data, including after a restart. Every API
request to such a member answers `503 cluster_conflict` with a hint that names
the two commands below and no `Retry-After` header. Nothing chooses a branch
automatically.

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
without a recorded conflict or divergence refuses without changes, with one
exception for a member whose primary is lost for good: see
[Keep a fenced member's data when its primary is lost for good](#keep-a-fenced-members-data-when-its-primary-is-lost-for-good).


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

### A member returns with different trust-anchor state

The old primary and the promoted primary can each have updated the trust-anchor
state on their own, and so can both sides of a promotion conflict. The member
that returns, or that lost the resolution, takes the current primary's
trust-anchor state with the rest of that primary's data on its next scheduled
pull. Its own state is kept with its other discarded rows as private quarantine.
You do not need to do anything.

A member that cannot take the primary's trust-anchor state refuses that
primary's data as a whole, at every pull, before it writes anything. It stays
fenced. It keeps answering DNS from its own last-good data, with zones and
records as they were when it last followed and with its own trust anchors, and
it restarts in that state. Its HTTP API stays closed and it accepts no writes.
Changes made on the primary after the split do not appear in its answers.
Nothing a member does in this state restores trust in a revoked root key, and
no member falls back to the shipped root anchors. The other members and the
primary are not affected.

A member in this state is not a promotion candidate: `cluster promote` needs a
follower. Until it follows again, the cluster depends on the current primary
and on the members that follow it. If a member that follows the primary
remains, promote that one when the primary is lost. If the primary and every
member that followed it are lost for good, keep the fenced member's data with
`cluster resolve --offline --abandon-term`, as described in
[Keep a fenced member's data when its primary is lost for good](#keep-a-fenced-members-data-when-its-primary-is-lost-for-good).

#### Recognize a member that refuses the primary's data

kudurru does not report this state directly yet. Use what exists today:

1. The member's HTTP API stays closed (`generation_pending`) long after the
   other members follow the current primary.
2. Stop the member and review its durable state. A member in this state is
   fenced by the current primary's term and holds no image of that term:

   ```sh
   kudurru cluster resolution --offline --data-dir /var/lib/kudurru
   ```

   The output has `"mode":"fenced"`, `"reason":"fenced_behind_term"`, and an
   `installed.term.epoch` that is lower than `term.epoch`. A member that
   follows prints `"mode":"follower"` and the same term in both places.
   `"resolvable":true` here only means that the member's data could be kept if
   the primary were lost for good; while the primary runs, do not resolve. Start the member again afterwards: it answers DNS only
   while it runs.
3. To narrow the cause, compare the trust-anchor version of the stopped member
   with the version of a stopped member that follows the primary (or of the
   primary itself, during a maintenance stop):

   ```sh
   kudurru anchors status --offline --database /var/lib/kudurru/state.db
   ```

   - `recoveries` is above 0 on either side: see "The two disagree about
     `anchors recover` history" below.
   - The following member prints `version.pure.generation` `"0"`: the primary
     never started managed trust anchors.
   - Otherwise the refusing member holds a key revocation that the primary has
     not recorded. No command lists the keys yet.

#### What ends the state

- **The returning member saw a key revocation that the current primary has not
  recorded yet.** The member does not trust that key again. An old primary that
  recorded the revocation itself answers DNS from its own data, which already
  treats the key as revoked. A member whose own root check saw the revocation
  while it was a follower, or while it was fenced, answers `SERVFAIL`, as
  described above, and keeps doing so; take it out of your DNS path meanwhile.
  The state ends on its own, on the first scheduled pull after the primary's
  next root-anchor check has recorded the same revocation. If the member
  carries more than one such key, the primary has to record all of them. That
  check follows the primary's refresh schedule (at the real root, about once a
  day; never more than 15 days after its last successful check) and nothing
  starts it earlier. What you can do is make sure it can happen: the primary
  needs outbound TCP 53 to the configured root servers and a synchronized
  clock. If the primary logged `root anchor updater stopped for this writer
  term`, its trust-anchor updates have stopped; fix the cause named in that
  line and restart the primary. After a restart it checks the root as soon as
  its stored refresh time has passed.

  The primary can never record the revocation in two cases. The root has
  stopped publishing the revoked key: the primary then records the key as
  missing and keeps trusting it, as RFC 5011 requires. Or the primary's history
  never contained the key: the member saw a new key appear and then its
  revocation during the split, before any member trusted it. In both cases the
  member stays fenced and never follows that primary. No command clears a
  revocation a member has seen or records one on the primary by operator
  decision. Your options are to replace the member, as described below, or to
  keep it as it is. A replacement holds
  the cluster's trust-anchor state: in the first case that state still trusts
  the key the old member saw revoked, and kudurru v0.2.0 gives you no way to
  change that on the primary.
- **The two disagree about `anchors recover` history.** The returning member
  holds a recovery record that the current primary does not have, or the primary
  recovered after the split while the returning member's own trust-anchor state
  had already moved at least as far. Running `anchors recover` on the primary as
  well does not reconcile them, even with the same replacement key: each
  recovery writes its own record. That database cannot follow the current
  primary. Replace the member.
- **The returning member had managed trust anchors and the current primary never
  started them.** The state ends without a replacement when the primary starts
  managed trust anchors from the same seeds: set
  `upstreams.recursion.anchorMode: managed` with the same
  `upstreams.recursion.trustAnchors` records the returning member started from
  (their order does not matter). The primary's first root-anchor check then
  creates its own trust-anchor state for those seeds, and the member takes it
  on its next scheduled pull. If the primary starts from other seeds, or stays
  on static trust anchors, replace the member.

#### Replace a member that cannot follow

A replacement is a new member with a new database. It never held the old
member's trust-anchor state, so it takes the primary's state exactly: it trusts
what the primary trusts and treats as revoked what the primary recorded as
revoked.

1. Leave the old member running while it still answers DNS from its own data.
   Stop it first if it answers `SERVFAIL`.
2. On the primary, issue a join token for a **new node name and a new internal
   address**. kudurru v0.2.0 has no command that removes a member, so the old
   member's name and internal address stay reserved, and `cluster join-token`
   refuses an invitation that reuses either one. Another port or another
   address on the same host is enough.
3. Join the receiver with a new data directory, as described at the top of
   this page, and configure its host settings for the new name and address. For
   a node behind managed edges, follow "Add a follower behind managed edges" in
   the [runbooks](runbooks.md).
4. Start it and check that its API answers (`kudurru cluster status`); the API
   of a member that does not follow stays closed.
5. Stop the old member for good. Keep its data directory and
   `member-identity.json` offline as evidence if you want them. Never start
   that directory again as a second node, and never copy its database or
   identity into the replacement's directory. The old member stays listed in
   the cluster's membership and counts toward the limit of 64 active members.
6. Point anything that addressed the old member directly (a VIP backend list, a
   resolver list) at the new address.

#### Keep a fenced member's data when its primary is lost for good

A fenced member that never installed the current primary's data (the state
above, or a member that was fenced a moment before the primary failed) cannot
be promoted, and a replacement needs a primary to join from. When the primary
of that term **and every member that followed it** are lost for good, make the
fenced member the primary of a new term on its own data:

1. Make sure that the lost primary and its followers are stopped or isolated
   and will not be started with their data directories while you decide. If
   any member still follows that primary, stop here and promote that member
   instead.
2. Stop kudurru on the fenced member and review it:

   ```sh
   kudurru cluster resolution --offline --data-dir /var/lib/kudurru > resolution.json
   ```

   Check `"mode":"fenced"`, `"reason":"fenced_behind_term"` and
   `"resolvable":true`. `term.epoch` is the epoch of the lost term.
3. Keep the member's data and name that epoch:

   ```sh
   kudurru cluster resolve --offline --data-dir /var/lib/kudurru --review resolution.json \
     --confirm-node node-a --abandon-term TERM_EPOCH \
     --request-id abandon-2026-11-11 --reason "primary node-b and its followers are lost"
   ```

   The command refuses without changes if `--abandon-term` is missing or is not
   the reviewed epoch, if the member's state changed since the review, or if
   the member follows a primary, is the primary, or holds a conflict. It writes
   a `cluster.resolve` audit event whose authorization names the abandoned
   term.
4. Start the member. It is the primary of a new term and serves its own data
   with its own trust anchors: every root key revocation it holds stays
   recorded, and a member whose own root check had denied a key keeps
   answering `SERVFAIL` until its own root check records that revocation, as
   after a promotion. Join new followers from it.

Changes made on the lost primary after this member's last pull are gone; make
them again on the new primary. If a member of the abandoned term returns after
all, it is fenced by the new term and takes this member's data on its next
pull, its own rows kept as private quarantine. With managed root anchors it
refuses that data, as described above, only if it holds a root key revocation
that this member does not.

#### Other states in which a managed member answers `SERVFAIL`

- **A member whose own root check saw a revocation.** It answers `SERVFAIL`
  until it installs trust-anchor state from the primary that records the same
  revocation (see above). A member in that state that is promoted stays in it
  as the primary: it still cannot publish, so it keeps answering `SERVFAIL`,
  and it cannot record any root-anchor check that omits the revocation. Its own
  check that sees the revocation records it and ends the state. Prefer to
  promote a member that serves. The members that follow such a primary keep
  serving, but while it is the primary no member's trust anchors are updated.
  If its own check can never see the revocation (the root has stopped
  publishing the key), the state does not end by itself: promote a member that
  serves, then replace this one.
- **A member that verified a revocation it could not write.** If its own root
  check verifies a signed revocation at the moment new data from the primary is
  installed, the member cannot record it against the data it now holds. It does
  not continue as if it had seen nothing: it answers `SERVFAIL` until it is
  restarted. After the restart it serves the installed data, and its next root
  check records the revocation.
- **A member whose list of locally seen revocations is full** (256 keys) stays
  retired for good. Replace it.
