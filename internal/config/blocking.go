package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"time"
)

// Blocking holds the cluster-wide blocking defaults (docs/architecture.md
// §6.2, §7.1 step 7, D58). Policies and BlockLists override the response
// and TTL per client group and per list.
type Blocking struct {
	// Enabled applies blocklists at all; false is a permanent off switch
	// (the temporary disable is a runtime toggle).
	Enabled bool `koanf:"enabled" json:"enabled"`
	// DefaultResponse is zeroIP, nxdomain, customIP or refused.
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
	// RefreshInterval between the leader's downloads.
	RefreshInterval Duration `koanf:"refreshInterval" json:"refreshInterval"`
}

// BlockingCustomIP is the customIP block response.
type BlockingCustomIP struct {
	V4 string `koanf:"v4" json:"v4"`
	V6 string `koanf:"v6" json:"v6"`
}

// Block responses accepted by blocking.defaultResponse (the BlockResponse
// values of api/v1alpha1).
var blockResponses = map[string]bool{"zeroIP": true, "nxdomain": true, "customIP": true, "refused": true}

func (b *Blocking) validate() []error {
	var errs []error
	if !blockResponses[b.DefaultResponse] {
		errs = append(errs, fmt.Errorf("blocking.defaultResponse: unknown response %q (zeroIP, nxdomain, customIP, refused)", b.DefaultResponse))
	}
	if b.BlockTTL < 0 {
		errs = append(errs, fmt.Errorf("blocking.blockTTL: %s is negative", b.BlockTTL))
	}
	if v := b.CustomIP.V4; v != "" {
		if a, err := netip.ParseAddr(v); err != nil || !a.Is4() {
			errs = append(errs, fmt.Errorf("blocking.customIP.v4: %q is not an IPv4 address", v))
		}
	}
	if v := b.CustomIP.V6; v != "" {
		if a, err := netip.ParseAddr(v); err != nil || !a.Is6() || a.Is4In6() {
			errs = append(errs, fmt.Errorf("blocking.customIP.v6: %q is not an IPv6 address", v))
		}
	}
	if b.DefaultResponse == "customIP" && b.CustomIP.V4 == "" && b.CustomIP.V6 == "" {
		errs = append(errs, fmt.Errorf("blocking.defaultResponse customIP needs blocking.customIP.v4 or blocking.customIP.v6"))
	}
	switch b.InitStrategy {
	case "fast", "blocking", "failOnError":
	default:
		errs = append(errs, fmt.Errorf("blocking.initStrategy: unknown strategy %q (fast, blocking, failOnError)", b.InitStrategy))
	}
	if b.RefreshInterval.Std() < time.Minute {
		errs = append(errs, fmt.Errorf("blocking.refreshInterval: %s is shorter than 1m", b.RefreshInterval))
	}
	if u := b.Catalogue.URL; u != "" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			errs = append(errs, fmt.Errorf("blocking.catalogue.url: %q is not an http or https URL", u))
		}
	}
	if b.Catalogue.RefreshInterval.Std() < time.Minute {
		errs = append(errs, fmt.Errorf("blocking.catalogue.refreshInterval: %s is shorter than 1m", b.Catalogue.RefreshInterval))
	}
	return errs
}
