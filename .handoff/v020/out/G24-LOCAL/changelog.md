### Added

- `hack/release-local-checks.sh` runs the release checks that no pull request
  workflow runs (version check and its unit tests, the candidate tag,
  `make check-generated`, `make release-manifests` with an image-tag
  inspection, the real dashboard bundle with `make size`, `goreleaser check`,
  the single-target and full goreleaser snapshots and, with `--live`, the
  live-internet checks) on a clean tree, with one log per step and the commit
  it ran on, and restores the tree afterwards. With `--full-snapshot` it checks
  that both snapshot images print the snapshot version and removes them
  afterwards. Nothing is published, signed, tagged or pushed.
- `hack/live-recursive-node.sh` starts a throwaway node with a recursive
  default group and checks against the real root servers that validated
  answers carry the AD flag, a bogus zone answers SERVFAIL with EDE 6 (and its
  data with `+cd`), and recursor timeouts stay low.
- `TestLiveIANATrustAnchors` (with `KUDURRU_LIVE_DNS=1`) compares the built-in
  root trust anchors with the DS records IANA's root-anchors.xml lists as valid
  today.

### Changed

- CI runs the version-synchronization unit tests in the go job's test step with
  the other `hack/` tests instead of in a separate helm-job step.

### Fixed

- A recursive upstream group set in the configuration file
  (`default: { mode: recursive, timeout: 4s }`) no longer stops the first start
  of a node on SQLite with "strategy: Unsupported value". A recursive group now
  has no strategy in its settings.
