package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// prepareServingNodeLog reads committed local preferences before constructing
// the process logger. Explicit host/environment values retain priority. The
// regular Store still owns all later writes through its actual command lane.
func prepareServingNodeLog(ctx context.Context, host *config.Config) (*config.Config, error) {
	if host.Store.Driver != "sqlite" {
		return host, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	local, err := sqlite.ReadNodeLogSettings(ctx, filepath.Join(host.Node.DataDir, sqlite.FileName), host.Node.Name)
	if err != nil {
		return nil, fmt.Errorf("node log settings: %w", err)
	}
	// Capture the final original host inputs (including entrypoint overrides)
	// before any API overlay is applied. Keep exact file leaf ownership.
	baseline := *host
	config.CopyNodeLogSource(&baseline, host)
	if baseline.NodeLogSource == nil {
		baseline.NodeLogSource = &config.NodeLogSource{}
	}
	baseline.NodeLogSource.Baseline = host.Log
	return config.WithNodeLogOverlay(&baseline, local.Level, local.Format)
}
