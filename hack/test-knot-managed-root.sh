#!/usr/bin/env bash
# Qualify managed root anchors (RFC 5011, D398) with actual Knot DNS 3.6.2:
# a separate kudurru process bootstraps from a Knot-signed root, and the RFC
# 5011 rollover timeline runs on freshly Knot-signed wires. Install the exact
# image digest declared in test/interop/knot_catalog_test.go on the private
# daemon first. No default Docker socket or mutable tag is used.
set -euo pipefail
cd "$(dirname "$0")/.."
source hack/pinned-interop.sh
export GOMAXPROCS=2
export GOFLAGS="${GOFLAGS:-} -p=2"
export KUDURRU_E2E=1
export KUDURRU_KNOT=1
export KUDURRU_KNOT_DOCKER_HOST="${KUDURRU_KNOT_DOCKER_HOST:-unix:///tmp/kudurru-lab-docker.sock}"
status=0
interop_test knot-managed-root ./test/interop TestKnotManagedRootBootstrap -- "$@" || status=1
interop_test knot-rfc5011 ./internal/trustanchors TestRFC5011RolloverFromKnotWires -- "$@" || status=1
exit "$status"
