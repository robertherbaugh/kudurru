# Runbooks

## Cold start without the apiserver

Kubernetes clusters that resolve through kudurru need DNS before (and
while) the control plane is reachable (dev-docs/design-record.md §4.4, D79-D80,
D91).

**How it works.** Every pod writes its materialized object set to
`/var/lib/kudurru/local.db` after the first full sync, then at most every
30 s when it changes and every 5 min while unchanged. Startup store reads
have deadlines bounded by `store.kube.startupTimeout` (15 s by default);
apiserver availability failures defer bootstrap and identity initialization.
After those bounded reads, the zone cache waits the startup timeout, loads
`local.db`, checks its identity against `store.kube.expectedClusterID`, reports ready for a fresh matching snapshot and serves cached and
forwarded answers and
primary zones from the snapshot (`kudurru_store_snapshot_serving 1`). Once
the snapshot is older than `store.kube.snapshot.maxAge` (24 h), names under
primary zones answer SERVFAIL (never NXDOMAIN) while forwarding continues.
A pod that never synced and has no snapshot answers SERVFAIL.

**Provision the independent cluster identity before an outage.** In a trusted
Kubernetes context, read the nonsecret UID of the `kube-system` namespace:

```sh
kubectl --context YOUR_TRUSTED_CONTEXT get namespace kube-system -o jsonpath='{.metadata.uid}'
```

Put that exact UID in the host-owned node configuration on every replica. With
Helm this is under `config.node`; with a directly managed configuration it is
`store.kube.expectedClusterID` (or
`KUDURRU_STORE__KUBE__EXPECTEDCLUSTERID`). For example, the node document is:

```yaml
store:
  kube:
    expectedClusterID: "REPLACE_WITH_TRUSTED_KUBE_SYSTEM_UID"
```

The value is an identity, not a secret. Distribute it through independently
managed host/Helm configuration, never through runtime settings, a copied
`local.db`, or a cached UI-settings document. Changing it requires restart.
A configured pin also checks authenticated online requests before permitting
ordinary Kubernetes reads, writes, discovery or lease traffic. A wrong online
cluster is refused; a missing/invalid identity response is not an availability
fallback. The bounded probe honors the configured Kubernetes credentials,
refuses redirects and reads at most 1 MiB. This is initial verification: the
verified identity is retained for the process lifetime, without periodic
identity probes. Restart after
retargeting the API endpoint or changing the intended cluster; an endpoint
that changes clusters while keeping valid transport trust is not continuously
reverified by this guard.

**Upgrade compatibility.** An empty pin preserves online startup, but disables
offline saved-state admission and cached shared-settings fallback. Before
relying on outage recovery, provision the pin while the API is reachable and
allow a complete live sync to refresh both saved objects and shared settings.
Old object snapshots without a cluster ID are refused during an outage even
with the correct pin. Old unpinned settings caches are not reused after pinning.
Snapshots that already contain the matching cluster ID remain compatible;
no SQLite format migration is required. Never stamp an old snapshot with a new
identity or copy the identity out of saved state to make it pass. A foreign or
identity-free snapshot contributes no policy namespace or serving objects;
readiness stays unavailable until a verified live sync supplies current data.

**Prepare (Helm):**

```yaml
priorityClassName: system-cluster-critical   # scheduled before other workloads
persistence:
  local:
    enabled: true                             # local.db on a PVC per replica (StatefulSet)
dnsPolicy: Default                            # kudurru never resolves through itself
```

Without `persistence.local`, `local.db` is on an `emptyDir`: it survives
container restarts and node reboots as long as the pod stays on its node,
but not rescheduling.

**During an outage:**

1. `kubectl` may be down too; probe the pods directly:
   `curl -s http://<pod-ip>:8081/healthz | jq .components.zonecache` and the
   metrics `kudurru_store_snapshot_serving` and
   `kudurru_store_snapshot_age_seconds` on `:9153/metrics`
   (alerts `KudurruServingFromSnapshot` after 15 min, `KudurruSnapshotStale`
   at 12 h).
2. Do not delete kudurru pods: a rescheduled pod without a PVC has no
   snapshot and answers SERVFAIL until the apiserver is back.
3. If the outage will exceed `maxAge`, raise it in `config.node` before it
   expires (`store: {kube: {snapshot: {maxAge: 72h}}}`); this rolls the pods,
   so only do it with `persistence.local.enabled`.
4. When the apiserver returns, the watches resync, the snapshot is swapped
   out and `kudurru_store_snapshot_serving` drops to 0. Leader-only work
   (status, serials, list downloads, NOTIFY) resumes on the Lease holder.

**Docker and compose** keep the whole store in `state.db`; there is no
apiserver dependency to recover from.

## Clients show only the gateway

Symptoms: the query log, top clients and per-client chart show a router
address for devices configured to use a DNS VIP directly. Per-device
ClientGroups also apply to that router address.

The version 0.1 listeners identify DNS clients by the packet's source
address, or by the source in an authenticated PROXYv2 header from dnsdist.
Plain DNS has no HTTP `X-Client-IP` or `X-Forwarded-For` header. The
dashboard's trusted reverse proxies affect API requests, not DNS identity.
Incoming ECS is not a trusted device identity. DoT/DoH client IDs and
MAC/hostname discovery are not populated by these listeners.

1. On two devices with different addresses, send different names to each
   published VIP over both transports, for example:

   ```sh
   dig @<dns-vip> device-a.identity-check.invalid A
   dig +tcp @<dns-vip> device-a.identity-check.invalid A
   ```

   Use `device-b` on the second device. Find the names in the query log
   with **All nodes** selected. Compare the recorded client addresses;
   then compare the corresponding node scopes and Clients inventory.
2. If the query log already has the gateway address, capture traffic on
   the node's external interface before it enters Kubernetes. If queries
   already arrive from the gateway, check its DNS proxy, DNS interception
   and source NAT rules. Preserve the device source for traffic to the
   DNS VIP and provide a return route to the device network. Changing
   kudurru's trusted-proxy CIDRs cannot recover an address already lost
   upstream.
3. If ingress has the device address but the log has a node or pod address,
   inspect the DNS Service and its endpoints:

   ```sh
   kubectl -n <namespace> get service <dns-service> -o yaml
   kubectl -n <namespace> get endpointslices \
     -l kubernetes.io/service-name=<dns-service> -o wide
   ```

   Use `service.dns.externalTrafficPolicy: Local` for LoadBalancer and
   NodePort Services (the chart default). With `edge.enabled`, the
   published Service must select the edge pods and target dnsdist's DNS
   ports. Extra Services created outside the chart need the same selectors
   and traffic policy; selecting kudurru's backend pods bypasses dnsdist.
4. If the address survives to dnsdist, check that backends use PROXYv2,
   `listeners.proxyProtocolFrom` permits the actual edge source, and both
   sides use the same TLV 0xE0 secret. Rejected headers drop the query;
   they do not silently fall back to the edge address. Check
   `kudurru_dns_dropped_total{reason=~"proxy_.*"}`.
5. Repeat the two-device UDP/TCP test after the network change. Apply an
   exact-IP ClientGroup to one device and confirm the other keeps its own
   policy. Old rollups still include the gateway's historical queries;
   use a time window beginning after the change.

## Rotating the encryption key

The cluster encryption key (`KUDURRU_ENCRYPTION_KEY_FILE`) seals sessions
and stored secrets and derives the internal listener's token and CA (D47,
D92). Rotation keeps the old key as the previous key until nothing depends
on it: kudurru decrypts with either key and re-encrypts stored secrets on
start, sessions sealed under the old key stay valid until their lifetime
(12 h by default), and during rotation nodes issue internal certificates
from the previous CA and clients retry the previous token, so old and new
pods interoperate.

AuditLog signing now uses independent operator-provisioned keys. Complete the
[AuditLog upgrade procedure](#auditlog-key-provisioning-and-upgrades) before
releasing master keys still needed to import historical HMAC verification
material. After that explicit cutover, master-key retention follows sessions,
stored secrets and internal transport compatibility; audit-key retention follows
retained audit history independently. SQLite and central PostgreSQL source
chains do not use Kubernetes batch signatures.

**Helm:**

```sh
NS=kudurru-system
# 1. keep the current key as the previous key
kubectl -n $NS get secret kudurru-encryption-key -o jsonpath='{.data.key}' | base64 -d > old.key
kubectl -n $NS create secret generic kudurru-encryption-key-previous --from-file=key=old.key
# 2. write a new key into the chart's Secret (the chart keeps it on upgrade)
kubectl -n $NS create secret generic kudurru-encryption-key \
  --from-literal=key="$(openssl rand -base64 32)" --dry-run=client -o yaml | kubectl apply -f -
# 3. roll the pods with both keys
helm upgrade kudurru oci://ghcr.io/robertherbaugh/charts/kudurru -n $NS --reuse-values \
  --set auth.encryptionKeyPreviousSecret=kudurru-encryption-key-previous
kubectl -n $NS rollout status deployment/kudurru
# 4. after old sessions expire and stored secrets/internal transport migrate,
# drop the previous master key (complete any legacy audit import first)
helm upgrade kudurru oci://ghcr.io/robertherbaugh/charts/kudurru -n $NS --reuse-values \
  --set auth.encryptionKeyPreviousSecret=
kubectl -n $NS delete secret kudurru-encryption-key-previous && shred -u old.key
```

Steps 3 and 4 change the pod template, so the pods roll; changing only the
Secret's content does not restart anything.

**Compose:** `mv secrets/encryption_key secrets/encryption_key.previous`,
create a new `secrets/encryption_key`, add the secret
`encryption_key_previous` and
`KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE: /run/secrets/encryption_key_previous`
to the kudurru service, `docker compose up -d`; remove both after the old
sessions expire and stored secrets/internal transport have migrated. Complete
any legacy AuditLog key import first. In
the HA shape do every step on all three hosts before moving to the next.

**Never** replace the key without keeping the previous one: stored secrets
would become unreadable and every node with the old key would reject the
others on :8082.

Related secrets: the PROXYv2 secret (`<release>-proxy-protocol`,
`secrets/proxy_secret`) has no previous-value mechanism; change it on the
edge and kudurru together (in Kubernetes: update the Secret, then restart
the edge and kudurru workloads; queries in between are dropped as
`proxy_unauthenticated`). The admin bootstrap Secret is read only while no
user exists; rotating it changes nothing.

## AuditLog key provisioning and upgrades

**Required for Kubernetes `audit.store: auditlog`: provision independent trust
before shipping resumes.** Missing trust stops cluster AuditLog shipping and
verification, not DNS startup or the durable local spool. Watch spool capacity
and retention during any interruption. SQLite and PostgreSQL audit stores do
not need this Secret. `audit.keySecret` is host-only and restart-required when
changing its name; ring updates at the same name are read on later operations.

For a new installation, create a private operator directory and initialize a
ring with the original `kube-system` namespace UID and the release namespace.
The UID is public identity metadata, not a credential. Commands never contact
the cluster or print private material; the file must remain mode 0600.

```sh
umask 077
mkdir audit-trust
cd audit-trust
NS=kudurru-system
CLUSTER_ID=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')
kudurru audit keys init --file audit-ring.json --cluster-id "$CLUSTER_ID" --namespace "$NS"
kubectl -n "$NS" create secret generic kudurru-audit-keys --from-file=ring.json=audit-ring.json
```

For an **existing installation with HMAC-sealed batches**, do this instead of a
fresh initialization:

1. Configure `audit.store: local` on every replica and complete the rollout so
   all old shippers have stopped. Keep their persistent spools. Do not allow an
   old binary to resume HMAC shipping after the cutover.
2. Securely obtain protected 0600 files for **every** old master key needed by
   retained batches. Usually these are the old current and previous keys;
   older already-lost keys cannot be reconstructed. Do not print them.
3. Choose an absolute whole-second RFC3339 cutoff after the final old batch was
   created. Kubernetes creation metadata has whole-second precision; fractional
   cutoffs are refused to prevent same-second downgrade admission. Wait until
   the apiserver clock has reached the cutoff before resuming new writers.
   Initialize once with explicit imports, then publish the Secret before
   restarting upgraded replicas with `audit.store: auditlog`:

   ```sh
   kudurru audit keys init --file audit-ring.json --cluster-id "$CLUSTER_ID" --namespace "$NS" \
     --legacy-master-file old-current.key --legacy-master-file old-previous.key \
     --legacy-before 2026-10-30T12:00:00Z
   kubectl -n "$NS" create secret generic kudurru-audit-keys --from-file=ring.json=audit-ring.json
   ```

   Substitute the actual cutoff; the example is not a deployment timestamp.
   Importing derives only the historical purpose keys, not stored session
   masters. Rotation/restart cannot extend the cutoff. Legacy batches must have
   authenticated, nonzero apiserver creation times strictly before it. Unsigned
   history, unknown keys and recreated metadata cannot silently regain trust.
4. Verify the retained trail through `kudurru audit verify` on each replica.
   Resolve missing trust explicitly; do not reseal or replay historical sources.

Rotate with `kudurru audit keys rotate --file audit-ring.json`. It keeps all old
public keys, discards the old private seed, and refuses at 128 retained keys
instead of silently pruning trust. Publish the updated file:

```sh
kubectl -n "$NS" create secret generic kudurru-audit-keys --from-file=ring.json=audit-ring.json \
  --dry-run=client -o yaml | kubectl apply --server-side -f -
```

Private file contents travel only through the pipe to Kubernetes. Protect shell
history, local files, Secret access and separately encrypted backups. Updates
are serialized with a persistent `.lock` file; do not remove that lock while
commands may run. File replacement is atomic and synced, but a storage failure
after rename can leave the replacement committed despite an error: inspect the
protected file before deciding on another operator action.

For compromise response, rotate first, then use
`kudurru audit keys revoke --file audit-ring.json --key-id <old-public-fingerprint>`
and publish. Public fingerprints are the `id` fields in the protected ring;
only those public values may be inspected separately. Active-key revocation is
refused. `revoke --legacy` withdraws trust in **all** imported HMAC keys. Affected
history remains evidence but verification becomes unavailable; an invalid
signature under a known trusted key is an integrity failure. New signatures
never fall back to legacy HMAC. Allow only one coordinated operator publisher
per Secret; file locks cannot serialize operators on different machines.

Back up both the ring Secret/file and AuditLog objects independently of normal
configuration backups and saved-state snapshots, which include neither. Exact
ring restoration in the original trusted cluster/namespace retains trust;
Secret UID/revision changes invalidate verification caches. Keep retained public
keys for as long as any retained or external history requires them. Public keys
alone cannot resume signing, lost seeds cannot be regenerated, and a new cluster
UID requires a separately designed trust migration. There is no automatic
archive import or cluster rebinding. Restoring old legacy objects gives them new
creation metadata and may make them ineligible for the original cutoff. This
procedure does not protect against a fully compromised signing pod, namespace
administrator or trust-root backup.

## Upgrading

kudurru stores a schema version in `state.db` and `local.db`. A binary that
finds a newer on-disk version refuses to start (`state.db was written by a
newer kudurru`); an older version is migrated in one transaction. Downgrades
across a schema change are therefore not supported: back up first.

**Helm:**

1. Read the release notes for CRD changes. Helm never upgrades CRDs; upgrade
   the CRD chart first:
   `helm upgrade kudurru-crds oci://ghcr.io/robertherbaugh/charts/kudurru-crds --version <v>`
   (or `kubectl apply --server-side -f crds.yaml` from the release).
2. `helm upgrade kudurru oci://ghcr.io/robertherbaugh/charts/kudurru --version <v> -n kudurru-system --reuse-values`.
   The Deployment rolls with `maxUnavailable: 0, maxSurge: 1`; each pod
   runs `kudurru lameduck <seconds>s` in preStop: readiness answers 503
   and `health.kudurru.internal.` answers SERVFAIL while ordinary DNS
   keeps serving, so Service endpoints and dnsdist drain the pod before
   it exits. SIGTERM also drains for `node.lameduck` (default 2 s).
   The PDB keeps at least one replica serving; the edge restarts its
   dnsdist once per surge batch.
3. Watch `kubectl -n kudurru-system rollout status` and the
   `KudurruServfailRatio` alert.

Rollback: `helm rollback kudurru` works while the release did not migrate
`local.db` to a newer layout; otherwise delete the pods' `local.db`
(emptyDir: delete the pods; PVC: clear the file) and let them resync from
the apiserver.

**Compose (single):** stop, back up the volume
([docker.md](docker.md#persistence-and-backups)), `docker compose pull`,
`docker compose up -d`, `docker compose exec kudurru kudurru health --ready`.

**Compose HA:** one host at a time, waiting for `kudurru health --ready` and
for the VIP to settle (`kudurru health --vrrp`) before the next host. The
VIP holder last.

**Packages:** `apt upgrade kudurru` / `dnf upgrade kudurru` / `apk upgrade
kudurru` replace the binary and keep `/etc/kudurru`; the post-install script
restarts the service only if it was running.

## Central database unavailable

Symptoms: the `KudurruCentralDatabaseDown` alert, `CentralUnreachable` on the
Diagnosis page, the Audit Log page and **All nodes** answer "unavailable".

DNS is not affected, and nothing is lost while the nodes keep their own
copies: each node writes its audit events and query log locally first and
sends them when the database answers again (`kudurru_central_ship_lag_seconds`
falls back to a few seconds). A node keeps the queries it has not sent past
`querylog.retention.raw`; only `querylog.retention.maxDBBytes` deletes them
earlier, and its audit spool keeps 50 000 events (`KudurruAuditSpoolDropping`
when it overflows).

1. `kudurru doctor` on a node checks the connection, the server version and
   the schema.
2. Fix the database or the network; the nodes reconnect within a minute.
3. Meanwhile **This node** in the scope selector still reads the node's own
   query log.

`CentralIncompatible` means a newer kudurru migrated the database past what
this node can use (during a rolling upgrade): upgrade the node. It keeps its
data locally until then.

## Recursive group gets no answers

Symptoms: every name routed to a recursive group (`mode: recursive`) gets
SERVFAIL with EDE 22 (No Reachable Authority) and status
`timeout_upstream`, the group's page shows the
resolver in backoff, `kudurru_upstream_healthy{upstream="<group>/recursive"}`
is 0 (`KudurruUpstreamUnhealthy`), the log warns "recursive resolver in
backoff: lookups time out and no authoritative server has answered
recently; check that outbound DNS (UDP and TCP port 53) to the internet is
allowed", and `kudurru_recursor_outgoing_queries_total` grows only with
`result="timeout"` or `"error"`. Timeouts for a few names whose own name
servers are dead do not cause backoff while other lookups succeed.

1. From the node, ask a root server directly over UDP and TCP:
   `dig @198.41.0.4 . NS +norec` and the same with `+tcp`. A timeout means
   outbound port 53 is blocked: open UDP and TCP 53 to any address in the
   firewall, the cloud security group or the cluster's egress policy.
2. If the answer does not come from the root server (for example
   `dig @198.41.0.4 id.server CH TXT` names your ISP's resolver), the ISP
   intercepts port 53. Recursion cannot work there; set the group back to
   forwarding, preferably over DoT or DoH.
3. If only IPv6 fails, set `upstreams.recursion.ipv6: off`.

A single name with SERVFAIL and status `dnssec_bogus` is not this case: the
zone's DNSSEC is broken (the EDE text names it), and `dig +cd` shows the data.

## AuditLog CRD missing

Symptom: the log line "the AuditLog CRD is not installed" and each replica's
Audit Log page showing only its own events. The `kudurru-crds` chart is older than
the app: upgrade it (`helm upgrade kudurru-crds …`) and restart the
kudurru pods. The events recorded meanwhile are in each pod's
`audit-spool.db` and are shipped after the restart, unless the pod was
replaced (its emptyDir is gone); the `"log":"audit"` lines keep them all.

## License key expired or invalid

Symptoms: a banner on every dashboard page (**The license key has expired**,
**The license key is invalid**), `kudurru_license_info{state="grace"}`,
`"expired"` or `"invalid"`, a licensed feature that answers
`402 license_required`, the dnsdist or keepalived container that stops at the
start with "needs a kudurru license key" (a first start only) or the warning
"the license key expired ... keeps running because it was licensed". DNS
answering is not affected in any of these cases.

1. `kudurru license status` on the node (or `GET /api/v1/license`) shows the
   state and, for an invalid key, the reason.
2. An **expired** key works for 30 days after its date (state `grace`), then
   the limits for running without a key apply: one node, one policy (the seeded default),
   no licensed feature. Alert on
   `kudurru_license_expires_timestamp_seconds - time()` before that.
3. Put the renewed key where the node reads it (`KUDURRU_LICENSE_KEY`,
   `KUDURRU_LICENSE_KEY_FILE`, `<node.dataDir>/license.key`, the Helm Secret
   `license.existingSecret`). A running node reads it within a minute; no
   restart. A supervisor (`kudurru edge run`, `kudurru vip run`) sits in the data
   path, so an expired key never stops it, not even at a restart: it starts with
   the warning above until you renew. It refuses only a start with no key at all.
4. An **invalid** key is one that cannot be verified (a typo, a cut line, a
   key from another build). `kudurru license verify <key>` says why. Request a
   new one ([kudurru.io/license](https://kudurru.io/license)).
5. A cluster over its node limit (`overLimit` in `GET /api/v1/license`) refuses
   configuration changes through the API with `402`; remove nodes or add a key
   with a higher limit.
