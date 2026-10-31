package config

import "slices"

// NodeLogSource retains the original process host layer independently of the
// desired API overlay. Treat it as immutable, like its owning Config.
type NodeLogSource struct {
	Baseline   Log
	FileLocked []string
}

// NodeLogLocked reports explicit host-file authority over one log leaf.
func NodeLogLocked(c *Config, path string) bool {
	return c != nil && c.NodeLogSource != nil && slices.Contains(c.NodeLogSource.FileLocked, path)
}

// NodeLogBaseline returns the original host/default/environment log values.
func NodeLogBaseline(c *Config) Log {
	if c.NodeLogSource != nil {
		return c.NodeLogSource.Baseline
	}
	return c.Log
}

// CopyNodeLogSource preserves detached source metadata through Config decoding.
func CopyNodeLogSource(dst, src *Config) {
	if src.NodeLogSource == nil {
		return
	}
	source := *src.NodeLogSource
	source.FileLocked = slices.Clone(source.FileLocked)
	dst.NodeLogSource = &source
}

// WithNodeLogOverlay applies local preferences below explicit host file and
// environment values. It neither mutates the input nor reads a live source.
func WithNodeLogOverlay(host *Config, level, format *string) (*Config, error) {
	next := *host
	CopyNodeLogSource(&next, host)
	if next.NodeLogSource == nil {
		next.NodeLogSource = &NodeLogSource{Baseline: host.Log}
	}
	next.Log = next.NodeLogSource.Baseline
	if level != nil && !NodeLogLocked(&next, "log.level") && !slices.Contains(next.EnvLocked, "log.level") {
		next.Log.Level = *level
	}
	if format != nil && !NodeLogLocked(&next, "log.format") && !slices.Contains(next.EnvLocked, "log.format") {
		next.Log.Format = *format
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return &next, nil
}
