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
The list and the item read work on both drivers: on Kubernetes
`kudurru iam rolebindings get` returns the RoleBinding object with its UID and
resource version, and you change it with `kubectl` or your GitOps tool.
Generic object aliases remain unavailable for these IAM mutations.

SQLite file and inline RoleBindings use the same durable grant-floor updates.
Removing or narrowing a source grant invalidates earlier frozen credentials;
recreating its original specification does not restore them. This also applies
to the first startup load and to remove/regrant changes made while the process
is stopped. Unchanged source reconciliation still checks the original writer
and accepted source before succeeding. Invalid or unreadable sources retain
their existing objects, and environment-installed inline objects retain their
startup precedence.

## Kubernetes

On the Kubernetes store, role bindings are `RoleBinding` objects of the
`kudurru.io/v1alpha1` API, together with the `rbac.bindings` entries of the
configuration file. Create, change and delete the objects with `kubectl` or
your GitOps tool. The create, replace and delete commands on this page answer
503 there. They do not replace the
[Kubernetes offline migration](iam-upgrade.md) of an installation that was
created before v0.2.

A browser session, and a personal API token that stored its owner's roles,
carry the roles that were resolved when they were issued. They do not carry
the groups that matched. kudurru honors such a role only while a current
binding still grants it and no binding change has removed it since. Removing or
narrowing a binding therefore ends that role for every session and token that
received it earlier, and granting the role again does not give it back to
them: the users sign in again or create new tokens. A binding that matches the
identity of the request itself, the built-in administrator and Kubernetes RBAC
(SubjectAccessReview) are evaluated on every request and are not affected.

Each replica records the time of such a removal in memory and in the Secret
`kudurru-grant-floors` (type `kudurru.io/grant-floors`) in the release
namespace. A restarted replica, or one that missed the change, reads the
removals from there and does not revive an earlier session.

- Every replica writes the removals it observes and reads the entries of the
  other replicas every five seconds. A recorded time only moves forward, and
  entries are never removed.
- After a start, a replica refuses the roles that sessions and tokens carry
  until it has read the Secret once. While the Secret cannot be read or is not
  valid, that refusal continues. Bindings that are evaluated on every request
  keep working.
- Do not edit or delete the Secret, and do not add owner references or
  further keys to it. kudurru refuses a Secret that does not have its own
  type and exactly the key `floors`; it does not replace one. Replicas that
  are running keep the removals they hold in memory when the Secret is
  deleted, but after their next restart a binding that was removed and granted
  again can give its role back to sessions from before the removal.
- A binding that is removed while no replica is running is not observed, and
  no time is recorded for it. Sessions still end after
  `auth.session.lifetime`.
- The Secret contains role names, scopes and times. It contains no
  credentials. It is limited to 10,000 entries and 1 MiB; beyond that, new
  removals stay in the memory of the replica that observed them.
- The chart's namespaced Role already lets the server manage Secrets in its
  namespace. The Secret is not part of the Helm release, so `helm uninstall`
  leaves it in place. Include it when you back up the namespace.
