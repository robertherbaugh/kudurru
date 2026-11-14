### Added

- Integrations can be created, replaced and deleted through the API and CLI on both SQLite and Kubernetes: `Integration` is now a kind of `/api/v1/objects/integrations`, `POST /api/v1/apply` and `POST /api/v1/integrations:batchDelete`, and `kudurru get|apply|delete integrations` work. Writes bind the exact `resourceVersion` (`If-Match`), refuse objects owned by files or other managers, and on SQLite commit with their audit row in one current command; an enrolled follower forwards them to its primary.
- `GET /api/v1/integrations/manifests` and `kudurru integrations manifests` list the provider types of the build with their configuration schema, credential keys (and which are required), and what this node's integration host refuses (on Kubernetes: webhook instances, `spec.unifi.inventory`, Integrations outside the release namespace; UniFi push fields on both drivers), whether the license covers integrations, and which credential reference forms this node resolves.

### Changed

- The `integrations.write`, `integrations.delete` and `integrations.secrets` permissions are now in use (previously reserved). A write that binds or changes an Integration's `spec.credentials` needs `integrations.secrets` in addition to `integrations.write`, because a reference can name any credential the node allowlists; so does changing the type, the UniFi endpoint or TLS pin, or the webhook endpoint of an Integration that has credentials, because that decides which server receives them. Reading an Integration (`integrations.read`) shows the credential reference names it uses, never values. Only the built-in `admin` role holds these permissions.
- Creating an Integration needs a license that covers integrations (`402 license_required`); replacing, suspending and deleting an existing one do not.
- `kudurru backup` and `kudurru export` without kinds still cover only the DNS objects and need no integration permission; `kudurru export integrations` exports Integrations.

### Fixed

- An invalid Integration document never repeats a string of its `spec.credentials` in the error (from `PUT`, `POST /apply` or a follower): a validation failure of a credential reference is answered with the field path only, so a credential value pasted into a `file` or `env` field by mistake, including one with a trailing newline, a quote or a backslash, is not echoed, relayed or logged.
