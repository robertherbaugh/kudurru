# kudurru-dnsdist: upstream dnsdist 2.1 (GPL-2.0, unmodified) plus the kudurru
# supervisor (`kudurru edge run`): render dnsdist.yml from the topology,
# `dnsdist --check-config`, run dnsdist as a child, forward signals, expose
# :8084/healthz/ready. docs/architecture.md §3.6, decisions D16/D17.
#
# The two programs are merely aggregated in one image; kudurru never links
# dnsdist. This image ships the GPL-2.0 text and the upstream source location.
#
# Process tree: tini (from the upstream image, MIT) is PID 1 and reaps
# zombies; `kudurru edge run` is its only child and dnsdist the supervisor's
# child. Compose/Kubernetes `command:` overrides CMD, so
# `command: [kudurru, edge, run, --backends=...]` works unchanged.
#
# Runtime contract (what the supervisor expects):
#   /etc/dnsdist      writable by uid 953: dnsdist.yml + dnsdist.lua (or
#                     dnsdist.conf with --renderer lua), *.candidate.*, *.prev
#   /run/kudurru      shared volume `kudurru-run`: dnsdist-api.key (0640) for
#                     the keepalived probe and the local kudurru's /edge page
#   /certs            optional TLS pair (tls.crt+tls.key), watched (--tls-dir)
#   /run/secrets/proxy_secret  PROXYv2 TLV 0xE0 secret (openssl rand -hex 32)
# Ports: 53/udp+tcp, 853/tcp (DoT), 853/udp (DoQ), 443/tcp (DoH), 443/udp
# (DoH3), 8084/tcp (supervisor: /healthz/*, /status, /metrics). The dnsdist
# API (8083) and console (5199) are loopback-only and never exposed.
#
# `make edge-check` runs `dnsdist --check-config` on the golden files inside
# this pinned upstream image.
ARG DNSDIST_IMAGE=powerdns/dnsdist-21:2.1.2
ARG DNSDIST_DIGEST=sha256:efabf173811e27438bebee5f1f40e047f263ea39093a8f7d6097df494cf1ed67
ARG KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:0.1.0-dev

FROM ${KUDURRU_IMAGE} AS kudurru

FROM ${DNSDIST_IMAGE}@${DNSDIST_DIGEST}
# ko puts the binary at /ko-app/kudurru (the Dockerfile at the repository
# root mirrors that layout).
COPY --from=kudurru /ko-app/kudurru /usr/local/bin/kudurru
COPY README.md /usr/share/doc/kudurru-dnsdist/README.md
COPY LICENSE.dnsdist /usr/share/doc/kudurru-dnsdist/LICENSE.dnsdist
USER root
# /run/kudurru is 1777: the first container to mount the empty named volume
# seeds its mode, and kudurru (65532), dnsdist (953) and keepalived_script
# (953) all write there. /etc/dnsdist is group-writable for a supervisor
# started as root with group 953 and no CAP_DAC_OVERRIDE (--dnsdist-user:
# eBPF, host networking).
RUN mkdir -p /etc/dnsdist /run/kudurru /certs \
 && chown -R 953:953 /etc/dnsdist && chmod 0775 /etc/dnsdist \
 && chmod 1777 /run/kudurru
USER 953
VOLUME ["/run/kudurru"]
EXPOSE 53/udp 53/tcp 853/tcp 853/udp 443/tcp 443/udp 8084/tcp
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=3 \
  CMD ["/usr/local/bin/kudurru", "health", "--ops-address", "127.0.0.1:8084", "--ready"]
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/kudurru", "edge", "run", "--api-key-out", "/run/kudurru/dnsdist-api.key"]
