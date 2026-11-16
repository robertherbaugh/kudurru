# syntax=docker/dockerfile:1.7
# The kudurru license portal (license-server/): the licensor's service that
# issues license keys after an OIDC sign-in. It is not part of kudurru and
# is not released with it; no node ever contacts it (D163). Published as
# ghcr.io/robertherbaugh/kudurru-license-server by
# .github/workflows/license-server.yml on license-server-v* tags, for
# linux/amd64 and linux/arm64. Build from the repository root, because the
# image carries the public keys kudurru releases trust
# (internal/license/keys/*.pub):
#   docker build -f license-server/Dockerfile -t kudurru-license-server:dev .
#   make license-server-image
# BuildKit reads license-server/Dockerfile.dockerignore (an allowlist) for
# this build instead of the root .dockerignore, so local secrets, .context/
# and node_modules never reach the build context.
#
# Runtime contract (license-server/README.md, "Configuration"): one process,
# uid 65532, listening on PORT (8790); configuration from the environment,
# every secret also as <NAME>_FILE; the signing key is mounted at run time
# (LICENSE_SIGNING_KEY_FILE) and never enters the image. GET /readyz answers
# 200 when the database is reachable and migrated; the HEALTHCHECK runs the
# separate dist/healthcheck.js entry, which only fetches /readyz, so it
# reads no secrets and runs no migrations. `bun /app/server.js migrate` and
# `bun /app/server.js grant-superadmin --email ...` are the other commands
# (the ENTRYPOINT takes them as arguments).
#
# Both stages are pinned by their multi-arch index digest; bump them
# together, and with bun-version in the workflows (1.4.3). The build stage
# runs on the build platform and produces only architecture-independent
# JavaScript (no native npm modules), so arm64 needs no QEMU; the runtime
# stage only copies files.

FROM --platform=$BUILDPLATFORM oven/bun:1.4.4@sha256:1d22548f177f4ff38e0c15baa3c2c46c6c2060ba2d1bc573bb4720b407548f1d AS build
WORKDIR /src/license-server
# The manifest and the lockfile first, so the dependency layer is reused
# until they change. Lifecycle scripts of dependencies stay blocked (no
# trustedDependencies). bunfig.tom[l] matches the file when it exists.
COPY license-server/package.json license-server/bun.lock license-server/bunfig.tom[l] ./
RUN --mount=type=cache,target=/root/.bun/install/cache \
    bun install --frozen-lockfile
COPY license-server/ ./
# The code default for LICENSE_TRUSTED_KEYS_DIR is <repo>/internal/license/keys,
# so the keys sit where a checkout has them.
COPY internal/license/keys/ /src/internal/license/keys/
RUN bun run build

# Everything the runtime stage copies, gathered in one tree so it is a
# single layer: the server and health-check bundles, the UI, the trusted
# public keys and the image's notices. Root-owned and not writable by the
# runtime user.
FROM --platform=$BUILDPLATFORM oven/bun:1.4.4@sha256:1d22548f177f4ff38e0c15baa3c2c46c6c2060ba2d1bc573bb4720b407548f1d AS out
COPY --from=build /src/license-server/dist/server.js /src/license-server/dist/healthcheck.js /out/
COPY --from=build /src/license-server/dist/ui/ /out/ui/
COPY --from=build /src/internal/license/keys/ /out/trusted-keys/
COPY --from=build /src/license-server/NOTICE /out/NOTICE
COPY --from=build /src/license-server/LICENSE.md /out/LICENSE.md
RUN set -eu; \
    test -f /out/ui/index.html || { echo "dist/ui/index.html is missing" >&2; exit 1; }; \
    test -f /out/ui/third-party-licenses.txt || { echo "dist/ui/third-party-licenses.txt is missing: the image must carry the notices of its dependencies" >&2; exit 1; }; \
    ls /out/trusted-keys/*.pub > /dev/null; \
    cp /out/ui/third-party-licenses.txt /out/third-party-licenses.txt; \
    chown -R 0:0 /out; \
    chmod -R a+rX,go-w /out

FROM oven/bun:1.4.4-distroless@sha256:153a951ae40106e125898103e6e990ee0f38e6aee7f561029e26cf9fbed23470
# Empty unless the build passes them (make license-server-image and the
# publish job do); config.ts then reports commit and version "dev".
ARG VERSION=
ARG COMMIT=
ARG BUILT_AT=
COPY --from=out /out/ /app/
# LICENSE_SERVER_VERSION, LICENSE_SERVER_COMMIT and LICENSE_SERVER_BUILT_AT
# feed /api/v1/system and the startup log (spec §12; server/config.ts
# buildInfo). LICENSE_TRUSTED_KEYS_DIR names the keys this image was built
# with; compose and tests point it elsewhere.
ENV NODE_ENV=production \
    PORT=8790 \
    LICENSE_TRUSTED_KEYS_DIR=/app/trusted-keys \
    LICENSE_SERVER_VERSION=$VERSION \
    LICENSE_SERVER_COMMIT=$COMMIT \
    LICENSE_SERVER_BUILT_AT=$BUILT_AT
# Every label oven/bun sets is overridden here (it would otherwise name bun
# and licenses=NOASSERTION), so local and compose builds carry the same
# labels as the published image.
LABEL org.opencontainers.image.title="kudurru license portal" \
      org.opencontainers.image.description="The licensor's service that issues kudurru license keys; not part of kudurru, and no node contacts it" \
      org.opencontainers.image.source="https://github.com/robertherbaugh/kudurru" \
      org.opencontainers.image.url="https://github.com/robertherbaugh/kudurru/tree/main/license-server" \
      org.opencontainers.image.vendor="kudurru" \
      org.opencontainers.image.licenses="LicenseRef-Kudurru-License-Server-Source-Viewing" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$COMMIT \
      org.opencontainers.image.created=$BUILT_AT
# The base image runs as root; nonroot (65532) exists in its /etc/passwd.
USER 65532:65532
WORKDIR /app
EXPOSE 8790
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=3 CMD ["bun", "/app/healthcheck.js"]
ENTRYPOINT ["bun", "/app/server.js"]
