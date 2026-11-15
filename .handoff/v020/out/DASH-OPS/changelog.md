### Added

- Dashboard: **Block at edge** on every address row of **Clients** and on a client's page blocks the client at every dnsdist edge for a chosen time (one minute to seven days) with an optional reason. **Edge blocks** under the client list shows the active blocks with their expiry, reason and author, and **Revoke** lifts one early. The actions need `edgeblocks.write` and are disabled on an enrolled follower. When the outcome of a block or revoke is unknown, the dashboard says so and does not send the command again.
- Dashboard: **Query-log sinks** on **Nodes** shows each `querylog.sinks` destination of the node: its type, queue and bound, accepted and dropped queries, failed attempts and last success. Raw dnstap sinks are marked. The panel never shows a sink address, credential or error text.
- Dashboard: **New role binding** on **Access > Identity** creates a RoleBinding on the SQLite driver. Its scope picker offers the whole cluster, one zone or a zone group. When the server cannot say whether the binding was created, the dialog says so and does not send it again. API tokens can also be limited to a zone group.

### Changed

- Dashboard: zone and zone-group grants from `/auth/me` now enable the matching record, NOTIFY, delete and settings actions on that zone's pages. Before, only cluster-wide permissions counted. **Zones** and a zone's page show the zone group.
- Dashboard: where `/auth/status` reports that the driver has no authenticator management (Kubernetes), the sign-in page has no authenticator code field, the **Authenticator** section says that no authenticators are offered, and **Identity** has no authenticator recovery.
