# Disposable container VIP acceptance

`hack/vip-lab/run.py` exercises the product backend, dnsdist supervisor and
keepalived supervisor in two isolated Linux network namespaces. It is useful
transport and failure evidence for F68c/G14/G23/G24. **It does not close the
[release checklist's two-VM gate](../release-checklist.md).**
Containers share one kernel and the backend stores are independent SQLite
seeds. This does not qualify replicated data, VM power loss, hypervisors,
physical networks, Windows, IPv6, HMAC spoof/replay attacks, stale snapshots or
encrypted client transports. Ownership is sampled; transient split-brain
between observations is not ruled out.

## Prerequisites

Use a disposable Linux Docker daemon with bridge networking, working VRRP
unicast between containers, and NET_ADMIN/NET_RAW/NET_BROADCAST capabilities.
The runner refuses to publish ports and does not use host networking. The
host must reach Docker's private bridge directly; remote Docker daemons and
Docker Desktop VM address translation are not supported. Python 3.9+, Docker
CLI and passwordless `sudo chown`/`sudo chmod` are needed. Only file ownership
of private, generated test credentials requires sudo. There are no Python
packages to install.

Build the backend and both supervisor images from the same exact application
commit using the tracked Dockerfiles and a disposable `licensetest` binary.
The runner verifies the backend linker commit and exact external executable
versions (dnsdist 2.1.3, keepalived 2.4.3), and records immutable image IDs.
All three application binaries must report the same expected linker commit.
Never publish these test-license images. No build or pull occurs in this runner.
The provided license defaults to the repository's test-only E2E license file;
it is mounted read-only. Its contents never enter arguments or result output.

For the isolated cloud daemon and the fixed acceptance images (binary-only
layers over the original `a3266d7` images):

```sh
python3 hack/vip-lab/run.py \
  --docker-host unix:///tmp/kudurru-lab-docker.sock \
  --app-sha b8fd96f14833a6a63f5c34682da87adc480568ac \
  --backend-image kudurru:vip-fix-b8fd96f \
  --edge-image kudurru-dnsdist:vip-fix-b8fd96f \
  --vip-image kudurru-keepalived:vip-fix-b8fd96f \
  --output .context/container-vip.json
```

`--app-sha` is the exact linker `Commit`, which may be a short Git SHA. Use the
commit actually built into the images, rather than a guessed representation.
A mismatch fails before the runtime topology is created. The original
source revision must also be recorded by the image builder.

## Topology and assertions

Each run owns a unique label (`io.kudurru.vip-lab.run=<run ID>`) and name prefix,
an internal bridge with a Docker-selected unused subnet, two namespace
holders, and backend/edge/VIP sidecars in each holder's namespace. Named
volumes provide per-node backend data and shared `/run/kudurru`. Random
PROXYv2 credentials are readable only by the backend UID and edge GID. HMAC
files are root-owned 0600 per VRID. The private temporary credential files are
removed on exit. Go processes use GOMAXPROCS=2; no cgroup limits are imposed
because the disposable cloud daemon may reside in a threaded cgroup.

Every edge lists both backends, whose DNS listeners bind `0.0.0.0:5353`.
Authenticated PROXYv2 and exact node-identity TXT checks remain enabled.
Explicit authoritative-zone routing replaces polling in this static lab.
Each backend seeds the same primary `lab.invalid.` zone and exact A answer
`vip-check.lab.invalid. = 203.0.113.68`. Outbound identity-provider acquisition
is not granted; it is unrelated to this private DNS fixture.

The runner uses supervisor-generated configurations without edits, auth_hmac,
nopreempt, real UDP/TCP and local dnsdist-API health checks. It checks two VIPs
through actual interface addresses, FIFO-derived supervisor state and
metrics, then exercises:

- Initial ownership and an explicit rebalance to preferred split ownership.
- Backend lameduck: both edges remove that backend while VIP ownership stays.
- Local edge failure: real probe failure demotes its VIP; healthy peer serves.
- Recovery to healthy BACKUP without preempting the surviving owner.
- Explicit rebalance, graceful VIP-supervisor shutdown and explicit drain.
- Whole-node hard loss by SIGKILL/removal of all four node containers,
  then fresh namespace/product creation with the original persistent volumes
  and original node address, followed by healthy BACKUP/nopreempt checks.

Killing only keepalived in a surviving namespace is never counted as node
loss. Docker disconnect/reconnect is also unsuitable for restoration here:
it renames the restored interface to `eth1`, while the unchanged generated VIP
configuration requires `eth0`. Boot order is never counted as an automatic
rebalance promise.

Four concurrent clients sample both VIPs over UDP and TCP at 100 ms intervals
with 250 ms socket timeouts. Each successful answer must contain the exact
question, owner, A target and AA bit. The JSON records failures and the maximum
time between successful samples. This is a conservative sampled success gap,
not an exact packet-loss count. Transition bounds derive from the unchanged
one-second advert, fall 3, script timeout 1 and backup priority; an explicit
one-second observation allowance covers Docker inspection/client sampling.
Graceful shutdown retains the original 15-second bound. A failed transition,
wrong answer, interruption beyond its bound or cleanup failure fails the run.

## Results and cleanup

The runner writes sanitized JSON and exits nonzero on failure. It never dumps
container logs, configuration, process arguments, environment, authentication
material or packet traces. The result identifies setup stage, image IDs,
versions, case timings, ownership, DNS samples, limits and cleanup outcome.
Keep this artifact with the source/build evidence for review; a failing setup
is not runtime acceptance.

Success, failure, SIGINT and SIGTERM cleanup inspect the UUID label and name
before deleting each owned container, bridge or volume. No global prune or
daemon restart is performed. SIGKILL cannot run cleanup. If interrupted that
way, use the recorded run ID to inspect **only** resources bearing that exact
label and prefix; never remove resources selected by a broad image/name match.
Do not print Docker inspect output, credentials or unfiltered child logs.

Runner-only checks need no Docker daemon:

```sh
python3 -m py_compile hack/vip-lab/run.py hack/vip-lab/test_run.py
python3 -m unittest discover -s hack/vip-lab -p 'test_*.py' -v
```

## Recorded container run (October 31, 2026)

Two complete runs passed after the health-cache fix. The final run completed
all 12 cases in 79.625 seconds. Application version 0.2.0, Go 1.27.3,
linux/amd64; linker commit `b8fd96f14833a6a63f5c34682da87adc480568ac`
(original `a3266d7` plus the health-cache fix, integrated as `d1502a6`).
Real executable versions were dnsdist 2.1.3 and keepalived 2.4.3. Both
`doctor` checks reported two auth_hmac instances, and both YAML and Lua
static-three-backend configurations passed real dnsdist preflight.

The tested binary SHA256 was
`71bbea57c4c6d1debf0b083fbecb9c16e8b233969e77d91c63f1d94fafeedb3e`.
The binary-only overlay image IDs were:

| Role | Image ID |
| --- | --- |
| Backend | `sha256:83801b73d5b2879fb58ee17470a8ed03257ad4c611cf8c6f38d58225c02d4633` |
| Edge | `sha256:89c7f196a871779f5df5da3d7057488f3a4f4ce192f21b37a1fa230b7fd5930e` |
| VIP | `sha256:bca30c6e6606c98aaab20b05dd599e66f1219a5875233f7b4c00dc5a7c8a4817` |

| Case | Ownership observation (seconds) |
| --- | ---: |
| initial two vips | 0.1137 |
| initial explicit rebalance | 0.777 |
| backend lameduck retains vips | 3.2638 |
| local edge failure probe demotes | 4.2382 |
| edge recovery nopreempt | 0.1216 |
| explicit rebalance | 0.6801 |
| graceful vip shutdown | 1.28 |
| vip recovery nopreempt | 0.1242 |
| explicit drain | 0.7571 |
| whole node hard loss | 3.5849 |
| whole node recovery nopreempt | 0.1196 |
| final explicit rebalance | 0.6613 |

Lameduck timing measures removal from both edges while VIP ownership stayed
unchanged. Recovery rows measure the recheck after a healthy MASTER/BACKUP
state was established; they are not startup latency measurements. No sampled
steady state had two owners. Final state was A:dns and B:dns2, with the peer
BACKUP for each VIP.

Across both VIPs/protocols, the final run collected 2,612 samples with zero
wrong answers. The largest UDP/TCP success gaps were 3.2562/3.2614 seconds
for dns and 0.2755/0.2741 seconds for dns2. The recorded 57 failed queries
were bounded transition interruptions; they were counted, not suppressed.
Owned cleanup inventory was zero containers, zero networks and zero volumes.
These are single-kernel container observations with the limitations above,
not completion of the original two-VM acceptance gate.
