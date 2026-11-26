# SQLite integration Delete progress (#152)

Local synthetic evidence on Linux/amd64 with Go 1.27.4. The baseline is
`ce3ab6e2e28ad11663f1659f9e19fa74e5a33e61`. This is neither a hosted run nor
hardware qualification. No real Cloudflare account or shared cluster was used.

The fake ledger injects 3.75 seconds into each cleanup ledger or operation
write and each forget command, without overriding the production 10-second
capture or 30-second operation bounds. The baseline reaches its operation
deadline with both rows still stored; after the fix the same input commits
`2 -> 1 -> 0` across two runs and sends exactly two owned deletes. The fix
defers new intents before the cut, leaving time for a reread and forget. It
does not guarantee progress if the store or peer cannot finish one step at
the original bound.

```sh
KUDURRU_INTEGRATION_SLOW_CLEANUP=1 GOMAXPROCS=2 \
  go test -p 2 -count=1 -v -run '^TestTerminatingSlowCommandsMakeDurableProgress$' ./ee/integration/host

tmp=$(mktemp -d)
base=ce3ab6e2e28ad11663f1659f9e19fa74e5a33e61
git show "$base:ee/integration/host/cleanup.go" > "$tmp/cleanup.go"
git show "$base:ee/integration/host/engine.go" > "$tmp/engine.go"
jq -n --arg cwd "$PWD" --arg old "$tmp" \
  '{Replace: {($cwd+"/ee/integration/host/cleanup.go"): ($old+"/cleanup.go"), ($cwd+"/ee/integration/host/engine.go"): ($old+"/engine.go")}}' > "$tmp/overlay.json"
KUDURRU_INTEGRATION_SLOW_CLEANUP=1 GOMAXPROCS=2 \
  go test -p 2 -overlay "$tmp/overlay.json" -count=1 -v \
  -run '^TestTerminatingSlowCommandsMakeDurableProgress$' ./ee/integration/host
```

The mounted SQLite variant reopens the database with an owned delete batch
held in flight at the local TLS Cloudflare double. The peer completes the
effect while the database is closed, so the successor must observe that
effect without replaying the batch. The test asserts the stored reservation
and finalizer before restart, the exact delete IDs and preservation of the
foreign record, and removal of the ledger and operation rows but not the
shared request budget.

```sh
GOMAXPROCS=2 go test -p 2 -count=1 \
  -run '^TestIntegrationDelete(RestartWithHeldBatch|ResumesAfterRestart|FailoverFencesHeldBatchAndSuccessorFinishes)$' ./cmd/kudurru
```

## Review regression: reserve the reread

`TestTerminatingSlowRereadMakesDurableProgress` runs by default. It uses a
four-second test deadline, 250 ms ledger commands, and 1.1-second provider
reads. The initial fix at `0e6334c2` reserved a forget but not the reread;
that version left both ledger rows at the cut. With the measured reread in
the estimate it commits `2 -> 1 -> 0`. The opt-in test still runs at the
unchanged production bounds and passed again after the review fix.

To reproduce the review negative control, use the overlay command above
with only `cleanup.go` replaced from `0e6334c2` and select
`^TestTerminatingSlowRereadMakesDurableProgress$`.
