# kudurru-keepalived

This image contains two separate programs:

- **keepalived** 2.4.3, compiled unmodified from the upstream release tarball,
  licensed under the GNU General Public License v2.0 or later (see `COPYING`).
- **kudurru**, Apache-2.0, acting as the supervisor (`kudurru vip run`).

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
