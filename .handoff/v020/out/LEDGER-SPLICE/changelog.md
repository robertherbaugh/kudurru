# LEDGER-SPLICE: changelog

No bullets of its own: this package folded the other packages' bullets into
`CHANGELOG.md` `[0.2.0]` (commit `a89ff890`).
The review fix (commit after `a89ff890`) qualifies the Fixed bullet on zone
serials after an upgrade from 0.1 on SQLite and adds an Upgrade notes bullet
on names with two CNAME or two DNAME targets (the kept target's own TTL, a
new serial).
