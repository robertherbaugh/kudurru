# syntax=docker/dockerfile:1.7
# Release images are built by goreleaser + ko (distroless static, nonroot, multi-arch).
# This Dockerfile is the equivalent for local builds and CI smoke tests.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG VERSION=dev
ARG COMMIT=unknown
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X github.com/robertherbaugh/kudurru/internal/version.Version=$VERSION -X github.com/robertherbaugh/kudurru/internal/version.Commit=$COMMIT" \
    -o /out/kudurru ./cmd/kudurru

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/kudurru /usr/local/bin/kudurru
# uid 65532 (nonroot). Binding :53 as non-root needs cap_net_bind_service; the
# release pipeline sets the file capability, compose/Helm examples use :5353 or
# NET_BIND_SERVICE as documented in docs/architecture.md §5.2 and §6.1.
USER 65532:65532
VOLUME ["/var/lib/kudurru"]
EXPOSE 53/udp 53/tcp 853/tcp 853/udp 443/tcp 443/udp 8080/tcp 8081/tcp 9153/tcp
HEALTHCHECK --interval=10s --timeout=3s --start-period=60s --retries=5 CMD ["kudurru", "health", "--ready"]
ENTRYPOINT ["kudurru"]
CMD ["serve"]
