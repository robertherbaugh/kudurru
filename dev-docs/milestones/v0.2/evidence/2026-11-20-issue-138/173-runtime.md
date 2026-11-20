# #173 — Runtime refusal regression

Local Linux sandbox, November 20, 2026, Go 1.27.4, base `ce3ab6e2` with
the #173 test patch. No hosted or live-cluster acceptance.

```sh
GOMAXPROCS=2 go test -p 2 -race ./cmd/kudurru ./internal/cluster ./internal/stages/upstream \
  -run '^(TestPrimaryRuntimeNativeUpdateForRetiringZoneIsAnsweredAtOnce|TestRunServeSQLitePrimaryAnswersAcrossStatusWrites|TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation|TestFailureUpstreamContacted|TestMemberUpdateRerunOnlyWhenNothingNewWasDecided|TestRootMemberUpdateReplayRefusedByAdmissionIsAnsweredAtOnce|TestRootMemberUpdateForRetiringZoneIsAnsweredAtOnce|TestRunnableLastError.*|TestLeaderOnlyWriterEndOfTermIsNotAFailure)$' \
  -count=3 -v
```

[All three initial repetitions passed](173-regression.log), and [all three
passed after review](173-review-regression.log.gz). The reviewed test separately
asserts that capture does not mark the retired zone stale, and that a signed
TCP request gets a TSIG-signed `SERVFAIL` without an Apply outcome. No test or
production deadline changed. The status-write regression is additional evidence
for the corrected probe comment; it is not the loaded matrix tracked in #172.

For the [negative control](173-native-negative.log), remove only the
retiring/retired terminal `if` branch in `primaryRuntime.captureUpdate` and run
`GOMAXPROCS=2 go test -p 2 ./cmd/kudurru -run '^TestPrimaryRuntimeNativeUpdateForRetiringZoneIsAnsweredAtOnce$' -count=1 -v`.
The test failed with `stale=true` even though the rejection code remained
`SERVFAIL`: the branch removal reclassified a terminal refusal as eligible
for recapture. The [same control failed after review](173-review-negative.log).
The branch was restored before each race run.
