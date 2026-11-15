# Shared-host historical calibration: failed

One predeclared recording ran November 15, 2026, from 20:08:00 to 20:26:31 UTC. The owner accepted the pinned VM with disclosed physical CPU sharing before timing. All 40 native commands completed successfully, but the blocking/zone product gate failed. The calibration is ineligible; no proposed limit files, approval template or candidate measurement were produced. Performance remains release-blocking.

| Failed historical metric | Five p99 values, ns | Median, ns | Fixed maximum, ns |
| --- | --- | ---: | ---: |
| `BenchmarkLookup100k/cname` | 2998, 4194, 4761, 4907, 10335 | 4761 | 2000 |
| `BenchmarkLookup100k/hit` | 2626, 1213, 2405, 3508, 2430 | 2430 | 2000 |
| `BenchmarkLookup100k/wildcard` | 2609, 2586, 9103, 2634, 8443 | 2634 | 2000 |

The other fixed product metrics pass. The cache, upstream, UDP, DoT, QUIC and persistence shape gates also pass. Those six gates validate shape, complete metric inventory and native outcomes; they do not claim that legacy hardware ceilings passed. The hardware profile was withheld because the required fixed product gate failed.

## Provenance and verification

- VM 20027, `kudurru-dev-test`, physical host `hm-pve02` (`hm-e-pve02.ssh`), eight vCPUs pinned to logical CPUs `0,2,4,6,8,10,12,14`, 16 GiB RAM and local NVMe. Other guests retain unrestricted affinity; no physical exclusivity is claimed.
- Ubuntu 24.04, Go 1.27.4, CGO disabled, six benchmark processors; five fresh processes per scope. Original corpora, commands and durations were retained.
- Original historical source: `0cc9565e9a878f8223fc1cee5201eb7eca8aacd2`; later socket/persistence source: `9cc2ae0423070fc514c94a25ab5b92e3816889e7`.
- Collector and amended method: `fef3a1fde4cf8b847ba833d9e1f0710e7ec3f998`.
- Stable execution cgroup: `/system.slice/kudurru-benchmark.service`. Service finished; no benchmark remains active.
- All 114 manifest files verify; source manifests match 406 original, 4,636 later and 5,348 control paths independently read from Git archives. All 40 receipts match the declared commands and have distinct process IDs, no resource stop and successful process-group cleanup.
- Minimum native-command free disk: 61,104,390,144 bytes. Physical host and VM configuration match before and after. Guest/hypervisor contention counters are retained; these observations do not identify a cause of the latency misses.
- Fixed product rules, original limits, baseline, samples and regression thresholds were not changed. No sample was replaced or timing run repeated.

Calibration manifest SHA256:

```text
5e1d12269c7875a980aa3330960a39418959382ff7fb280dc2ede185c50a3f0f
```

The complete raw recording is under `guest-evidence/calibration/`, including the original reports, logs, receipts, rules, source manifests, plan and failed result. `verification.json` records the independent checks, and `fixed-product-metrics.json` contains every fixed-product vector and evaluated median. Top-level host decisions, execution receipts, service properties and before/after counters accompany it.

Another attempt requires a diagnosed fix or a separately reviewed method change. The current recording cannot establish an accepted calibration or authorize candidate timing.
