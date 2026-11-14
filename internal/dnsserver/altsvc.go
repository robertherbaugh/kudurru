package dnsserver

import (
	"net"
	"net/http"
	"strings"
)

// connectHTTP3Alternatives runs after every bind and before any server starts.
// Only identical native endpoints are interchangeable: address/port, TLS key
// pair, path and client-ID policy must match. In particular, independently
// chosen ephemeral ports or an HTTP/3 service on another interface do not
// establish a reachable alternative for the original HTTPS authority.
func (r *Registry) connectHTTP3Alternatives() {
	for _, listener := range r.listeners {
		doh, ok := listener.(*dohListener)
		if !ok {
			continue
		}
		doh.handler.altSvc = nil
		for _, candidate := range r.listeners {
			h3, ok := candidate.(*doh3Listener)
			if !ok || doh.Addr().String() != h3.Addr().String() ||
				doh.spec.Path != h3.spec.Path || doh.spec.ClientIDs != h3.spec.ClientIDs ||
				doh.spec.CertFile != h3.spec.CertFile || doh.spec.KeyFile != h3.spec.KeyFile {
				continue
			}
			_, port, err := net.SplitHostPort(h3.Addr().String())
			if err != nil {
				continue
			}
			doh.handler.altSvc = func(req *http.Request) string {
				if req.TLS == nil || !h3.Ready() || !directHTTPSAuthority(req, port) {
					return ""
				}
				return `h3=":` + port + `"; ma=60`
			}
			break
		}
	}
}

// Forwarding headers never grant authority to advertise a native UDP socket.
// Reverse proxies own their externally reachable HTTP/3 advertisement. Even
// without these headers, a different public HTTPS port is not interchangeable.
func directHTTPSAuthority(req *http.Request, port string) bool {
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Port", "X-Forwarded-Proto"} {
		if len(req.Header.Values(name)) > 0 {
			return false
		}
	}
	if req.Host == "" {
		return false
	}
	_, requestPort, err := net.SplitHostPort(req.Host)
	if err != nil {
		// A DNS name or bracketed IPv6 literal without a port uses 443.
		if strings.Contains(req.Host, ":") && (!strings.HasPrefix(req.Host, "[") || !strings.HasSuffix(req.Host, "]")) {
			return false
		}
		requestPort = "443"
	}
	return requestPort == port
}
