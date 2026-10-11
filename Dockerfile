# syntax=docker/dockerfile:1.7
# Release images are built by goreleaser + ko (.goreleaser.yaml: distroless
# static, nonroot, linux/amd64 + linux/arm64, cosign-signed, SBOM). This
# Dockerfile is the equivalent for local builds and CI smoke tests:
#   docker build -t kudurru:dev .                  # release-shaped image
#   docker build --target debug -t kudurru:dev-debug .
# The distroless bases are pinned by their multi-arch index digest (the same
# digests as base_image in .goreleaser.yaml); bump both together.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w \
      -X github.com/robertherbaugh/kudurru/internal/version.Version=$VERSION \
      -X github.com/robertherbaugh/kudurru/internal/version.Commit=$COMMIT \
      -X github.com/robertherbaugh/kudurru/internal/version.Date=$DATE" \
    -o /out/kudurru ./cmd/kudurru

# -debug variant: the same binary on distroless's busybox image (a shell and
# nslookup for troubleshooting; dig is not part of distroless).
FROM gcr.io/distroless/static:debug-nonroot@sha256:2a581fcbda6320d4d17fd6ff4774bb96e4825d2be9c2bca59f0777b429997f51 AS debug
COPY --from=build /out/kudurru /usr/local/bin/kudurru
USER 65532:65532
VOLUME ["/var/lib/kudurru"]
EXPOSE 53/udp 53/tcp 853/tcp 853/udp 443/tcp 443/udp 8080/tcp 8081/tcp 8082/tcp 9153/tcp
HEALTHCHECK --interval=10s --timeout=3s --start-period=60s --retries=5 CMD ["kudurru", "health", "--ready"]
ENTRYPOINT ["kudurru"]
CMD ["serve"]

# Release-shaped image (the default target). uid 65532 binds :53 because
# Docker starts the container's unprivileged port range at 0 and the chart
# sets net.ipv4.ip_unprivileged_port_start=0 (docs/deploy/helm.md); no file
# capability is needed.
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS release
COPY --from=build /out/kudurru /usr/local/bin/kudurru
USER 65532:65532
VOLUME ["/var/lib/kudurru"]
EXPOSE 53/udp 53/tcp 853/tcp 853/udp 443/tcp 443/udp 8080/tcp 8081/tcp 8082/tcp 9153/tcp
HEALTHCHECK --interval=10s --timeout=3s --start-period=60s --retries=5 CMD ["kudurru", "health", "--ready"]
ENTRYPOINT ["kudurru"]
CMD ["serve"]
