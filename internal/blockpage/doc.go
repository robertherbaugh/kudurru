// Package blockpage serves the block page (docs/deploy/blocking-page.md).
//
// Under the blockPage block response (BlockList spec.response, Policy
// spec.blockMode or blocking.defaultResponse) the blocking stage answers
// blocked A and AAAA queries with blockPage.ipv4/ipv6, the address of this
// listener. A browser that follows the answer lands here and gets a
// self-contained, kudurru-branded page: the blocked name (the Host header,
// or the TLS server name), why it was blocked (looked up in the recent query
// log by client address and name, else evaluated against the client's
// current policy), the client address, the node, the time and the
// administrator contact. Every value is plain text escaped by html/template;
// the page loads nothing but its favicon (the logos are inlined data: URIs,
// the style sheet is allowed by its hash), runs no script and cannot be
// framed.
//
// The listener answers every path of every Host with the page (GET and
// HEAD 200, other methods 405), /favicon.ico, /robots.txt (disallow all)
// and /health, rate-limits each client (blockPage.rateLimit per minute,
// per address or IPv6 /64) and never proxies or fetches anything.
//
// HTTPS (listeners.blockPage.https) can only ever present a certificate
// that is not valid for the blocked name (self-signed into
// <node.dataDir>/keys/blockpage.crt, or the configured one), so browsers
// show a certificate warning first, and sites on the HSTS preload list or
// with a remembered HSTS policy give no way past it at all. Plain HTTP is
// the case that works; most HTTPS visits to a blocked name end in the
// browser's own error page. The page never sends Strict-Transport-Security.
package blockpage
