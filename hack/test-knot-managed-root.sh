#!/usr/bin/env bash
# Qualify managed root anchors (RFC 5011, D398) with actual Knot DNS 3.6.2:
# a separate kudurru process bootstraps from a Knot-signed root, and the RFC
# 5011 rollover timeline runs on freshly Knot-signed wires. Install the exact
# image digest declared in test/interop/knot_catalog_test.go on the private
# daemon first. No default Docker socket or mutable tag is used.
set -euo pipefail
export GOMAXPROCS=2
export GOFLAGS="${GOFLAGS:-} -p=2"
export KUDURRU_E2E=1
export KUDURRU_KNOT=1
export KUDURRU_KNOT_DOCKER_HOST="${KUDURRU_KNOT_DOCKER_HOST:-unix:///tmp/kudurru-lab-docker.sock}"
go test -p 2 ./test/interop -run '^TestKnotManagedRootBootstrap$' -count=1 -v "$@"
exec go test -p 2 ./internal/trustanchors -run '^TestRFC5011RolloverFromKnotWires$' -count=1 -v "$@"
