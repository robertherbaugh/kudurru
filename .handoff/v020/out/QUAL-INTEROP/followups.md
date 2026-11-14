Rows for `dev-docs/followups.md` (integrator: add them; this lane may not edit
that file). Suggested section: "Build, CI, docs" for the first row, "Zone,
controller, objects" for the AA row, and "Needs the project owner's hardware"
or "End to end and fuzzing" for the dnsdist diagnosis. The dnsdist diagnosis row was
resolved by QUAL-OPEN3 (`d2039002`, D383); its follow-up row is in
`out/QUAL-OPEN3/followups.md`.

| Item | Source | Target |
|---|---|---|
| Hosted job for the local-only interop suites: start a private dockerd on the runner and run `hack/test-knot-catalog.sh` (Knot `cznic/knot@sha256:174b0fcb24c6c3bc5065c9d1ce46b588eec607a5f32bc40ff0cda60150f00bd9`, knotd 3.6.2; `TestKnotCatalogConsumer` and `TestKnotInboundSecondaryTransfer`, last unskipped pass at `2e4e89fc`) and `hack/test-dnsdist-live.sh` (`powerdns/dnsdist-21:2.1.3@sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837`; all live dnsdist tests: F43 YAML/Lua, F28, F39b (both tests), anchor edge and F18c, last unskipped pass at `d2039002`, `out/QUAL-OPEN3`). Until then both are local-only evidence | QUAL-INTEROP, G23-R5; D383 | after v0.2.0 |
| Decide whether the secondary receiver should accept a signed IXFR reply that is one SOA at the client's serial without AA (Knot 3.6.2 sends it; BIND sets AA). Today such a reply is refused (`secondary: non-authoritative response`); a refresh against one consistent primary never asks for it, but it is reachable when the SOA poll and the IXFR reach different instances behind one address or the primary moves back to the stored serial between them. Effect: one refresh error, no renewed confirmation for that cycle, no data loss | QUAL-INTEROP review; `test/interop/knot_secondary_test.go` | v0.2 or later |
