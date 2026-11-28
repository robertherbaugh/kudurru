# Webhook retention and event coverage (#132)

Local synthetic evidence from Linux/amd64 with Go 1.27.4, 8 CPUs shared with
other jobs (1-minute load average 2.2 to 2.4 at the start and end). The
baseline is `c67e87ba`. These are not hosted runs. No real peer or shared
cluster was used. Each negative control uses a local Go overlay to replace
production files with their baseline version or a mutated copy. The tests
stay as committed, except where the baseline test file is named below.

## Retention across short leases (D427)

`TestRetentionRunsAcrossShortLeases` (`ee/integration/host`) runs a host
whose elector grants back-to-back terms of 2/7 of the 600 ms retention
interval (2 ms polls). The instance's lease has no hook role. The test
checks four things. The first lease of the new process prunes another
owner's expired row. A later short lease dead-letters the instance's own row
whose deadline passed after that first run, and prunes a second foreign
row. The run count never exceeds one per interval of elapsed time.

| Log | Code | Result |
| --- | --- | --- |
| `retention-positive.log` | this change, `-race -count=10`, together with `TestRetentionTickOfRunningLeaseWithoutHookRole` and `TestRetentionRecordForgottenOnlyByACompleteCapture` | 30/30 pass |
| `retention-negative-per-lease-ticker.log` | `commands.go`, `events.go`, `types.go` and `lifecycle.go` from the baseline (a ticker per lease) | 3/3 fail: nothing pruned in 6 s, ten intervals |
| `retention-negative-due-every-lease.log` | this change, but every lease is due at once (`time.NewTimer(0)`) | 3/3 fail: two runs within 175 ms |
| `retention-negative-before-first-sync.log` | this change, but the timer is read before the lease's first sync | `TestRetentionTickOfRunningLeaseWithoutHookRole` fails 3 of 10 runs under `-race`: that sync cleared the `EventDeadLettered` status |
| `retention-negative-forget-on-failed-capture.log` | this change, but a failed capture also forgets every instance's last run | `TestRetentionRecordForgottenOnlyByACompleteCapture` fails |

```sh
tmp=$(mktemp -d)
base=c67e87ba
for f in commands events types lifecycle; do
  git show "$base:ee/integration/host/$f.go" > "$tmp/$f.go"
done
jq -n --arg cwd "$PWD" --arg old "$tmp" '{Replace: {
  ($cwd+"/ee/integration/host/commands.go"): ($old+"/commands.go"),
  ($cwd+"/ee/integration/host/events.go"): ($old+"/events.go"),
  ($cwd+"/ee/integration/host/types.go"): ($old+"/types.go"),
  ($cwd+"/ee/integration/host/lifecycle.go"): ($old+"/lifecycle.go")}}' > "$tmp/overlay.json"
go test -p 1 -overlay "$tmp/overlay.json" -count=3 -v \
  -run '^TestRetentionRunsAcrossShortLeases$' ./ee/integration/host
```

The other two retention controls replace only `commands.go` with
`sed 's/time.NewTimer(h.retentionDelay(j.key))/time.NewTimer(0)/'`, and with
`if true || timer == nil {` in place of the `if timer == nil {` that guards
`retentionTick = retention.C`. The last control drops `&& err == nil` from
the loop in `lifecycle.go` that forgets `lastRetention` entries.

## Mixed-version IntegrationEventCursor

`TestReplicationRefusesImageWithoutIntegrationEventCursor`
(`internal/store/sqlite`) builds the scheme of a release before the kind:
every Integration carrier except `IntegrationEventCursor`. A follower of
that release installs the primary's image without the kind. A follower of
this release installs the full image with its cursor row. Each follower
then refuses the other release's next image with
`replication.ErrIncompatible`, and its installed generation stays the same.

| Log | Code | Result |
| --- | --- | --- |
| `mixed-version-positive.log` | this change, `-race -count=3` | 3/3 pass |
| `mixed-version-negative.log` | `internal/store/replication/codec.go` without the manifest length and per-kind version checks | fails: the image without the kind was accepted |

## Exact A1 audit rows

`webhookMountActAudit` (`cmd/kudurru/integration_webhook_mount_test.go`) now
compares the whole `integration.sync` and `integration.act` row: authority,
client address, method and path, actor, manager, action, key, the reviewed
resource version, result, and the exact reason with the parameter digest.
It also requires the store-assigned ID, time, chain version, request ID and
hashes. The runs select
`^(TestIntegrationWebhookMountedActionsAndOriginalRefusals|TestIntegrationWebhookMountedOriginalClientObservation)$`.

| Log | Code | Result |
| --- | --- | --- |
| `a1-audit-positive.log` | this change | pass |
| `a1-missing.log` | `integration_operation.go` without the `RecordAudit` call | the sync, `valid` and `uncertain` cases fail: no row |
| `a1-misattributed.log` | `integration_operation.go` writing `integrations:sync` for every command, no resource version and no parameter digest | the same cases fail on the row |
| `a1-misattributed-oldtest.log` | the same misattributed rows, with the baseline test file | all pass: the old assertion missed them |

The A1 mutations change `integration_operation.go` like this:
`sed 's/^\t\t\tif audit != nil {$/\t\t\tif false \&\& audit != nil {/'`
for the missing row. For the misattributed row the sed expressions are
`s/Authz: "integrations:" + o.proofVerb(a),/Authz: "integrations:sync",/`,
`s/ResourceVersion: revision,/ResourceVersion: revision[:0],/` and
`s/^\t\tif a.params != "" {$/\t\tif false \&\& a.params != "" {/`.

## Not covered here

Pruning when no Integration runs is an owner decision. Its default (A) is
documented and was not changed: until a lease runs, each deleted hook
instance leaves at most 384 delivery rows and one event cursor. No topic
was added, and no per-namespace filter either.
