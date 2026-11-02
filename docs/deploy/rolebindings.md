# Reviewed RoleBinding commands

The complete SQLite runtime supports public RoleBinding creation, replacement
and deletion through the API and `kudurru iam rolebindings`. Removing a grant
records its revocation floor in the same transaction as the binding change.
Regranting the same role does not revive an earlier frozen session or personal
token. A newly authenticated identity must qualify under the current policy.

List the configured bindings and inspect the exact public object before editing:

```sh
kudurru iam rolebindings list
kudurru iam rolebindings get default operators -o yaml
```

Creation takes one desired manifest, capped at 64 KiB. It accepts `apiVersion`,
`kind`, `spec`, and operator metadata (`name`, `namespace`, `labels`,
`annotations`). Remove server identity/lifecycle fields from an exported object;
do not supply a manager annotation. The target must be absent.

```sh
kudurru iam rolebindings create -f operators.yaml
```

Replacement and deletion require the exact UID and resource version from the
separate review. Only API-owned objects can be changed. File, bootstrap and
other managers cannot be adopted through these commands.

```sh
kudurru iam rolebindings replace default operators -f operators.yaml \
  --uid 'REVIEWED_UID' --resource-version 'REVIEWED_VERSION'
kudurru iam rolebindings delete default operators \
  --uid 'REVIEWED_UID' --resource-version 'REVIEWED_VERSION'
```

A stale review returns a conflict. Inspect the new object and decide what to do;
the CLI does not fetch a replacement review, follow a mutation redirect, or
retry a refused or uncertain request. Replacement never creates a missing
object. `-f -` reads a desired manifest from standard input.

The corresponding paths are `GET /api/v1/iam/rolebindings/{namespace}/{name}`,
`POST /api/v1/iam/rolebindings`, and `PUT`/`DELETE` on the item path. PUT carries
`{uid, resourceVersion, binding}`; DELETE carries `{uid, resourceVersion}`.
Current cluster `rolebindings.read`, `rolebindings.write` or
`rolebindings.delete` authority applies, with CSRF protection for cookies.
Legacy and Kubernetes mutation runtimes return 503 after permission admission.
Generic object aliases remain unavailable for these IAM mutations.

This completes the public SQLite writer. File/inline loader revocation and
Kubernetes durable accepted-authority work remain separate open items. The
[Kubernetes offline migration](iam-upgrade.md) is not replaced by this API.
