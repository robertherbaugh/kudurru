# Week-1 spike reports

Each report records what was built, the exact commands, the measurements against the gates in [architecture.md](../architecture.md) §7.3, §13.1 and §14, and what changed in the architecture as a result (decisions D70 to D78).

| Spike | Report | Verdict | Headline numbers |
|---|---|---|---|
| (c) kube Store driver on envtest | [store-kube.md](store-kube.md) | pass | conformance 7/7 (one case rewritten, D70); elector never exits on lease loss; List 1,000 objects 45 ms; +18.8 MiB binary for client-go/controller-runtime |
| (d) blocking engine | [blocking.md](blocking.md) | pass | p99 1.18 µs hit / 0.71 µs miss at 1 M entries; 90 MB steady RSS at 3 M; 0.84 s compile; 0 allocs per lookup |
| (e) OIDC login against dex | [auth-oidc.md](auth-oidc.md) | pass | 546-byte session cookie for a 200-group user; 17/17 dex e2e subtests; seal 2.8 µs / open 3.7 µs |
| (g) CRD + CEL validator | [store-validator.md](store-validator.md) | pass | +12.9 MB binary (+6.7 MB with controller-runtime); found two CRDs the apiserver refused (fixed) |
| (i) DoT/DoH/DoQ/DoH3 + PROXYv2 | [dnsserver-quic.md](dnsserver-quic.md) | pass | all six transports through one registry; 15/15 forgery cases dropped; +448 KiB for quic-go; DoQ 100 k q/s multiplexed on loopback |

Still open because the sandbox has no Docker, kind, dnsdist or second VM: (a) dnsdist console under YAML, (b) PROXYv2 + chashed with a real dnsdist, (f) mixed-protocol LoadBalancer on kind + MetalLB, (h) keepalived 2.4.3 on two VMs.
