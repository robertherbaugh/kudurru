# Cloud-sandbox historical calibration: eligible for review

All 40 historical native commands and seven shape/product gates pass. The calibration is eligible for owner review of these exact vectors and proposed limits. Approval remains pending; no candidate timing has run.

Performance remains release-blocking until actual limit review and passing final qualification. The original failed hosted D412 and failed shared homelab recording remain failed. No sample was replaced or bound relaxed.

## Source and host

- Method/source `3ff60b9f21f9d2abe114eed418b2179d6f28032d`, profile class `shared-cloud-sandbox-v1`. Runtime tree matches `5e19a32b` except retained benchmark tooling and documentation.
- Original reference `0cc9565e9a878f8223fc1cee5201eb7eca8aacd2` for blocking/zone/cache/upstream; later `9cc2ae0423070fc514c94a25ab5b92e3816889e7` for UDP/DoT/QUIC/persistence.
- This named eight-vCPU Vercel sandbox, Intel Xeon family6/model85/stepping7, KVM, Go1.27.4, CGO0, six benchmark processors, five fresh processes per scope. Physical host, pinning, SMT placement and tenant sharing are unavailable and explicitly accepted by the owner in the retained method decision.
- The actual owner-reviewed proposal, approval evidence and guest identity are retained. Guest/cgroup/toolchain/storage/source guards run around every native process. Unrelated local builds/tests finished before measurement.
- Verified 120 artifact hashes and source path counts original: 406, later: 4636, control: 5349.
- All 40 native receipts are retained; minimum free disk 11,105,906,688 bytes. Gate results are in verification.json.

## Exact review identity

Calibration manifest SHA256:

```text
3b38bf0389bb04e2ed9b0546d79e276ffdbf70090ae8588afa7c5fb2a0bf7266
```

Accepting a host or this report generation does not accept the values. The separate review record must bind this exact digest. `release_qualified` remains false.

## Fixed product requirements

| Metric | Unit | Five values | Median | Fixed maximum | Result |
| --- | --- | --- | ---: | ---: | --- |
| `BenchmarkLatency/suffix-1M/hit` | p99-ns | 1381, 1415, 1545, 1973, 1516 | 1516 | 2000 | PASS |
| `BenchmarkLatency/suffix-1M/miss` | p99-ns | 897, 816, 871, 1100, 959 | 897 | 2000 | PASS |
| `BenchmarkMemory/3M` | rss-MB | 90.34, 90.32, 90.11, 90.06, 90.1 | 90.11 | 150 | PASS |
| `BenchmarkCompile/adblock-1M` | ns/op | 1.13126e+09, 1.0351e+09, 1.07448e+09, 1.08644e+09, 1.03496e+09 | 1.07448e+09 | 5e+09 | PASS |
| `BenchmarkCompile/hosts-1M` | ns/op | 9.89404e+08, 9.55622e+08, 9.61048e+08, 1.07387e+09, 9.59717e+08 | 9.61048e+08 | 5e+09 | PASS |
| `BenchmarkEvaluate/exact-1M/hit` | allocs/op | 0, 0, 0, 0, 0 | 0 | 0 | PASS |
| `BenchmarkEvaluate/exact-1M/miss` | allocs/op | 0, 0, 0, 0, 0 | 0 | 0 | PASS |
| `BenchmarkEvaluate/policy-3M/hit` | allocs/op | 0, 0, 0, 0, 0 | 0 | 0 | PASS |
| `BenchmarkEvaluate/policy-3M/miss` | allocs/op | 0, 0, 0, 0, 0 | 0 | 0 | PASS |
| `BenchmarkEvaluate/suffix-1M/hit` | allocs/op | 0, 0, 0, 0, 0 | 0 | 0 | PASS |
| `BenchmarkEvaluate/suffix-1M/miss` | allocs/op | 0, 0, 0, 0, 0 | 0 | 0 | PASS |
| `BenchmarkLookup100k/cname` | p99-ns | 2602, 1831, 1706, 1890, 2042 | 1890 | 2000 | PASS |
| `BenchmarkLookup100k/hit` | p99-ns | 1112, 873, 828, 920, 901 | 901 | 2000 | PASS |
| `BenchmarkLookup100k/nodata` | p99-ns | 855, 600, 665, 737, 691 | 691 | 2000 | PASS |
| `BenchmarkLookup100k/nxdomain` | p99-ns | 730, 514, 582, 613, 500 | 582 | 2000 | PASS |
| `BenchmarkLookup100k/wildcard` | p99-ns | 1755, 1455, 1325, 1509, 1456 | 1456 | 2000 | PASS |
| `BenchmarkLookup100k/cname` | allocs/op | 2, 2, 2, 2, 2 | 2 | 3 | PASS |
| `BenchmarkLookup100k/hit` | allocs/op | 1, 1, 1, 1, 1 | 1 | 3 | PASS |
| `BenchmarkLookup100k/nodata` | allocs/op | 0, 0, 0, 0, 0 | 0 | 3 | PASS |
| `BenchmarkLookup100k/nxdomain` | allocs/op | 0, 0, 0, 0, 0 | 0 | 3 | PASS |
| `BenchmarkLookup100k/wildcard` | allocs/op | 3, 3, 3, 3, 3 | 3 | 3 | PASS |

## Proposed hardware profiles

Compared with the legacy profiles: 63 more restrictive, 35 less restrictive, 40 unchanged. Fixed product requirements do not change. Every individual old/new bound is shown below and in profile-comparison.csv.

All values below derive exclusively from the immutable historical recordings. Upper bounds are median plus max(20%, 25ns/op or 64B/op where applicable); other upper units retain 20%. Throughput and entry minima are 80% of median. Every loss/failure vector must be zero. The original repository profiles and fixed product rules are unchanged.

### cache

| Metric | Unit | Five values | Median | Rule | Proposed bound | Legacy bound |
| --- | --- | --- | ---: | --- | ---: | ---: |
| `BenchmarkGet/hit` | B/op | 0, 0, 0, 0, 0 | 0 | limit | 64 | 64 |
| `BenchmarkGet/hit` | allocs/op | 0, 0, 0, 0, 0 | 0 | limit | 0 | 0 |
| `BenchmarkGet/hit` | ns/op | 762.6, 654.4, 711.2, 897, 722.6 | 722.6 | limit | 867.12 | 638.88 |
| `BenchmarkGet/hit-parallel` | B/op | 0, 0, 0, 0, 0 | 0 | limit | 64 | 64 |
| `BenchmarkGet/hit-parallel` | allocs/op | 0, 0, 0, 0, 0 | 0 | limit | 0 | 0 |
| `BenchmarkGet/hit-parallel` | ns/op | 164.6, 152.3, 152, 157.3, 150.5 | 152.3 | limit | 182.76 | 156.48 |
| `BenchmarkGet/miss` | B/op | 0, 0, 0, 0, 0 | 0 | limit | 64 | 64 |
| `BenchmarkGet/miss` | allocs/op | 0, 0, 0, 0, 0 | 0 | limit | 0 | 0 |
| `BenchmarkGet/miss` | ns/op | 136.1, 138.3, 137.8, 138.4, 144.3 | 138.3 | limit | 165.96 | 110.53 |
| `BenchmarkMakeKey` | B/op | 24, 24, 24, 24, 24 | 24 | limit | 88 | 88 |
| `BenchmarkMakeKey` | allocs/op | 1, 1, 1, 1, 1 | 1 | limit | 1.2 | 1.2 |
| `BenchmarkMakeKey` | ns/op | 57.52, 48.63, 46.56, 50.46, 48.17 | 48.63 | limit | 73.63 | 89.1 |
| `BenchmarkMemory/1000k` | B/op | 3.75204e+08, 3.75313e+08, 3.75149e+08, 3.75422e+08, 3.75258e+08 | 3.75258e+08 | limit | 4.5031e+08 | 4.50277e+08 |
| `BenchmarkMemory/1000k` | allocs/op | 4.01217e+06, 4.01218e+06, 4.01216e+06, 4.01218e+06, 4.01217e+06 | 4.01217e+06 | limit | 4.81461e+06 | 4.8146e+06 |
| `BenchmarkMemory/1000k` | bytes/entry | 292.7, 292.7, 292.6, 292.8, 292.7 | 292.7 | limit | 351.24 | 351.36 |
| `BenchmarkMemory/1000k` | ns/op | 1.34712e+09, 1.30138e+09, 1.30023e+09, 1.38577e+09, 1.5394e+09 | 1.34712e+09 | limit | 1.61654e+09 | 1.04685e+09 |
| `BenchmarkMemory/1000k` | overhead-bytes/entry | 173.8, 173.8, 173.8, 173.9, 173.8 | 173.8 | limit | 208.56 | 208.68 |
| `BenchmarkSet` | B/op | 112, 112, 112, 112, 112 | 112 | limit | 176 | 176 |
| `BenchmarkSet` | allocs/op | 1, 1, 1, 1, 1 | 1 | limit | 1.2 | 1.2 |
| `BenchmarkSet` | ns/op | 831.3, 741, 747.7, 794.7, 751.8 | 751.8 | limit | 902.16 | 706.92 |

### upstream

| Metric | Unit | Five values | Median | Rule | Proposed bound | Legacy bound |
| --- | --- | --- | ---: | --- | ---: | ---: |
| `BenchmarkStrategies/fast/parallel_best` | B/op | 12992, 12988, 12985, 12990, 12992 | 12990 | limit | 15588 | 15580.8 |
| `BenchmarkStrategies/fast/parallel_best` | allocs/op | 202, 202, 202, 202, 202 | 202 | limit | 242.4 | 242.4 |
| `BenchmarkStrategies/fast/parallel_best` | ns/op | 92024, 90482, 89828, 97191, 93868 | 92024 | limit | 110429 | 146994 |
| `BenchmarkStrategies/fast/parallel_best` | p50-us | 86, 84, 84, 91, 87 | 86 | limit | 103.2 | 139.2 |
| `BenchmarkStrategies/fast/parallel_best` | p99-us | 208, 202, 193, 214, 212 | 208 | limit | 249.6 | 379.2 |
| `BenchmarkStrategies/fast/random` | B/op | 6251, 6248, 6250, 6250, 6248 | 6250 | limit | 7500 | 7501.2 |
| `BenchmarkStrategies/fast/random` | allocs/op | 100, 101, 101, 101, 101 | 101 | limit | 121.2 | 121.2 |
| `BenchmarkStrategies/fast/random` | ns/op | 83970, 79982, 85503, 85103, 76981 | 83970 | limit | 100764 | 133277 |
| `BenchmarkStrategies/fast/random` | p50-us | 78, 75, 80, 79, 72 | 78 | limit | 93.6 | 114 |
| `BenchmarkStrategies/fast/random` | p99-us | 152, 143, 160, 155, 139 | 152 | limit | 182.4 | 328.8 |
| `BenchmarkStrategies/fast/rtt_weighted` | B/op | 6278, 6274, 6272, 6275, 6273 | 6274 | limit | 7528.8 | 7530 |
| `BenchmarkStrategies/fast/rtt_weighted` | allocs/op | 102, 102, 102, 102, 102 | 102 | limit | 122.4 | 122.4 |
| `BenchmarkStrategies/fast/rtt_weighted` | ns/op | 87258, 80360, 81983, 84875, 81047 | 81983 | limit | 98379.6 | 137978 |
| `BenchmarkStrategies/fast/rtt_weighted` | p50-us | 81, 75, 76, 79, 75 | 76 | limit | 91.2 | 114 |
| `BenchmarkStrategies/fast/rtt_weighted` | p99-us | 160, 145, 147, 151, 146 | 147 | limit | 176.4 | 345.6 |
| `BenchmarkStrategies/fast/strict` | B/op | 6239, 6238, 6236, 6242, 6238 | 6238 | limit | 7485.6 | 7488 |
| `BenchmarkStrategies/fast/strict` | allocs/op | 101, 101, 101, 101, 101 | 101 | limit | 121.2 | 121.2 |
| `BenchmarkStrategies/fast/strict` | ns/op | 81439, 79805, 76359, 89348, 79995 | 79995 | limit | 95994 | 124840 |
| `BenchmarkStrategies/fast/strict` | p50-us | 75, 74, 71, 83, 74 | 74 | limit | 88.8 | 110.4 |
| `BenchmarkStrategies/fast/strict` | p99-us | 147, 148, 137, 164, 147 | 147 | limit | 176.4 | 292.8 |
| `BenchmarkStrategies/jitter/parallel_best` | B/op | 13162, 13167, 13175, 13172, 13166 | 13167 | limit | 15800.4 | 15810 |
| `BenchmarkStrategies/jitter/parallel_best` | allocs/op | 204, 204, 205, 204, 204 | 204 | limit | 244.8 | 244.8 |
| `BenchmarkStrategies/jitter/parallel_best` | ns/op | 1.82113e+06, 1.70894e+06, 1.75554e+06, 1.71742e+06, 1.75165e+06 | 1.75165e+06 | limit | 2.10198e+06 | 2.30486e+06 |
| `BenchmarkStrategies/jitter/parallel_best` | p50-us | 1351, 1278, 1361, 1289, 1304 | 1304 | limit | 1564.8 | 1850.4 |
| `BenchmarkStrategies/jitter/parallel_best` | p99-us | 3500, 3456, 3505, 3469, 3445 | 3469 | limit | 4162.8 | 4458 |
| `BenchmarkStrategies/jitter/random` | B/op | 6339, 6342, 6340, 6338, 6338 | 6339 | limit | 7606.8 | 7597.2 |
| `BenchmarkStrategies/jitter/random` | allocs/op | 102, 102, 102, 102, 102 | 102 | limit | 122.4 | 122.4 |
| `BenchmarkStrategies/jitter/random` | ns/op | 2.2884e+06, 2.21555e+06, 2.21572e+06, 2.19946e+06, 2.26344e+06 | 2.21572e+06 | limit | 2.65887e+06 | 2.87887e+06 |
| `BenchmarkStrategies/jitter/random` | p50-us | 2318, 2285, 2281, 2292, 2350 | 2292 | limit | 2750.4 | 2986.8 |
| `BenchmarkStrategies/jitter/random` | p99-us | 3449, 3400, 3407, 3439, 3489 | 3439 | limit | 4126.8 | 4410 |
| `BenchmarkStrategies/jitter/rtt_weighted` | B/op | 6366, 6354, 6363, 6363, 6364 | 6363 | limit | 7635.6 | 7627.2 |
| `BenchmarkStrategies/jitter/rtt_weighted` | allocs/op | 103, 103, 103, 103, 103 | 103 | limit | 123.6 | 123.6 |
| `BenchmarkStrategies/jitter/rtt_weighted` | ns/op | 2.23208e+06, 2.20444e+06, 2.24829e+06, 2.24885e+06, 2.3056e+06 | 2.24829e+06 | limit | 2.69795e+06 | 2.86782e+06 |
| `BenchmarkStrategies/jitter/rtt_weighted` | p50-us | 2303, 2283, 2315, 2314, 2343 | 2314 | limit | 2776.8 | 2973.6 |
| `BenchmarkStrategies/jitter/rtt_weighted` | p99-us | 3447, 3417, 3506, 3448, 3487 | 3448 | limit | 4137.6 | 4407.6 |
| `BenchmarkStrategies/jitter/strict` | B/op | 6329, 6329, 6327, 6328, 6327 | 6328 | limit | 7593.6 | 7593.6 |
| `BenchmarkStrategies/jitter/strict` | allocs/op | 102, 102, 102, 102, 102 | 102 | limit | 122.4 | 122.4 |
| `BenchmarkStrategies/jitter/strict` | ns/op | 2.19727e+06, 2.22052e+06, 2.22511e+06, 2.21018e+06, 2.20717e+06 | 2.21018e+06 | limit | 2.65221e+06 | 2.87011e+06 |
| `BenchmarkStrategies/jitter/strict` | p50-us | 2291, 2295, 2275, 2279, 2286 | 2286 | limit | 2743.2 | 2989.2 |
| `BenchmarkStrategies/jitter/strict` | p99-us | 3426, 3411, 3415, 3425, 3448 | 3425 | limit | 4110 | 4392 |
| `BenchmarkStrategies/slow-first/parallel_best` | B/op | 13059, 13060, 13060, 13059, 13061 | 13060 | limit | 15672 | 15666 |
| `BenchmarkStrategies/slow-first/parallel_best` | allocs/op | 203, 203, 203, 203, 203 | 203 | limit | 243.6 | 243.6 |
| `BenchmarkStrategies/slow-first/parallel_best` | ns/op | 93897, 93251, 93093, 101086, 95085 | 93897 | limit | 112676 | 157238 |
| `BenchmarkStrategies/slow-first/parallel_best` | p50-us | 87, 87, 87, 95, 89 | 87 | limit | 104.4 | 144 |
| `BenchmarkStrategies/slow-first/parallel_best` | p99-us | 223, 219, 209, 231, 216 | 219 | limit | 262.8 | 386.4 |
| `BenchmarkStrategies/slow-first/random` | B/op | 6264, 6277, 6271, 6272, 6271 | 6271 | limit | 7525.2 | 7519.2 |
| `BenchmarkStrategies/slow-first/random` | allocs/op | 101, 101, 101, 101, 101 | 101 | limit | 121.2 | 121.2 |
| `BenchmarkStrategies/slow-first/random` | ns/op | 840108, 814667, 786388, 828348, 780948 | 814667 | limit | 977600 | 1.1831e+06 |
| `BenchmarkStrategies/slow-first/random` | p50-us | 132, 119, 122, 124, 122 | 122 | limit | 146.4 | 340.8 |
| `BenchmarkStrategies/slow-first/random` | p99-us | 2337, 2354, 2424, 2350, 2354 | 2354 | limit | 2824.8 | 3116.4 |
| `BenchmarkStrategies/slow-first/rtt_weighted` | B/op | 6269, 6279, 6269, 6270, 6280 | 6270 | limit | 7524 | 7528.8 |
| `BenchmarkStrategies/slow-first/rtt_weighted` | allocs/op | 102, 102, 102, 102, 102 | 102 | limit | 122.4 | 122.4 |
| `BenchmarkStrategies/slow-first/rtt_weighted` | ns/op | 138023, 128597, 133166, 141601, 151991 | 138023 | limit | 165628 | 230468 |
| `BenchmarkStrategies/slow-first/rtt_weighted` | p50-us | 83, 76, 78, 84, 97 | 83 | limit | 99.6 | 130.8 |
| `BenchmarkStrategies/slow-first/rtt_weighted` | p99-us | 2250, 2234, 2235, 2257, 2281 | 2250 | limit | 2700 | 2841.6 |
| `BenchmarkStrategies/slow-first/strict` | B/op | 6329, 6330, 6333, 6329, 6331 | 6330 | limit | 7596 | 7580.4 |
| `BenchmarkStrategies/slow-first/strict` | allocs/op | 102, 102, 102, 102, 102 | 102 | limit | 122.4 | 122.4 |
| `BenchmarkStrategies/slow-first/strict` | ns/op | 2.32044e+06, 2.29637e+06, 2.26936e+06, 2.31463e+06, 2.29281e+06 | 2.29637e+06 | limit | 2.75565e+06 | 2.96409e+06 |
| `BenchmarkStrategies/slow-first/strict` | p50-us | 2312, 2290, 2268, 2308, 2289 | 2290 | limit | 2748 | 2977.2 |
| `BenchmarkStrategies/slow-first/strict` | p99-us | 2455, 2439, 2388, 2426, 2388 | 2426 | limit | 2911.2 | 3200.4 |

### udp

| Metric | Unit | Five values | Median | Rule | Proposed bound | Legacy bound |
| --- | --- | --- | ---: | --- | ---: | ---: |
| `BenchmarkUDPSockets/sockets=1` | B/op | 2310, 2311, 2311, 2310, 2310 | 2310 | limit | 2772 | 2772 |
| `BenchmarkUDPSockets/sockets=1` | allocs/op | 33, 33, 33, 33, 33 | 33 | limit | 39.6 | 39.6 |
| `BenchmarkUDPSockets/sockets=1` | lost | 0, 0, 0, 0, 0 | 0 | zero | 0 | 0 |
| `BenchmarkUDPSockets/sockets=1` | ns/op | 6467, 6836, 6938, 6904, 7218 | 6904 | limit | 8284.8 | 10525.2 |
| `BenchmarkUDPSockets/sockets=1` | q/s | 154632, 146281, 144126, 144834, 138546 | 144834 | minimum | 115867 | 91212 |
| `BenchmarkUDPSockets/sockets=6` | B/op | 2360, 2362, 2371, 2370, 2370 | 2370 | limit | 2844 | 2842.8 |
| `BenchmarkUDPSockets/sockets=6` | allocs/op | 33, 33, 33, 33, 33 | 33 | limit | 39.6 | 39.6 |
| `BenchmarkUDPSockets/sockets=6` | lost | 0, 0, 0, 0, 0 | 0 | zero | 0 | 0 |
| `BenchmarkUDPSockets/sockets=6` | ns/op | 4256, 4327, 4170, 4236, 4429 | 4256 | limit | 5107.2 | 6828 |
| `BenchmarkUDPSockets/sockets=6` | q/s | 234954, 231131, 239782, 236059, 225801 | 234954 | minimum | 187963 | 140590 |

### dot

| Metric | Unit | Five values | Median | Rule | Proposed bound | Legacy bound |
| --- | --- | --- | ---: | --- | ---: | ---: |
| `BenchmarkDoTSockets/sockets=1/churn=false` | B/op | 2367, 2369, 2370, 2367, 2368 | 2368 | limit | 2841.6 | 2845.2 |
| `BenchmarkDoTSockets/sockets=1/churn=false` | allocs/op | 34, 34, 34, 34, 34 | 34 | limit | 40.8 | 40.8 |
| `BenchmarkDoTSockets/sockets=1/churn=false` | failed | 0, 0, 0, 0, 0 | 0 | zero | 0 | 0 |
| `BenchmarkDoTSockets/sockets=1/churn=false` | ns/op | 5388, 5265, 5694, 5252, 5884 | 5388 | limit | 6465.6 | 8218.8 |
| `BenchmarkDoTSockets/sockets=1/churn=false` | q/s | 185602, 189943, 175608, 190393, 169944 | 185602 | minimum | 148482 | 116799 |
| `BenchmarkDoTSockets/sockets=1/churn=true` | B/op | 129625, 129614, 130461, 129618, 129562 | 129618 | limit | 155542 | 155550 |
| `BenchmarkDoTSockets/sockets=1/churn=true` | allocs/op | 919, 919, 920, 919, 919 | 919 | limit | 1102.8 | 1102.8 |
| `BenchmarkDoTSockets/sockets=1/churn=true` | failed | 0, 0, 0, 0, 0 | 0 | zero | 0 | 0 |
| `BenchmarkDoTSockets/sockets=1/churn=true` | ns/op | 247384, 243551, 249544, 260107, 266000 | 249544 | limit | 299453 | 298706 |
| `BenchmarkDoTSockets/sockets=1/churn=true` | q/s | 4042, 4106, 4007, 3845, 3759 | 4007 | minimum | 3205.6 | 3213.6 |
| `BenchmarkDoTSockets/sockets=6/churn=false` | B/op | 2369, 2368, 2370, 2367, 2370 | 2369 | limit | 2842.8 | 2846.4 |
| `BenchmarkDoTSockets/sockets=6/churn=false` | allocs/op | 34, 34, 34, 34, 34 | 34 | limit | 40.8 | 40.8 |
| `BenchmarkDoTSockets/sockets=6/churn=false` | failed | 0, 0, 0, 0, 0 | 0 | zero | 0 | 0 |
| `BenchmarkDoTSockets/sockets=6/churn=false` | ns/op | 5639, 5275, 5690, 5174, 5840 | 5639 | limit | 6766.8 | 8101.2 |
| `BenchmarkDoTSockets/sockets=6/churn=false` | q/s | 177330, 189564, 175739, 193291, 171227 | 177330 | minimum | 141864 | 118496 |
| `BenchmarkDoTSockets/sockets=6/churn=true` | B/op | 129609, 129593, 129578, 129578, 129599 | 129593 | limit | 155512 | 155516 |
| `BenchmarkDoTSockets/sockets=6/churn=true` | allocs/op | 919, 919, 919, 919, 919 | 919 | limit | 1102.8 | 1102.8 |
| `BenchmarkDoTSockets/sockets=6/churn=true` | failed | 0, 0, 0, 0, 0 | 0 | zero | 0 | 0 |
| `BenchmarkDoTSockets/sockets=6/churn=true` | ns/op | 294240, 262947, 260867, 356718, 284552 | 284552 | limit | 341462 | 299165 |
| `BenchmarkDoTSockets/sockets=6/churn=true` | q/s | 3399, 3803, 3833, 2803, 3514 | 3514 | minimum | 2811.2 | 3208.8 |

### quic

| Metric | Unit | Five values | Median | Rule | Proposed bound | Legacy bound |
| --- | --- | --- | ---: | --- | ---: | ---: |
| `BenchmarkQUICSockets/doh3/sockets=1` | B/op | 18254, 18239, 18245, 18236, 18248 | 18245 | limit | 21894 | 21884.4 |
| `BenchmarkQUICSockets/doh3/sockets=1` | allocs/op | 262, 262, 262, 262, 262 | 262 | limit | 314.4 | 314.4 |
| `BenchmarkQUICSockets/doh3/sockets=1` | ns/op | 35771, 40055, 35420, 35779, 37169 | 35779 | limit | 42934.8 | 45196.8 |
| `BenchmarkQUICSockets/doh3/sockets=4` | B/op | 18237, 18218, 18224, 18235, 18230 | 18230 | limit | 21876 | 21870 |
| `BenchmarkQUICSockets/doh3/sockets=4` | allocs/op | 262, 262, 262, 262, 262 | 262 | limit | 314.4 | 314.4 |
| `BenchmarkQUICSockets/doh3/sockets=4` | ns/op | 38519, 41132, 36201, 35519, 34418 | 36201 | limit | 43441.2 | 42603.6 |
| `BenchmarkQUICSockets/doq/sockets=1` | B/op | 10069, 10046, 10040, 10043, 10047 | 10046 | limit | 12055.2 | 12046.8 |
| `BenchmarkQUICSockets/doq/sockets=1` | allocs/op | 138, 138, 138, 138, 138 | 138 | limit | 165.6 | 165.6 |
| `BenchmarkQUICSockets/doq/sockets=1` | ns/op | 46711, 26018, 25132, 26951, 26635 | 26635 | limit | 31962 | 34975.2 |
| `BenchmarkQUICSockets/doq/sockets=4` | B/op | 10037, 10022, 10019, 10041, 10030 | 10030 | limit | 12036 | 12034.8 |
| `BenchmarkQUICSockets/doq/sockets=4` | allocs/op | 138, 138, 138, 138, 138 | 138 | limit | 165.6 | 165.6 |
| `BenchmarkQUICSockets/doq/sockets=4` | ns/op | 26749, 26289, 25200, 31587, 25811 | 26289 | limit | 31546.8 | 31488 |

### persist

| Metric | Unit | Five values | Median | Rule | Proposed bound | Legacy bound |
| --- | --- | --- | ---: | --- | ---: | ---: |
| `BenchmarkPersist200k/gzip=false/restore` | B/op | 3.78623e+08, 3.78492e+08, 3.78858e+08, 3.78959e+08, 3.78925e+08 | 3.78858e+08 | limit | 4.5463e+08 | 4.54293e+08 |
| `BenchmarkPersist200k/gzip=false/restore` | allocs/op | 1.27839e+06, 1.27774e+06, 1.27898e+06, 1.27937e+06, 1.27941e+06 | 1.27898e+06 | limit | 1.53478e+06 | 1.5336e+06 |
| `BenchmarkPersist200k/gzip=false/restore` | ns/op | 1.62914e+09, 1.65537e+09, 1.78748e+09, 1.63891e+09, 1.67876e+09 | 1.65537e+09 | limit | 1.98644e+09 | 1.07548e+09 |
| `BenchmarkPersist200k/gzip=false/save` | B/op | 5.15819e+08, 5.15745e+08, 5.1595e+08, 5.15915e+08, 5.15915e+08 | 5.15915e+08 | limit | 6.19098e+08 | 6.18881e+08 |
| `BenchmarkPersist200k/gzip=false/save` | allocs/op | 591435, 591198, 591773, 591956, 591921 | 591773 | limit | 710128 | 709616 |
| `BenchmarkPersist200k/gzip=false/save` | bytes | 8.72994e+07, 8.72669e+07, 8.73525e+07, 8.73791e+07, 8.73726e+07 | 8.73525e+07 | limit | 1.04823e+08 | 1.04746e+08 |
| `BenchmarkPersist200k/gzip=false/save` | entries | 197033, 196958, 197153, 197214, 197199 | 197153 | minimum | 157722 | 157606 |
| `BenchmarkPersist200k/gzip=false/save` | ns/op | 1.44152e+09, 1.56908e+09, 1.56099e+09, 1.46943e+09, 1.5227e+09 | 1.5227e+09 | limit | 1.82725e+09 | 1.38043e+09 |
| `BenchmarkPersist200k/gzip=true/restore` | B/op | 2.24653e+08, 2.24623e+08, 2.24577e+08, 2.24524e+08, 2.25109e+08 | 2.24623e+08 | limit | 2.69548e+08 | 2.69659e+08 |
| `BenchmarkPersist200k/gzip=true/restore` | allocs/op | 1.3174e+06, 1.31664e+06, 1.3166e+06, 1.31653e+06, 1.31986e+06 | 1.31664e+06 | limit | 1.57997e+06 | 1.58264e+06 |
| `BenchmarkPersist200k/gzip=true/restore` | ns/op | 1.81494e+09, 1.83084e+09, 1.80856e+09, 2.05444e+09, 1.87579e+09 | 1.83084e+09 | limit | 2.19701e+09 | 1.24105e+09 |
| `BenchmarkPersist200k/gzip=true/save` | B/op | 2.02201e+08, 2.02122e+08, 2.02119e+08, 2.02052e+08, 2.02458e+08 | 2.02122e+08 | limit | 2.42546e+08 | 2.42585e+08 |
| `BenchmarkPersist200k/gzip=true/save` | allocs/op | 591984, 591807, 591751, 591595, 593266 | 591807 | limit | 710168 | 710306 |
| `BenchmarkPersist200k/gzip=true/save` | bytes | 8.7541e+06, 8.77162e+06, 8.77209e+06, 8.74943e+06, 8.79599e+06 | 8.77162e+06 | limit | 1.05259e+07 | 1.04317e+07 |
| `BenchmarkPersist200k/gzip=true/save` | entries | 197163, 197103, 197088, 197034, 197590 | 197103 | minimum | 157682 | 157743 |
| `BenchmarkPersist200k/gzip=true/save` | ns/op | 1.04635e+09, 1.19014e+09, 1.10508e+09, 1.06562e+09, 1.09761e+09 | 1.09761e+09 | limit | 1.31713e+09 | 8.05516e+08 |

The raw calibration directory retains every native log/receipt, complete report, original-source manifest, exact command plan, shape rules and result. No native process is rerun by this verifier or report generator.
