### Added

- Kubernetes store: background Cloudflare publication now runs on Kubernetes. Every replica runs the integration host, one Lease per Integration elects the replica that publishes, and that replica rechecks its Lease and the Integration by live reads before every Cloudflare request and every ledger write. Ledger, operation and request-budget state is kept in private Secrets in the release namespace, each written by one exact compare-and-swap. Only Integrations in kudurru's release namespace run; one in any other namespace reports `Ready=False` with reason `NamespaceNotAllowed`, because the credential allowlists are node-wide.
- Kubernetes store: the building blocks for inbound integration sources on Kubernetes. Immutable source chunks are staged first and one source head per Integration is committed by a single compare-and-swap. Lease terms can be checked against the API server before an effect.

### Changed

- Kubernetes store: UniFi and webhook Integrations, and Integrations with `deletionPolicy: Delete`, now report `Ready=False` with reason `UnsupportedOnKubernetes` or `DeletionPolicyUnsupported` on the object. Before, they were silently inert. No provider is contacted for them. If writing that status fails, it is written again within one poll interval.
- Kubernetes store: Cloudflare publication pauses while kudurru reports a published record set as `Duplicated`, `Occluded` or `Invalid` for its current generation, as it already does on SQLite. Status updates on zones and record sets no longer interrupt a publication in flight.
- Kubernetes store: a damaged private integration Secret (ledger, operation, budget, delivery, event cursor or source carrier) is now an error when it is read or listed. It is no longer skipped as if it were absent.
- The `/api/v1/integrations` administration routes still answer unavailable on Kubernetes. Native Kubernetes Secret credential references stay disabled on both stores; mount the Secret as a file and allowlist its path.
