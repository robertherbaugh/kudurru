# `bun run dev` only (wrangler.jsonc, env "local"): the portal image that
# `make license-server-image` builds, under a name wrangler can build.
# Wrangler builds whatever `image` names by piping the Dockerfile to
# `docker build -f -`, which cannot build license-server/Dockerfile itself
# (BuildKit then ignores its allowlist, Dockerfile.dockerignore, and reads
# the repository's .dockerignore, which excludes license-server/). This
# file adds nothing. On Apple silicon build the image for linux/amd64:
#   docker build --platform linux/amd64 -f license-server/Dockerfile \
#     -t ghcr.io/robertherbaugh/kudurru-license-server:dev .
FROM ghcr.io/robertherbaugh/kudurru-license-server:dev
