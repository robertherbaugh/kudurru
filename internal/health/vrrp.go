package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/edge/dnsdist"
)

// VRRPProbe is the keepalived track_script check (docs/architecture.md
// §3.6): a real DNS query over UDP and TCP to the VIP while this node
// holds it (loopback otherwise: a loopback-only check passes with a
// dnsdist that is not bound to the VIP), plus at least RequireBackends
// backends up in the local dnsdist API. keepalived runs it every second
// with a 1 s timeout, so the whole probe is bounded by Timeout.
type VRRPProbe struct {
	// DNS is the VIP host:port (10.0.0.53:53).
	DNS string
	// QName is queried for TXT; default health.kudurru.internal.
	QName string
	// DNSDistAPI is the dnsdist webserver base URL; empty skips the
	// backend check.
	DNSDistAPI string
	// DNSDistAPIKeyFile holds the per-start API key.
	DNSDistAPIKeyFile string
	// RequireBackends is the minimum number of backends up; default 1.
	RequireBackends int
	// Timeout bounds the whole probe; default 900 ms.
	Timeout time.Duration
	// LocalAddrs lists this host's addresses; default the interface
	// addresses. Tests inject it.
	LocalAddrs func() ([]netip.Addr, error)
}

// VRRPResult reports what the probe saw.
type VRRPResult struct {
	// Target is the address actually probed (the VIP or loopback).
	Target string
	// HoldsVIP is true when the VIP is assigned to a local interface.
	HoldsVIP bool
	// UDP and TCP are the DNS rcodes ("NOERROR"), empty on failure.
	UDP, TCP string
	// BackendsUp is the dnsdist up count, -1 when not checked.
	BackendsUp int
}

func (r VRRPResult) String() string {
	return fmt.Sprintf("target=%s holds_vip=%t udp=%s tcp=%s backends_up=%d", r.Target, r.HoldsVIP, r.UDP, r.TCP, r.BackendsUp)
}

func interfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr())
		}
	}
	return out, nil
}

// Run executes the probe; a nil error means healthy.
func (p VRRPProbe) Run(ctx context.Context) (VRRPResult, error) {
	res := VRRPResult{BackendsUp: -1}
	if p.QName == "" {
		p.QName = "health.kudurru.internal."
	}
	if p.RequireBackends == 0 {
		p.RequireBackends = 1
	}
	if p.Timeout <= 0 {
		p.Timeout = 900 * time.Millisecond
	}
	if p.LocalAddrs == nil {
		p.LocalAddrs = interfaceAddrs
	}
	ap, err := netip.ParseAddrPort(p.DNS)
	if err != nil {
		return res, fmt.Errorf("vrrp probe: --dns %q: %w", p.DNS, err)
	}
	target := ap
	if local, err := p.LocalAddrs(); err == nil {
		for _, a := range local {
			if a.Unmap() == ap.Addr().Unmap() {
				res.HoldsVIP = true
			}
		}
	}
	if !res.HoldsVIP {
		lo := netip.MustParseAddr("127.0.0.1")
		if ap.Addr().Is6() && !ap.Addr().Is4In6() {
			lo = netip.IPv6Loopback()
		}
		target = netip.AddrPortFrom(lo, ap.Port())
	}
	res.Target = target.String()
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	fail := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for _, network := range []string{"udp", "tcp"} {
		wg.Go(func() {
			rcode, err := query(ctx, network, res.Target, p.QName)
			mu.Lock()
			if network == "udp" {
				res.UDP = rcode
			} else {
				res.TCP = rcode
			}
			mu.Unlock()
			if err != nil {
				fail(fmt.Errorf("dns/%s %s: %w", network, res.Target, err))
			}
		})
	}
	if p.DNSDistAPI != "" {
		wg.Go(func() {
			c := &dnsdist.APIClient{BaseURL: p.DNSDistAPI, KeyFile: p.DNSDistAPIKeyFile}
			st, err := c.Status(ctx)
			if err != nil {
				fail(fmt.Errorf("dnsdist API: %w", err))
				return
			}
			up := st.UpCount()
			mu.Lock()
			res.BackendsUp = up
			mu.Unlock()
			if up < p.RequireBackends {
				fail(fmt.Errorf("dnsdist reports %d of %d backends up, need %d", up, len(st.Servers), p.RequireBackends))
			}
		})
	}
	wg.Wait()
	return res, errors.Join(errs...)
}

func query(ctx context.Context, network, addr, qname string) (string, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), dns.TypeTXT)
	m.RecursionDesired = false
	c := &dns.Client{Net: network}
	if dl, ok := ctx.Deadline(); ok {
		c.Timeout = time.Until(dl)
	}
	r, _, err := c.ExchangeContext(ctx, m, addr)
	if err != nil {
		return "", err
	}
	rc := dns.RcodeToString[r.Rcode]
	if r.Rcode != dns.RcodeSuccess {
		return rc, fmt.Errorf("rcode %s", rc)
	}
	return rc, nil
}
