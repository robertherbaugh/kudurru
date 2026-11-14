package config

import "github.com/robertherbaugh/kudurru/internal/clientnames"

// ClientNames configures optional local observations, not authentication.
type ClientNames struct {
	ReversePTR ReversePTR `koanf:"reversePTR" json:"reversePTR"`
}

// ReversePTR defines host-only sources and bounds for optional PTR observations.
type ReversePTR struct {
	Enabled     bool     `koanf:"enabled" json:"enabled"`
	Servers     []string `koanf:"servers" json:"servers"`
	Networks    []string `koanf:"networks" json:"networks"`
	MaxEntries  int      `koanf:"maxEntries" json:"maxEntries"`
	Workers     int      `koanf:"workers" json:"workers"`
	Timeout     Duration `koanf:"timeout" json:"timeout"`
	MaxAge      Duration `koanf:"maxAge" json:"maxAge"`
	NegativeTTL Duration `koanf:"negativeTTL" json:"negativeTTL"`
}

// Options captures host-only authority and bounds for the asynchronous resolver.
func (c ReversePTR) Options() clientnames.Options {
	return clientnames.Options{Servers: c.Servers, Networks: c.Networks, MaxEntries: c.MaxEntries, Workers: c.Workers, Timeout: c.Timeout.Std(), MaxAge: c.MaxAge.Std(), NegativeTTL: c.NegativeTTL.Std()}
}

func (c ReversePTR) validate() error {
	if !c.Enabled && len(c.Servers) == 0 && len(c.Networks) == 0 {
		return nil
	}
	return c.Options().Validate()
}
