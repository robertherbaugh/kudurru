// Package parser reads block-list sources (hosts files, domain lists,
// adblock-style DNS filter lists and regex lists) into rules for the
// matchers in internal/blocking/matcher.
//
// # Clean-room notice
//
// The adblock grammar is implemented from the public AdGuard DNS filtering
// syntax specification (https://adguard-dns.io/kb/general/dns-filtering-syntax/)
// and from the behaviour of common lists. No code from AdGuard Home or
// urlfilter (both GPL-3.0) was read or used; see CONTRIBUTING.md.
//
// # Grammar and decisions
//
// One line parser serves the hosts, domains and adblock formats, because the
// specification lets every adblock consumer read hosts and domains lines and
// real lists mix them. The format only changes comment handling for regex
// lists and is reported back to the UI. The regex format reads one RE2
// expression per line, with or without /.../ delimiters.
//
//	line            meaning                                         rule
//	||x^  ||x  ||x|   x and every subdomain of x                    KindSuffix  SyntaxAdblock
//	||*.x^            x and every subdomain of x (see below)        KindSuffix  SyntaxAdblockWildcard
//	*.x  *.x^         x and every subdomain of x (see below)        KindSuffix  SyntaxWildcard
//	|x^  |x|          exactly x                                     KindExact   SyntaxAdblockExact
//	x                 exactly x (domains-only and plain host rules) KindExact   SyntaxDomain
//	0.0.0.0 x y       exactly x and exactly y                       KindExact   SyntaxHosts
//	/re/              hostnames matching the RE2 expression re      KindRegex   SyntaxRegex
//	192.0.2.1 ||192.0.2.1^  answers containing that address         KindIP      SyntaxIP
//	@@<any of these>  exception (Rule.Exception)
//	! ...  # ...  [Adblock Plus 2.0]   comments
//
// Decisions recorded here and in docs/spikes/blocking.md:
//
//   - Hosts and domains entries match exactly, never subdomains, as in
//     AdGuard Home, Pi-hole and blocky. Options.HostsSubdomains turns them
//     into suffix entries for "domains with subdomains" lists.
//   - *.x matches x itself as well as its subdomains, like ||x^. This
//     follows blocky and the lists published in the *.domain wildcard
//     format for it; a literal reading of the adblock grammar would exclude
//     x itself.
//   - ||x without the closing ^ is read as ||x^. DNS lists never mean the
//     substring match ("x.evil", "xample.organic") a browser engine would
//     apply.
//   - Only 0.0.0.0, ::, 127.0.0.0/8 and ::1 make a hosts line a block. Any
//     other address is a rewrite in AdGuard Home's semantics and is counted
//     as unsupported (ReasonHostsRewrite) until rewrites land in v0.2.
//     Standard hosts-file boilerplate (localhost, broadcasthost,
//     ip6-allnodes, ...) is counted as ignored rather than blocked.
//   - Every rule with modifiers ($important, $badfilter, $dnstype,
//     $dnsrewrite, $client, $ctag, $denyallow, $respgeo) and every pattern
//     with a mid-pattern wildcard or unanchored start is parsed, kept with
//     its tokenised modifiers in Result.UnsupportedRules, and counted as
//     unsupported rather than dropped silently. A rule with a modifier the
//     specification does not define is invalid and ignored as a whole, so
//     browser lists such as EasyList cannot block by accident.
//   - Cosmetic and HTML-filtering rules (##, #@#, $$, ...), URL rules with a
//     path and malformed lines are counted as invalid with a reason.
//   - Names are lower-cased, a trailing dot is dropped, and non-ASCII names
//     are converted to their IDNA A-label form (UTS #46 lookup mapping).
//     Labels may contain letters, digits, '-' and '_' and be up to 63 bytes;
//     names may be up to 253 bytes.
//
// Parse streams rules to a callback, so a multi-million-line list never
// exists as a slice of rules. It refuses HTML pages and binary bodies
// (ErrHTML, ErrBinary), extracts "! Title:" and "# Title:" headers, and
// computes the CRC-32 of the normalised rule lines, which Checksum computes
// without parsing so that unchanged lists can skip recompilation.
package parser
