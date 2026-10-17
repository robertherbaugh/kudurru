# kudurru-keepalived: keepalived 2.4.3 compiled from the upstream release
# tarball (GPL-2.0+, unmodified) plus the kudurru supervisor (`kudurru vip
# run`): render keepalived.conf, `keepalived -t`, run keepalived as a child,
# SIGHUP on re-render; keepalived runs `kudurru vip fifo` as its notify FIFO
# script. dev-docs/design-record.md §3.6, decisions D46/D53.
#
# Alpine's keepalived package is still 2.3.x, which lacks auth_hmac (the only
# authenticated VRRP option), so we build from source. The written source
# offer required by the GPL is in README.md; the tarball and this build script
# are kept with every release.
#
# Process tree: tini (Alpine package, MIT) is PID 1; `kudurru vip run` is its
# only child, keepalived the supervisor's child (it must run as root: VRRP
# sockets and VIP management need NET_ADMIN/NET_RAW). Scripts (the VRRP probe,
# the FIFO consumer) run as keepalived_script under enable_script_security,
# so /usr/local/bin/kudurru is root-owned and not writable.
#
# keepalived_script is uid/gid 953, the uid dnsdist runs as in the
# kudurru-dnsdist image: the 0640 dnsdist API key on the shared volume is
# readable by the probe without extra group plumbing.
#
# Runtime contract:
#   /etc/keepalived   keepalived.conf (+ .candidate, .prev), root
#   /run/kudurru      shared volume `kudurru-run`: vrrp.fifo, weight-<vip>
#                     track files (written by the local kudurru), vip-state.json
#                     (written by `kudurru vip fifo`), dnsdist-api.key (read)
#   /run/secrets/vrrp_hmac_<vrid>  auth_hmac keys, 32-64 ASCII bytes without a
#                     trailing newline (openssl rand -hex 16 | tr -d '\n')
# Port 8085/tcp: supervisor /healthz/*, /status, /metrics (kudurru_vip_master).
# Needs cap_add NET_ADMIN, NET_RAW, NET_BROADCAST and host networking.
ARG ALPINE_VERSION=3.23
ARG KEEPALIVED_VERSION=2.4.3
ARG KEEPALIVED_SHA256=a0faef8e401c143487b131b526df7541c1e33d9b8814642fa9dfe8bb250a9632
ARG KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:0.1.0-dev

FROM ${KUDURRU_IMAGE} AS kudurru

FROM alpine:${ALPINE_VERSION} AS build
ARG KEEPALIVED_VERSION
ARG KEEPALIVED_SHA256
# configure: VRRP only. No LVS (IPVS), no BFD and no firewall integration:
# keepalived only installs iptables/nftables rules for vrrp_strict or
# no_accept, which the rendered configuration never sets. ipsets are part of
# the iptables support, so --disable-iptables already turns libipset off
# (configure.ac rejects an explicit --disable-libipset next to it).
# --enable-json keeps the SIGJSON state dump for troubleshooting. The result
# links only libcrypto (auth_hmac) and libmagic (script file checks): no libnl
# (IPVS only), no glib (DBus only), no pcre2 (--enable-regex only).
RUN apk add --no-cache build-base autoconf automake curl file-dev openssl-dev linux-headers
WORKDIR /src
RUN curl -fsSLo keepalived.tar.gz "https://www.keepalived.org/software/keepalived-${KEEPALIVED_VERSION}.tar.gz" \
 && echo "${KEEPALIVED_SHA256}  keepalived.tar.gz" | sha256sum -c - \
 && tar -xzf keepalived.tar.gz --strip-components=1 \
 && ./configure --prefix=/usr --sysconfdir=/etc --disable-lvs --disable-bfd --enable-json \
      --disable-iptables --disable-nftables \
 && make -j"$(nproc)" && make install DESTDIR=/out \
 && mkdir -p /out/usr/share/keepalived && cp COPYING /out/usr/share/keepalived/COPYING

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache libcrypto3 libmagic iproute2 tini \
 && addgroup -S -g 953 keepalived_script \
 && adduser -S -D -H -u 953 -G keepalived_script keepalived_script \
 && mkdir -p /etc/keepalived /run/kudurru /run/secrets \
 && chmod 1777 /run/kudurru
COPY --from=build /out/usr/sbin/keepalived /usr/sbin/keepalived
COPY --from=build /out/usr/share/keepalived/COPYING /usr/share/doc/kudurru-keepalived/COPYING
# ko puts the binary at /ko-app/kudurru (the Dockerfile at the repository
# root mirrors that layout).
COPY --from=kudurru /ko-app/kudurru /usr/local/bin/kudurru
COPY README.md /usr/share/doc/kudurru-keepalived/README.md
# The licenses of the programs this image ships: the kudurru supervisor
# (Kudurru Community License, plus the ee/ terms) and keepalived
# (GPL-2.0-or-later, compiled unmodified from the upstream tarball). Two
# separate programs, not one combined work (README.md). Alpine's packages
# (iproute2, tini, musl ...) keep their own licenses
# (apk info -L <package>). `make edge-check` asserts this label.
LABEL org.opencontainers.image.licenses="LicenseRef-Kudurru-Community AND GPL-2.0-or-later"
VOLUME ["/run/kudurru"]
EXPOSE 8085/tcp
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/kudurru", "health", "--ops-address", "127.0.0.1:8085", "--ready"]
STOPSIGNAL SIGTERM
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/usr/local/bin/kudurru", "vip", "run"]
