# syntax=docker/dockerfile:1.7
# ghcr.io/robertherbaugh/kudurru-base:<version>[-debug]: the distroless base
# of the ko release images with the writable directories kudurru uses:
#
#   /var/lib/kudurru  0755 65532:65532  dataDir (state.db, querylog.db, ...)
#   /var/lib/kudurru-querylog, /var/lib/kudurru-local
#                    0755 65532:65532  opt-in separate SQLite mounts
#   /run/kudurru      1777 65532:65532  admin socket, files shared with the
#                                       edge containers (953)
#
# A fresh Docker named volume (docker run -v kudurru-data:/var/lib/kudurru,
# every compose file) takes the owner and mode of the image's directory at
# its mount point; distroless has neither directory, so the volume would come
# up root-owned 0755 and kudurru (uid 65532) could not open its database.
# ko cannot add directories or set owners, so publish.yml builds this image
# first and passes its digest to goreleaser (KUDURRU_BASE_IMAGE,
# KUDURRU_DEBUG_BASE_IMAGE; .goreleaser.yaml kos.base_image). The Dockerfile
# at the repository root and images/hostnetwork create the same directories.
# hack/volume-check.sh checks every image.
#
#   docker build -t kudurru-base images/base
#   docker build --build-arg BASE_IMAGE=gcr.io/distroless/static:debug-nonroot@sha256:... images/base
ARG BASE_IMAGE=gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

# Only directories: the stage runs on the build platform for every target.
FROM --platform=$BUILDPLATFORM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS dirs
RUN mkdir -p /out/var/lib/kudurru /out/var/lib/kudurru-querylog /out/var/lib/kudurru-local /out/run/kudurru \
 && chmod 0755 /out/var/lib/kudurru /out/var/lib/kudurru-querylog /out/var/lib/kudurru-local && chmod 1777 /out/run/kudurru \
 && chown 65532:65532 /out/var/lib/kudurru /out/var/lib/kudurru-querylog /out/var/lib/kudurru-local /out/run/kudurru

FROM ${BASE_IMAGE}
# The parents' contents: COPY of a directory creates the target 0755 and
# drops the 1777; this keeps the stage's owner and mode.
COPY --from=dirs /out/var/lib/ /var/lib/
COPY --from=dirs /out/run/ /run/
USER 65532:65532
VOLUME ["/var/lib/kudurru"]
