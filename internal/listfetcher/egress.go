package listfetcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// ErrLocalAddress refuses a list URL that resolves to an address the list
// fetcher may not reach (see DownloadOptions.AllowLocalAddresses). It is a
// content-independent, permanent failure: the fetch is not retried.
var ErrLocalAddress = errors.New("listfetcher: list URLs may not reach loopback, link-local, unspecified or multicast addresses (set blocking.allowLocalListURLs to allow them)")

// metadataAddrs are cloud metadata endpoints outside the link-local ranges:
// AWS IMDS over IPv6 (fd00:ec2::254, in the ULA range LANs use) and the
// Alibaba Cloud metadata service (100.100.100.200, in the CGNAT range).
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),
	netip.MustParseAddr("100.100.100.200"),
}

var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

// localAddress reports whether a is an address a BlockList URL must not
// reach by default: loopback (the node's own services: dnsdist's console
// and webserver, the kubelet, sidecars), link-local (169.254.169.254 and
// fe80::, cloud metadata), unspecified and "this network" (0.0.0.0/8
// connects to the local host on Linux), multicast, and the known cloud
// metadata addresses outside those ranges. Private LAN ranges (RFC 1918,
// RFC 4193) stay reachable: home users host lists on their NAS.
func localAddress(a netip.Addr) bool {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsLoopback(), a.IsUnspecified(), a.IsLinkLocalUnicast(),
		a.IsLinkLocalMulticast(), a.IsInterfaceLocalMulticast(), a.IsMulticast():
		return true
	case a.Is4() && thisNetwork.Contains(a):
		return true
	}
	for _, m := range metadataAddrs {
		if a == m {
			return true
		}
	}
	return false
}

// checkURLHost refuses a URL whose host is a literal local address before
// any request is made (this also covers fetches through a proxy, whose
// dial goes to the proxy rather than to the target).
func checkURLHost(u *url.URL) error {
	host := u.Hostname()
	if a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")); err == nil && localAddress(a) {
		return fmt.Errorf("%w: %s", ErrLocalAddress, a)
	}
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") || strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".localhost") {
		return fmt.Errorf("%w: %s", ErrLocalAddress, host)
	}
	return nil
}

// HTTPClient returns the client a Downloader uses by default for the
// given blocking.allowLocalListURLs value: unless allowLocal, its dialer
// refuses the local addresses BlockList URLs may not reach. The catalogue
// refresher (blocking.catalogue.url) downloads through it too.
func HTTPClient(allowLocal bool) *http.Client {
	if allowLocal {
		return &http.Client{Timeout: DefaultHTTPTimeout, Transport: http.DefaultTransport}
	}
	return guardedClient()
}

// guardTransport refuses every request, the first and each redirect, to
// a literal local host before it is sent. Through an HTTP(S) proxy the
// dialer only ever sees the proxy, so without it a redirect to
// http://169.254.169.254/ would be fetched by the proxy.
type guardTransport struct{ base http.RoundTripper }

func (g guardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkURLHost(req.URL); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return g.base.RoundTrip(req)
}

// guardedClient returns the default list client: DefaultHTTPTimeout, the
// proxy from the environment, and a dialer that refuses local addresses
// at connect time. The check runs on the resolved address of every
// connection (redirect targets and DNS-rebinding answers included); dials
// to a configured HTTP(S) proxy are allowed, and every request URL (the
// first and each redirect target) is checked for a literal local host
// first, which covers fetches through the proxy.
func guardedClient() *http.Client {
	proxies := proxyAddrs()
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: d.Timeout, KeepAlive: d.KeepAlive, ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrLocalAddress, address)
		}
		if localAddress(ap.Addr()) {
			return fmt.Errorf("%w: %s", ErrLocalAddress, ap.Addr())
		}
		return nil
	}}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if proxies[strings.ToLower(addr)] {
			return d.DialContext(ctx, network, addr)
		}
		return guarded.DialContext(ctx, network, addr)
	}
	return &http.Client{Timeout: DefaultHTTPTimeout, Transport: guardTransport{base: tr}}
}

// proxyAddrs lists the host:port of the HTTP(S) proxies named by the
// environment, which the guarded dialer may reach whatever their address.
func proxyAddrs() map[string]bool {
	out := map[string]bool{}
	for _, env := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		v := os.Getenv(env)
		if v == "" {
			continue
		}
		if !strings.Contains(v, "://") {
			v = "http://" + v
		}
		u, err := url.Parse(v)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			switch u.Scheme {
			case "https":
				port = "443"
			case "socks5", "socks5h":
				port = "1080"
			default:
				port = "80"
			}
		}
		out[strings.ToLower(net.JoinHostPort(u.Hostname(), port))] = true
	}
	return out
}
