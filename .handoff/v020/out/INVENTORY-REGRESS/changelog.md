### Changed

- DNS client identification reads the built-in DHCP server's lease MAC for a query only when some `ClientGroup` has a `mac` matcher, so servers without MAC-based groups do no extra lease lookup or allocation per query.
