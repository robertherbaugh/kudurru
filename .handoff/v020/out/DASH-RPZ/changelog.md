### Added

- Dashboard: a block list with RPZ feeds shows an "RPZ feeds on this node" section on its page. For each feed it shows the state the answering node reports (pending, serving, expired, unavailable), a plain SERVFAIL warning for every state but serving (pending included), the reason and message of the node's diagnosis, the next transfer attempt, serial, primary, last confirmation, expiry, last error, the compiled rules by trigger and action, and a table of ignored reasons with counts and example owners. It names the node, rereads the state after every list or source change and every few seconds while a feed is not serving (every 30 seconds otherwise), and flags a feed that answers SERVFAIL on this node beside the list's shared health badge.
- Dashboard: the Lists page shows the worst RPZ feed state of each block list with feeds under its health.
- Dashboard: RPZ feed sources can be added with **Add source → RPZ feed** on a block list's page and with **Add an RPZ feed** in the new-list wizard (policy zone, primaries in order, TCP or TLS, optional TSIG key name).

### Fixed

- Dashboard: an RPZ source is no longer shown as an inline source ("ConfigMap" or "0 inline entries", "inline, nothing to fetch"); the sources table names its zone and primaries and offers no entries browser for it. A list whose only fetched sources are RPZ feeds shows its last refresh as "per node" and no refresh interval, since feeds follow their zone's SOA timers.
