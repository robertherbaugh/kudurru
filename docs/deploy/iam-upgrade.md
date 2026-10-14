# Kubernetes IAM upgrade to v0.2

v0.2 moves ServiceAccount, RoleBinding and IdentityProvider configuration from
private Secrets to public `kudurru.io/v1alpha1` objects. LocalUser, APIToken and
PrincipalState remain private. SQLite performs its conversion transactionally
when opening the database; the commands here apply to Kubernetes only.

Kubernetes assigns new UIDs to the public objects. A private migration journal
records the exact source and target identities so existing tokens can retain
their account binding without granting privileges to a recreated name. Ordinary
annotations are never proof of a migrated identity.

## Prepare an offline upgrade

Stop every old and new kudurru process and every other IAM writer, including
GitOps reconciliation of the affected objects, admin socket operations and
previous migration commands. Keep them stopped through completion. This is an
offline migration; `--offline` records the operator's acknowledgment and cannot
detect or stop a remote writer.

Take a protected backup of the release namespace's private Secrets and existing
public objects. Retain the old deployment configuration and image. The ordinary
safe object export omits credentials; the ordinary object backup does not carry
the migration journal and immutable source archives. Use a Kubernetes backup
that includes these private Secrets and protects them as credentials. A logical
restore that assigns new UIDs cannot preserve old UID-bound credentials; reissue
them after such a restore. A full rollback must restore the matching pre-upgrade
identity state, not mix old readers with partially migrated objects.

Install the new CRDs before running the migration. Use the node's server
configuration with `store.driver: kube`; add any legacy provider file path to
the exact host `credentials.allowedFiles` grant. The migrator checks the grant
without reading the file. Credential Secret references use the release
namespace, and no credential values are written into public objects.

## Inspect and migrate

These commands talk directly to Kubernetes. `--kubeconfig` and `--kube-context`
select that connection; the ordinary kudurru API context is not used. Always
specify `--release-namespace`, which contains the private source Secrets and may
differ from the namespace of a public service account or role binding.

```sh
kudurru --config /etc/kudurru/kudurru.yaml iam migrate \
  --release-namespace dns status
kudurru --config /etc/kudurru/kudurru.yaml iam migrate \
  --release-namespace dns plan
kudurru --config /etc/kudurru/kudurru.yaml iam migrate \
  --release-namespace dns begin --offline
```

Output is JSON containing safe counts, state, object identifiers and the journal
resource version. `plan` validates the complete source set and target conflicts
without writing. `begin` recaptures and validates that set, archives it, creates
the public objects, records their returned UIDs and restores supported status.
It preserves the original private Secrets. Token hashes, scopes, expiry and
private authority are never copied into public metadata.

`native` means no migration is needed. `complete` with `RemoteAllowed: true`
means the private journal and archives verified. Restart the new deployment
only after checking this result. Ambiguous UID-less tokens, including an account
and token created in the same second without other identity proof, remain
unbound; their owners must issue replacements.

## Resume and recover

After an interrupted command, keep all readers and writers stopped and run
`status`. Supply its exact `JournalResourceVersion` to resume:

```sh
kudurru --config /etc/kudurru/kudurru.yaml iam migrate \
  --release-namespace dns resume --offline --resource-version 'VERSION_FROM_STATUS'
```

A resource version is opaque. The command never replaces a stale supplied value
with a newer one. Timeouts have a default whole-operation limit of five minutes;
`--timeout` may raise it to at most 30 minutes. An interrupted operation remains
resumable under the same rules.

`recovery-required` with `uncertain-create` means Kubernetes may have created an
object, but the migrator did not durably record its returned UID. Matching
contents, migration labels or annotations do not authorize adoption. Inspect the
reported object and the private journal while all processes remain stopped.
After verifying that the object is the incomplete migration target, an operator
may delete it through the Kubernetes API with an explicit `preconditions.uid`
matching the inspected UID, then inspect status and resume. Do not delete by
name alone. Alternatively, restore the complete pre-upgrade backup.

Source changes, foreign targets, missing archives, unsupported journal versions
and mismatched private authority stop the migration. Resolve the reported cause
or roll back; do not edit the journal to manufacture a receipt or completion.

## Runtime admission

The server checks private migration admission before identity bootstrap and
every remote API authentication attempt. It captures credential mappings from
that same verified journal. IAM mutations, including file-managed writes,
token-use status and local account operations, recheck current admission before
writing. The checks are bounded and fail closed during Kubernetes errors.
Incomplete migration does not run account bootstrap. Core DNS processing does
not call the migration gate.

These checks do not create a Kubernetes transaction across objects and do not
make concurrent migration safe. Keep the offline deployment requirement even
when a partially upgraded server refuses login. Broader coherent authentication
publication and current-grant mutation authorization are separate v0.2 work.
