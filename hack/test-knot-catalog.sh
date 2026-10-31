#!/usr/bin/env bash
# Qualify the RFC 9432 ordinary-primary catalog fixture with actual Knot 3.6.2.
# Install the exact image digests declared in test/interop/knot_catalog_test.go
# on the private daemon first. No default Docker socket or mutable tag is used.
set -euo pipefail
export GOMAXPROCS=2
export GOFLAGS="${GOFLAGS:-} -p=2"
export KUDURRU_E2E=1
export KUDURRU_KNOT=1
export KUDURRU_KNOT_DOCKER_HOST="${KUDURRU_KNOT_DOCKER_HOST:-unix:///tmp/kudurru-lab-docker.sock}"
exec go test -p 2 ./test/interop -run '^TestKnotCatalogConsumer$' -count=1 "$@"
