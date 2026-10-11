# kudurru-dnsdist

This image contains two separate programs:

- **dnsdist** from the official `powerdns/dnsdist-21` image, unmodified,
  licensed under the GNU General Public License v2.0 (see `LICENSE.dnsdist`).
  Source: https://github.com/PowerDNS/pdns (the `pdns/dnsdistdist` directory)
  and https://downloads.powerdns.com/releases/.
- **kudurru**, Apache-2.0, acting as the supervisor (`kudurru edge run`).

They communicate only through dnsdist's configuration file, console and HTTP
API. kudurru does not link against or modify dnsdist. The digest of the
upstream image is pinned in the Dockerfile and bumped by the CVE tracking job.

## Supervisor contract

`kudurru edge run` (see `kudurru edge run --help` and docs/m3/edge.md):

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
