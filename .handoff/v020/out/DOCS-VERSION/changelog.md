# DOCS-VERSION: lines for `CHANGELOG.md` `[0.2.0]`

### Changed

- The guides and the documentation site describe v0.2.0 ("clay copies").
  The migration comparison and the Pi-hole, AdGuard Home and Technitium
  guides compare against v0.2.0: secondary and stub zones, RFC 2136 updates,
  opt-in validation of forwarded answers, the adblock modifiers `$dnstype`,
  `$dnsrewrite`, `$client`, `$ctag`, `$important`, `$badfilter` and
  `$denyallow`, per-policy rewrites, client IDs on DoT, DoQ and DoH, and the
  enrolled SQLite cluster are no longer listed as deferred or missing. The
  Technitium guide has a new section for secondary and stub zones.
- The administrator guide explains what the client ID and MAC address
  matchers of a client group need (they were "Reserved"), the three DNSSEC
  choices of a forward zone, and temporary validation exceptions.
  `docs/deploy/client-identity.md` has a table of where each matcher gets its
  identity.

### Fixed

- `docs/deploy/kustomize.md` said that releases up to v0.1.0 keep the
  kustomize tree under `config/`. The v0.1.0 tag has
  `deploy/kustomize/overlays/<name>`; only the release candidates up to
  `v0.1.0-rc.6` use `config/overlays/<name>`.
- The migration guides said that `kudurru query` "must show an allow-list
  decision" for an allowed name. It shows the status `forwarded` or `cached`
  and no list; the guides now say so. The Pi-hole guide no longer says that
  kudurru has no local DNSSEC validation.
- `docs/architecture/zones.md` no longer says that a SQLite node has no
  `Unsupported` verdict for an unserved zone type; the controller writes it
  there too, and the API cannot show it.
- The client identity page, the shared-address runbook and the AdGuard Home
  guide say that an IP, MAC, CIDR, hostname or name-glob matcher that matches
  the arriving address wins over a client ID.

### Only while the three product defects of the ledger are not fixed

Drop the matching line, and the "in this version" sentences that the ledger
lists for it, when a defect is fixed before the release.

- Known limitation: on the SQLite store one run of `kudurru apply` applies
  one new or changed document of a file and refuses the later ones with
  `unavailable`. Repeat the command until it reports no error (cutover
  checklist, "Apply results").
- Known limitation: do not apply a `DNSZone` of the type `external`,
  `catalog` or `secondaryCatalog`. These types are in the schema and are not
  served, and a node with the SQLite store that accepts one stops answering
  for its own zones and refuses every API request until its data directory is
  put back from an earlier copy. After a restart in that state the node
  answers SERVFAIL for every name, forwarded names included; do not restart
  it.
- Known limitation: on the SQLite store, a `BlockList` with an enabled URL
  source that the node has never downloaded (host unreachable, no outbound
  HTTPS, 404) has the same effect, even when no policy refers to the list,
  until a later download attempt succeeds. This includes lists created by
  `kudurru import ... --apply`. Download each list URL once from the kudurru
  host before applying it, apply lists with URLs one at a time, and keep a
  copy of the data directory (migration overview and cutover checklist,
  "Apply results").
