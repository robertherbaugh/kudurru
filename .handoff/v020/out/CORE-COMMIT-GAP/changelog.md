### Fixed

- A zone keeps answering while a change to its own `DNSZone` (query ACL,
  name servers, selector) is being applied. A primary zone answers its last
  committed records under the new query access rules (zone transfers wait for
  the new configuration), and a forward zone uses its new configuration.
  Before, the zone answered SERVFAIL for up to about 0.3 seconds after each
  such write.
- A DNS query that arrives just before the node publishes a configuration
  change now gets its answer from the data it arrived on. Before,
  about one query in a few thousand around each publication got no reply
  and the client timed out.
