### Added

- Interoperability tests with Knot DNS 3.6.2 as the primary of a kudurru secondary zone: AXFR, journal IXFR across a serial wrap, AXFR fallback without a journal, TSIG rejection, zone transfer over TLS with certificate verification, and expiry after the primary stops (`hack/test-knot-catalog.sh`).
- `hack/test-dnsdist-live.sh` runs the live dnsdist edge suites against the official dnsdist image, pinned by digest.

### Changed

- The live dnsdist edge suites use the upstream dnsdist 2.1.3 image by registry digest, the same image the edge image is built from, instead of a locally built image, so any machine can reproduce them.
- The benchmark README lists every hosted benchmark run with its CPU and outcome, and states how the final paired run on the mixed runner pool is counted.
