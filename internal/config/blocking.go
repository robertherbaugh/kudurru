package config

import (
	"fmt"
	"time"
)

// Blocking holds the cluster-wide blocking defaults (docs/architecture.md
// §6.2, §7.1 step 7, D58). Policies and BlockLists override the response
// and TTL per client group and per list.
type Blocking struct {
	// Enabled applies blocklists at all; false is a permanent off switch
	// (the temporary disable is a runtime toggle).
	Enabled bool `koanf:"enabled" json:"enabled"`
	// DefaultResponse is a BlockResponses value.
	DefaultResponse string `koanf:"defaultResponse" json:"defaultResponse"`
	// BlockTTL is the TTL of synthesised block responses.
	BlockTTL Duration `koanf:"blockTTL" json:"blockTTL"`
	// AllowDegraded keeps the node ready while referenced lists are not
	// loaded yet.
	AllowDegraded bool `koanf:"allowDegraded" json:"allowDegraded"`
	// CustomIP holds the answers of the customIP response.
	CustomIP BlockingCustomIP `koanf:"customIP" json:"customIP"`
	// RefreshInterval applies to BlockLists without spec.refreshInterval.
	RefreshInterval Duration `koanf:"refreshInterval" json:"refreshInterval"`
	// InitStrategy is fast, blocking or failOnError.
	InitStrategy string `koanf:"initStrategy" json:"initStrategy"`
	// ListsDir is the list body cache; "" means <node.dataDir>/lists.
	ListsDir string `koanf:"listsDir" json:"listsDir"`
	// AllowLocalListURLs lets BlockList URLs reach loopback, link-local
	// (cloud metadata), unspecified and multicast addresses; off by default
	// so a BlockList writer cannot use the node for SSRF.
	AllowLocalListURLs bool `koanf:"allowLocalListURLs" json:"allowLocalListURLs"`
	// Catalogue is the blocked-services and safe-search catalogue source.
	Catalogue BlockingCatalogue `koanf:"catalogue" json:"catalogue"`
}

// BlockingCatalogue configures the runtime refresh of the blocked-services
// and safe-search catalogue (D60).
type BlockingCatalogue struct {
	// URL of a catalogue document; empty serves the embedded catalogue.
	URL string `koanf:"url" json:"url"`
	// AdGuardServicesURL opts in to AdGuard's HostlistsRegistry services
	// (GPL-3.0), downloaded by this instance and merged into the catalogue
	// next to kudurru's own entries; empty (the default) is off.
	AdGuardServicesURL string `koanf:"adguardServicesURL" json:"adguardServicesURL"`
	// RefreshInterval between the leader's downloads.
	RefreshInterval Duration `koanf:"refreshInterval" json:"refreshInterval"`
}

// BlockingCustomIP is the customIP block response.
type BlockingCustomIP struct {
	V4 string `koanf:"v4" json:"v4"`
	V6 string `koanf:"v6" json:"v6"`
}

// validate checks what the metadata cannot express (it checks the
// response, the customIP addresses and the catalogue URL).
func (b *Blocking) validate() []error {
	var errs []error
	if b.BlockTTL < 0 {
		errs = append(errs, fmt.Errorf("blocking.blockTTL: %s is negative", b.BlockTTL))
	}
	if b.DefaultResponse == "customIP" && b.CustomIP.V4 == "" && b.CustomIP.V6 == "" {
		errs = append(errs, fmt.Errorf("blocking.defaultResponse customIP needs blocking.customIP.v4 or blocking.customIP.v6"))
	}
	if b.RefreshInterval.Std() < time.Minute {
		errs = append(errs, fmt.Errorf("blocking.refreshInterval: %s is shorter than 1m", b.RefreshInterval))
	}
	if b.Catalogue.RefreshInterval.Std() < time.Minute {
		errs = append(errs, fmt.Errorf("blocking.catalogue.refreshInterval: %s is shorter than 1m", b.Catalogue.RefreshInterval))
	}
	return errs
}
