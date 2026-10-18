package keepalived

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/robertherbaugh/kudurru/ee/edge"
)

// Track-file weights (dev-docs/design-record.md §3.6): keepalived multiplies the
// value by the track_file weight (-1) and adds it to the priority. With
// every instance on nopreempt only Fault moves a VIP away from the current
// MASTER; PreferNot only matters for the next election. These mirror
// vip.Serving, vip.PreferNot and vip.Fault.
const (
	WeightServing   = 0
	WeightPreferNot = 60
	WeightFault     = 300
)

// TrackFilePath is the track_file of a VIP: <runDir>/weight-<vip>.
func TrackFilePath(runDir, vip string) string {
	return filepath.Join(runDir, "weight-"+vip)
}

// WriteWeight atomically writes w to the VIP's track file (mode 0644:
// keepalived reads it as root, `kudurru vip` commands as any user).
func WriteWeight(runDir, vip string, w int) error {
	if w < 0 || w > 1000 {
		return fmt.Errorf("keepalived: weight %d out of range", w)
	}
	if err := edge.WriteFileAtomic(TrackFilePath(runDir, vip), []byte(strconv.Itoa(w)+"\n"), 0o644); err != nil { //nolint:gosec // keepalived must read it
		return fmt.Errorf("keepalived: write track file: %w", err)
	}
	return nil
}

// ReadWeight returns the VIP's current weight; a missing file is
// WeightServing (keepalived's init_file 0).
func ReadWeight(runDir, vip string) (int, error) {
	b, err := os.ReadFile(TrackFilePath(runDir, vip))
	if errors.Is(err, os.ErrNotExist) {
		return WeightServing, nil
	}
	if err != nil {
		return 0, err
	}
	w, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("keepalived: track file %s: %w", TrackFilePath(runDir, vip), err)
	}
	return w, nil
}
