# Separate SQLite storage

By default, `state.db`, `local.db` and `querylog.db` remain in `node.dataDir`
(`/var/lib/kudurru` in the supplied deployments). Existing installations do
not move. Two optional host-only settings select independent directories:

```yaml
node:
  dataDir: /var/lib/kudurru
  localDir: /var/lib/kudurru-local
  querylogDir: /var/lib/kudurru-querylog
```

`node.localDir` contains `local.db`: last-good Kubernetes snapshots,
transferred zone data and the response-cache dump. `node.querylogDir`
contains the SQLite query log, `querylog.db`. Empty values use `node.dataDir`.
The environment equivalents are `KUDURRU_NODE__LOCALDIR` and
`KUDURRU_NODE__QUERYLOGDIR`. Both settings require restart and cannot be
changed through the API. Other node files, including `state.db`, keys,
settings, DHCP state, the audit spool and list caches, stay in their existing
locations. A non-SQLite query-log sink does not create `querylog.db`.

Use clean absolute directory paths, not filenames or SQLite URIs. Serving
checks the complete layout before opening databases or initializing node
state. Existing directory aliases are resolved; selected directories cannot
occupy another database/sidecar or reserved product entry. Existing database
and sidecar leaves must be regular readable/writable files, without symlinks;
Unix startup also rejects hard links. Existing permissions are preserved,
not repaired. Newly created directories use mode 0750 and databases 0600.
The operator must provide writable, searchable directories to the serving
UID (65532 in the supplied containers). ACL behavior on other platforms still
belongs to the actual file opener. Preflight is bounded filesystem inspection,
not an atomic defense against a host replacing files concurrently with startup.

SQLite WAL requires compatible local or block storage. Do not share database
files between pods or nodes, or use RWX/NFS as a SQLite HA mechanism. Splitting
volumes provides neither a cross-volume transaction nor an atomic backup.

## Helm

Existing `persistence.querylog.enabled` and `persistence.local.enabled`
retain their legacy behavior: in Kubernetes mode either requests the shared
`data` claim, with querylog taking precedence for claim size/class when both
are enabled. Standalone's `data` claim is still controlled only by
`persistence.standalone`.

Opt into independent mounts with `separate: true`:

```yaml
persistence:
  querylog:
    separate: true
    enabled: true
    size: 10Gi
    accessModes: [ReadWriteOnce]
  local:
    separate: true
    enabled: true
    size: 1Gi
    accessModes: [ReadWriteOnce]
```

Each selected component mounts `/var/lib/kudurru-<component>` and sets its
host directory automatically. Select either independently. `enabled: true`
uses an independent per-pod claim template named `querylog` or `local`;
`enabled: false` uses an independent `emptyDir` and loses that component's
files when the pod is replaced. In Kubernetes mode, **any** existing enabled
flag still selects the original shared `data` claim using its original
precedence, size and storage class, even with `separate: true`.
New querylog/local claims are additional; the data mount and remaining
node files keep their existing durability. Standalone's data claim remains
controlled by `persistence.standalone`. With no data claim selected, `data`
remains an `emptyDir` as before. A separate mount refuses a contradictory
`config.node.node.<component>Dir`.

The original data PVC can retain the old querylog/local database copies after
an explicit offline copy into the separate claims. They are not read or
merged after path selection. Keep them until restart acceptance; reclaim
that space later while stopped, after verifying backups and the selected
paths. Enabling separate storage does not remove or repurpose the data PVC.

Kubernetes mode uses a StatefulSet whenever a claim is needed. Multiple
replicas have distinct per-pod claims; none share SQLite files. Standalone
always runs one replica, even when `replicaCount` is greater than one.
Separate claims accept `ReadWriteOnce` or `ReadWriteOncePod`; this does not
validate a storage provider's filesystem or durability guarantees. A
hostNetwork DaemonSet cannot request per-pod claims: use ephemeral separate
mounts or operator-provisioned per-node `extraVolumes`/`extraVolumeMounts`
and matching host directory settings. The existing pod UID/fsGroup remains
65532; the storage provider must honor it or the operator must provision the
permissions offline.

## Compose

Keep the selected base file first. For example, from `deploy/compose/single`:

```sh
docker compose -f docker-compose.yml -f ../storage-querylog.yml \
  -f ../storage-local.yml config --quiet
docker compose -f docker-compose.yml -f ../storage-querylog.yml \
  -f ../storage-local.yml up -d
```

Either override can be omitted. They add distinct project-scoped named
volumes and the corresponding environment setting; the base `kudurru-data`
volume stays mounted. The same override files work with `single-edge` and
each HA node base. Each independent HA node must keep its own volumes;
these options do not create database replication. Always use the same file
selection for subsequent Compose commands. Do not use `down --volumes` when
preserving data.

## Move an existing installation offline

Enabling a new path with an empty volume starts that component with an empty
database. There is no automatic copy, live move, merge, or fallback to the
old location when a selected path fails. Preserve existing cache expirations;
do not modify database contents or timestamps to prolong saved data.

1. Stop **all** processes using the node's SQLite files. For Kubernetes,
   disable automatic recreation while migrating and handle each pod's files
   separately. Confirm shutdown finished; never copy a live WAL database as
   independent files.
2. Back up the entire original node directory and any already-separated
   directories, preserving ownership and modes. A coherent node backup needs
   a common stopped window across all volumes. `kudurru backup` exports
   objects and is not a full database/identity/query-log backup.
3. Provision the destination directories/volumes. Copy the selected
   `local.db` or `querylog.db` **and any remaining matching `-wal`, `-shm` or
   `-journal` sidecars** while stopped, preserving ownership and permissions.
   A clean shutdown normally checkpoints/removes transient sidecars. If
   performing an explicit checkpoint, use a compatible SQLite tool while
   every writer remains stopped; do not delete a WAL to force a clean state.
   Keep fixed filenames and verify the copy before starting. Never hard-link
   databases or place them behind leaf symlinks.
4. Change mounts and host settings together. Helm StatefulSet claim templates
   are immutable: changing the claim layout requires planned StatefulSet
   recreation with old claims retained, destination claims provisioned and
   per-pod offline copying. This is not an ordinary rolling upgrade. Inspect
   rendered claim names/capacities and retain the original PVCs; do not let
   old and replacement pods write the same files simultaneously. Volume
   access modes alone do not enforce this operating procedure.
5. Restart and verify DNS readiness, saved state/cache behavior, query-log
   history and new entries at the chosen locations. Retain the originals and
   backups until acceptance. Roll back only while stopped, restoring matching
   directory contents and configuration; copying an old source over newer
   data discards intervening history. Never combine files from different
   database copies.
