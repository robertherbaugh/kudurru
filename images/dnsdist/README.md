# kudurru-dnsdist

This image contains two separate programs:

- **dnsdist** from the official `powerdns/dnsdist-21` image, unmodified,
  licensed under the GNU General Public License v2.0 (see `LICENSE.dnsdist`).
  Source: https://github.com/PowerDNS/pdns (the `pdns/dnsdistdist` directory)
  and https://downloads.powerdns.com/releases/.
- **kudurru**, source-available (Kudurru Community License plus the `ee/`
  terms; see LICENSING.md), acting as the supervisor (`kudurru edge run`). The
  supervisor needs a license key (`KUDURRU_LICENSE_KEY`) that covers the edge.

The image label `org.opencontainers.image.licenses` says so as an SPDX
expression, `LicenseRef-Kudurru-Community AND GPL-2.0-only`: each program keeps
its own license, and neither license applies to the other program. The terms of
kudurru (the key, the community-use limits) do not restrict what you do with
dnsdist, which you can run without kudurru under the GPL. The base image
carries other packages under their own licenses (`/usr/share/doc/*/copyright`).

They communicate only through dnsdist's configuration file, console and HTTP
API. kudurru does not link against or modify dnsdist. The digest of the
upstream image is pinned in the Dockerfile and bumped by the CVE tracking job.

## Supervisor contract

`kudurru edge run` (see `kudurru edge run --help` and dev-docs/milestones/m3/edge.md):

1. builds the topology from its flags and the membership source
   (`--backends` or `--backends-from endpointslice:<ns>/<svc>`),
2. renders `/etc/dnsdist/dnsdist.candidate.yml` plus the functions-only
   sibling `dnsdist.candidate.lua` (or `dnsdist.candidate.conf` with
   `--renderer lua`),
3. runs `dnsdist --check-config -C /etc/dnsdist/dnsdist.candidate.yml`, then
   renames the candidates over `dnsdist.yml`/`dnsdist.lua` (the previous files
   are kept as `*.prev`),
4. starts `dnsdist --supervised --disable-syslog -C /etc/dnsdist/dnsdist.yml`
   as its child, forwards SIGHUP/SIGUSR1/SIGUSR2, and on SIGTERM stops dnsdist
   (SIGTERM, then SIGKILL after `--stop-timeout`),
5. writes the per-start API key to `--api-key-out` (0640) and serves
   `:8084/healthz/ready` (ready = dnsdist reports at least one backend up),
   `/healthz/live`, `/status` and `/metrics` (`kudurru_edge_backend_up{backend}`),
6. re-renders and restarts dnsdist after membership changes (debounced 5 s,
   only ready endpoints), certificate changes in `--tls-dir`, and changes of
   the zone set polled from the backends' `/internal/zones`; serial changes
   flush the authoritative packet cache when `--authoritative-cache` is on.

The webserver/API (127.0.0.1:8083) and console (127.0.0.1:5199) are bound
to loopback with credentials generated on every start; they are never
exposed. tini (from the upstream image) is PID 1.

## Users and capabilities

The image runs as `pdns` (uid 953) by default and binds :53 through the
container's `net.ipv4.ip_unprivileged_port_start=0`. Two cases need more than
a non-root process can get from Kubernetes or Docker, which make added
capabilities effective only for root: eBPF DynBlocks (`--ebpf`, creating the
BPF maps needs `CAP_BPF`/`CAP_SYS_ADMIN`) and host networking (the edge
DaemonSet, compose HA), where the sysctl does not apply. There the container
starts as root with group 953 (`user: "0:953"`, chart `runAsUser: 0`,
`runAsGroup: 953`), drops every capability except `NET_BIND_SERVICE`,
`SETUID`, `SETGID` and `KILL` (plus `BPF` and `SYS_ADMIN` for eBPF), and the
supervisor runs dnsdist with `--dnsdist-user=953:953`: dnsdist binds its
sockets and creates the maps as root, then switches to 953 and keeps only
`capabilities_to_retain` (`CAP_SYS_ADMIN` with eBPF). `KILL` lets the root
supervisor signal the switched dnsdist. `make edge-check` runs this case.
