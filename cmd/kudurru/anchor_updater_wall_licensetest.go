//go:build licensetest

package main

import (
	"os"
	"time"
)

// This file is compiled only with -tags licensetest, which the separate-process
// e2e fixtures and the Playwright suite use (internal/license/testkey.go). It
// lets the enrolled offline recovery test (F28-HA-4, D369) start one primary
// process whose anchor updater sees its committed RFC 5011 refresh as due
// without waiting the one-hour minimum: KUDURRU_TEST_ANCHOR_UPDATER_AHEAD=<d>
// moves that process's updater wall clock d ahead, for its due and checkpoint
// comparisons only. Observation samples, receipts and stored state keep the
// real session clock, so the acquisition and everything it commits are
// production behavior. A release binary never carries this file.
const anchorUpdaterAheadEnv = "KUDURRU_TEST_ANCHOR_UPDATER_AHEAD"

func init() {
	ahead, err := time.ParseDuration(os.Getenv(anchorUpdaterAheadEnv))
	if err != nil || ahead <= 0 {
		return
	}
	anchorUpdaterBuildWall = func() time.Time { return time.Now().Add(ahead) }
}
