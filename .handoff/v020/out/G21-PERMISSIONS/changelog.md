### Changed

- Downloading the query log (`GET /api/v1/queries/export`, **Export up to 1,000 queries** on the Query log page) now needs the `querylog.export` permission, which the `auditor` and `admin` roles have. The `operator` role still reads the query log but no longer sees or uses the export, as the role table always stated. A denied export answers 403 with `missing: querylog.export`.
- Allowing or denying a domain from a logged query (`POST /api/v1/queries/{id}/actions`) now also needs `querylog.read`, because it reads that query. Built-in roles that can edit lists already have it.

### Added

- `internal/rbac/permissions.yaml` lists the declared permissions that nothing checks yet under `reserved:`, each with its target release (v0.2 for the IAM and integration administration verbs that v0.2 still has to build, v1.0 for the rest), and a reverse lint fails the build for a declared permission that no route or handler checks and that is not reserved.
