### Added

- Runbook "Add a follower behind managed edges": the order for adding a SQLite follower behind dnsdist edges with managed root anchors (edge backends first, then the offline join, then start).
- Committed golden documents and tests that pin the byte formats of managed root-anchor history (pure state formats 1 and 2, the owner envelope, the local carrier), plus a test that fails if any of these formats changes without a store schema or required-feature change.

### Changed

- `upstreams.recursion.anchorMode: managed` now fails validation on the Kubernetes store driver (startup, settings reloads and settings changes through the API; a refused reload keeps the last good configuration, and a refused change stores nothing). Before, it was accepted and the seeds ran as plain static anchors. Managed root anchors are SQLite-only in v0.2.
- The `kudurru anchors` help text and the recovery runbook now say what to check before distributing recovered history: every member runs kudurru v0.2.0 or later (the only earlier release, v0.1.0, has no cluster replication and cannot be a member).

### Fixed

- A node with no installed cluster image (an unfinished or interrupted `cluster join`) started with `managedTopology: direct` or `edges` now fails with an error that names `kudurru cluster join --offline` and `kudurru cluster join-reconcile --offline`. Before, it failed with the generic "root anchor policy unavailable or changed" or a raw store error. Like before, it contacts no edge and fetches nothing from the root.
