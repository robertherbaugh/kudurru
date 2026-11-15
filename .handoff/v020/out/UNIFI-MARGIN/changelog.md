### Fixed

- Integrations: periodic syncs now start one poll interval after the previous sync began, not after it finished. On a busy node, waiting for the previous observation's publication could push the next UniFi poll past the three-poll lifetime of a client's address assignment. The client then briefly lost its hostname identity and policy between two healthy polls. Syncs still never overlap, and a provider is still polled at most once per interval.
- UniFi inventory: an observation is no longer refused because the target zone's status changed while it was being acquired. Kudurru updates that status itself after every record change. Before, the update could cost a whole poll of freshness.
- Integrations on SQLite nodes now start polling once the node has published its first complete serving state. A UniFi acquisition at startup no longer delays the node's readiness.
- DNS client identification: when the first identification attempt for a query found no policy index yet, a later attempt with a `mac`-matcher index now still fills the query's MAC from the DHCP lease.
