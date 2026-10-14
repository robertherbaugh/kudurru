# Background integrations

The public `Integration` object describes Cloudflare publication, UniFi
inventory, or a Tier-B webhook. It is namespaced and uses the same generated
schema in SQLite and Kubernetes. Provider blocks must match `spec.type`;
Cloudflare requires an `apiToken` reference and UniFi an `apiKey` reference.
Deletion defaults to `Retain`, and every optional UniFi write defaults to off.
The [sample](../../config/samples/kudurru.io_v1alpha1_integration.yaml) starts
suspended so applying an example cannot publish records.

The schema, credential resolver, pure publication planner and durable ownership
ledger are implemented. Provider execution, election and API/dashboard controls
are subsequent parts of the full [v0.2 implementation](../v0.2/README.md).

```mermaid
flowchart LR
    Object["Integration: provider settings and references"] --> Resolve
    Host["Host: permitted paths, environment names, secret namespace"] --> Resolve
    Resolve["Bounded credential resolver"] --> Provider["Background provider lifecycle"]
    Resolve --> Fingerprint["Keyed fingerprint for rotation detection"]
    Provider --> Inventory["Immutable client inventory"]
    Provider --> Desired["Owned desired DNS records"]
    Inventory --> Query["DNS query snapshots"]
    Desired --> Query
```

`internal/credentials` accepts exactly one Secret, file, or environment
reference. File and environment references require explicit host allowlists.
Secret references cannot choose a namespace: the operator supplies it to the
resolver. These capabilities are captured immutably and replaced on host reload.
The resolver bounds credential sizes, returns private copies, and removes
provider error bodies before reporting lookup failures. A keyed HMAC fingerprint
detects changes without publishing a plain hash of the credential. Secret values
belong only to the provider instance; they never enter object spec, status,
query logs, or audit details.

The host and providers must remain outside the DNS query path. Queries consume
the inventory and zone snapshots already published in memory, including during
a provider outage. Source identity includes the Integration UID and relevant
spec identity, so deleting and recreating a provider cannot adopt stale client
claims or another instance's external records.

## Publication plans and receipts

`ee/integration/plan` selects original admitted record objects, including
public-only endpoints withheld from internal DNS. Global selection and zone
publication rules share type/domain/exclusion limits; endpoint labels override
record-set labels. Disabled or expired endpoints are omitted. Ambiguous rules
and incomplete source or remote snapshots refuse planning. Preview identity
includes source, configuration, credential fingerprint, remote versions and
private ledger versions, so stale preview cannot silently authorize new work.

One plan covers a complete destination with per-record `sync`, `upsert-only`
or `create-only` policy. The ledger retains the previous policy for removed
sources. Foreign ownership and CNAME conflicts protect the entire affected
owner. Adoption must be explicit. Ownership binds the cluster and Integration
UID; a legacy cluster-only remote marker additionally requires private ledger
evidence for the exact provider IDs.

`ee/integration/ledger` writes an immutable operation intent before reserving
an exact ledger baseline. Partial receipts retain provider IDs and completed
targets; they cannot regress or advance the baseline. Only a confirmed complete
receipt advances it. A current same-UID guard may record recovered historical
facts, but does not authorize replay under stale configuration or credentials.
Abandonment terminalizes the intent before releasing the reservation and requires
fresh authenticated before-state evidence after provider calls have stopped.

```mermaid
flowchart LR
    Inputs["Complete sources, remote records and ownership"] --> Plan["Deterministic destination plan"]
    Plan --> Intent["Persist immutable intent"]
    Intent --> Reserve["CAS exact ledger baseline"]
    Reserve --> Provider["Provider call under current host admission"]
    Provider --> Receipt["Persist partial or complete receipt"]
    Receipt -- "confirmed complete" --> Confirm["CAS last-applied baseline"]
    Receipt -- "uncertain" --> Recover["Inspect remote outcome before retry"]
```

IntegrationLedger, IntegrationOperation, IntegrationBudget and IntegrationDelivery
are bounded private runtime kinds. The shared runtime registry includes them in
both store drivers, full backups and complete replication manifests, including
empty kinds. Safe configuration exports omit them. Real SQLite tests cover
fenced receipt recovery across restart; carrier tests cover native Kubernetes
Secrets, validation, exact creation semantics and safe export. Host-side current
lease/configuration/credential checks, provider execution and multi-node failover
acceptance remain open. External side effects are not atomic with database commits.
