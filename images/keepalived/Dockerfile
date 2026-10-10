# kudurru-keepalived: keepalived 2.4.3 compiled from the upstream release
# tarball (GPL-2.0+, unmodified) plus the kudurru supervisor as PID 1
# (`kudurru vip run`): render keepalived.conf, `keepalived -t`, exec, SIGHUP on
# re-render, consume the notify FIFO. docs/architecture.md §3.6, decision D46.
#
# Alpine's keepalived package is still 2.3.x, which lacks auth_hmac (the only
# authenticated VRRP option), so we build from source. The written source
# offer required by the GPL is in README.md; the tarball and this build script
# are kept with every release.
#
# UNTESTED: this Dockerfile is the M4 build target and has not been built yet.
ARG ALPINE_VERSION=3.23
ARG KEEPALIVED_VERSION=2.4.3
ARG KEEPALIVED_SHA256=0000000000000000000000000000000000000000000000000000000000000000
ARG KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:0.1.0-dev

FROM ${KUDURRU_IMAGE} AS kudurru

FROM alpine:${ALPINE_VERSION} AS build
ARG KEEPALIVED_VERSION
ARG KEEPALIVED_SHA256
RUN apk add --no-cache build-base autoconf automake curl file-dev glib-dev \
      libnl3-dev libnfnetlink-dev openssl-dev pcre2-dev linux-headers
WORKDIR /src
RUN curl -fsSLo keepalived.tar.gz "https://www.keepalived.org/software/keepalived-${KEEPALIVED_VERSION}.tar.gz" \
 && echo "${KEEPALIVED_SHA256}  keepalived.tar.gz" | sha256sum -c - \
 && tar -xzf keepalived.tar.gz --strip-components=1 \
 && ./configure --prefix=/usr --sysconfdir=/etc --disable-lvs --disable-bfd --enable-json --disable-iptables --disable-libipset \
 && make -j"$(nproc)" && make install DESTDIR=/out \
 && mkdir -p /out/usr/share/keepalived && cp COPYING /out/usr/share/keepalived/COPYING

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache libnl3 openssl glib pcre2 file iproute2 \
 && addgroup -S keepalived_script && adduser -S -G keepalived_script keepalived_script \
 && mkdir -p /etc/keepalived /run/kudurru
COPY --from=build /out/usr/sbin/keepalived /usr/sbin/keepalived
COPY --from=build /out/usr/share/keepalived/COPYING /usr/share/doc/kudurru-keepalived/COPYING
COPY --from=kudurru /usr/local/bin/kudurru /usr/local/bin/kudurru
COPY README.md /usr/share/doc/kudurru-keepalived/README.md
# keepalived itself must run as root (VRRP, VIP management); scripts run as
# keepalived_script under enable_script_security.
ENTRYPOINT ["/usr/local/bin/kudurru", "vip", "run"]
