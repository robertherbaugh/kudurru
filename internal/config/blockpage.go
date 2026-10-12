package config

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// BlockPage configures the block page (docs/deploy/blocking-page.md): the
// blockPage block response answers A and AAAA queries with these addresses,
// and the listeners.blockPage listener serves a page explaining the block
// to the browsers that follow them.
type BlockPage struct {
	// Enabled starts the block page listener and makes the blockPage
	// response available.
	Enabled bool `koanf:"enabled" json:"enabled"`
	// IPv4 and IPv6 are the addresses blocked names resolve to under the
	// blockPage response; empty derives them from the listen addresses
	// (BlockPageAddrs).
	IPv4 string `koanf:"ipv4" json:"ipv4"`
	IPv6 string `koanf:"ipv6" json:"ipv6"`
	// Title, Message and Contact are the page's heading, explanation
	// (plain text) and the "ask your administrator" contact (an email
	// address or an http(s) URL; empty omits the line).
	Title   string `koanf:"title" json:"title"`
	Message string `koanf:"message" json:"message"`
	Contact string `koanf:"contact" json:"contact"`
	// ShowDetails shows the deciding list, rule and policy to the client.
	ShowDetails bool `koanf:"showDetails" json:"showDetails"`
	// DashboardURL is the dashboard base URL the page links administrators
	// to (…/queries?domain=); empty derives it from the block page address
	// and listeners.api.
	DashboardURL string `koanf:"dashboardURL" json:"dashboardURL"`
	// RateLimit is the page requests per minute one client address may
	// make; 0 disables the limit.
	RateLimit int `koanf:"rateLimit" json:"rateLimit"`
}

// UnblockRequests configures the "Request access" form of the block page
// (docs/deploy/blocking-page.md, "Unblock requests"): users ask for a
// blocked name, administrators decide on the Requests page.
type UnblockRequests struct {
	// Enabled shows the form (with blockPage.enabled).
	Enabled bool `koanf:"enabled" json:"enabled"`
	// RateLimit is the requests per hour one client address (IPv6 /64)
	// may submit.
	RateLimit int `koanf:"rateLimit" json:"rateLimit"`
	// MaxPending caps the undecided requests cluster-wide; beyond it the
	// form says requests are paused.
	MaxPending int `koanf:"maxPending" json:"maxPending"`
	// PendingExpiry expires undecided requests; Retention prunes decided
	// and expired ones.
	PendingExpiry Duration `koanf:"pendingExpiry" json:"pendingExpiry"`
	Retention     Duration `koanf:"retention" json:"retention"`
}

// minUnblockAge bounds unblockRequests.pendingExpiry and retention below.
const minUnblockAge = Duration(time.Hour)

// validateUnblockRequests checks the durations (the ints are bounded by
// the schema).
func (c *Config) validateUnblockRequests() []error {
	var errs []error
	u := c.UnblockRequests
	if u.PendingExpiry < minUnblockAge {
		errs = append(errs, errors.New("unblockRequests.pendingExpiry: at least 1h"))
	}
	if u.Retention < minUnblockAge {
		errs = append(errs, errors.New("unblockRequests.retention: at least 1h"))
	}
	return errs
}

// BlockPageListener is the block page's HTTP and HTTPS binds. Every key is
// hostOnly, like the other listeners.
type BlockPageListener struct {
	// HTTP is the plain-HTTP bind (":80"); empty disables it.
	HTTP string `koanf:"http" json:"http"`
	// HTTPS is the optional TLS bind (":443"); empty disables it.
	HTTPS string `koanf:"https" json:"https"`
	// TLS names the certificate of the HTTPS bind; empty generates a
	// self-signed one into <node.dataDir>/keys/.
	TLS TLSFiles `koanf:"tls" json:"tls"`
}

// BlockPage length limits.
const (
	blockPageMaxTitle   = 200
	blockPageMaxMessage = 2000
	blockPageMaxContact = 254
)

// BlockPageAddrs returns the addresses the blockPage response answers:
// blockPage.ipv4 and blockPage.ipv6 when set, else the first specific
// (not unspecified) address of each family among the block page listeners
// or, when those bind every address, among the DNS listeners: clients reach
// the DNS server there, so they reach the block page there too. Loopback
// addresses are skipped when a PROXYv2 edge (listeners.proxyProtocolFrom)
// sits in front, since the clients then talk to the edge. Invalid results
// mean "none" for that family.
func (c *Config) BlockPageAddrs() (v4, v6 netip.Addr) {
	if a, err := netip.ParseAddr(c.BlockPage.IPv4); err == nil && a.Unmap().Is4() {
		v4 = a.Unmap()
	}
	if a, err := netip.ParseAddr(c.BlockPage.IPv6); err == nil && a.Is6() && !a.Is4In6() {
		v6 = a
	}
	if v4.IsValid() || v6.IsValid() {
		return v4, v6
	}
	edge := len(c.Listeners.ProxyProtocolFrom) > 0
	pick := func(addrs []string) (a4, a6 netip.Addr) {
		for _, hp := range addrs {
			a, ok := specificHost(hp)
			if !ok || (edge && a.IsLoopback()) {
				continue
			}
			switch {
			case a.Is4() && !a4.IsValid():
				a4 = a
			case a.Is6() && !a6.IsValid():
				a6 = a
			}
		}
		return a4, a6
	}
	v4, v6 = pick([]string{c.Listeners.BlockPage.HTTP, c.Listeners.BlockPage.HTTPS})
	if v4.IsValid() || v6.IsValid() {
		return v4, v6
	}
	dns := make([]string, 0, len(c.Listeners.DNS))
	for _, l := range c.Listeners.DNS {
		dns = append(dns, l.Address)
	}
	return pick(dns)
}

// specificHost returns the IP of a host:port bind unless it is empty,
// a name or the unspecified address.
func specificHost(hostport string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(host)
	if err != nil || a.IsUnspecified() || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// validateBlockPage checks what the metadata cannot express (the address
// formats, the listener syntax, the URL scheme and the rate bound are
// checked from the schema); it needs the whole Config because the default
// addresses come from the listeners.
func (c *Config) validateBlockPage() []error {
	var errs []error
	b, l := c.BlockPage, c.Listeners.BlockPage
	if utf8.RuneCountInString(b.Title) > blockPageMaxTitle {
		errs = append(errs, fmt.Errorf("blockPage.title: longer than %d characters", blockPageMaxTitle))
	}
	if utf8.RuneCountInString(b.Message) > blockPageMaxMessage {
		errs = append(errs, fmt.Errorf("blockPage.message: longer than %d characters", blockPageMaxMessage))
	}
	if _, err := BlockPageContactURL(b.Contact); err != nil {
		errs = append(errs, err)
	}
	if u := b.DashboardURL; u != "" {
		if p, err := url.Parse(u); err == nil && (p.RawQuery != "" || p.Fragment != "") {
			errs = append(errs, fmt.Errorf("blockPage.dashboardURL: %q must not carry a query or fragment", u))
		}
	}
	if l.HTTP != "" && l.HTTP == l.HTTPS {
		errs = append(errs, fmt.Errorf("listeners.blockPage: http and https bind the same address %q", l.HTTP))
	}
	if (l.TLS.CertFile == "") != (l.TLS.KeyFile == "") {
		errs = append(errs, errors.New("listeners.blockPage.tls: certFile and keyFile must be set together"))
	}
	if c.Blocking.DefaultResponse == "blockPage" && !b.Enabled {
		errs = append(errs, errors.New("blocking.defaultResponse blockPage needs blockPage.enabled: true (the block page listener)"))
	}
	if !b.Enabled {
		return errs
	}
	if l.HTTP == "" && l.HTTPS == "" {
		errs = append(errs, errors.New("blockPage.enabled needs listeners.blockPage.http or listeners.blockPage.https"))
	}
	if v4, v6 := c.BlockPageAddrs(); !v4.IsValid() && !v6.IsValid() && b.IPv4 == "" && b.IPv6 == "" {
		errs = append(errs, errors.New("blockPage.enabled: no address to send blocked clients to: set blockPage.ipv4 and/or blockPage.ipv6 "+
			"to an address of this node (or of a VIP or load balancer in front of the block page), "+
			"or bind listeners.blockPage.http or a DNS listener to a specific address; every listener binds all addresses (0.0.0.0 / ::)"))
	}
	return errs
}

// BlockPageContactURL returns the link of blockPage.contact: mailto: for an
// email address, the URL itself for an http(s) URL, "" for no contact.
func BlockPageContactURL(contact string) (string, error) {
	c := strings.TrimSpace(contact)
	switch {
	case c == "":
		return "", nil
	case len(c) > blockPageMaxContact:
		return "", fmt.Errorf("blockPage.contact: longer than %d characters", blockPageMaxContact)
	case strings.HasPrefix(c, "http://") || strings.HasPrefix(c, "https://"):
		if p, err := url.Parse(c); err == nil && p.Host != "" {
			return p.String(), nil
		}
	case strings.HasPrefix(c, "mailto:"):
		if a, err := mail.ParseAddress(strings.TrimPrefix(c, "mailto:")); err == nil && !strings.ContainsAny(a.Address, "?&") {
			return "mailto:" + a.Address, nil
		}
	default:
		if a, err := mail.ParseAddress(c); err == nil && a.Address == c && !strings.ContainsAny(c, "?&") {
			return "mailto:" + c, nil
		}
	}
	return "", fmt.Errorf("blockPage.contact: %q is neither an email address nor an http(s) URL", contact)
}
