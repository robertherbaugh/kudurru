### Added

- Documentation: a "Managed root anchors (RFC 5011)" page (`docs/deploy/managed-root-anchors.md`) that covers requirements, seed choice, the enable order for standalone nodes, enrolled clusters and dnsdist edges, monitoring, and the places where kudurru departs from RFC 5011 (permanent revocation tombstones, hold-down credit only within one process, seeds fixed after the first bootstrap).
- Managed root anchors are qualified against real authorities. Separate kudurru processes bootstrap from BIND 9 roots (RSA/SHA-256, ECDSA P-256, Ed25519) and from a Knot DNS root, and answer AD-validated recursion. They refuse wrong, revoked and expired trust, and they observe a real revocation. The RFC 5011 timeline is replayed on BIND- and Knot-signed wires. An opt-in check (`KUDURRU_LIVE_DNS=1 go test -run TestLiveRootManagedBootstrap ./cmd/kudurru`) runs the production acquisition against all 13 root servers.

### Changed

- `docs/deploy/dnssec-validation.md`: the statement that no RFC 5011 worker runs now applies only to forwarded validation, and the page links to managed root anchors for recursive groups.
