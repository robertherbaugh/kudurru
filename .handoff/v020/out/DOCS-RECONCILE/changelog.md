### Added

- A new operator page, `docs/deploy/capabilities.md`, shows what works on each deployment shape (standalone, static, enrolled primary and follower, fenced or conflicted, Kubernetes). It also has a glossary of every refusal key that names a deployment limit, with its HTTP status, meaning and next step.

### Changed

- The migration guides, the cutover checklist and the admin guide now describe this release's behavior. One `kudurru apply` applies every document. On SQLite the API refuses a zone of an unserved type or an update policy kudurru cannot use. A block list whose URL cannot be downloaded serves no rules and reports the error. The earlier warnings said such a list stops the node, and told you to check each URL with curl first; both are removed.
- `docs/deploy/dynamic-updates.md` lists every update-policy shape that the SQLite store refuses at write, including a `*` inside a name, an empty or malformed `tsigKeyRef` and more than 4,096 grants.
- The enrolled-cluster page now matches the follower contract. Sign-out and cluster settings are forwarded, and refused changes answer `503 primary_required`. `kudurru cluster status` shows the role and the primary. Repeating a cut apply through a follower does not reliably finish it.

### Fixed

- The client-identity page, the gateway runbook and the AdGuard Home guide said that every CIDR group on the arriving address hides a client ID. A catch-all `0.0.0.0/0` or `::/0` group does not hide one.
- The Enterprise page no longer lists the Cloudflare and UniFi integrations as shipped. They stay on the roadmap, and this version runs a partial form of them on a licensed SQLite node only.
