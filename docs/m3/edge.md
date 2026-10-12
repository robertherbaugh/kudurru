# M3/M4 work package: edge (dnsdist and keepalived supervisors)

Branch `worktree-wf_287eb68b-e67-5`, based on `3e52e5a`. Implements §3.6 in full: the dnsdist supervisor (`kudurru edge render|run`), the keepalived supervisor (`kudurru vip render|run|fifo|rebalance|drain|probe`), the keepalived `vip.Provider` and a kube-vip/MetalLB annotation stub, the EndpointSlice and static membership sources, the VRRP probe, and the two images. Decisions it implements: D16-D20, D45/D56/D72 (PROXYv2 TLV secret), D46/D53 (keepalived from source), D51 (lameduck never moves the VIP) and D92 (internal credentials for the zone poll).

There is no dnsdist or keepalived binary on this machine and no Docker. The renderers are covered by golden tests and structural checks. The supervisors are tested end to end against fake `dnsdist` and `keepalived` binaries: the test binary re-executes itself through a symlink. `make edge-check` runs the real validators inside the pinned images; it has not been run here (see "What edge-check needs").

## What was built

### Packages

| Package | Files | What |
|---|---|---|
| `ee/edge` | `topology.go`, `options.go`, `child.go` | `Topology` (binds, pools, backends, health check, console, webserver, rules, DynBlocks, PROXYv2 secret, eBPF), `Build(Options, []Backend, Credentials)`, `Validate`, `BackendID` = uuid5(fixed namespace, node name), per-start `NewCredentials`, `FindTLS`, `ValidSecret`, plus the process helpers both supervisors share: `Child` (start, signal, stop with SIGTERM→SIGKILL), `Check` (validator run), `WriteFileAtomic`, `SwapFile` (previous file kept as `.prev`) |
| `ee/edge/dnsdist` | `render.go`, `render_yaml.go`, `render_lua.go`, `api_client.go`, `supervisor.go` | YAML renderer (`dnsdist.yml` plus a functions-only `dnsdist.lua` sibling holding the 2.1 validation functions), Lua renderer (`dnsdist.conf`, the D17 fallback), API client (`GET /api/v1/servers/localhost`, `DELETE /api/v1/cache`), supervisor |
| `ee/edge/keepalived` | `render.go`, `supervisor.go`, `fifo.go`, `trackfile.go` | `keepalived.conf` renderer, supervisor, FIFO line parser + `Consumer` (state file), track-file weights |
| `ee/edge/vip` | `keepalived.go`, `service.go` (`provider.go` existed) | `Keepalived` provider; `ServiceProvider` stub for kube-vip/metallb; `WithReason`/`ReasonLameduck` enforcing D51; `AuditVIPChanged`, `EventMasterChanged` names |
| `internal/cluster` | `source.go` (new), `internalapi/zones.go` (new) | `MembershipSource` with `StaticSource` and `EndpointSliceSource` (discovery.k8s.io/v1 via the typed client only), `ParseStaticEndpoints`, `ParseSourceSpec`, `FilterServing`; `internalapi.Client.Zones` + `PathZones` (client side of `/internal/zones`) |
| `internal/health` | `vrrp.go` (new) | `VRRPProbe`: TXT `health.kudurru.internal.` over UDP and TCP to the VIP while it is local (loopback otherwise), plus `>= RequireBackends` up in the dnsdist API, bounded by 900 ms |
| `cmd/kudurru` | `edge.go`, `vip.go`, `health_vrrp.go` (new), one line in `main.go` | the commands; `installVRRPHealth(cmd)` hook for the health command |

All are additive. No existing test was changed. The shared-package diffs are new files only (`internal/cluster/source.go`, `internal/cluster/internalapi/zones.go`, `internal/health/vrrp.go`).

### dnsdist: the rendered configuration

The golden files are in `ee/edge/dnsdist/testdata/<case>/{dnsdist.yml,dnsdist.lua,dnsdist.conf}`. There are six cases: `single-loopback` (compose `edge` profile), `static-3` (ha-static), `endpointslice` (built through the real `EndpointSliceSource` against a fake clientset; a not-ready pod is filtered out), `tls` (every protocol on two listen addresses), `authoritative-cache`, and `dynblocks-ebpf`. Every rendered file contains:

- **Pools.** `resolver` (chashed, packet cache 200k entries) and `authoritative` (leastOutstanding, uncached; `--authoritative-cache` adds a 20k-entry cache with `max_ttl` 30 s). `load_balancing_policies.servfail_on_no_server: true`.
- **Backends.** `id` = uuid5(node name), so the chashed ring is stable. `use_proxy_protocol: true`, weight 100, 2 sockets, and both pools. Health checks: mode auto, qname `health.kudurru.internal.`, TXT, interval 1, timeout 1000 ms, max_failures 2, rise 2, must_resolve. Named backends also get `validation_function: kudurru_validate_<id>`, defined in the sibling `dnsdist.lua`. It parses the answer with `newDNSPacketOverlay` and requires the first TXT string to be `<node>` or to start with `<node> `. Anonymous static backends (`--backends 10.0.0.11:5353`) have no node name to check and rely on must_resolve.
- **Query rules**, in order, each with a stable name and a uuid5:
  1. `kudurru-refuse-xfr-notify-update`: `Or{Opcode Notify, Opcode Update, QType AXFR, QType IXFR}`, wrapped in `And{…, Not{NetmaskGroup --transfer-sources}}` when sources are given. Action `RCode Refused`.
  2. `kudurru-proxy-protocol-secret`: `All` → `SetAdditionalProxyProtocolValue{proxy_type: 224, value: <secret>}`.
  3. `kudurru-authoritative-zones`: `QNameSuffix` → `Pool authoritative`.
  4. `kudurru-resolver-default`: `All` → `Pool resolver`.
- **Console and webserver.** Both are loopback only. `Validate` rejects anything else, because the credentials are per start and the API has no TLS. The console key is base64 of 32 random bytes. The API key and dashboard password are 32 random bytes in hex, rendered with `hash_plaintext_credentials: true`. `api_read_write: false`.
- **DynBlocks** (`--dynblocks`): one group `kudurru` (masks /32, /64) with `query-rate` and `rcode-rate NXDomain` rules. **eBPF** (`--ebpf`): `ebpf.{ipv4,ipv6,qnames}.max_entries` plus `general.capabilities_to_retain: [CAP_SYS_ADMIN]`.
- **Lua renderer.** The same topology through `setLocal/addLocal/addTLSLocal/addDOHLocal/addDOQLocal/addDOH3Local`, `newPacketCache`, `setPoolServerPolicy`, `newServer{…}` + `setHealthCheckResponseValidator`, `addAction(…, {name=, uuid=})`, `dynBlockRulesGroup` + `maintenance()`, and `newBPFFilter`/`setDefaultBPFFilter`. `enableLuaConfiguration()` is never rendered; a test asserts this.

Field names follow `dnsdist-settings-definitions.yml`, `dnsdist-selectors-definitions.yml` and `dnsdist-actions-definitions.yml` on the `rel/dnsdist-2.1.x` branch (read, not copied). One example is `proxy_type`, not `type`, for the TLV action.

**Secret format.** The PROXYv2 secret must be printable ASCII without spaces or quotes, at least 32 bytes (`openssl rand -hex 32`). YAML escapes are Unicode code points, not bytes, so a binary secret cannot reach dnsdist byte for byte. The file is read with `proxyproto.LoadSecretFile`, which applies the same trailing CR/LF trim as the server.

### dnsdist: the exact command lines

```
dnsdist --check-config -C /etc/dnsdist/dnsdist.candidate.yml    # sibling dnsdist.candidate.lua next to it
dnsdist --supervised --disable-syslog -C /etc/dnsdist/dnsdist.yml
# --renderer lua: dnsdist.candidate.conf / dnsdist.conf
```

### `kudurru edge run`

1. Build the topology from the flags and the membership source: `--backends [name=]host[:port],…` (static), or `--backends-from endpointslice:<ns>/<svc>`. The port named `--backend-port-name` (default `dns`) is used, falling back to `--backend-port` 5353. The name comes from `targetRef.name`, else `hostname`. Only ready endpoints are rendered; when none is ready, the serving ones are, which covers a rollout replacing every pod at once. Generate per-start credentials.
2. Write candidates (0600, since they hold credentials), run `--check-config`, then swap siblings first and the main file last, keeping `*.prev`. Write the API key to `--api-key-out` with mode 0640, optionally chgrp'd with `--api-key-group`.
3. Start dnsdist as a child. SIGHUP, SIGUSR1 and SIGUSR2 are forwarded. SIGTERM/SIGINT stop dnsdist (SIGTERM, then SIGKILL after `--stop-timeout`, default 10 s) and exit 0. If dnsdist exits unexpectedly, it is restarted with backoff from 0.5 s up to 30 s.
4. Membership events, zone-set changes and certificate changes (fsnotify on `--tls-dir`, which handles the Secret `..data` symlink swap) are debounced together for `--debounce` (5 s), then re-rendered. If the rendered bytes are unchanged and no certificate changed, nothing happens. Otherwise readiness reports 503 for `--drain` (1 s) and dnsdist restarts (v0.1, D17). A certificate change re-runs `--check-config` before the restart. A failed re-render or check keeps the running dnsdist and configuration.
5. The zone poll runs every `--zone-poll` (5 s) against each backend's `https://<pod>:8082/internal/zones`, using credentials derived from `KUDURRU_ENCRYPTION_KEY[_FILE]` exactly as serve does (`readEncryptionKeys`), or from `--internal-token-file`/`--internal-ca-file`. A serial change sends `DELETE /api/v1/cache?pool=authoritative&name=<zone>&suffix=true` when `--authoritative-cache` is on. A change in the zone set re-renders the routing rule. A backend that answers 404 is logged once, and `--authoritative-zones` remains the only routing input.
6. `:8084` serves `/healthz/ready` (dnsdist running, API status fresh, at least one backend `up`; the message reports "N of M backends up"), `/healthz/live`, `/status` (endpoints, zones, dnsdist status JSON) and `/metrics`. The metrics are `kudurru_edge_backend_up{backend}` (reset on every poll, so removed backends disappear), `kudurru_edge_dnsdist_restarts_total{reason=membership|zones|certificates|exit}`, `kudurru_edge_renders_total{result=ok|unchanged|invalid|check_failed}`, `kudurru_edge_cache_flushes_total` and `kudurru_edge_backends`, plus Go and process collectors.
7. Logs are structured slog, JSON by default (`--log-format text`). D56 is enforced up front: non-loopback backends without `--proxy-protocol-secret-file` are refused, and an endpointslice source always needs the secret.

### keepalived: the rendered configuration

Golden files are in `ee/edge/keepalived/testdata/`: `two-vips-node-a.conf` and `two-vips-node-c.conf` (reversed priorities 200/180 and 180/200), `legacy.conf` and `preempt-delay.conf`. Each rendered file has:

- `global_defs`: `router_id`, `enable_script_security`, `script_user keepalived_script`, `vrrp_version 3`, `vrrp_garp_master_refresh 30`, `vrrp_notify_fifo /run/kudurru/vrrp.fifo`, `vrrp_notify_fifo_script "/usr/local/bin/kudurru vip fifo --node <node>"` (keepalived appends the FIFO path).
- One `vrrp_script chk_<vip>` per VIP, with interval 1, timeout 1, fall 3, rise 2, weight 0 (FAULT on failure) and `user keepalived_script`. One `track_file tf_<vip>` per VIP: `file "/run/kudurru/weight-<vip>"`, `weight -1`, `init_file 0`.
- One `vrrp_instance VI_<vip>` per VIP: state BACKUP, interface, `virtual_router_id`, priority, advert_int 1, then nopreempt (or `preempt_delay N` with `--preempt-delay vip=N`), unicast_src_ip, unicast_peer, unicast_fault_no_peer, check_unicast_src, then `auth_hmac { key 1 file:<--hmac-key-file with {vrid}>; active_key 1; anti_replay monotonic }` (dropped with `--legacy`), `virtual_ipaddress { <addr>/<len> dev <iface> }`, track_script and track_file.
- **Priority.** 200 - 10*idx. idx is `--priority-index` for even VIP positions and `node-count-1-index` for odd ones; never 255, at most 20 nodes.
- **Validation.** VRIDs 1-255 and unique; peers in the same family as the source and never the source itself; at least one peer; every rendered string restricted to a safe character set, so nothing can inject a directive or a script.

The keywords were checked against `keepalived.conf.5.in` of the 2.4.3 tarball (sha256 `a0faef8e…a9632`, now pinned in `images/keepalived/Dockerfile`): `auth_hmac` with `key N file:`, `active_key`, `anti_replay time|monotonic`, `init_file [VALUE] [overwrite]`, and `vrrp_notify_fifo_script` receiving the FIFO as its last parameter.

### keepalived: the exact command lines

```
keepalived -t -f /etc/keepalived/keepalived.candidate.conf
keepalived --dont-fork --log-console --vrrp --use-file /etc/keepalived/keepalived.conf
/usr/local/bin/kudurru vip fifo --node <node> /run/kudurru/vrrp.fifo          # started by keepalived
/usr/local/bin/kudurru vip probe --dns <vip>:53 --qname health.kudurru.internal. \
  --dnsdist-api http://127.0.0.1:8083 --dnsdist-api-key-file /run/kudurru/dnsdist-api.key --require-backends 1
```

### `kudurru vip run|fifo|rebalance|drain|probe`

- **run**: render, `-t`, swap (keepalived.conf.prev kept), then start keepalived. A `--peers-file` change (one address per line; fsnotify on the directory, so ConfigMap updates work) re-renders, checks and sends SIGHUP. `Rerender` refuses to change the VRID of an existing VIP. If keepalived exits unexpectedly it is restarted with backoff. SIGTERM gives keepalived up to `--stop-timeout` (15 s, under compose's 20 s) to release the VIPs. `:8085` serves `/healthz/*`, `/status` and `/metrics`, with `kudurru_vip_master{vip}`, `kudurru_vip_priority{vip}` and `kudurru_vip_reloads_total{result}`. The supervisor polls the state file each second and logs each transition with `event=vip.master_changed audit=cluster.vip.changed`, then calls `SupervisorConfig.OnEvent`.
- **fifo**: parses `INSTANCE "VI_<vip>" STATE PRIO` lines. It handles `MASTER_PRIORITY`/`BACKUP_PRIORITY`, ignores `MASTER_RX_LOWER_PRI`, maps `DELETED` to STOP, and logs and skips bad lines. It maintains `/run/kudurru/vip-state.json` (`{node, updated, vips:{name:{state,priority,since}}}`) with atomic writes. The FIFO is opened read-write, so the open never blocks and a keepalived reload never produces EOF. This consumer never blocks keepalived.
- **rebalance**: for every VIP that this node holds at a priority below 200 (or any held VIP named with `--vip`), write 300, wait until this node is no longer MASTER, then write 0. On timeout it resets to 0 and fails.
- **drain** `<vip>…` writes 300; `--undo` writes 0. This is D51's explicit way to move a VIP.
- **probe**: the track_script, implemented by `health.VRRPProbe` (see the health hook below).

### VIP providers

- `vip.NewKeepalived(runDir, node, vips)`. `Owner` reports this node while it is MASTER, according to the FIFO state; otherwise `ok=false`. keepalived only knows its own state, so the Nodes page scatter-gathers. `SetWeight` writes the track file and accepts only Serving 0, PreferNot 60 or Fault 300. A context tagged `vip.WithReason(ctx, vip.ReasonLameduck)` gets `ErrLameduckFault` for Fault (D51). `VIPs` reports Owner and the current track-file weight. `Run(ctx)` polls the state file and emits `Events()` without blocking: if nobody reads, the event is dropped and logged.
- `vip.NewServiceProvider("kube-vip"|"metallb", vips)`. `Annotations(vip, sharingKey)` returns `kube-vip.io/loadbalancerIPs`, or `metallb.io/loadBalancerIPs` plus `metallb.io/allow-shared-ip`. `SetWeight` is a no-op and `Owner` is unknown (stub).

### Health hook (deliverable 4)

`cmd/kudurru/health.go` belongs to the CLI agent and builds the command in `newHealthCmd()`. There is no `healthCmd` variable, and `main.go` may only gain the one `AddCommand` line, so the init() coupling is unworkable from this package. As the brief allows, the probe ships as **`kudurru vip probe`**, which keepalived.conf renders by default (`keepalived.DefaultProbeCommand`). `cmd/kudurru/health_vrrp.go` also defines `installVRRPHealth(cmd *cobra.Command)`. It adds `--vrrp`, `--dns`, `--qname`, `--dnsdist-api`, `--dnsdist-api-key-file` and `--require-backends` to the health command, and dispatches to the probe when `--vrrp` is set. The health command's own `--timeout` bounds the probe only when set explicitly; otherwise the probe uses 900 ms. Once `newHealthCmd` calls `installVRRPHealth(cmd)`, `--probe-command "/usr/local/bin/kudurru health --vrrp"` renders §3.6 verbatim. A test covers the hook against a stand-in command.

## Wiring

**`cmd/kudurru/serve.go`: none required.** The supervisors are separate entrypoints in their own images. The only edit outside the owned files is the single line in `cmd/kudurru/main.go`: `root.AddCommand(newEdgeCmd(), newVIPCmd())`.

Optional integrator wiring, all behind configuration that does not exist yet (`edge.*` and `vip.*` are only `{enabled: false}` in §6.2):

1. **`vip.provider: keepalived` in serve** (the node shares `/run/kudurru` with the keepalived container):
   ```go
   vp := vip.NewKeepalived("/run/kudurru", cfg.Node.Name, []vip.VIP{{Name: "dns", Address: "10.0.0.53", VRID: 53}})
   go func() { _ = vp.Run(ctx) }()
   go func() { for ev := range vp.Events() { /* audit vip.AuditVIPChanged, event vip.EventMasterChanged */ } }()
   ```
   Lameduck must not call `SetWeight(…, vip.Fault)`. If it ever writes a hint, it passes `vip.WithReason(ctx, vip.ReasonLameduck)`, which makes Fault impossible.
2. **`/internal/zones`**: register `internalapi.PathZones` on the internal server, returning `internalapi.ZoneList{Node, Zones: [{name, serial, type}]}` for primary and secondary zones from the zone cache. The client and the edge poller already exist and tolerate the 404 meanwhile.
3. **health.go** (CLI agent): call `installVRRPHealth(cmd)` at the end of `newHealthCmd`.

## Images

- `images/dnsdist/Dockerfile`: pinned to `powerdns/dnsdist-21:2.1.2@sha256:efabf173811e27438bebee5f1f40e047f263ea39093a8f7d6097df494cf1ed67` (from the registry API; the upstream image runs as `pdns` uid 953 with `tini`). `ENTRYPOINT ["/usr/bin/tini","--"]`, `CMD ["/usr/local/bin/kudurru","edge","run","--api-key-out","/run/kudurru/dnsdist-api.key"]`, `/etc/dnsdist` chowned to 953, `/run/kudurru` mode 1777 as a VOLUME, `HEALTHCHECK` through `kudurru health --ops-address 127.0.0.1:8084 --ready`.
- `images/keepalived/Dockerfile`: the keepalived 2.4.3 sha256 is pinned. Adds `tini`. `keepalived_script` is uid/gid **953**, so the 0640 API key written by dnsdist's uid is readable by the probe. `ENTRYPOINT ["/sbin/tini","--"]`, `CMD ["/usr/local/bin/kudurru","vip","run"]`, `HEALTHCHECK` on :8085.
- The previous `ENTRYPOINT ["kudurru","edge","run"]` would have broken the compose files, whose `command: [kudurru, edge, run, …]` would have been appended to it. Commands now override CMD.
- Both READMEs keep their GPL text and source offer, and gained a "Supervisor contract" section.

## What `make edge-check` needs

- Docker and network, plus `openssl` on the host.
- The target builds the kudurru image (`make docker`), then `images/keepalived` (from source, a few minutes) and `images/dnsdist` with `KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:$(VERSION)`. Then:
  - It re-runs the golden tests.
  - It generates a throwaway P-256 certificate into `bin/edge-check/certs`, mode 0644 so uid 953 can read it, for the `tls` case.
  - It runs `dnsdist --check-config -C <case>/dnsdist.yml` and `-C <case>/dnsdist.conf` for every golden case inside the pinned upstream image, with the testdata mounted read-only at `/golden` and the certificate at `/certs`. The `dynblocks-ebpf` case runs as root with `--cap-add SYS_ADMIN`, because `--check-config` creates the BPF maps.
  - It runs `keepalived -t -f <golden>.conf` for every keepalived golden inside the built image, after writing 32-byte HMAC keys (mode 0600) to `/run/secrets/vrrp_hmac_53` and `_54`.
  - Finally it runs `kudurru version` from the built dnsdist image.
- `.github/workflows/edge.yml` runs it on `ubuntu-latest` for PRs touching `ee/edge`, the two command files, `images/` or the Dockerfile, and on demand. Since the v0.1 fix round it is a real gate (no `continue-on-error`).
- The Lua validation helper (`newDNSPacketOverlay`, `getRecordsCountInSection`) only runs when a health check fires. That is covered by hardware spike (b), not by `--check-config`.

First real run (v0.1 fix round, Docker 25 in the sandbox): all 12 dnsdist files and all 4 keepalived files pass. It found three problems, all fixed:

- The keepalived image did not build. `configure` rejects `--disable-libipset` next to `--disable-iptables` (ipsets are part of the iptables support), so the build stopped before compiling. The image now configures with `--disable-iptables --disable-nftables` and links only libcrypto and libmagic.
- `dnsdist --check-config` on the `dynblocks-ebpf` Lua file fails as uid 953: creating a BPF map needs an effective CAP_SYS_ADMIN or CAP_BPF.
- `keepalived -t` rejects an `auth_hmac` key file that is readable by group or others (a security config error, so the supervisor's pre-swap check would refuse every render). The chart mounts the Secret with mode 0400; the compose files now say `chmod 0600 secrets/vrrp_hmac`.

## Measurements

| What | Value |
|---|---|
| Stripped CGO_ENABLED=0 binary, base `3e52e5a` → this branch | 47,808,672 → 48,341,152 bytes (+520 KiB; D75 budget 51,234,000, 94 % used) |
| Rendered `dnsdist.yml` size | 2.3 KiB (one backend) to 4.7 KiB (TLS, 10 binds) |
| Rendered `keepalived.conf` size | 1.4 KiB (one VIP) to 2.3 KiB (two VIPs) |
| YAML render with 100 backends (6 shared CPUs, loaded machine) | 11.3 ms, 6.7 MB, 13.4k allocs per render; once per debounce at most |
| `go test -race` for the edge packages | dnsdist 25 s (8 supervisor tests, each spawns the race-instrumented test binary as the fake), keepalived 9.5 s, vip 1.1 s, edge 1.3 s, cluster 2.2 s, health 1.4 s, cmd 1.7 s |
| Golden files | 18 dnsdist (6 cases × yml/lua/conf) + 4 keepalived |

## Surprises

- **dnsdist YAML naming.** Selector names in the definitions file are mixed case (`opcode`, `all`, `QType`). The YAML `type:` values are the capitalised forms (`Opcode`, `All`), going by the reference section titles (`OpcodeSelector`). The TLV action's parameter is `proxy_type`, because `type` is taken. `make edge-check` is the arbiter.
- **The 2.1 response validator gets a `DNSResponse`** and returns a boolean. dnsdist offers no TXT accessor, so the helper walks the packet with `newDNSPacketOverlay`.
- **Binary PROXYv2 secrets cannot be rendered into YAML.** The supervisor requires printable secrets (above).
- **The old image ENTRYPOINTs conflicted with the compose files** (above). The upstream dnsdist image already ships tini, so tini is PID 1 in both images. The supervisor is its only child, which also takes care of zombie reaping. §3.6 says "the supervisor is PID 1"; the boundary and behaviour are unchanged.
- **The keepalived FIFO script is keepalived's child**, not the supervisor's. So `kudurru vip fifo` publishes through the state file, which the supervisor (metrics), the local kudurru's `vip.Keepalived` provider and `vip rebalance` all read.
- **keepalived only knows its own state.** With unicast VRRP, a BACKUP never learns who the MASTER is, so `Owner` is "this node or unknown".
- **`kudurru lameduck` has no effect on the edge yet.** The ingress stage keeps answering `health.kudurru.internal.` with NOERROR while lameduck, so dnsdist never drains the node (D51 relies on that). This is a follow-up for the ingress owner.

## Follow-ups (added to docs/followups.md)

- `/internal/zones` server route.
- Ingress health answer failing while lameduck (D51).
- `installVRRPHealth` wiring in health.go.
- Compose and chart updates: named static backends for node-id validation, the endpointslice RBAC for the edge ServiceAccount, `--hmac-key-file` per VRID.
- Edge packet-cache flush on blocking toggles and list refreshes.
- `vip.*`/`edge.*` settings keys and serve wiring of the keepalived provider.
- kube-vip/MetalLB `Owner` from Service status.
- `kudurru doctor` warning for `--legacy`.
- v0.2 console diff-apply and `reloadAllCertificates()` (D17, spike (a)).
- The first real `make edge-check` run (done in the v0.1 fix round, see above).
