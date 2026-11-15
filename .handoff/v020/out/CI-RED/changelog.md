### Fixed

- CI: the helm job's generated-assets check (`make deploy-assets`) no longer needs the `v0.1.0` git tag. The 0.1.0 chart fixture that the chart upgrade test compares against is regenerated only with its own flag, `go test ./deploy -run TestChartUpgradeFromV01 -update-v01-chart`, and a separate test checks it against the chart of the `v0.1.0` tag, which the helm job now fetches.
- CI: `TestServingBlockingWriterCanceledAfterCommitBeforeObservation` no longer fails intermittently with `providerresources: owner closed`; its borrowed provider coordinator is now closed after the fixture runtime has stopped.
- CI: serving command tests no longer race the running fixture runtime when they swap its clock (a data race the race detector reported intermittently in `TestServingBlockingCurrentProofFinalClockAndRollback`).
