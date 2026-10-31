# Background integrations on SQLite

A licensed SQLite `serve` runtime runs persisted Cloudflare and UniFi
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
refused before provider work pending cleanup/finalizer support. Tier-B webhook
providers, optional UniFi mutations and integration management API/CLI/UI remain
separate work. The external-dns webhook listener is a separate inbound feature.

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
