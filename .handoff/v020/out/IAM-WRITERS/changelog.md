### Added

- IAM writers on the SQLite driver: change a local account's display name, groups or disabled state and delete it (`kudurru iam users update|delete`, `PATCH`/`DELETE /api/v1/iam/users/{name}`); create, replace and delete kudurru service accounts and issue their API tokens (`kudurru iam serviceaccounts`, `/api/v1/iam/serviceaccounts`); create, replace and delete IdentityProvider objects (`kudurru iam identityproviders`, `/api/v1/iam/identityproviders`). Every change needs the exact reviewed UID and resourceVersion and is never retried.
- `kudurru iam users create` and `kudurru iam users reset-password` for the existing local-user API commands; passwords are read from a prompt or stdin.
- `GET /api/v1/iam/serviceaccounts/{namespace}/{name}` and `GET /api/v1/iam/identityproviders/{name}` on both drivers, and `capabilities.iamObjectCommands` in `GET /api/v1/auth/status`.

### Changed

- Disabling, deleting or changing the groups of a local account, and disabling or deleting a service account, now end its sessions and delete its API tokens in the same commit; enabling restores none. A service account token is shown once and only its hash is kept; it expires no later than the API token that issued it, and none is issued for the bootstrap account or for accounts another manager owns.
- On Kubernetes the new commands answer `503 iam_management_unavailable` or `503 user_management_unavailable` with a hint; change those objects with `kubectl`. On an enrolled follower they answer `503 primary_required`.
- Each deleted account or provider, and the first disable or group change of an account, keeps a permanent revocation record. These records share a limit of about 4,000 entries with active sessions; at the limit, deletes and new sign-ins answer `503 unavailable` and change nothing.
