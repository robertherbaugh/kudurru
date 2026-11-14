# CHANGELOG.md `[0.2.0]` bullets from QUAL-KUBE-UPGRADE (D373)

No product behavior changed in this package; these bullets are for the
upgrade notes of `[0.2.0]` (Kubernetes), verified with the 0.1.0 binary
built from the tag and this release's built binaries on envtest.

### For the upgrade notes (Kubernetes, verified with the released 0.1.0 binary)

- The CRDs stay `kudurru.io/v1alpha1` with no conversion. Apply the CRD
  chart (or `deploy/kustomize/crd`) first: it replaces the nine 0.1.0 CRDs in
  place and adds five new ones (Kudurru, IdentityProvider, Integration,
  RoleBinding, ServiceAccount). Every object 0.1.0 stored keeps its UID,
  generation, spec and status, and is read and written by 0.2.
- Objects that 0.1 accepted and 0.2's schema refuses (a secondary or stub
  zone without its configuration, a rewrite without targets or CNAME) stay
  stored on Kubernetes and do not stop the node; everything else is served
  as before. A change to such an object is refused, naming the rule, until
  the object is corrected.
- The upgrade is offline when the release namespace holds 0.1 IAM Secrets
  (users, tokens, service accounts) or AuditLog batches: stop every kudurru
  process, apply the CRDs, run `kudurru iam migrate status`, `plan` and
  `begin --offline` (see `docs/deploy/iam-upgrade.md`) and continue only on
  `complete` with `RemoteAllowed: true`; write the audit key ring with
  `kudurru audit keys init --legacy-master-file <the 0.1 encryption key>
  --legacy-before <a time after the last 0.1 batch>`, publish it as the
  `kudurru-audit-keys` Secret, and start 0.2 with
  `store.kube.expectedClusterID` set. The answers, the 0.1 tokens and
  `kudurru audit verify` over the 0.1 batches carry over.
- The first break-glass token that 0.1 minted almost always does not
  survive the IAM migration (401 after the upgrade): 0.1 created its
  `break-glass` service account and minted the token in the same request,
  so both nearly always share a creation second. Mint a new one with
  `kudurru admin create-token` before or after the upgrade. Later break-glass tokens and personal tokens keep working.
- `helm upgrade` from the 0.1.0 chart keeps every field Kubernetes does not
  let an upgrade change: workload selectors, the StatefulSet's service name,
  pod management policy and volume claim templates, Service cluster IPs,
  and Secret names, types and keys.
- A node started while the API server is unreachable needs
  `store.kube.expectedClusterID`: without it the node exits before it
  answers (under Kubernetes the pod crash-loops until the API server is
  back). With it, and a `local.db` from a complete live sync, it answers
  from `local.db` over UDP and TCP and reports ready with the snapshot age.
