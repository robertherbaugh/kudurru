# Background integrations on SQLite

A licensed SQLite `serve` runtime runs persisted Cloudflare, UniFi and webhook
`Integration` objects in the background. Each instance has its own elected
lease and serialized lifecycle. DNS queries use complete published snapshots;
provider acquisition never runs on the query path. The host is joined during
shutdown and joins the old provider before starting a replacement configuration
or credential. Followers consume replicated finite evidence without polling;
a promoted writer must acquire its own authority before observing or publishing.

This mount does not close every integration capability. Kubernetes execution is
currently disabled: its per-object CAS cannot atomically admit source objects,
allocations and global inventory. Native Kubernetes Secrets are not resolved by
the SQLite host. `Retain` is supported; instances configured with `Delete` are
refused before provider work until the cleanup job is mounted. Tier-B hooks and
record publication, optional UniFi mutations and remaining management UI work
remain separate. The external-dns webhook listener is a separate inbound feature.

An Integration declared with `deletionPolicy: Delete` is stored with the
reserved finalizer `kudurru.io/integration-cleanup`. You do not list it in the
document, and no `metadata.finalizers` entry adds or removes it: only the
declared policy decides it. Removing the document
then leaves the object in place with a deletion timestamp instead of deleting
it, and declaring it again with `deletionPolicy: Delete` does not cancel the
deletion. The delete is requested once: while the object waits, later loads
of the objects directory report no further deletion. Until the cleanup job
is mounted, release such an object by declaring it once more with
`deletionPolicy: Retain`: that removes the terminating object without
contacting the provider and leaves its remote records where they are. The
still-declared document then creates a new `Retain` instance, which you can
remove normally.

`Retain` is the default, so a document without a `deletionPolicy` line
releases a terminating object in the same way. Keep the `deletionPolicy:
Delete` line when you re-add a document, or restore an older copy of it, for
an Integration that is still waiting for cleanup; otherwise its records stay
at the provider. A release is logged as `objects: finalized` and recorded in
the audit log as `integration.finalize` with the reason `released without
cleanup: deletionPolicy is Retain; dropped kudurru.io/integration-cleanup`.
Changing a live Integration from `Delete` to `Retain`, or dropping the line,
is recorded as an update with the reason `disarmed: ...`.

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
Tier-B protocol and advertise supported clients/actions capabilities; peers
requiring hooks or records are refused. Actions use the reviewed integration
API/CLI and their advertised parameter schemas. An uncertain action is reported
without automatically replaying it.

Complete client/network polls publish finite inventory. A genuine `LastSeen`
expires after three minutes; another poll cannot renew it. `FixedIP` does not
establish an observed address. This adapter creates no desired DNS records or
ClientGroup membership. Actions-only peers do not poll inventory. Client polling
is capped at one minute, or the configured shorter resync interval.

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
--revision REVISION` lists only advertised actions. Current built-in providers
advertise none. `integrations act NAME ACTION --uid UID --revision REVISION
--params '{}'` cannot enable an unsupported provider action.

These routes require complete SQLite runtime authority. Kubernetes execution
remains unsupported by this command tranche. List/get
status is a local observation and can be unknown when the exact revision has no
matching observation. Public previews omit credential references, fingerprints,
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

`--changes-file` sends an external-dns Changes document. `--event-file` with
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
