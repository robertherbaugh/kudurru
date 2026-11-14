# DOCS-ARCH: lines for `CHANGELOG.md` `[0.2.0]`

### Changed

- The architecture tour (`docs/architecture/`) now describes v0.2.0 as built.
  Its index lists the integrations and stopped-source rollup pages and gains a
  table of what behaves differently on the SQLite and Kubernetes store drivers
  (RPZ feeds, the integration host, RFC 2136, stored TSIG keys, the blocking
  toggle, settings writes, TOTP, sessions, RoleBinding writes, user and
  external-account administration, enrollment and promotion). Statements that
  enrolled SQLite membership, follower forwarding, Kubernetes TSIG management,
  Kubernetes RFC 2136, RPZ evaluation, static read-only twins and MAC, hostname
  and client-ID matching were unfinished have been replaced with how they work.
- The architecture tour no longer describes a SQLite node as always the
  leader. The leader is the node with durable write authority: a standalone
  node, each static twin, or the enrolled primary. The tour also says where the
  objects loader runs: on every replica on Kubernetes and only on the leader on
  SQLite, so an enrolled follower never applies its local `objects:` and
  `objectsDir` files. The overview, configuration-flow and policy-decision
  diagrams carry the same corrections, and the overview says that it draws
  seven of the ten pipeline stages.
- New architecture sections: local sign-in on an enrolled SQLite follower, how
  an external (OIDC) session is re-evaluated on every request without
  contacting the identity provider, and static twins compared with an enrolled
  SQLite cluster.

### Fixed

- Eleven sequence diagrams in the architecture tour did not render on the docs
  site or on GitHub (a semicolon in message text ends a Mermaid statement, and
  one diagram's closing fence carried trailing text that swallowed the next
  paragraphs). All 108 diagrams of the tour now parse with the bundled Mermaid.
- The dashboard no longer says that MAC address, host name and name pattern
  matchers of a client group "never match" or "arrive in v0.2". The help text
  now names the sources each needs: an inventory integration for MAC
  addresses; built-in DHCP, an inventory integration or reverse-PTR lookup for
  names.
- The settings help for an identity provider's `cliClientID` no longer refers
  to `kudurru login` as a v0.2 feature.
- The settings help for the `zonegroup` role-binding scope and the RoleBinding
  CRD reference no longer say that zone groups arrive in v0.2. Zone groups are
  not implemented: the `zonegroup` scope (like `clientgroup`) is accepted but
  reserved, and a binding with it grants nothing.
