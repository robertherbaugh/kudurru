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
