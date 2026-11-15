### Fixed

- A query-log filter (client, domain, upstream, list, zone, policy, or a suggestion prefix) that contains a NUL byte is now refused with 400 instead of matching every row (a wildcard such as `*%00` was evaluated as `*`) or failing on the central PostgreSQL log.
- A query signed with a wrong, replaced or expired TSIG key now gets its NOTAUTH (BADKEY, BADSIG or BADTIME) reply when it arrives between a commit and the node's next publication, as ordinary answers do; before, it got no reply and the client timed out. Refusals of UPDATE, NOTIFY and zone transfers are unchanged.
