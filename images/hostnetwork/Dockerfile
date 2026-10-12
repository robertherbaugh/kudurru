# syntax=docker/dockerfile:1.7
# ghcr.io/robertherbaugh/kudurru:<version>-hostnetwork: the release binary
# with the file capability cap_net_bind_service=+ep, for pods and containers
# in the host network namespace (the chart's hostNetwork.enabled, `docker run
# --network host`). There the namespaced net.ipv4.ip_unprivileged_port_start
# sysctl that lets uid 65532 bind :53 in its own namespace is not available,
# and Kubernetes gives a non-root process no ambient capabilities, so
# NET_BIND_SERVICE has to come from the file. ko cannot set file
# capabilities; publish.yml builds this image from the ko image it just
# pushed (the same binary, the same distroless base digest) and signs it.
#
# Runtime contract: NET_BIND_SERVICE must be in the container's bounding set
# (Kubernetes `capabilities: {drop: [ALL], add: [NET_BIND_SERVICE]}`, Docker
# `--cap-drop ALL --cap-add NET_BIND_SERVICE`). Without it the kernel refuses
# to exec a binary whose file capability it cannot grant ("operation not
# permitted"). allowPrivilegeEscalation: false (no_new_privs) is fine:
# kudurru is the container's entrypoint, and the runtime already holds the
# capability in its permitted set when it execs it.
#
# Local build (make docker-hostnetwork):
#   docker build --build-arg KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:<v> \
#     -t ghcr.io/robertherbaugh/kudurru:<v>-hostnetwork images/hostnetwork
ARG KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:0.1.0-dev

FROM ${KUDURRU_IMAGE} AS kudurru

# setcap only writes the security.capability extended attribute, so this
# stage runs on the build platform for every target platform.
FROM --platform=$BUILDPLATFORM alpine:3.23@sha256:85fe1e81d6758c208f3e1eed4338a1997e19d4be002d4dd32d3100c9a8c010a0 AS caps
RUN apk add --no-cache libcap-utils
COPY --from=kudurru /ko-app/kudurru /out/kudurru
RUN setcap cap_net_bind_service=+ep /out/kudurru \
 && getcap /out/kudurru | grep -q cap_net_bind_service=ep \
 && mkdir -p /out/var/lib/kudurru /out/run/kudurru \
 && chmod 0755 /out/var/lib/kudurru && chmod 1777 /out/run/kudurru \
 && chown 65532:65532 /out/var/lib/kudurru /out/run/kudurru

# The distroless digest of images/base/Dockerfile, with the same writable
# directories (a fresh named volume takes their owner; hack/volume-check.sh).
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=caps /out/kudurru /ko-app/kudurru
# The parents' contents: COPY of a directory creates the target 0755 and
# drops the 1777; this keeps the stage's owner and mode.
COPY --from=caps /out/var/lib/ /var/lib/
COPY --from=caps /out/run/ /run/
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/ko-app
USER 65532:65532
VOLUME ["/var/lib/kudurru"]
EXPOSE 53/udp 53/tcp 853/tcp 853/udp 443/tcp 443/udp 8080/tcp 8081/tcp 8082/tcp 9153/tcp
HEALTHCHECK --interval=10s --timeout=3s --start-period=60s --retries=5 CMD ["kudurru", "health", "--ready"]
ENTRYPOINT ["/ko-app/kudurru"]
CMD ["serve"]
