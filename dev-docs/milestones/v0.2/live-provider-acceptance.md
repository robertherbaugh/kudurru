# Live UniFi and Cloudflare acceptance (issue #153)

**Status: blocked, not run.** The synthetic TLS provider tests and local
recovery results do not qualify a live controller, Network application, or
Cloudflare account. Do not turn this checklist into a scheduled or hosted CI
job. Only the owner or an explicitly authorized operator can run it.

## Prerequisites not supplied

- Explicit owner authorization naming the operator, disposable resources,
  allowed writes and fault, test window, and cleanup responsibility. No
  production, shared, or homelab-wide changes are authorized by this checklist.
- A dedicated disposable UniFi controller and site, with a disposable DHCP
  network and test station. The owner provides controller and Network
  application versions and documents the test API key and local-user
  permission names, not their values.
- A dedicated disposable Cloudflare account and zone, with a test token
  restricted to that zone. Record its permission names and resource scope,
  not its value. Confirm the test operator can verify cleanup independently.
- An approved fault-restoration plan that affects only the disposable
  fixtures. Do not revoke real credentials. If there is no approved way to
  cause a partial effect, leave the corresponding check pending.
- A tested kudurru revision, isolated SQLite instance, approved synthetic
  DNS records, and a way to record sanitized responses, affected IDs, and
  final absence. Keep credentials out of commands recorded as evidence,
  logs, screenshots, receipts, issues, and pull requests.

## Owner acceptance checks

- [ ] UniFi mirror: preview, apply the reviewed sync, repair drift, and
  delete. Keep a foreign policy as a control and verify it is unchanged.
- [ ] UniFi recovery: cause a partial update or delete only in the approved
  test scope. Verify the intent stays pending while provider state is mixed,
  then settle it by the [documented operator recovery](../../../docs/deploy/integrations.md#unifi-optional-writes).
  Record whether polling confirms or abandons the intent, and show that it
  did not replay the write.
- [ ] UniFi DHCP: preview and apply the DNS server override on the disposable
  network. Verify the controller accepts the PUT without `id` or `metadata`.
- [ ] UniFi station: block and unblock the disposable station; verify its
  final state is unblocked.
- [ ] Cloudflare: publish to the disposable zone, verify recovery after an
  approved partial failure, and demonstrate `deletionPolicy: Delete` cleanup.
- [ ] Record the tested versions, exact permission names and scopes, date,
  revision, and result of each check. Commit sanitized cleanup receipts under
  `dev-docs/milestones/v0.2/evidence/`: IDs created, IDs removed, and the final
  verification for both providers. Check the receipts contain no credentials.
- [ ] Update the UniFi optional writes entry in
  [`dev-docs/followups.md`](../../followups.md) only once the receipts are
  reviewed. Mark any skipped or failed check as pending.
