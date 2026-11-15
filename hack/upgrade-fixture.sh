#!/usr/bin/env bash
# Regenerates the golden state.db files of the 0.1.0 release that the
# in-package upgrade tests open (internal/store/sqlite/testdata/v0.1.0):
#
#   state.db.gz         an ordinary 0.1.0 installation
#   state-legacy.db.gz  the same with the objects 0.1.0 accepted and this
#                       release cannot serve (decision D373)
#
# The files are written by the released binary, not by today's code: the
# upgrade tests of test/e2e (TestUpgradeFromV01DataDirectory and
# TestUpgradeFromV01RefusesLegacyObjects) run `kudurru serve` of 0.1.0, use
# it through its own API and admin socket, stop it, and copy the state.db it
# left when KUDURRU_E2E_WRITE_V01_FIXTURES names a directory. This script
# sets that variable and runs the two tests, so the committed files are what
# the tests themselves upgrade.
#
# The 0.1.0 binary is built from the v0.1.0 tag of this clone
# (`git fetch origin tag v0.1.0` in a shallow clone) in a temporary directory
# outside the working tree, or is the file KUDURRU_E2E_PREVIOUS_BINARY names
# (the published release binary). Run it only to change what the fixtures
# contain: every run produces new UIDs, timestamps and credentials, so the
# files differ byte for byte each time. After a run, adjust the object counts
# in internal/store/sqlite/golden_v01_test.go if the recipe changed and run
#
#   go test ./internal/store/sqlite -run 'ReleasedV01'
#   go test ./cmd/kudurru -run 'TestRunServeSQLiteUpgrade'
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
out=$root/internal/store/sqlite/testdata/v0.1.0
mkdir -p "$out"

cd "$root"
KUDURRU_E2E=1 KUDURRU_REQUIRE_PREVIOUS_RELEASE=1 KUDURRU_E2E_WRITE_V01_FIXTURES=$out \
	go test -count=1 -timeout 30m -v \
	-run '^(TestUpgradeFromV01DataDirectory|TestUpgradeFromV01RefusesLegacyObjects)$' ./test/e2e/

ls -l "$out"/*.db.gz
