### Added

- `kudurru tsigkeys list|create|replace|reseal|revoke` manages stored TSIG keys on complete SQLite and Kubernetes nodes. The secret comes only from `--secret-file` (a file, or `-` for stdin) and is never printed. Replace, reseal and revoke take the exact `--uid` and `--resource-version` from `list`. The output format, key name and secret are checked before anything is sent, and a receipt that does not name the requested key and reviewed binding is reported as an unknown outcome. Each command is sent once and never retried. A `202` receipt exits 0 with a note that publication was not confirmed. A follower's `503 primary_required` says that nothing changed and points to the primary. (D394)

### Changed

- The stored TSIG key and dynamic-update guides now use `kudurru tsigkeys` instead of hand-written JSON request files for `kudurru api`.
