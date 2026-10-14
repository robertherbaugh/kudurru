# Runbooks

## Cold start without the apiserver

Kubernetes clusters that resolve through kudurru need DNS before (and
while) the control plane is reachable (docs/architecture.md §4.4, D79-D80,
D91).

**How it works.** Every pod writes its materialised object set to
`/var/lib/kudurru/local.db` after the first full sync, then at most every
30 s when it changes and every 5 min while unchanged. A pod that starts
while the apiserver is unreachable waits `store.kube.startupTimeout` (15 s),
loads `local.db`, reports ready and serves cached and forwarded answers and
primary zones from the snapshot (`kudurru_store_snapshot_serving 1`). Once
the snapshot is older than `store.kube.snapshot.maxAge` (24 h), names under
primary zones answer SERVFAIL (never NXDOMAIN) while forwarding continues.
A pod that never synced and has no snapshot answers SERVFAIL.

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

## Rotating the encryption key

The cluster encryption key (`KUDURRU_ENCRYPTION_KEY_FILE`) seals sessions
and stored secrets and derives the internal listener's token and CA (D47,
D92). Rotation keeps the old key as the previous key until nothing depends
on it: kudurru decrypts with either key and re-encrypts stored secrets on
start, sessions sealed under the old key stay valid until their lifetime
(12 h by default), and during rotation nodes issue internal certificates
from the previous CA and clients retry the previous token, so old and new
pods interoperate.

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
# 4. after the session lifetime (12 h), drop the previous key
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
to the kudurru service, `docker compose up -d`; remove both after 12 h. In
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
   sleeps `lameduckSeconds` in preStop, then drains (readiness 503) on
   SIGTERM. The PDB keeps at least one replica serving; the edge restarts
   its dnsdist once per surge batch. A preStop `sleep` only covers the
   Service endpoints: dnsdist keeps sending queries until the process stops
   answering `health.kudurru.internal.`. `kudurru lameduck <seconds>s` as
   the preStop command (readiness 503 *and* the health name SERVFAIL, so
   every dnsdist drains the pod within its 2 s check window, D51) closes
   that gap; the chart switches to it in a follow-up (docs/followups.md).
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
Diagnosis page, the Audit tab and **All nodes** answer "unavailable".

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

## AuditLog CRD missing

Symptom: the log line "the AuditLog CRD is not installed" and each replica's
Audit tab showing only its own events. The `kudurru-crds` chart is older than
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
