# Runbooks


## Recover a quarantined managed root anchor

Use this only after canonical loss of every managed root anchor, with independently
verified replacement root DS records. A missing/corrupt database, a follower, a live
trusted owner or a key that was already revoked is not a recovery candidate.

1. Stop kudurru and every other process using the database. In an enrolled fleet,
   stop writers and coordinate the recovery on the primary; do not recover each
   follower independently. Preserve a backup and retain managed edge cache bypass.
2. Check that every member runs kudurru v0.2.0 or later (`kudurru version` on
   each host). Every release that can exchange cluster images with v0.2.0 (store
   schema 9 and the same required-feature list) also reads recovered history
   (recovery format 2), so there is no separate format to check. The only
   earlier release, v0.1.0, has no cluster replication and cannot be a member.
   Do not roll back the database or the binaries to evade revocation or
   recovery history.
3. Read the exact retained accepted-envelope digest with local OS permissions:

   ```sh
   kudurru anchors status --offline --database /var/lib/kudurru/state.db
   ```

   Besides `version`, `quarantined` and `recoveries`, the output lists the
   owner's keys (`owner.keys`: DS key tag, RFC 5011 status, first seen,
   revocation time), the recovery record generations, this member's local
   denials (`local.denied`) and its authority mode (`authority.mode`). Recover
   only when `quarantined` is true and `authority.mode` is `primary` or
   `standalone`: `recover` needs the database's own writer term and refuses a
   follower's database before any acquisition, changing nothing.

4. Supply that `version.digest`, an audit reason, independently verified DS records
   and numeric authoritative root endpoints. Each `--ds` is one complete DNS
   presentation record; repeat the flag if needed. For example, replacing the
   placeholders with verified values:

   ```sh
   kudurru anchors recover --offline --database /var/lib/kudurru/state.db \
     --expected-digest ACCEPTED_ENVELOPE_SHA256 \
     --reason 'Replacement root verified through the operator ceremony' \
     --ds '. 0 IN DS KEYTAG ALGORITHM DIGEST_TYPE VERIFIED_DIGEST' \
     --root-server 198.41.0.4:53
   ```

The command uses one original deadline (at most 30 seconds), captures the actual
writer and accepted owner before TCP acquisition, and samples the first complete
response before parsing. It selects at most three originally supplied endpoints,
with at most four seconds each within the original deadline. An invalid complete
response is terminal. It authenticates the new keys, commits only the exact
proposal and original lifetime, and writes the OS user ID, reason and before/after
versions to the hash-chained audit in that same transaction. An audit failure rolls
back the trust change. It creates no database, upgrades no schema and repairs no
missing history. SQLite may recover the retained WAL normally.

Inspect `outcome` even when the command returns an error: `committed` describes a
real commit, `rolled-back` a confirmed rollback, and `unknown` requires inspection
before any new attempt. `not-attempted` means mutation was never entered. An output
failure can hide a real commit; use `anchors status` rather than replaying an old
expected digest. The command grants no permission to serve by itself.

In an enrolled cluster, restart the primary first. Followers install the
recovered history through their scheduled pulls, without a restart, and answer
again as soon as they hold it. A follower whose own root check denied the lost
key keeps that denial (the recovered history revokes the key too), but its
deny-only root check stopped with the denial for that process: restart the
follower once it serves again to resume checking the replacement key.
`kudurru cluster anchors` on each member shows the owner digest, the recovery
record and `supervisor.state`.

Restart with the **original managed policy and initial configured seeds**. Recovery
records the separate replacement trust without changing that identity. Revoked keys
and local denials remain permanent. Complete publication, native/cache/output
retirement and installed edge admission still apply. A successful recovery does
not credit hold-down time or authorize static fallback. At most 32 recovery records
are retained; exhaustion refuses further recovery instead of pruning evidence.

## A member with managed root anchors stays fenced or answers SERVFAIL

On an enrolled SQLite cluster with `upstreams.recursion.anchorMode: managed`, a
member that returns after a promotion or a conflict resolution normally takes
the current primary's trust-anchor state on its next pull. When it cannot, it
refuses the primary's data and stays fenced, and it never trusts a revoked root
key again or falls back to the shipped anchors to get out of that state.
[A member returns with different trust-anchor state](sqlite-join.md#a-member-returns-with-different-trust-anchor-state)
says how to recognize such a member with the offline commands, what ends each
case (the primary's next root-anchor check, managed trust anchors started on the
primary from the same seeds, a restart, or a replacement receiver), and how to
replace a member under a new node name and internal address.

## Add a follower behind managed edges

A SQLite node started with `upstreams.recursion.managedTopology: edges` must
already hold its first cluster image. The offline join is the only way to
install it; there is no startup lane that waits for one (D370). Use this order:

1. Add the new backend to every edge's static `--backends` list (every edge
   lists every kudurru node). Each edge child restarts with its retained
   `managedCacheBypass` identity, so both pools stay uncached.
2. Join the receiver offline, as described in
   [Join a new SQLite receiver](sqlite-join.md). Require `primary.committed: true`
   and a nonempty `installed` generation. If the join was interrupted after the
   primary accepted it, run `kudurru cluster join-reconcile --offline` with the
   original invitation instead; never start the node to "wait" for an image.
   The installed image may have a dormant or a managed anchor owner.
3. Configure the follower's host settings: `managedTopology: edges` and one
   `managedEdges` entry per edge with this node's exact `backendAddress` and
   `backendInternalAddress`, `node.name` matching the backend name, and the
   same internal credentials as the edges.
4. Start the follower. Startup authenticates every declared edge once and
   requires each to list this backend exactly once in its installed membership;
   any missing or mismatched edge refuses startup. The follower then runs the
   deny-only supervisor and serves through the edges.
5. A follower restart repeats the complete fleet observation; nothing from an
   earlier start is reused.

A node with no installed image refuses `direct` and `edges` startup with
`managed root topology: this node has no installed cluster image` and names
both join commands. It contacts no edge and fetches nothing from the root, and
its database stays unchanged. Finish the join (or `join-reconcile`), then start
it again.

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

**RPZ feeds during an outage.** A running pod keeps enforcing its RPZ feeds
from memory. A pod that restarts while the apiserver is unreachable reuses an
unexpired copy of an unsigned feed from `local.db`, but cannot read the stored
TSIG keys, so a signed feed stays unbound and every recursive query from a
client whose policy selects it answers SERVFAIL (EDE "not ready") until the pod
is restarted with the apiserver reachable. Once the snapshot is older than
`maxAge`, every recursive query from a client whose policy selects any RPZ list
answers SERVFAIL as well, not only names under primary zones.

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
offline saved-state admission and cached shared-settings fallback: an
unpinned node that starts while the API server is unreachable cannot read the
shared settings and exits with an error, so it neither answers nor reports
ready until it can reach the API server. Under Kubernetes its pod restarts
(crash loop) for the length of the outage. This differs from the pinned
refusals (a foreign pin, a snapshot without identity, a first boot), where
the process stays up, answers SERVFAIL and reports
unready. Before
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
   so only do it with `persistence.local.enabled`. A rolled pod cannot read
   stored TSIG keys, so clients under a policy with a signed RPZ feed get
   SERVFAIL from it until the apiserver is back and the pod restarts.
4. When the apiserver returns, the watches resync, the snapshot is swapped
   out and `kudurru_store_snapshot_serving` drops to 0. Leader-only work
   (status, serials, list downloads, NOTIFY) resumes on the Lease holder.

**Docker and compose** keep the whole store in `state.db`; there is no
apiserver dependency to recover from.

## Clients show only the gateway

Symptoms: the query log, top clients and per-client chart show a router
address for devices configured to use a DNS VIP directly. Per-device
ClientGroups also apply to that router address.

The DNS listeners take a client's address from the packet's source
address, or from the source in an authenticated PROXYv2 header from dnsdist.
Plain DNS has no HTTP `X-Client-IP` or `X-Forwarded-For` header. The
dashboard's trusted reverse proxies affect API requests, not DNS identity.
Incoming ECS is not a trusted device identity. MAC addresses and hostnames
come from inventory and DHCP data that kudurru looks up by this same address,
so they cannot tell apart devices behind a router that replaces the source
address. An opt-in DoT, DoQ or DoH [client ID](client-identity.md) travels
with the connection and can still select a client group, but only when no
IP, MAC, CIDR, hostname or name-glob matcher matches the router's address
(the first matching tier wins). A catch-all `0.0.0.0/0` or `::/0` CIDR does
not count: it comes after the client ID. Remove the other address matchers
that match the router first. The query log keeps recording the address the query arrived from.

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

## Block an abusive client at the edge

Symptoms: one client address or network floods the edges with queries and
the `--dynblocks` rate rules are not enough, or you want it gone for a fixed
time. An operator edge block drops that prefix's queries at every dnsdist
edge until an expiry you choose (one minute to seven days). It needs the
`edgeblocks.write` permission (operator or admin role) and edges that poll
the backends' internal listener (the shipped chart, Kustomize and Compose
files mount the encryption key the poll needs).

1. Block it, with a note for the audit trail:

   ```sh
   kudurru edge block 192.0.2.7 30m --reason "query flood"
   kudurru edge block 2001:db8:1::/48 --until 2026-11-10T18:00:00Z
   ```

   IPv4 prefixes must be /16 or longer and IPv6 prefixes /32 or longer. On
   an enrolled SQLite cluster, run it against the primary; a follower
   answers `primary_required`.
2. Within a few seconds every edge drops the client's queries (no answer at
   all). Each edge's `:8084/status` shows `edgeBlocks.active` and
   `edgeBlocks.installed`; `kudurru_edge_blocks` counts the installed
   entries. An `edgeBlocks.error` means a console update was not
   acknowledged; the edge resends the set and keeps serving meanwhile.
3. List and lift blocks early:

   ```sh
   kudurru edge blocks -o wide
   kudurru edge unblock <id>
   ```

The block also holds while an edge restarts and when no backend is
reachable: each edge keeps the last set in its configuration directory and
dnsdist expires every entry on its own. A revoke clears dnsdist's dynamic
block table and reinstalls the remaining blocks, so clients that the rate
rules had blocked are let through until they exceed the rate again.

Each edge remembers the newest revision of the block set it applied, so a
follower that lags behind cannot undo a block or revoke while the primary
misses a poll. After restoring an older backup (or promoting a follower that
had lagged), the edges keep the newer set for five minutes and then take the
restored one; to apply it at once, restart the edges after deleting
`edge-blocks.json` in their configuration directory.

## The primary of an enrolled SQLite cluster is lost

Symptoms: the API and the dashboard of the primary do not answer. The
followers answer DNS as before, but a change through a follower and a
password sign-in on a follower answer 503.

DNS is not affected: every follower keeps answering from the last state it
installed, also after a restart. Nothing is promoted automatically.

1. Decide whether the primary comes back soon. If it does, wait: the
   followers catch up by themselves when it returns.
2. Otherwise make sure that the old primary is stopped or isolated, choose
   one follower and promote it with a CLI context that uses a token:
   [Promote a follower](sqlite-cluster.md#5-promote-a-follower).
3. Point the dashboard, the CLI, your automation and RFC 2136 update clients
   at the new primary.
4. When the old primary is repaired, start it with its own data directory.
   It becomes a follower by itself
   ([When the old primary returns](sqlite-cluster.md#6-when-the-old-primary-returns)).

If no follower remains and the only surviving members are fenced (each
`kudurru cluster resolution --offline` prints `"reason":"fenced_behind_term"`),
follow
[Keep a fenced member's data when its primary is lost for good](sqlite-join.md#keep-a-fenced-members-data-when-its-primary-is-lost-for-good)
on one of them only. The others follow it by themselves; replace any that
still refuses its data.

If two followers were promoted, see
[Recover from conflicting promotions](sqlite-join.md#recover-from-conflicting-promotions).

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

**Kubernetes, from v0.1 to v0.2:** v0.2 keeps ServiceAccount, RoleBinding and
IdentityProvider configuration in public objects, and an offline migration
moves them there. This one upgrade is therefore not a rolling upgrade. Stop
every kudurru process, do step 1 of the Helm steps below (the new CRDs), run
the migration of [Kubernetes IAM upgrade to v0.2](iam-upgrade.md), and only
then continue with step 2.

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

Rollback: after `helm rollback kudurru`, delete every persisted `local.db`
(emptyDir: delete the pods; PVC: clear the file) and let the pods resync from
the apiserver. Every start of a release stamps `local.db` with its own schema
version (9 for 0.2.0) whether or not the layout changed, and a release refuses
a `local.db` stamped by a newer one, so the older pods do not start on the
file. A rollback from 0.2 to 0.1 also needs the identity and audit state of
before the upgrade: restore the private IAM Secrets and the AuditLog state
from the backup taken for the upgrade (see [Upgrading from 0.1.x](#upgrading-from-01x)).

**Compose (single):** stop, back up the volume
([docker.md](docker.md#persistence-and-backups)), `docker compose pull`,
`docker compose up -d`, `docker compose exec kudurru kudurru health --ready`.

**Compose HA:** one host at a time, waiting for `kudurru health --ready` and
for the VIP to settle (`kudurru health --vrrp`) before the next host. The
VIP holder last.

**Packages:** `apt upgrade kudurru` / `dnf upgrade kudurru` / `apk upgrade
kudurru` replace the binary and keep `/etc/kudurru`; the post-install script
restarts the service only if it was running.

### Upgrading from 0.1.x

0.1.0 is the only release 0.2.0 upgrades from. Work through this list in
order before the first 0.2 process starts; the steps that do not apply to an
installation say so.

1. **Back up, because there is no way back.** The first 0.2 start migrates
   `state.db` from schema 1 to 9, `querylog.db` from 2 to 3 and stamps
   `local.db` with 9. 0.1.0 then refuses every one of these files
   (`state.db was written by a newer kudurru`) and changes nothing, so a
   return to 0.1.0 is a restore of the backup: stop kudurru and copy the data
   directory (Docker volume, PVC or `/var/lib/kudurru`). On Kubernetes also
   back up the release namespace's private Secrets and the `kudurru.io`
   objects ([iam-upgrade.md](iam-upgrade.md#prepare-an-offline-upgrade)).
2. **SQLite (Docker, systemd, the chart's `standalone` mode): stored objects
   0.2 refuses stop the upgrade.** Before it migrates `state.db`, 0.2 checks
   every stored object and, if one fails, exits naming it and leaves the file
   as 0.1.0 wrote it. Correct the objects with 0.1.0 and start 0.2 again
   ([Upgrade refused](#upgrade-refused-stored-objects-are-not-valid-in-this-release)).
   On Kubernetes such an object stays stored and does not stop serving; a
   change to it is refused until it is corrected.
3. **Kubernetes: upgrade `kudurru-crds` first.** It replaces the nine 0.1.0
   CRDs in place (still `v1alpha1`, no conversion) and adds five: `Kudurru`,
   `IdentityProvider`, `Integration`, `RoleBinding` and `ServiceAccount`.
4. **Kubernetes with private IAM Secrets: migrate offline.** If 0.1.0 created
   ServiceAccounts, RoleBindings or identity providers (the first
   `break-glass` account counts), stop every kudurru process, run
   `kudurru iam migrate status|plan|begin --offline` as
   [iam-upgrade.md](iam-upgrade.md) describes and wait for `complete` before
   any 0.2 pod starts. A 0.1.0 token issued in the same second as its account
   stays unbound and is refused after the migration; this almost always
   includes the first break-glass token, so mint a replacement with
   `kudurru admin create-token`.
5. **Kubernetes with `audit.store: auditlog`: provision the audit key ring
   before shipping resumes.** Import the 0.1.0 encryption keys as legacy HMAC
   masters with a cutoff after the last 0.1.0 batch and publish the
   `kudurru-audit-keys` Secret
   ([AuditLog key provisioning](#auditlog-key-provisioning-and-upgrades)).
   Without it audit shipping stops; DNS does not.
6. **Kubernetes: pin `store.kube.expectedClusterID` while the apiserver is
   reachable.** A pod that starts during an apiserver outage serves its
   `local.db` only when the pin matches; without the pin it does not start
   until the apiserver answers ([Cold start without the apiserver](#cold-start-without-the-apiserver)).
7. **Kubernetes: give ServiceAccount clients a rotating projected token.**
   0.2 accepts only tokens with the audience `kudurru` and a lifetime of at
   most 600 seconds; legacy Secret tokens and hour-long tokens are refused
   ([kubernetes-service-account-auth.md](kubernetes-service-account-auth.md)).
8. **A central PostgreSQL database moves to schema 6, which 0.1 nodes cannot
   use.** The first 0.2 node migrates it (compat version 6). A 0.1.0 node that
   connects afterwards reports `CentralIncompatible`, keeps answering DNS and
   keeps its query log locally; once its binary is replaced, it ships the
   backlog ([postgres.md](postgres.md#upgrades-and-mixed-versions)). Upgrade
   every node that shares the database in the same window.
9. **Compose: take the new files.** The shipped files now run
   `command: [serve]`; the HA files add `cluster.role: static` with
   `cluster.peers`, which refuses API writes of objects and cluster settings,
   so `objects.d/` and `kudurru.yaml` must be the same on every host
   ([compose-ha.md](compose-ha.md)). Upgrade one host at a time: a 0.2 twin
   lists a 0.1.0 twin on **Nodes** with its version and readiness, and
   reports its health detail as `peer health: unsupported` until that host is
   upgraded.
10. **SQLite enrolled clusters are new in 0.2.** A 0.1.0 binary can never be
    a member, and every member must run the same release
    ([sqlite-join.md](sqlite-join.md#versions-of-the-members)).
11. **Block lists stored by 0.1 keep `refreshInterval: 24h`.** 0.1.0 wrote
    the CRD default into every BlockList; 0.2 has no default, and a list
    without the field follows `blocking.refreshInterval`. Remove the field
    from the lists that should follow the setting.
12. **Optional, offline: separate SQLite storage.** Moving `state.db` and
    the other databases to their own volumes is a separate offline step
    ([sqlite-storage.md](sqlite-storage.md)); the upgrade does not need it.

Managed root trust anchors (RFC 5011) are new in 0.2 and stay off after the
upgrade: the first 0.2 start records a dormant anchor owner, static anchors
keep working as before, and `kudurru anchors status --offline` reads the
directory. Enable them only after every member runs 0.2, in two steps:
declare the ingress in the node file (`upstreams.recursion.managedTopology`,
`direct` or `edges`, which needs a restart), then write the cluster settings
`upstreams.recursion.anchorMode: managed`, `dnssec: true` and the DS seeds in
`trustAnchors` with `kudurru settings set` or the Settings page. On SQLite the
first 0.2 start copies the cluster settings of `kudurru.yaml` into the
cluster settings object, and later edits of those keys in the file do not
change it ([configuration-and-store.md](../architecture/configuration-and-store.md#cluster-settings-object-foundation)).
The node then fetches the root keys once and keeps them across restarts.
A declared edge that still runs 0.1 has no observation route, so a start with
`edges` is refused with `managed edge observation is unsupported` until that
edge is upgraded.

**Static twins (Compose HA) cannot use that settings write.** A twin with
`cluster.role: static` refuses settings writes (`503 read_only`), and after
its first 0.2 start it keeps the cluster settings that start copied from
`kudurru.yaml`: a later edit of a cluster key in the file is not applied.
The start then logs the warning `cluster settings in kudurru.yaml are not in
effect` for the differing keys that reload without a restart, with the
stored values now in effect (list and object values are left out; read them
with `kudurru settings get` or `GET /api/v1/settings`). Differing keys that
need a restart get a second warning, `cluster settings in kudurru.yaml
differ from the cluster settings object`, which names them and says the
process keeps the values it started with. This is a known gap of 0.2.0,
and it applies to every cluster setting, not only the anchor keys. On
static twins, put the managed anchor settings (`anchorMode: managed`,
`dnssec: true`, `trustAnchors` and, if you use them, `rootHints`) and every
other cluster setting you want into `kudurru.yaml` **before** that host's
first 0.2 start; `managedTopology` is a host key and can also be declared
then. There is no supported way in 0.2.0 to change a cluster setting of a
static twin after that start. Host keys (listeners, `cluster.*`,
`managedTopology`) and the objects in `kudurru.yaml` and `objects.d/` still
follow the file.

## Upgrade refused: stored objects are not valid in this release

Docker, systemd and the chart's `standalone` mode keep their objects in
`state.db`. Before a release migrates that file it checks every stored
object against its own rules. If one fails, kudurru does not start:

```text
error: state.db was written by an earlier kudurru: store/sqlite: state.db (schema 1) cannot be upgraded to schema 9: 2 stored objects are not valid in this release:
  DNSZone lab-secondary: spec: Invalid value: secondary zones require secondary configuration
  Policy default/default: spec.rewrites[0]: Invalid value: provide either a CNAME or targets, not both
state.db was not changed. Start that earlier release on this data directory, correct or delete the listed objects (in the configuration file or the objects directory when they are managed there), stop it and start this release again
```

Nothing was migrated. The data directory is as the earlier release left it,
and that release still serves it:

1. An object that a file manages (`objectsDir`, or `objects:` in
   `kudurru.yaml`) is corrected in, or removed from, that file first.
2. Start the earlier release on the same data directory (the previous image
   tag or package version). DNS answers as before, and the files are applied
   when it starts.
3. Correct or delete every other object the message lists, with the
   dashboard, `kudurru delete <kind> <name>` or `kudurru apply -f`.
4. Stop it and start the new release. If the message appears again with
   other objects or other fields, repeat: some rules are only checked once
   an object is structurally valid.

What 0.1 stored and 0.2 does not accept:

| Object | What to do |
|---|---|
| DNSZone of type `secondary` or `stub` without `spec.secondary.primaries` or `spec.stub.nameservers` | 0.1 did not serve these types. Add the configuration or delete the zone. |
| DNSZone of type `external`, `catalog` or `secondaryCatalog` | The SQLite store does not serve these types (`external` zones are served on Kubernetes only, with `externalZones.enabled`). Delete the zone. |
| Two or more DNSZones with one `spec.zoneName` | Keep one and delete the others. 0.1 kept serving the zone as it was before the second object appeared, and later changes to either object had no effect. |
| DNSZone with an entry in `spec.policy.queryACL` or `transferACL` that is not an address or a network, or an entry in `notifyTargets` that is not an address | 0.1 reported it on the zone and did not serve the zone. Correct the entry. |
| Primary DNSZone with `spec.policy.update` other than `deny` or `allow` | Updates were not implemented in 0.1, which stored any access mode. Set `deny`, or `allow` with `updatePolicies`. |
| Primary DNSZone with a `spec.policy.updatePolicies` entry whose `name` is outside the zone (or has a `*` other than a leading `*.`), whose `types` name a type that does not exist or that an update cannot carry, or whose `tsigKeyRef` is empty | Updates were not implemented in 0.1, which stored any grant; this release compiles every entry, also of a zone that denies updates. Correct or remove the entry. |
| Policy with `spec.rewrites` entries that have neither `targets` nor `cname`, have both, or have a `type` that is not one of the listed record types in upper case | 0.1 stored rewrites and did not apply them. Correct or remove the entries. |

A secondary or stub zone that 0.1 stored with its configuration is valid: it
was not served by 0.1 and is served by 0.2, which starts transferring it
after the upgrade. Remove it first if that is not wanted.

After the upgrade the same rules apply to every change: the API answers
`400 invalid` and names the field, and the object is not stored. A zone
created under a new object name for the zone name of a zone deleted on this
node is also refused, naming the deleted object: create it under that name.

Two things 0.1 stored are not refused and are served as 0.1 served them:
whatever the zone cannot hold of a record set (an endpoint that does not
parse, such as an `A` target that is not an address, two endpoints for one
name and type, a CNAME next to other data, a CNAME or NS endpoint at the
apex) is left out and reported on its record set, and a block list whose URL
has never downloaded is served empty and reported as failing until the URL
answers. A block list whose URL accepts the connection and does not answer
is also served empty while the download runs, and so is a block list whose
status names a published body that this node's `lists` directory does not
hold, until the body is downloaded again. While it waits the list does not
block on that node; the node logs `listfetcher: list source has a published
digest but this node holds no body for it` once for the source, and
`awaited body stored` when the list blocks again.

An endpoint of a record set written at a name that holds a record a dynamic
update added is not served; the other endpoints of the record set are. The
record set's own conditions name each such endpoint as `Duplicated`
(`Degraded` while other endpoints are served, not `Ready` when none is), and
the zone's source diagnostics report a record set with nothing served as
`duplicated`. The dynamic record keeps answering and the rest of the zone
keeps taking changes. Delete the dynamic record (or the endpoint) to resolve
it.

A primary zone that has a child zone (for example `sub.example.test.` under
`example.test.`) can answer `SERVFAIL` and keep readiness at 503 for up to
about a minute on the first start after the upgrade, while the parent and
child commit their first generation under this release. It then serves as
before. Plan the upgrade of such a node for a quiet moment, or keep a
second resolver in the clients' list.

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

## Inspect the NOTIFY source address

On a SQLite or Kubernetes serving node with a matching local TSIG key file, run:

```sh
kudurru doctor --notify --notify-key secondary-key --notify-target dns.example:53
```

Use the intended reachable UDP listener and run with the node's normal config.
The command reports the listener-observed address only after verifying its
signed reply. It binds `notify.source` when configured and makes one bounded
exchange. It does not send zone refreshes or change records. A missing key,
source-bind failure, unavailable writer or stale serving source fails closed;
check readiness and routing before retrying the diagnostic.

The default wildcard listener resolves to loopback and produces a warning:
that only proves local connectivity. A hairpin route may have different NAT
from the route to your secondary; confirm its logs before changing allowlists.
On Kubernetes, the listener uses its complete startup TSIG inventory, including
stored keys. Supply a matching local key file to the doctor command; it does
not export stored secrets. A successful diagnostic confirms the authenticated
packet path, not Kubernetes leadership or zone readiness. Pending or refused
startup key inventory fails even when a local file key would otherwise match.

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

## External zone stays empty or stale

Symptoms: a DNSZone of type `external` answers NXDOMAIN for names its
Ingresses, Gateway API routes or LoadBalancer Services publish, or keeps
answering an old address.

1. `kubectl get dnszone <name> -o yaml`: `Degraded` with reason
   `SourcesNotRead` means the server runs without `externalZones.enabled`, so
   nothing reads the sources. Set the Helm value `externalZones.enabled: true`
   (it also grants the read RBAC) and upgrade. A zone that is Degraded for a
   more specific reason (for example `Duplicated`) keeps that reason, and its
   message adds that no sources are read. On the SQLite store the message
   says that this store does not read Kubernetes sources: the setting cannot
   be turned on there, and the zone keeps only record sets derived earlier.
2. The elected replica's log warns `external zone reconciliation incomplete`
   with the refused kind. `forbidden` means the ClusterRole lacks `list` and
   `watch` on that kind (with the kustomize bases, add the rules from
   [the Helm guide](helm.md#external-zones-externalzonesenabled)).
   `object too large` means the kind has more than 4,096 objects or 32 MiB in the
   cluster (every Service counts, whatever its type). A single list page of
   64 objects over 4 MiB is cut off at 4 MiB instead, and the warning then
   names a decode error of the list answer (for example `unexpected EOF`).
   Either way that kind is skipped on every pass: its record sets are kept
   but no longer follow their sources. Reduce the number of
   objects of that kind or disable the kind in `spec.external` of every zone.
3. A warning `external zone record set name held by another writer` means a
   record set with the derived name exists and belongs to someone else; it is
   never taken over. Delete it if it is not needed.

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
