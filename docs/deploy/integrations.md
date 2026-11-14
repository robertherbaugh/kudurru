# Background integrations on SQLite

A licensed SQLite `serve` runtime runs persisted Cloudflare, UniFi and webhook
`Integration` objects in the background. Each instance has its own elected
lease and serialized lifecycle. DNS queries use complete published snapshots;
provider acquisition never runs on the query path. The host is joined during
shutdown and joins the old provider before starting a replacement configuration
or credential. Followers consume replicated finite evidence without polling;
a promoted writer must acquire its own authority before observing or publishing.

This mount does not close every integration capability. On Kubernetes the host
runs Cloudflare publication and UniFi inventory and groups under one Lease per
Integration, and every replica serves the committed UniFi inventory and group
grants. Replicas watch the committed heads, so they follow each poll; an
observation expires three polls after it was made, so keep `spec.unifi.poll`
at two seconds or more on Kubernetes (at the one-second minimum a slow poll
can let grants lapse until the next one). UniFi record publication (`spec.unifi.inventory`) is refused on its
object with reason `RecordPublicationUnsupportedOnKubernetes`, and webhook
instances with `UnsupportedOnKubernetes`. UniFi inventory (client names, IPs,
MACs) is stored in private Secrets in the release namespace, readable by
anyone who can read Secrets there. On Kubernetes only Integrations in
kudurru's release namespace run; one in any other namespace is refused with
reason `NamespaceNotAllowed`. The reason is the credential trust model: the
`credentials.allowedFiles` and `credentials.allowedEnvironment` allowlists are
node-wide, and an Integration may name any allowlisted credential. Whoever can
create an Integration in the release namespace can therefore use every
allowlisted credential, so grant that only to principals you already trust
with kudurru's own Secrets there. A refused Integration's credential
references are not read, and no credential fingerprint is written to its
status. On Kubernetes, while the controller reports any DNSRecordSet that an
enabled primary zone admits as `Duplicated`, `Occluded` or `Invalid` (see its
`Ready` and `Degraded` conditions), every Cloudflare publisher stops
publishing and shows `Ready=False` with reason `SourceConflicted` and a
message naming up to five such record sets; UniFi inventory and groups keep
running. Anyone who may create a DNSRecordSet that such a zone admits can
therefore pause all Cloudflare publication until the record set is fixed or
removed, after which publication resumes on its own. Restrict who may create
record sets in zones you publish, or narrow the zones' record set selectors.
Native Kubernetes Secret references
are not resolved by either host: mount the Secret as a file and allowlist its
path under `credentials.allowedFiles`. Both deletion policies are supported on both stores; see
[Deleting an integration](#deleting-an-integration). Integrations can be
written from files, with `kubectl` on Kubernetes, or through the API and CLI
(see [Writing integrations through the API](#writing-integrations-through-the-api)).
Optional UniFi writes run on SQLite only (see [UniFi optional writes](#unifi-optional-writes));
on Kubernetes an Integration that enables one is refused with reason
`UniFiPushUnsupportedOnKubernetes`. The dashboard's Integrations page manages
Integrations as well (see [The Integrations page](#the-integrations-page)). The external-dns webhook listener is a separate inbound feature.

## Deleting an integration

`spec.deletionPolicy` decides what happens to what an Integration published
when its object is deleted. An Integration declared in the objects directory
is deleted by removing its document; one written through the API is deleted
with `DELETE /api/v1/objects/integrations/{namespace}/{name}` (or `kudurru
delete integrations NAME`), which answers `204` even while a `Delete` policy
keeps the object terminating.

With `Retain`, the default, the object is removed at once. Its records stay
at the provider, and the private ledger rows that describe them stay in the
store. A new Integration of the same name is a different incarnation: it
cannot adopt the old records and reports them as conflicts.

With `Delete`, the object is stored with the reserved finalizer
`kudurru.io/integration-cleanup`. You do not list it in the document, and no
`metadata.finalizers` entry adds or removes it: only the declared policy
decides it. Removing the document then leaves the object in place with a
deletion timestamp, and the elected writer cleans up before the object goes:

1. It reads every configured destination completely and deletes the record
   sets this incarnation provably owns: every row carries its marker and the
   ledger lists every provider ID. `Delete` overrides a zone's `upsert-only`
   or `create-only` policy for those record sets. Records of other owners,
   hand-made records, and anything that only looks like the instance's are
   never touched.
2. It forgets the ledger rows of records that are gone.
3. It removes, in one commit, the local state of the instance's source: the
   record sets and zones it generated, its inventory clients, its name
   reservations and its stored observations. Records you declared yourself
   and members you added to a group by hand stay.
4. It removes the finalizer, which removes the object, and records an
   `integration.cleanup` row in the audit log.

A UniFi instance, and a webhook instance that never began publishing
records, owns nothing at its provider, so its cleanup is steps 3 and 4 only:
it makes no provider call and does not need the credential or a reachable
peer. A webhook instance that publishes records (see
[Tier-B webhook records](#tier-b-webhook-records)) is cleaned up like a
Cloudflare one: step 1 deletes the record sets whose registry row names it,
including those outside domains that were narrowed since, and leaves every
other row at the peer. While the peer is unreachable the object stays
terminating with its finalizer. An instance whose `domains` were all removed
still cleans up the record sets it published if its peer offers another
capability; a records-only peer refuses an instance without domains, so its
cleanup stops at provisioning: restore one domain, or set `deletionPolicy:
Retain` to release the object and keep the records.

While this runs the Integration is shown with phase `Terminating`, and
`kudurru integrations get` and `list` (and `GET /api/v1/integrations`) show
`deletionPolicy`, `terminating: true`, `deletionRequestedAt`, the reason and
the counts described below (`remaining`, `blocked`, `uncertain`). Check,
preview, sync and actions are refused for it with `409 Conflict`. The cleanup
works in bounded runs and shares the Cloudflare request budget with every
other instance, so a large zone can take several budget windows.

Everything the cleanup needs is in the store (the finalizer, the ledger and
the recorded operations), so it continues from there:

- After a restart, the new process resumes the cleanup. A delete whose answer
  was lost (the provider applied it but the connection failed) stays pending
  and is never sent again: the next run reads the zone completely and
  confirms that the records are gone.
- After a failover, the old writer can no longer record a receipt or remove
  the finalizer, even when its last delete reaches the provider. The promoted
  writer resolves that delete by a reread and finishes.
- When the license expires in the middle of a cleanup, the cleanup pauses
  with `LicenseUnavailable` and makes no provider request; the object stays
  `Terminating`. Installing a valid license resumes it, and a release works
  without one. Setting `spec.suspend` pauses it the same way, with
  `Suspended`, until it is cleared.
- When the credential is missing, a publisher's cleanup pauses with
  `CredentialsUnavailable` and makes no provider request; restoring the
  credential resumes it. A rotated value of the same reference is accepted,
  because ownership is proven by the marker and the ledger, not by the token.

The object stays `Terminating` with a reason, and nothing is finalized, while
any of these holds. With the reason the integration status carries three
counts, which the cleanup also writes to `status.records` of the object:
`managed` is the number of records not yet removed from the ledger,
`conflicts` the number it must leave in place, and `drift` the number of
provider writes with an unresolved outcome. The first is counted when a run
starts, so a run that stops early (no budget, a provider error) still says
what is left.

| Reason | Meaning | What to do |
| --- | --- | --- |
| `LicenseUnavailable` | The license does not allow integrations. | Install a valid license, or release. |
| `Suspended` | `spec.suspend` is set. | Clear it, or release. |
| `CredentialsUnavailable` | The credential reference does not resolve. | Restore the file or variable, or release. |
| `ProvisionFailed`, `ProviderFailed`, `RateLimited`, `BudgetExhausted` | The provider or the request budget refused, or a provider request did not answer within the run's 30-second bound; retried with backoff (a failed setup from one poll interval up to five minutes, so a provider that stays unreachable is contacted about every five minutes). A setup whose peer offers no role the instance can take (below) is not retried until the object, credential or license changes. | Usually nothing. |
| `SnapshotIncomplete` | A zone could not be read completely, so nothing was decided. | Usually nothing. |
| `RecordsConflict` | A record of the instance shares its name or record set with a record of another owner. | Remove or move the other record, or release. |
| `OwnershipUnproven` | A row carries the instance's marker without ledger evidence, or a record the ledger lists still exists without the marker. | Decide what the row is; delete it or release. |
| `PendingRecovery` | An earlier write has an uncertain outcome that a reread did not resolve. | Release, then check the record at the provider. |
| `DestinationRemoved` | The ledger has records of a zone the spec no longer names. | Name the zone again, or release. |
| `LocalStateRemains` | A record set or zone the instance generated could not be removed, or a row of the instance was stored after the purge. | Check that object's own finalizers; a later run removes late rows. |
| `CapacityExceeded` | The store holds more rows than a bounded read of the cleanup admits (see the limits below). Nothing was decided from a partial read. | Release. |
| `CleanupFailed` | A read or a write of the local store failed, or was too slow for the run's 30-second bound (then retried with backoff). | Usually nothing; check the server log if it stays, or release. |
| `ProviderUnavailable` | This build has no provider of the Integration's type. | Run a build that has it, or release. |
| `ReleaseFailed` | A release could not remove the finalizer yet. | Usually nothing; check the server log if it stays. |

`LicenseUnavailable` and `Suspended` start no cleanup job, so only the
integration status shows them: the object's own `Ready` condition and
`status.records` keep what was last written. Every other reason is also
written to the object.

`CleanupInProgress` and `AuthorityChanged` are passing states of a cleanup
that is working. A blocked cleanup is checked again at the instance's resync
interval, at most every ten minutes, and immediately when you change the
declared object.

The cleanup has these limits. It reads each configured zone as a sync does
(at most 10,000 records per zone), and the store may hold at most 100,000
ledger rows of all Integrations together. Every create, update and delete of
a record set leaves one stored operation, and nothing removes them while the
Integration lives; the cleanup forgets them in bounded commits, a stored page
at a time, so it never holds the whole history at once. A run that made
progress ends before its 30-second bound would cut its next commit, and a
commit that the bound cut without any progress is retried at half its size.

What bounds the history is the SQLite store as a whole. Root's integration
capture starts by reading one complete generation of the store, and that
read refuses a store of more than 100,000 stored objects or 256 MiB of
stored documents, all kinds together (the replication image limits). Past
that, no Integration job gets a frame, cleanup included, and replication
and the other whole-generation reads stop as well. Operation rows count
toward it like every other object, so a store whose Integrations, including
removed `Retain` Integrations, have stored close to 100,000 record changes
is near the limit. The purge and the finalization have their own scan bound
of 200,000 rows of one kind, which this store limit always reaches first.

To release an Integration that is `Terminating`, review it and run the
release command with the exact UID and revision you reviewed:

```sh
kudurru integrations get publisher
kudurru integrations release publisher --uid <uid> --revision <revision>
```

(`POST /api/v1/integrations/{namespace}/{name}/release` with the body
`{"uid": "...", "revision": "..."}`.) It needs the `integrations.release`
permission, which only the `admin` role has. It refuses a live object, a
changed UID or revision (`409 Conflict`, review again), and runs only on the
elected writer. It removes the object without contacting the provider and
needs neither the license nor the credential. The provider records and the
ledger rows stay, as under `Retain`, and the answer says how many ledger rows
and operations were retained. A delete batch that the cleanup had already
sent may still complete at the provider; its ledger row then keeps its
unresolved intent. The audit log records `integration.release` with the
reason `released without cleanup by the reviewed release command: ...`.

You can also release it by declaring it once more with
`deletionPolicy: Retain`. That removes the terminating object without
contacting the provider, needs neither the license nor the credential, and
leaves its remote records and ledger rows where they are. The still-declared
document then creates a new `Retain` instance, which you can remove normally.
`Retain` is the default, so a document without a `deletionPolicy` line
releases a terminating object in the same way. Keep the `deletionPolicy:
Delete` line when you re-add a document, or restore an older copy of it, for
an Integration that is still waiting for cleanup; otherwise its records stay
at the provider. Re-adding the document with `deletionPolicy: Delete` does
not cancel the deletion. A release is logged as `objects: finalized` and
recorded in the audit log as `integration.finalize` with the reason `released
without cleanup: deletionPolicy is Retain; dropped
kudurru.io/integration-cleanup`. Changing a live Integration from `Delete` to
`Retain`, or dropping the line, is recorded as an update with the reason
`disarmed: ...`.

A live `Delete` Integration that is stored without the finalizer, which a
database written by an earlier build can hold, is shown as `Paused` with
reason `CleanupFinalizerMissing` and makes no provider call. The next load of
its unchanged document adds the finalizer and it runs.

On Kubernetes kudurru adds the finalizer itself, by a metadata patch, to a
live `Delete` Integration that runs there, and removes it again when the policy
changes back to `Retain`. Until it is added the instance is `Paused` with
reason `CleanupFinalizerMissing` and makes no provider call. `kubectl delete`
then leaves a terminating object while the elected replica runs the same
cleanup as on SQLite; the local purge is a sequence of exact deletes that a
later run resumes, and the summary is a log line (`integration cleanup`)
instead of an audit row. To give up on a cleanup, patch the policy to
`Retain`, use the release command below, or remove the
`kudurru.io/integration-cleanup` finalizer by hand; each leaves what was
published, like `Retain`. A refused Integration is never armed, but a UniFi
Integration armed for `Delete` that later gains `spec.unifi.inventory` keeps
its finalizer: deleting it purges its source Secrets locally, with no
provider request, and then removes the object. Deleting a `Retain` object removes it and leaves
what it published, and its private ledger Secrets stay in the release
namespace.

## Credentials and permissions

Grant only the credential files or environment names the host may resolve:

```yaml
credentials:
  allowedFiles:
    - /run/secrets/unifi-api-key
  allowedEnvironment:
    - CLOUDFLARE_API_TOKEN
```

An Integration references a granted file with `file: /run/secrets/unifi-api-key`
or an environment name with `env: CLOUDFLARE_API_TOKEN`. These are references,
never inline credentials. File grants use clean absolute paths. Background
capture bounds and copies the bytes, detects rotation by keyed fingerprint, and
replaces the old instance. Keep grants narrow: permission to create an Integration
is not permission to read arbitrary host files. Runtime status and private
source/ledger objects do not persist resolved credential bytes.

Cloudflare requires `credentials.apiToken`; UniFi requires `credentials.apiKey`.
The host also checks the current integrations license, exact Integration
incarnation/configuration, original source objects, credential cut and elected
SQLite writer at durable effects and final commit. Losing authority refuses the
operation; it does not grant permission to replay a refused mutation. External
provider effects cannot be atomic with the local receipt transaction: uncertain
outcomes remain pending for ownership-aware recovery.

## Writing integrations through the API

Integrations are objects of the generic object routes (D400), on both drivers:
`GET`, `PUT` and `DELETE /api/v1/objects/integrations/{namespace}/{name}`,
`POST /api/v1/objects/integrations`, `POST /api/v1/apply` and
`POST /api/v1/integrations:batchDelete`. The CLI uses the same routes
(`kudurru get integrations`, `kudurru apply -f`, `kudurru delete
integrations NAME`). The rules are those of every object write:

- `integrations.write` creates and replaces, `integrations.delete` deletes.
  A write that binds or changes `spec.credentials` (a create with
  credentials, an added, removed or changed reference) also needs
  `integrations.secrets`, because a reference can name any credential this
  node's allowlist grants. So does a write that, while credentials are
  bound, changes where the host presents them: `spec.type`,
  `spec.unifi.endpoint`, `spec.unifi.tlsFingerprint` or
  `spec.webhook.endpoint`. A write that keeps the stored references and
  their destination (suspending, the deletion policy, the resync interval,
  what an instance publishes) needs only `integrations.write`. Only the
  built-in `admin` role holds these verbs; on Kubernetes a ClusterRole can
  grant them separately.
- Credentials are references only (`file`, `env` or, on Kubernetes,
  `secretRef`). There is no field for a value; a document that carries one is
  refused with `400`. A validation failure that names `spec.credentials` is
  answered with a fixed message and the field paths only, never the refused
  value, so a pasted token (also with a trailing newline, a quote or a
  backslash) is not repeated; other refusals have every string of
  `spec.credentials` removed. Reads return the references, never the
  resolved bytes, and the audit row of the write holds the same document.
  A holder of `integrations.read` sees the reference names that Integrations
  use (file paths, environment variable names); the manifests route only
  withholds the rest of the node's allowlist from callers without
  `integrations.secrets`.
- A new Integration needs a license that covers integrations (`402
  license_required` otherwise). Replacing and deleting one do not, so an
  operator without a license can still suspend an instance, change its
  deletion policy or remove it.
- `If-Match` binds the stored `resourceVersion` exactly (`412` otherwise), and
  an Integration that a file or another manager owns answers `409 managed_by`.
  On SQLite the write runs in the current command: the original credential is
  re-authorized against the object the command reads, and the object, its
  reserved finalizer (with `deletionPolicy: Delete`) and its audit row commit
  together. An enrolled follower forwards these writes to its primary like any
  object write; the provider commands (`check`, `preview`, `sync`, actions,
  `release`) stay primary-only.

`GET /api/v1/integrations/manifests` (`integrations.read`, `kudurru
integrations manifests`) lists the provider types of this build with their
configuration schema, credential keys and which are required, and what this
node's host refuses: on Kubernetes the release namespace is the only one that
runs, webhook instances are unsupported and `spec.unifi.inventory`,
`spec.unifi.push.dhcpDNSServers`, `spec.unifi.push.mirrorRecords.enabled` and
`spec.unifi.push.clientActions` are refused; SQLite refuses none of them.
`fieldLimits` gives each such field with its effect: `refused`, with the
status reason and message the Integration would show, or `inert` for a field
that is accepted but does nothing because the field it needs is refused
(`spec.unifi.push.dnsServers` and `networks` on Kubernetes). A
`deletionPolicy: Delete` UniFi Integration refused on Kubernetes for
`spec.unifi.inventory`, `dhcpDNSServers` or `clientActions` is still cleaned
up when it is deleted (its local state is purged, with no request to the
controller); one refused for `mirrorRecords.enabled` stays Terminating until
that switch is turned off or its cleanup is released. It also reports whether the
license covers integrations and which reference forms this node resolves; the
allowlisted file paths and environment names are included only for a caller
that holds `integrations.secrets`.

## The Integrations page

The dashboard's **System → Integrations** page lists every Integration with
the host's state of its stored revision (phase, reason, deletion policy, last
sync, who manages it), and each instance has its own page.

- **Delete N selected** deletes the selected instances with
  `POST /api/v1/integrations:batchDelete`, one request per namespace, one at
  a time, after a confirmation that names each instance and its deletion
  policy. Each is deleted as `DELETE` without `If-Match` (the revision on
  screen is not compared): a `Retain` instance goes at once, a `Delete`
  instance stays terminating until its cleanup finishes. Instances that a
  file, Kubernetes or the system manages, and terminating ones, cannot be
  selected; the batch needs `integrations.delete` and is refused in static
  mode.

- **New integration** opens a wizard built from `GET
  /api/v1/integrations/manifests`: the provider types of this build (a type
  this node does not run, such as a webhook on Kubernetes, is shown with the
  reason and cannot be chosen), the provider's settings from its
  configuration schema without the fields this node refuses (an optional
  group of settings, such as a Cloudflare zone's `adoptFrom` or the UniFi
  `inventory`, is left out unless one of its fields is set, and only then
  are its required fields required), and one
  reference per credential the provider reads. The UniFi `push` block is
  offered on SQLite with the checks the provider makes (DHCP DNS servers
  needs IPv4 `dnsServers` and `networks`, client actions need `legacy`, the
  record mirror needs `domains`); on Kubernetes it is not offered, and the
  form says why (`UniFiPushUnsupportedOnKubernetes`). A reference is a file or an
  environment name the node configuration grants; the page has no field for
  a credential value. It needs `integrations.write`, `integrations.secrets`
  to bind a credential, and a license that covers integrations.
- **Check** proves the endpoint and the credential. For a provider that
  publishes records (Cloudflare, a webhook with domains, UniFi with
  `push.mirrorRecords.enabled`), **Preview** shows the planned changes, the records it
  leaves alone and the plan's digest, and **Publish this plan** applies
  exactly that plan. When the plan expired or no longer matches (the
  instance, its zones or the provider changed), the server refuses it and
  nothing is published: preview again. When the instance's revision moves
  after a preview (a host status write moves it too), the page drops the
  plan and says the preview was superseded. An inventory provider has **Sync
  now** instead. When the provider does not acknowledge a sync, the page
  says the outcome is unknown and does not offer another sync of that
  instance until a Check or a Preview succeeds. That hold is kept by the
  open page only: reloading or leaving the page drops it, and another
  client is not held.
- **Load actions** lists the actions the provider advertises, with their
  parameters, and runs one with `integrations.act`.
- **Configuration** edits the instance in place; a change of a credential
  reference, or of where bound credentials are sent, needs
  `integrations.secrets`. An Integration from a file or another manager is
  read-only.
- **Delete** removes a `Retain` instance at once. A `Delete` instance stays
  on its page as terminating, with what is left, what blocks and what is
  uncertain, until its cleanup finishes; **Release without cleanup** (admin,
  `integrations.release`) removes it as described above.

On an enrolled follower the configuration and delete controls work (the
follower forwards them); check, preview, sync, actions and release are
disabled with the primary named. In static mode nothing that writes is
offered.

## Tier-B webhook clients and actions

Configure an HTTPS provider with explicit named credentials and trust:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: Integration
metadata: {name: inventory, namespace: default}
spec:
  type: webhook
  credentials:
    bearerToken: {file: /run/secrets/webhook-token}
    ca: {file: /run/secrets/webhook-ca.pem}
  webhook:
    endpoint: https://inventory.example.com/provider
    timeout: 10s
  resync: 1m
  deletionPolicy: Retain
```

Grant those exact files in `credentials.allowedFiles`. Mutual TLS uses named
`clientCertificate` and `clientKey` references. The peer must speak the versioned
Tier-B protocol and advertise supported clients, actions or hooks
capabilities, or `records` together with configured `domains` (see
[Tier-B webhook records](#tier-b-webhook-records)). A peer that offers only
records while no domains are configured is refused. Actions use the reviewed integration
API/CLI and their advertised parameter schemas. An uncertain action is reported
without automatically replaying it.

Complete client/network polls publish finite inventory. A genuine `LastSeen`
expires after three minutes; another poll cannot renew it. `FixedIP` does not
establish an observed address. This adapter creates no desired DNS records or
ClientGroup membership. Actions-only peers do not poll inventory. Client polling
is capped at one minute, or the configured shorter resync interval.

### Hook events

A peer that advertises `hooks` receives events at `POST /events`. It needs a
separate signing key, named `hmacKey`, of 32 to 4096 raw bytes (not hex):

```yaml
  credentials:
    bearerToken: {file: /run/secrets/webhook-token}
    ca: {file: /run/secrets/webhook-ca.pem}
    hmacKey: {file: /run/secrets/webhook-hmac}
```

A hooks peer without `hmacKey` is refused. Each event is one JSON body,
`{"topic":"record.changed","time":"2026-11-10T12:00:00Z","payload":{...}}`,
with `X-Kudurru-Delivery` (a stable ID; deduplicate on it) and
`X-Kudurru-Signature: sha256=<hex HMAC-SHA256 of the exact body>`. Answer 2xx
to acknowledge. A 5xx answer, a timeout or a busy request budget is retried
with backoff under the same ID and body for up to 24 hours; a 4xx answer drops
the event and sets the instance Degraded (`EventDeadLettered`). Events are
at-least-once and unordered across retries; every payload carries a version.

The topics are `record.changed`, `zone.changed`, `blocking.toggled`,
`integration.error` and `cluster.leader_changed`. A new instance reports
changes from its start on, not existing objects. Events survive restarts and
failover through a durable per-instance cursor; when that cursor can no longer
resume, the peer receives one `zone.changed` per zone with `change: "resync"`
and the individual changes in between are not sent.
`query.blocked`, `vip.master_changed`, `client.seen` and `client.left` are not
produced in this release. `blocking.toggled` is sent when a blocking change is
committed; a timed disable that runs out is not an event, so use the
`disabledUntil` of the disabling event.

Every hook instance receives events for all namespaces: record set, zone and
Integration names and namespaces, error reasons and the primary node ID. Treat
creating a hooks Integration as a cluster-level privilege, and point it only at
a peer that may see that. A per-namespace filter is not available yet.

`zone.changed` reports a zone's committed serial (or its deletion), once per
serial; a resync event is versioned by its resync, so it is sent even for a
serial the peer already received.

Kudurru keeps at most 384 delivery records per hook instance: it stops
reading new changes while 256 events are undelivered and removes delivered or
dropped records above 128, older versions of a subject first. A status-only
write of an object can send its current event again under the same
`X-Kudurru-Delivery` ID once that record is gone (when 128 newer events of
other subjects completed, or a day and an hour after it was created), so keep
deduplicating on the ID. Every running Integration, not only hook instances,
removes delivery records an hour past their 24-hour deadline, and an instance
whose peer stopped advertising hooks drops its own undelivered events at
their deadline (`EventDeadLettered`). With no Integration running at all,
nothing is removed until one runs.

### Tier-B webhook records

A webhook Integration that names `domains` publishes the selected records of
those domains to a peer that serves the external-dns record routes
(`GET /records`, `POST /records`, `POST /adjustendpoints`), under either the
external-dns media type or the Tier-B one. Any external-dns webhook provider,
and the external-dns listener of another Kudurru, can be the peer:

```yaml
  webhook:
    endpoint: https://records.example.com/provider
    domains: [example.com]
    policy: sync        # or upsert-only, create-only
```

Each domain must lie inside the domain filter the peer negotiates, or the
instance is refused. Records of names the peer's filter excludes, and names
whose first label starts with `_kudurru-`, are not published: such a name
refuses the whole sync.

The external-dns protocol has no row IDs, versions, owners or conditional
writes, so Kudurru keeps ownership at the peer in a TXT registry. Every record
set it manages has one companion TXT record at `_kudurru-<type>.<name>` (for
a wildcard `*.example.com`, `_kudurru-<type>-wildcard.example.com`) with the
same set identifier and the value
`"heritage=kudurru,kudurru/owner=<digest of the cluster ID and Integration UID>"`.
The peer must store TXT records and underscore labels. A record set is the
instance's only when its registry record names it; a registry record of
another Kudurru instance, or an external-dns heritage record
(`heritage=external-dns,external-dns/owner=...` at the same name or
`<type>-<name>`), makes it another owner's, and anything else is unowned.
Records of other owners and unowned records are never changed; a desired
record at such a name is reported as a conflict. Adoption is not offered for
webhook peers. Only rows inside the configured domains are considered, so the
peer may serve other names too, with one exception: a record set whose
registry record names this instance stays the instance's after `domains` is
narrowed. Under `policy: sync` the next sync deletes it with its registry
record, as it deletes a record removed from the sources; under `upsert-only`
and `create-only` it stays, and a `deletionPolicy: Delete` cleanup removes
it. Removing every domain ends publication instead: the instance no longer
syncs records (against a peer that offers only records it is refused as
`ProvisionFailed`), and the record sets it owns stay at the peer as under
`Retain`. To remove them, narrow `domains` instead, or delete the Integration
with `deletionPolicy: Delete`, whose cleanup still reads the peer and removes
them. A registry record of another Kudurru with no data record still claims
its name: a desired record there is reported as a conflict and never
created. The claim holds the whole name, as a record of another owner does:
no record set of this instance at that name, of any type, is updated or
deleted while it exists, and a `deletionPolicy: Delete` cleanup stops with
`RecordsConflict` instead of removing them. Such a stale row is typically
left by an Integration that was deleted under `deletionPolicy: Retain`. To
clear it, delete the `_kudurru-<type>.<name>` TXT row at the peer (or let
that Kudurru's own Integration remove it), and the next sync or cleanup
proceeds. Claims count against the same 10,000 record sets per destination
as listed records; above that every sync fails as over capacity.

Each change is one `POST /records` with the record set and its registry
record. The answer, even a 204, is never taken as the result: Kudurru rereads
the complete list (no pagination headers, at most 32,768 rows and 10,000
record sets) and confirms the change only when the reread shows the intended
record set under its registry record. A peer that drops the registry row,
accepts the body without effect, fails or answers nothing leaves the change
pending with its targets marked ambiguous: the instance reports
`ConflictsOrPending`, a reviewed sync answers `503 integration_uncertain`,
and the next sync settles that same operation from a fresh reread. It is
confirmed without sending anything when the peer did apply it; when the
reread still shows the old state, recovery sends the change once more under
the same operation. A record set whose data the peer applied but whose
registry record it dropped is no longer provably the instance's, so it stays
pending and nothing is sent again until an operator removes the data record
or the peer stores the registry record; while it is pending the instance
publishes nothing else. Nothing closes the window
between Kudurru's reread and its POST; another writer changing the same record
set in that window is detected by the verifying reread, not prevented.

Each change costs about three requests (reread, change, reread) against the
instance's request budget of 1,000 requests per five minutes, so the first
publication of a large zone takes a while. A reviewed sync of a webhook
publisher uses `--mode publish` with the preview digest, as for Cloudflare.
With `deletionPolicy: Retain`, deleting the Integration leaves its records and
registry records at the peer; a new Integration has a new UID and sees them as
another owner's. Leftover registry records of deleted record sets, if a peer
kept them, are hidden and reused.

Use `kudurru integrations webhook conformance --expect-endpoints-file` (see
[Check an external webhook](#check-an-external-webhook)) to check that a peer
reads back what it applied, TXT registry rows included, before pointing an
Integration at it.

## UniFi inventory

For a supported UniFi Network 10.6.106 controller with verified HTTPS, configure a
namespaced Integration referencing an existing cluster-scoped primary DNSZone:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: Integration
metadata: {name: inventory, namespace: default}
spec:
  type: unifi
  credentials:
    apiKey: {file: /run/secrets/unifi-api-key}
  unifi:
    endpoint: https://controller.example.com
    poll: 1m
    inventory:
      zoneRef: {name: home}
      reverseZones: disabled
      nameTemplate: '{{ .Name | slug }}'
    groups:
      byNetwork: {LAN: devices}
  deletionPolicy: Retain
```

The target DNSZone must admit RecordSets from the Integration namespace. Group
contributions additionally require an explicit same-namespace public grant:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: {name: devices, namespace: default}
spec:
  sources:
    integrationRef: {name: inventory}
```

Manual group matchers remain unchanged. Private MAC membership requires the
original captured inventory source and original exclusive expiry; a different
source reporting the same MAC cannot inherit that contribution. Devices without
MACs cannot obtain private group membership through an IP address alone.

Successful complete polls atomically stage owned desired records, permanent name
allocations and source/global inventory. Publication waits for exact primary
record receipts. Failed polls do not renew expiry. Complete omission withdraws
live claims and owned desired records while retaining name reservations.
Suspension, deletion or configuration replacement closes live inventory/group
authority; `Retain` can leave historical DNS evidence until its original finite
expiry. Restart and replica promotion preserve those absolute times. A new
successful observation is required to extend them. In-flight provider work from
a fenced writer cannot commit under successor authority.

A UniFi Integration polls every `spec.unifi.poll` (default one minute) or every
`spec.resync` (default ten minutes), whichever is shorter. Before v0.2.0 it
ignored `spec.resync`, so a poll longer than ten minutes now runs every ten
minutes unless you raise `resync` as well. Observations still expire three
`spec.unifi.poll` intervals after they were made.

## UniFi optional writes

Every UniFi write is off by default, enabled per Integration, and runs only as
an explicit reviewed operator command: inventory polling and background ticks
never write to the controller. These writes run on SQLite only. They were
tested against a TLS double of the documented UniFi Network API (10.6.106),
not against a live controller.

```yaml
spec:
  type: unifi
  unifi:
    endpoint: https://controller.example.com
    legacy: true                 # required by clientActions only
    push:
      dhcpDNSServers: true
      dnsServers: [192.0.2.53, 192.0.2.54]
      networks: [LAN, IoT]
      clientActions: true
      mirrorRecords:
        enabled: true
        domains: [example.com]
        types: [A, CNAME]        # empty mirrors both
        labels: {mirror: unifi}  # optional
        exclude: ["*.internal.example.com"]
```

**Mirrored records as DNS policies.** With `mirrorRecords.enabled`, the
Integration is also a publisher whose destination is the site's DNS policies,
so the gateway can answer when kudurru is down. Records are selected as for
Cloudflare (`domains`, `labels`, `exclude`, and zone publish rules naming the
Integration), limited to A and CNAME. Records with a view, wildcards and names
longer than 127 characters cannot be mirrored and make the preview fail;
exclude them. TTLs above the policy maximum are capped. Review and apply
exactly as for Cloudflare:

```sh
kudurru integrations preview gateway -n default --uid UID --revision REVISION
kudurru integrations sync gateway -n default --uid UID --revision REVISION --mode publish --preview-digest DIGEST
```

A DNS policy has no field that could carry an ownership marker, so ownership
comes only from the private ledger: a policy is the Integration's when a
confirmed operation of this Integration created it. Every other policy at a
selected name, including forwarded domains and other record types, is a
conflict in the preview and is never modified. A repeated preview of an
applied plan is empty, a changed address or TTL of an owned policy previews as
an update (drift), and a record removed from the selection previews as a
delete of only its own policy. Disabling `mirrorRecords` leaves applied
policies in place; `deletionPolicy: Delete` removes them when the Integration
is deleted. If a create is not acknowledged, the sync answers
`integration_uncertain` and nothing is retried. The policy it may have made is
never treated as owned (it shows as a conflict), and further reviewed syncs
are refused until the next poll finds the site exactly as before the intent.
Removing such a policy in UniFi lets that happen. A changed address that was
not acknowledged settles by itself at the next poll, without a write. The same
holds for a record with several addresses whose creation stopped part way (a
failed, refused or timed-out second create): the policies already
acknowledged are this Integration's unfinished work, so they are not
conflicts, but they keep the intent pending. Remove them in UniFi; the next
poll, finding the site exactly as before and none of those policies left,
settles the intent without a write, and the record previews as a create
again. An update or delete that stopped part way (one policy of the record
was changed or removed, and a later write failed) leaves no acknowledged
policy for the poll to look for. The intent stays pending until the site
shows either exactly the state before the intent, with the same policy IDs,
or the state the intent was writing. A removed policy cannot come back with
its ID, so finish such a change in UniFi instead: make the record's
remaining policies what the preview showed (or remove them, for a delete),
and the next poll confirms the intent without a write. Every failed write, including a definite refusal by the controller, is
reported as `integration_uncertain`; a record the controller keeps refusing
stops each sync at that record until you exclude it.

**DHCP DNS servers.** With `dhcpDNSServers`, the `dhcp-dns-servers` action
writes `dnsServers` (one to four IPv4 addresses, for example the kudurru VIP,
in order) into the DHCP DNS server override of each named network. Preview,
then apply with the returned digest:

```sh
kudurru integrations act gateway dhcp-dns-servers -n default --uid UID --revision REVISION --params '{"mode":"preview"}'
kudurru integrations act gateway dhcp-dns-servers -n default --uid UID --revision REVISION --params '{"mode":"apply","digest":"DIGEST"}'
```

The preview lists each network as `update` (with its current servers),
`unchanged`, `missing` or `unsupported` (not a gateway network with a DHCP
server). The digest binds every network body read; any change to one of those
networks refuses the apply with `409` before a write. Apply refuses a plan with
a missing or unsupported network (`501`), sends one update per changed network
(the network as read, without its `id` and `metadata`, with only the IPv4 DHCP
DNS server override replaced), and rereads to confirm. A later preview shows
drift if someone changes the servers back. The previous servers appear in the
preview; restore them in UniFi if you want to undo the change.

**Block and unblock clients.** With `clientActions` (which requires
`legacy: true`), `block-client` and `unblock-client` take `{"mac":"..."}` and
send one legacy `cmd/stamgr` `block-sta` or `unblock-sta` command. They are
never run automatically.

All three actions require `integrations.act`, are audited (actor, action and
parameter digest; a client action also names its client in lowercase, for
example `for client 02:ab:cd:00:00:01`, whatever case was sent), and an unacknowledged write answers `integration_uncertain`
without a retry.

## Cloudflare publication

Use the [Integration sample](../../deploy/kustomize/samples/kudurru.io_v1alpha1_integration.yaml)
for provider selection, replacing its Secret reference with an explicitly granted
file/environment reference for SQLite. The sample starts suspended. Empty global
label selection includes all admitted records within configured domains; use
selection labels and exclusions to limit publication. Ownership binds both the
cluster and Integration UID. Foreign records are protected, and adoption requires
explicit configuration. `Retain` leaves remote provider records on deletion.

The root host writes durable intent, reservation, request budget and confirmed
receipts through concrete SQLite commands. The official SDK adapter disables
hidden retries and redirects; actual requests enter the host fence and shared
budget. Provider outages preserve DNS serving snapshots and original inventory
expiry. Review pending/uncertain effects before another explicit operation;
remote side effects are not rolled back by local transaction refusal.

### external-dns webhook sidecar

Run `kudurru externaldns-webhook` in the external-dns pod when its webhook client
cannot send your upstream bearer header. The command relays the four existing
webhook operations; the Kudurru server still reconciles records and enforces its
namespace, zone selector, licensing and write authority.

```sh
kudurru externaldns-webhook \
  --listen 127.0.0.1:8888 \
  --upstream https://dns.example.com/external-dns \
  --upstream-token-file /run/secrets/external-dns-token \
  --upstream-ca-file /run/secrets/webhook-ca.pem
```

Point external-dns at `http://127.0.0.1:8888` with `--provider=webhook` and
`--webhook-provider-url=http://127.0.0.1:8888`. Only literal loopback listeners
are accepted, so the two containers must share a pod network namespace. The
upstream's fixed path prefix is preserved. The embedded Kudurru webhook serves
HTTP: for a remote upstream, provide a verified HTTPS frontend that routes this
prefix to that listener. Plain HTTP is allowed only to literal loopback endpoints
for development or colocated containers. The optional CA file adds trust to the
system roots; there is no TLS-verification bypass.

The upstream token file is required for remote servers. It must be a regular
file of at most 4 KiB containing a bearer token of at least 16 characters.
Both the token and CA are captured at startup; restart after rotation. Caller
cookies, authorization and forwarding headers are never forwarded. The relay
reads no saved CLI credentials and opens no database or Kubernetes client.

Requests retain their original cancellation, with a 15-second operation deadline,
32 MiB request/response limits, 16 KiB headers and at most two concurrent requests.
The relay refuses redirects and never automatically retries an operation. A lost
response can leave the upstream outcome unknown; inspect the existing records
before manually replaying a write. A canceled process stops its listener and
joins shutdown within a bounded grace period. Direct `DNSEndpoint` reconciliation
is separate from this command.


## Review and run an integration

On complete SQLite serving, first read the exact instance and retain its `uid`
and `revision`:

```sh
kudurru integrations list -n default
kudurru integrations get publisher -n default -o json
kudurru integrations check publisher -n default --uid UID --revision REVISION
kudurru integrations preview publisher -n default --uid UID --revision REVISION
kudurru integrations sync publisher -n default --uid UID --revision REVISION --mode publish --preview-digest DIGEST
```

Use the digest returned by that complete preview. Changed source, credentials,
remote state or ledger ownership can invalidate it. A refusal is final for that
command; inspect the current state before intentionally reviewing new work.
An uncertain response can mean an external effect occurred. The client never
replays it automatically.

For a UniFi source, run one inventory acquisition with explicit observation mode:

```sh
kudurru integrations sync inventory -n default --uid UID --revision REVISION --mode observe
```

Permissions are `integrations.read`, `.check`, `.sync` and `.act` as appropriate;
server-side cookie calls also require CSRF. `integrations actions NAME --uid UID
--revision REVISION` lists only advertised actions. Of the built-in providers
only UniFi advertises actions, and only those its spec enables (see
[UniFi optional writes](#unifi-optional-writes)). `integrations act NAME ACTION --uid UID --revision REVISION
--params '{}'` cannot enable an unsupported provider action. An acknowledged
action's answer is returned in `result`; the audit row of an act names the
action and the SHA-256 digest of its parameters.

On SQLite these routes require complete runtime authority, and each effect
re-authorizes the original credential. On Kubernetes list and get answer on
every replica from the status the Lease holder wrote on the Integration, and
`leader` tells which replica holds it. Check, preview, sync, actions and act
run only on the Lease holder; another replica answers `503` with the holder's
name in `hint.holder` and does not forward the request, so a client behind a
Service must send the command to that pod. When no other replica holds the
Lease (the instance is suspended, unlicensed, paused or between two jobs)
the `503` names no holder and carries the instance's `phase` and `reason`
in its hint instead. A reviewed sync whose source head write was not
acknowledged is resolved by one read of the head; if that read does not
show the write, the answer is `503 integration_uncertain` and the sync is
not repeated. On Kubernetes the
route permission is checked once per request, not again inside each effect,
and a reviewed sync or act is logged on the holder rather than written to an
audit table. List/get status is unknown when no observation matches the
exact revision. The host's own status write is a new revision of the object
but changes nothing you reviewed: the status follows it, and once the host
has read that write back (its next capture, at most one capture interval
later, two seconds by default), a command that names the revision before one or more such writes (up
to eight) still runs. In the short window between the write and that capture,
a command at either the old or the new revision can still answer `409`, and so
can one that names a revision the host never captured (two status writes
inside one capture); reread the Integration and send it once more.
Any other change (spec, labels, annotations, finalizers) makes the review
stale, and the command answers `409`. Public previews omit credential references, fingerprints,
private provider IDs and ledger receipts.

## Check an external webhook

`integrations webhook conformance` checks an explicitly selected external-dns
or Tier-B webhook. It connects directly to that target using the supplied trust
and credential files. Saved Kudurru API credentials, `--server`, proxies and
Kubernetes contexts do not supply target credentials.

```sh
kudurru integrations webhook conformance https://webhook.example.test/provider \
  --ca-file /run/secrets/provider-ca.pem \
  --bearer-file /run/secrets/provider-token
```

HTTPS verifies the server certificate. `--ca-file` selects explicit trust roots;
omit it to use system roots. Use `--client-cert` and `--client-key` instead of
`--bearer-file` for mTLS. Plain HTTP is accepted only for literal loopback
addresses. Target URLs cannot contain user information, a query or a fragment.
A fixed path prefix is supported.

The default probes negotiate the versioned protocol, read advertised records,
clients, networks and actions, and normalize an empty endpoint array. An
explicit `--endpoints-file` supplies a different normalization input. The JSON
report contains fixed check names and outcomes without peer bodies or secrets.
`passed: true` means the attempted checks passed. `complete: false` means an
advertised mutation or another action remains untested; a successful read-only
run is not full conformance evidence.

Run mutation probes only against an intentionally selected disposable target.
Each probe needs `--allow-mutations` and its explicit input:

```sh
kudurru integrations webhook conformance https://webhook.example.test/provider \
  --ca-file /run/secrets/provider-ca.pem \
  --bearer-file /run/secrets/provider-token \
  --allow-mutations --changes-file reviewed-changes.json
```

`--changes-file` sends an external-dns Changes document.
`--expect-endpoints-file FILE` rereads the record list after any apply and
checks `{"contain":[endpoints],"absent":[endpoints]}`: `records_contain`
passes when each listed endpoint is present with the same name, type, set
identifier and targets (and TTL when given), `records_absent` when none of
the absent identities is listed. It is a read probe and needs no
`--allow-mutations`. Both record list reads accept up to 32,768 endpoints,
the bound the webhook publisher reads. `--event-file` with
`--hmac-file` sends the exact event bytes with HMAC-SHA256 and a fresh
`X-Kudurru-Delivery` ID. `--action NAME --params-file FILE` invokes that exact
advertised action once. The HMAC file contains 32–4096 raw bytes, not a hex
encoding. Changes, event and parameter files are captured before any request.

There are no automatic retries, redirect follows or cleanup writes. An
`effect_uncertain` result means a mutation may have happened; later mutations
stop. Inspect the target before intentionally running another command. Even a
complete report describes protocol acknowledgments, not physical DNS
publication, durable provider effects or activation of a Kudurru integration.

The default whole-run timeout is one minute, with a maximum of five minutes;
each request defaults to ten seconds within the original deadline. Responses
and Changes/event inputs are capped at 32 MiB, with additional collection and
nesting limits. Files are captured once. Cancellation bounds the caller's wait
for filesystem access, but an OS file operation can remain blocked; it retains
the single capture slot until it returns and cannot send a late request.
