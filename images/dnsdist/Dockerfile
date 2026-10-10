# kudurru-dnsdist: upstream dnsdist (GPL-2.0, unmodified) plus the kudurru
# supervisor binary as PID 1 (`kudurru edge run`): render dnsdist.yml from the
# topology, `dnsdist --check-config`, exec dnsdist, forward signals, expose
# :8084/healthz/ready. docs/architecture.md §3.6, decision D16.
#
# The two programs are merely aggregated in one image; kudurru never links
# dnsdist. This image ships the GPL-2.0 text and the upstream source location.
# The supervisor arrives in milestone M4; until then the image is a build
# target only.
ARG DNSDIST_IMAGE=powerdns/dnsdist-21
ARG DNSDIST_DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000
ARG KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:0.1.0-dev

FROM ${KUDURRU_IMAGE} AS kudurru

FROM ${DNSDIST_IMAGE}@${DNSDIST_DIGEST}
COPY --from=kudurru /usr/local/bin/kudurru /usr/local/bin/kudurru
COPY README.md /usr/share/doc/kudurru-dnsdist/README.md
COPY LICENSE.dnsdist /usr/share/doc/kudurru-dnsdist/LICENSE.dnsdist
USER root
RUN mkdir -p /etc/dnsdist /run/kudurru && chown -R 953:953 /etc/dnsdist /run/kudurru
USER 953
EXPOSE 53/udp 53/tcp 853/tcp 443/tcp 8083/tcp 8084/tcp
ENTRYPOINT ["/usr/local/bin/kudurru", "edge", "run"]
