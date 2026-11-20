# #174 — Integration budget and rotation controls

November 20, 2026. Local Linux sandbox, Go 1.27.4, base `ce3ab6e2` with
the #174 patch. Envtest 1.37.0 and a disposable local TLS peer, not a live
Kubernetes cluster or Cloudflare account. No timeout is increased.

## Joined reservation error

`TestJoinedBudgetContentionConsumerClassification` makes a budget CAS lose
when its context ends, then feeds the returned conflict plus deadline error
to the host's consumers. The reservation never invokes the outbound callback.

| Consumer | Observed classification |
|---|---|
| Event delivery | Local refusal; neither an attempt nor an error class is recorded on the delivery. |
| Terminating cleanup | Moved frame / ledger conflict, using that branch's existing increasing backoff rather than the provider failure branch. |
| Sync status | `ReconcileFailed` / `ErrProvider` even though this is local contention and nothing was sent. This diagnostic mismatch is documented, not changed. |

These are the classes of the joined error at the host boundary, not evidence
of a provider response. The cleanup class is pinned, not a proof of scheduling
under repeated external wakeups; the test did not reproduce a hot loop.
The reservation regression checks each observed inter-attempt pause against
half of the corresponding exponential pause and retains the no-send assertion.
It no longer assumes a fixed number of attempts fits into the 300 ms bound.

## Credential rotation

The mounted test taps the old provider's actual outbound listing context and
holds that request at the local TLS peer. It verifies the context is live
before replacing the credential, verifies the peer sees the disconnect, and
requires `context.Canceled` from the client context (not a deadline). It
still checks that the new provider starts after the old request is canceled.

```sh
make envtest
export KUBEBUILDER_ASSETS="$PWD/bin/k8s/1.37.0-linux-amd64" KUDURRU_REQUIRE_ENVTEST=1
GOMAXPROCS=2 go test -p 2 ./ee/integration/host -race -count=5 -v
GOMAXPROCS=2 go test -p 2 ./cmd/kudurru -run '^TestRunServeKubeCloudflare' -race -count=3 -v
```

[Envtest setup](174-envtest.log) succeeded. The [host suite](174-host-race.log.gz)
passed all five race repetitions (350 s). The [envtest command suite](174-kube-race.log.gz)
passed all three race repetitions (306 s); all three rotation legs recorded
`context canceled` with over 29 s remaining. The compressed logs retain the full
test output.

The [focused smoke test](174-rotation-smoke.log) passed with about 29.74 s
remaining on the deadline. For the [timeout-only control](174-rotation-negative.log),
remove the staged file instead of renaming it over the original credential,
then run `GOMAXPROCS=2 go test -p 2 ./cmd/kudurru -run '^TestRunServeKubeCloudflareConflictMidFlightReviewAndRotation$' -count=1 -v`
with the same envtest assets. The peer saw the disconnect after the original
deadline, but the test failed with `cause=context deadline exceeded`. Restore
the rename for the positive run.
