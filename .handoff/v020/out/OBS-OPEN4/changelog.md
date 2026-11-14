### Changed

- Tracing: traces kept only because a query failed or was slow are now capped at 100 in any one second (a sliding second), so a burst across a second boundary can no longer keep twice that. A failed or slow trace that is dropped because the export queue is full no longer uses up part of the cap, and returning its share never frees another trace's.
- dnstap listen: a listener whose connections cannot be accepted (for example, too many open files) now logs at most one warning a minute, with the number of failures since the previous warning, instead of one a second. Every failure is still counted in `kudurru_querylog_dnstap_connections_total{result="accept_failed"}`.

### Fixed

- dnstap listen: a response message with a correct response time but a wildly wrong query time was dated from the wrong time, and a relayed response to a query kudurru had already logged could be recorded a second time. Each time in a message is now checked on its own and corrected before the row is dated and checked for duplicates.
- With tracing off, a query again allocates exactly as much memory as it did before tracing was added (the per-query request had grown by 32 bytes).
