//go:build !licensetest

package main

import "testing"

// A release build has no way to move the anchor updater's wall clock: the
// KUDURRU_TEST_ANCHOR_UPDATER_AHEAD seam of the separate-process tests
// exists only under -tags licensetest (anchor_updater_wall_licensetest.go).
func TestDefaultBuildHasNoAnchorUpdaterWall(t *testing.T) {
	if anchorUpdaterBuildWall != nil {
		t.Fatal("a default build carries the test anchor updater wall clock")
	}
}
