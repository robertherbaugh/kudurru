### Added

- Dashboard: the Identity tab of the Access page creates, edits, disables, enables, resets the password of and deletes local accounts, creates, edits and deletes service accounts and issues their tokens (the token is shown once and forgotten when the dialog closes), creates, edits and deletes identity providers with a client secret reference (never a value), and edits and deletes role bindings (an edit keeps the binding's labels and annotations; a binding with more than one subject or a scope the form does not offer is changed with `kudurru iam rolebindings replace`). Every change names the exact version you reviewed and is sent once; a refusal or an unknown outcome is shown and nothing is sent again.
- Dashboard: the IAM dialogs state the permanent revocation record a disable, group change, password reset or delete keeps, and the cluster-wide limit at which deletes, disables and new sign-ins are refused.

### Changed

- Dashboard: the identity actions are disabled with the reason on a follower, in static mode, without the permission, and on the Kubernetes driver (GET /auth/status capabilities).

### Fixed

- Dashboard: after a sync with an unknown outcome, an Integration's page now holds further syncs until a Check reports the endpoint reachable and the credential accepted; a Check that reported either one failing used to lift the hold.
