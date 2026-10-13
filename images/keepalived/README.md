# kudurru-keepalived

This image contains two separate programs:

- **keepalived** 2.4.3, compiled unmodified from the upstream release tarball,
  licensed under the GNU General Public License v2.0 or later (see `COPYING`).
- **kudurru**, source-available (PolyForm Noncommercial 1.0.0 plus the `ee/`
  terms; see LICENSING.md), acting as the supervisor (`kudurru vip run`). The
  supervisor needs a license key (`KUDURRU_LICENSE_KEY`) that covers the edge.

The image label `org.opencontainers.image.licenses` says so as an SPDX
expression, `PolyForm-Noncommercial-1.0.0 AND GPL-2.0-or-later`: each program
keeps its own license, and neither license applies to the other program. The
terms of kudurru (the key, the noncommercial limit) do not restrict what you do
with keepalived, which you can run without kudurru under the GPL. Alpine's
packages (iproute2, tini, musl) keep their own licenses.

## Written offer for source code

The complete corresponding source of keepalived as shipped in this image is
the upstream release tarball `keepalived-2.4.3.tar.gz` from
https://www.keepalived.org/software/ (SHA-256 recorded in the Dockerfile). The
Dockerfile in this directory is the complete build script. Both are kept with
every kudurru release at https://github.com/robertherbaugh/kudurru/releases.
On request to the kudurru maintainers we will provide the tarball on physical
media for the cost of distribution, as required by GPL-2.0 §3(b).

kudurru does not link against or modify keepalived. They communicate only
through keepalived.conf, `track_file`, the notify FIFO and signals.

## Supervisor contract

`kudurru vip run` (see `kudurru vip run --help` and docs/m3/edge.md):

1. renders `/etc/keepalived/keepalived.candidate.conf` from its flags
   (`--vip name=addr/len@iface#vrid`, `--peers`/`--peers-file`,
   `--priority-index`, `--hmac-key-file`, `--legacy` for 2.3.x),
2. runs `keepalived -t -f /etc/keepalived/keepalived.candidate.conf`, then
   renames it over `keepalived.conf` (previous kept as `keepalived.conf.prev`),
3. starts `keepalived --dont-fork --log-console --vrrp --use-file
   /etc/keepalived/keepalived.conf` as its child; re-renders (peers file
   changes) are checked the same way and applied with SIGHUP; a VRID never
   changes on re-render,
4. keepalived runs `/usr/local/bin/kudurru vip fifo --node <node>
   /run/kudurru/vrrp.fifo` as its notify FIFO script, which maintains
   `/run/kudurru/vip-state.json`; the supervisor mirrors it into
   `kudurru_vip_master{vip}` on `:8085/metrics`,
5. the VRRP check is `kudurru health --vrrp` (the rendered default since
   the M5 burndown: udp+tcp DNS on the VIP, loopback while BACKUP, and at least
   one dnsdist backend up; `kudurru vip probe --dns <vip>:53 ...` still
   works for hand-written configurations),
6. weights are written by the local kudurru to `/run/kudurru/weight-<vip>`
   (0 serving, 60 prefer-not, 300 fault); `kudurru vip drain <vip>` and
   `kudurru vip rebalance` are the operator's tools.

tini (Alpine package, MIT) is PID 1.
