# CHANGELOG.md `[0.2.0]` bullets from QUAL-MIXED-VERSION (D373)

### Installation and upgrade notes

- Upgrading from 0.1.x has its own ordered checklist: "Upgrading from 0.1.x"
  in `docs/deploy/runbooks.md` (backup first, because 0.1.0 refuses every
  database 0.2 has opened; SQLite stops an upgrade on stored objects 0.2
  refuses; `kudurru-crds` first with five new CRDs; the offline Kubernetes
  IAM migration; the AuditLog key ring; `store.kube.expectedClusterID`;
  projected ServiceAccount tokens of at most 600 seconds; central database
  schema 6; the new Compose files; BlockLists stored by 0.1 keep
  `refreshInterval: 24h`; the optional separate SQLite storage).
- During a host-by-host upgrade of static twins, a 0.2 twin lists a 0.1.0
  twin on **Nodes** with its version and readiness and reports its health
  detail as `peer health: unsupported`; both keep answering from their own
  files.
- A 0.1.0 node that connects to a central database a 0.2 node has migrated
  reports `CentralIncompatible`, keeps answering DNS and keeps its query log;
  once upgraded it sends the backlog once.
- Managed root anchors stay off after the upgrade. Enable them with
  `upstreams.recursion.managedTopology` in the node file and the anchor
  settings through `kudurru settings set` or the Settings page.
- On SQLite (Docker, systemd, Compose, the chart's `standalone` mode) the
  first 0.2 start copies the cluster settings of `kudurru.yaml` into the
  node's database, and from then on they live there: later edits of cluster
  keys in the file are not applied (change them with `kudurru settings set`
  or the Settings page). Host keys and objects still follow the files.
- Known gap: static twins (`cluster.role: static`, Compose HA) refuse
  settings writes, so after a twin's first 0.2 start none of its cluster
  settings can be changed, and the start logs `settings applied` for file
  keys it did not apply. Put every cluster setting a twin needs, including
  the managed anchor settings, into `kudurru.yaml` before that host's first
  0.2 start.

### Fixed

- The rollback steps of the upgrade runbook: after `helm rollback`, a
  persisted `local.db` must always be removed (every start stamps it with its
  own schema version), and IAM and AuditLog state come from the pre-upgrade
  backup.
