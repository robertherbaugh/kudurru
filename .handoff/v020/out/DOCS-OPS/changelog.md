# DOCS-OPS: lines for `CHANGELOG.md` `[0.2.0]`

### Added

- Operator guide for the enrolled SQLite cluster, end to end
  (`docs/deploy/sqlite-cluster.md`): what to prepare (the same explicit
  encryption key on every member, names and internal addresses, a token-based
  CLI context for each member), activation of the first primary, invitation
  and join, how to check the members, which changes a follower passes on to
  the primary and what its 503 answers mean, promotion of a follower, the return of
  the old primary, recovery after two promotions, the limits of this version
  and a table of the answers of the cluster commands. The join page, the
  Compose HA page and the deployment index link to it, and the runbooks gain
  "The primary of an enrolled SQLite cluster is lost".
- Operator guide for RFC 2136 dynamic updates on both store drivers
  (`docs/deploy/dynamic-updates.md`): creating a TSIG key, the
  `spec.policy.update` and `spec.policy.updatePolicies` fields, four things
  to check in a policy before it is applied, an `nsupdate` example, what an update may not change, the meaning of each answer, where
  SQLite keeps dynamic records and what a backup contains, and the
  `rfc2136-<zone>` record set on Kubernetes.
- `docs/deploy/rolebindings.md` describes role bindings on Kubernetes: which
  credentials carry the roles they had when they were issued, what removing a
  binding does to them, and the `kudurru-grant-floors` Secret (do not edit or
  delete it; include it in namespace backups).
- The administrator guide explains the authenticator (set up, replace,
  remove, remove for another person, recover the only administrator's
  authenticator from the host) and the browser session list, and the command
  line chapter explains sign-in through an identity provider with the browser
  or with `--device`. The sign-in steps mention the Authenticator code field.
  The screenshots of the Access page and of the sign-in page show the current
  dashboard.
- `docs/deploy/README.md` lists the task pages that it did not link before
  (enrolled cluster, join, dynamic updates, stored TSIG keys, role bindings,
  DNSSEC validation, response processing, DNS controls, client identity,
  integrations, SQLite storage, VRRP keys, the Kubernetes IAM upgrade).

### Changed

- `docs/deploy/tsig-keys.md` no longer says that stored keys are read once at
  startup on Kubernetes or that rotation on other drivers is unqualified. It
  shows how to call the key routes with `kudurru api`.
- The upgrade runbook gives the order for a Kubernetes installation that
  comes from v0.1: stop every kudurru process, install the new CRDs (step 1
  of the Helm steps), run the offline identity migration
  (`docs/deploy/iam-upgrade.md`), then continue with the Helm upgrade.
- Eleven places of the administrator guide (zones and upstreams, system, the
  Upstreams page) are reworded to the guide's writing rules; `python3
  hack/guide-lint.py` reports no error. No statement changes.

### Fixed

- The administrator guide no longer says that the Identity tab is "Read-only
  in this release"; `docs/deploy/rolebindings.md` no longer says that durable
  revocation on Kubernetes is open; `deploy/compose/ha/README.md` no longer
  says that follower replication and enrollment are separate work.

### Known limitations

These are not changes of this package. They were found while the pages were
checked against running nodes, and the pages state them. They are listed here
for the integrator to place, or to turn into fixes, with the rest of the
0.2.0 section.

- On SQLite, one run of `kudurru apply` or `kudurru restore` applies only the
  first document that is new or changed. Every later document of the same
  file answers `503 unavailable` ("current writable serving generation
  unavailable"), also a document that changes nothing. Run the command again
  until it reports no error: a file with N new or changed objects needs N
  runs, and documents that are already applied are reported `unchanged`.
  Not checked on Kubernetes.
- Through a follower of an enrolled SQLite cluster, `kudurru apply` with
  several documents applies a first part of the file and refuses the rest
  with `503 unavailable`, and repeating the command through the follower does
  not reliably apply the rest. Run such a file against the primary. (This
  describes the release with the HA lane's forwarding fix. Without that fix a
  follower refuses every `kudurru apply`, every `kudurru restore` and the
  dashboard's block list, client group and policy editors with "the
  follower's source changed"; list that instead if the fix is not part of
  the release.)
- `kudurru login --username` does not work against a follower of an enrolled
  SQLite cluster, whether or not the primary is up: the sign-in is passed on
  to the primary, the API token that the CLI saves is not. Save a context
  with `--token-stdin` for a follower. A password sign-in in the browser
  works on a follower while the primary is reachable.
- On SQLite, a zone whose update policy kudurru cannot use goes out of
  service (`SERVFAIL`, node not ready) until the policy is corrected, and the
  API accepts such a zone. Four shapes do this: `spec.policy.update` other
  than `deny` and `allow`, an update grant for a name outside the zone, a
  grant name without the final dot, and a `types` entry that is not a data
  record type (for example `BOGUS`, `OPT` or `AXFR`). On Kubernetes the zone
  keeps answering and its updates are refused.
- On SQLite, a signed update is answered `SERVFAIL` instead of `REFUSED` when
  the zone does not allow updates, and when the node has the key but no grant
  of the zone names it (also `update: allow` without a grant). Kubernetes
  answers `REFUSED`.
- BIND's `nsupdate` reports a wrong TSIG key name or secret as "clocks are
  unsynchronized", because the error answer carries a signing time of zero.
- `kudurru cluster status`, `kudurru nodes`, the Nodes page and the node count
  of the license status describe only the answering node of an enrolled SQLite
  cluster, and a follower names itself as leader there.
- An enrolled cluster issues at most 64 invitations in its lifetime: expired
  and used records are not removed, and the next `cluster join-token` answers
  `503 unavailable`.
- `kudurru backup` followed by `kudurru restore` on Kubernetes recreates the
  `rfc2136-<zone>` record set as an API-owned object; updates for that zone
  are refused until it is deleted (from the code, not from a run).
